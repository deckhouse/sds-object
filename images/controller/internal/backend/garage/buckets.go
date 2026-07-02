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

package garage

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
	"github.com/deckhouse/sds-object/images/controller/internal/backend"
	"github.com/deckhouse/sds-object/images/controller/internal/backend/s3util"
)

// EnsureBucket creates the bucket and an access key scoped to it, and reports
// the credentials so the reconciler can (re)write the bucket's Secret. It is
// idempotent: an existing bucket is reused, and an access key is only created
// when the bucket's credentials Secret does not already reference a live key.
func (d *Driver) EnsureBucket(ctx context.Context, cluster *v1alpha1.ObjectStorageCluster, bucket *v1alpha1.ObjectBucket) (backend.BucketState, error) {
	token, err := d.adminToken(ctx, cluster)
	if err != nil {
		return backend.BucketState{}, err
	}
	if token == "" {
		return backend.BucketState{Message: "Garage admin token is not provisioned yet"}, nil
	}
	svc := newAdminClient(adminEndpoint(cluster, d.namespace, d.clusterDomain), token)

	name := bucketDisplayName(bucket)

	// Ensure the bucket exists (reuse an existing one with the same alias).
	b, found, err := svc.getBucketByAlias(ctx, name)
	if err != nil {
		return backend.BucketState{}, fmt.Errorf("look up bucket %q: %w", name, err)
	}
	if !found {
		b, err = svc.createBucket(ctx, name)
		if err != nil {
			return backend.BucketState{}, fmt.Errorf("create bucket %q: %w", name, err)
		}
	}

	// Reuse the access key from the existing credentials Secret when it still
	// references a live key; otherwise mint a new one (the secret access key
	// cannot be recovered from Garage after creation).
	accessKeyID, secretAccessKey, err := d.existingCreds(ctx, bucket)
	if err != nil {
		return backend.BucketState{}, err
	}
	if accessKeyID != "" {
		exists, err := svc.keyExists(ctx, accessKeyID)
		if err != nil {
			return backend.BucketState{}, fmt.Errorf("check access key: %w", err)
		}
		if !exists {
			accessKeyID, secretAccessKey = "", ""
		}
	}
	if accessKeyID == "" {
		key, err := svc.createKey(ctx, keyDisplayName(bucket, name))
		if err != nil {
			return backend.BucketState{}, fmt.Errorf("create access key: %w", err)
		}
		accessKeyID, secretAccessKey = key.AccessKeyID, key.SecretAccessKey
	}

	// Grant the key read/write on the bucket (idempotent).
	if err := svc.allow(ctx, b.ID, accessKeyID, permissions{Read: true, Write: true}); err != nil {
		return backend.BucketState{}, fmt.Errorf("grant key on bucket: %w", err)
	}

	return backend.BucketState{
		Ready:           true,
		Message:         "bucket and access key provisioned",
		BucketName:      name,
		AccessKeyID:     accessKeyID,
		SecretAccessKey: secretAccessKey,
	}, nil
}

// DeleteBucket removes the access key and, when the reclaim policy is Delete,
// the bucket itself. It is idempotent and tolerates an already-deleted cluster.
func (d *Driver) DeleteBucket(ctx context.Context, cluster *v1alpha1.ObjectStorageCluster, bucket *v1alpha1.ObjectBucket) error {
	token, err := d.adminToken(ctx, cluster)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil // cluster secret gone: nothing to clean up
		}
		return err
	}
	if token == "" {
		return nil
	}
	svc := newAdminClient(adminEndpoint(cluster, d.namespace, d.clusterDomain), token)

	accessKeyID, secretKey, err := d.existingCreds(ctx, bucket)
	if err != nil {
		return err
	}

	if bucket.Spec.ReclaimPolicy == v1alpha1.BucketReclaimDelete {
		name := bucketDisplayName(bucket)

		// Garage's admin DELETE /v1/bucket refuses a non-empty bucket
		// (409 BucketNotEmpty), so empty it over S3 first. This uses the
		// bucket's own credentials and therefore must run before the access
		// key is deleted below.
		if accessKeyID != "" && secretKey != "" {
			mc, cerr := s3util.NewClient(s3HostPort(cluster, d.namespace, d.clusterDomain), accessKeyID, secretKey)
			if cerr != nil {
				return fmt.Errorf("build S3 client to empty bucket %q: %w", name, cerr)
			}
			if eerr := s3util.EmptyBucket(ctx, mc, name); eerr != nil {
				return fmt.Errorf("empty bucket %q before delete: %w", name, eerr)
			}
		}

		b, found, err := svc.getBucketByAlias(ctx, name)
		if err != nil {
			return fmt.Errorf("look up bucket %q: %w", name, err)
		}
		if found {
			if err := svc.deleteBucket(ctx, b.ID); err != nil {
				return fmt.Errorf("delete bucket %q: %w", name, err)
			}
		}
	}

	// Delete the access key last: emptying the bucket above needs it.
	if accessKeyID != "" {
		if err := svc.deleteKey(ctx, accessKeyID); err != nil {
			return fmt.Errorf("delete access key: %w", err)
		}
	}
	return nil
}

// bucketDisplayName is the S3 bucket name: spec.bucketName, or metadata.name.
func bucketDisplayName(bucket *v1alpha1.ObjectBucket) string {
	if bucket.Spec.BucketName != "" {
		return bucket.Spec.BucketName
	}
	return bucket.Name
}

// keyDisplayName is the human-readable name given to the Garage access key.
func keyDisplayName(bucket *v1alpha1.ObjectBucket, bucketName string) string {
	return fmt.Sprintf("%s.%s", bucket.Namespace, bucketName)
}

// existingCreds reads the access key id and secret from the bucket's
// credentials Secret, returning empty strings when it does not exist yet.
//
// The Secret name must match credentialsSecretName in the controller package
// (<bucket>-s3-credentials).
func (d *Driver) existingCreds(ctx context.Context, bucket *v1alpha1.ObjectBucket) (string, string, error) {
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: bucket.Namespace, Name: bucket.Name + "-s3-credentials"}
	// Read through the non-cached APIReader: a stale cache here would make us
	// mint a fresh access key on every reconcile until the cache catches up.
	if err := d.apiReader.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", nil
		}
		return "", "", err
	}
	return string(secret.Data[v1alpha1.SecretKeyAccessKeyID]), string(secret.Data[v1alpha1.SecretKeySecretAccessID]), nil
}
