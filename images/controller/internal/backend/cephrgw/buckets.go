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

// EnsureBucket provisions the per-bucket owner RGW user (CephObjectStoreUser →
// Rook issues its keys in a Secret) and creates the bucket via the S3 API with
// that key. Per-access credentials are issued separately (see access.go);
// cross-user access is granted through the bucket policy.
func (d *Driver) EnsureBucket(ctx context.Context, cluster *v1alpha1.ObjectStore, bucket *v1alpha1.Bucket) (backend.BucketState, error) {
	if err := d.ensureUser(ctx, cluster, ownerUID(bucket)); err != nil {
		if isNoMatch(err) {
			return backend.BucketState{Message: "CephObjectStoreUser CRD not found; is the sds-elastic module installed?"}, nil
		}
		return backend.BucketState{}, fmt.Errorf("ensure owner CephObjectStoreUser: %w", err)
	}

	accessKey, secretKey, err := d.userKeys(ctx, cluster, ownerUID(bucket))
	if err != nil {
		return backend.BucketState{}, err
	}
	if accessKey == "" || secretKey == "" {
		return backend.BucketState{Message: "waiting for Rook to issue the bucket owner credentials"}, nil
	}

	name := backend.BucketDisplayName(bucket)
	mc, err := s3util.NewClient(rgwHostPort(cluster, d.clusterDomain), accessKey, secretKey)
	if err != nil {
		return backend.BucketState{}, fmt.Errorf("build S3 client: %w", err)
	}
	if err := s3util.EnsureBucket(ctx, mc, name, s3Region); err != nil {
		return backend.BucketState{}, err
	}

	return backend.BucketState{Ready: true, Message: "bucket provisioned", BucketName: name}, nil
}

// DeleteBucket removes the bucket (when reclaimPolicy=Delete) and the owner
// CephObjectStoreUser. Idempotent.
func (d *Driver) DeleteBucket(ctx context.Context, cluster *v1alpha1.ObjectStore, bucket *v1alpha1.Bucket) error {
	if bucket.Spec.ReclaimPolicy == v1alpha1.BucketReclaimDelete {
		accessKey, secretKey, err := d.userKeys(ctx, cluster, ownerUID(bucket))
		if err != nil {
			return err
		}
		if accessKey != "" && secretKey != "" {
			mc, err := s3util.NewClient(rgwHostPort(cluster, d.clusterDomain), accessKey, secretKey)
			if err != nil {
				return fmt.Errorf("build S3 client: %w", err)
			}
			if err := s3util.DeleteBucket(ctx, mc, backend.BucketDisplayName(bucket)); err != nil {
				return err
			}
		}
	}

	return d.deleteUser(ctx, ownerUID(bucket))
}

// ensureUser creates or updates the CephObjectStoreUser with the given uid.
func (d *Driver) ensureUser(ctx context.Context, cluster *v1alpha1.ObjectStore, uid string) error {
	desired := buildCephObjectStoreUser(cluster, uid)
	if err := controllerutil.SetControllerReference(cluster, desired, d.client.Scheme()); err != nil {
		return err
	}

	existing := newUnstructured(cephObjectStoreUserGVK)
	if err := d.apiReader.Get(ctx, client.ObjectKey{Namespace: elasticNamespace, Name: uid}, existing); err != nil {
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

// deleteUser removes the CephObjectStoreUser with the given uid (idempotent).
func (d *Driver) deleteUser(ctx context.Context, uid string) error {
	user := newUnstructured(cephObjectStoreUserGVK)
	user.SetNamespace(elasticNamespace)
	user.SetName(uid)
	if err := d.client.Delete(ctx, user); err != nil && !apierrors.IsNotFound(err) && !isNoMatch(err) {
		return fmt.Errorf("delete CephObjectStoreUser %q: %w", uid, err)
	}
	return nil
}

// userKeys reads the access/secret key from the Rook-generated user Secret for
// the given uid (empty strings when it does not exist yet).
func (d *Driver) userKeys(ctx context.Context, cluster *v1alpha1.ObjectStore, uid string) (string, string, error) {
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: elasticNamespace, Name: rgwUserSecretName(cluster, uid)}
	if err := d.apiReader.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", nil
		}
		return "", "", err
	}
	return string(secret.Data[rookSecretAccessKey]), string(secret.Data[rookSecretSecretKey]), nil
}
