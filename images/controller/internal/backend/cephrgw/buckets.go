/*
Copyright 2026 Flant JSC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cephrgw

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
	"github.com/deckhouse/sds-object/images/controller/internal/backend"
	"github.com/deckhouse/sds-object/images/controller/internal/backend/s3util"
)

// Keys in the Rook-generated CephObjectStoreUser secret.
const (
	rookSecretAccessKey = "AccessKey"
	rookSecretSecretKey = "SecretKey"
)

// EnsureBucket provisions an RGW user (CephObjectStoreUser → Rook issues its
// access/secret key in a Secret) and creates the bucket via the S3 API with
// that key. The reconciler then writes the standard credentials Secret. Rook
// owns the key, so it is stable across reconciles.
func (d *Driver) EnsureBucket(ctx context.Context, cluster *v1alpha1.ObjectStorageCluster, bucket *v1alpha1.ObjectBucket) (backend.BucketState, error) {
	if err := d.ensureUser(ctx, cluster, bucket); err != nil {
		if isNoMatch(err) {
			return backend.BucketState{Message: "CephObjectStoreUser CRD not found; is the sds-elastic module installed?"}, nil
		}
		return backend.BucketState{}, fmt.Errorf("ensure CephObjectStoreUser: %w", err)
	}

	accessKey, secretKey, err := d.userKeys(ctx, cluster, bucket)
	if err != nil {
		return backend.BucketState{}, err
	}
	if accessKey == "" || secretKey == "" {
		return backend.BucketState{Message: "waiting for Rook to issue RGW user credentials"}, nil
	}

	name := bucketDisplayName(bucket)
	mc, err := s3util.NewClient(rgwHostPort(cluster, d.clusterDomain), accessKey, secretKey)
	if err != nil {
		return backend.BucketState{}, fmt.Errorf("build S3 client: %w", err)
	}
	if err := s3util.EnsureBucket(ctx, mc, name, s3Region); err != nil {
		return backend.BucketState{}, err
	}

	return backend.BucketState{
		Ready:           true,
		Message:         "bucket and access key provisioned",
		BucketName:      name,
		AccessKeyID:     accessKey,
		SecretAccessKey: secretKey,
	}, nil
}

// DeleteBucket removes the bucket (when reclaimPolicy=Delete) and the
// CephObjectStoreUser (which revokes the key). Idempotent.
func (d *Driver) DeleteBucket(ctx context.Context, cluster *v1alpha1.ObjectStorageCluster, bucket *v1alpha1.ObjectBucket) error {
	if bucket.Spec.ReclaimPolicy == v1alpha1.BucketReclaimDelete {
		// Use the user's key (while it still exists) to empty + remove the bucket.
		accessKey, secretKey, err := d.userKeys(ctx, cluster, bucket)
		if err != nil {
			return err
		}
		if accessKey != "" && secretKey != "" {
			mc, err := s3util.NewClient(rgwHostPort(cluster, d.clusterDomain), accessKey, secretKey)
			if err != nil {
				return fmt.Errorf("build S3 client: %w", err)
			}
			if err := s3util.DeleteBucket(ctx, mc, bucketDisplayName(bucket)); err != nil {
				return err
			}
		}
	}

	user := newUnstructured(cephObjectStoreUserGVK)
	user.SetNamespace(elasticNamespace)
	user.SetName(userName(bucket))
	if err := d.client.Delete(ctx, user); err != nil && !apierrors.IsNotFound(err) && !isNoMatch(err) {
		return fmt.Errorf("delete CephObjectStoreUser: %w", err)
	}
	return nil
}

// ensureUser creates or updates the CephObjectStoreUser for the bucket.
func (d *Driver) ensureUser(ctx context.Context, cluster *v1alpha1.ObjectStorageCluster, bucket *v1alpha1.ObjectBucket) error {
	desired := buildCephObjectStoreUser(cluster, bucket)
	if err := controllerutil.SetControllerReference(cluster, desired, d.client.Scheme()); err != nil {
		return err
	}

	existing := newUnstructured(cephObjectStoreUserGVK)
	if err := d.apiReader.Get(ctx, client.ObjectKey{Namespace: elasticNamespace, Name: userName(bucket)}, existing); err != nil {
		if apierrors.IsNotFound(err) {
			return d.client.Create(ctx, desired)
		}
		return err
	}
	existing.Object["spec"] = desired.Object["spec"]
	existing.SetLabels(desired.GetLabels())
	existing.SetOwnerReferences(desired.GetOwnerReferences())
	return d.client.Update(ctx, existing)
}

// userKeys reads the access/secret key from the Rook-generated user Secret
// (empty strings when it does not exist yet).
func (d *Driver) userKeys(ctx context.Context, cluster *v1alpha1.ObjectStorageCluster, bucket *v1alpha1.ObjectBucket) (string, string, error) {
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: elasticNamespace, Name: rookUserSecretName(cluster, bucket)}
	if err := d.apiReader.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", nil
		}
		return "", "", err
	}
	return string(secret.Data[rookSecretAccessKey]), string(secret.Data[rookSecretSecretKey]), nil
}
