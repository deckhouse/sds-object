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

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
	"github.com/deckhouse/sds-object/images/controller/internal/backend"
	"github.com/deckhouse/sds-object/images/controller/internal/backend/s3util"
)

// EnsureBucket creates the bucket in Garage (no credentials — access keys are
// issued per BucketAccess). Idempotent: an existing bucket with
// the same alias is reused.
func (d *Driver) EnsureBucket(ctx context.Context, cluster *v1alpha1.ObjectStore, bucket *v1alpha1.Bucket) (backend.BucketState, error) {
	svc, ready, err := d.adminClientFor(ctx, cluster)
	if err != nil {
		return backend.BucketState{}, err
	}
	if !ready {
		return backend.BucketState{Message: "Garage admin token is not provisioned yet"}, nil
	}

	name := backend.BucketDisplayName(bucket)
	if _, found, err := svc.getBucketByAlias(ctx, name); err != nil {
		return backend.BucketState{}, fmt.Errorf("look up bucket %q: %w", name, err)
	} else if !found {
		if _, err := svc.createBucket(ctx, name); err != nil {
			return backend.BucketState{}, fmt.Errorf("create bucket %q: %w", name, err)
		}
	}

	return backend.BucketState{Ready: true, Message: "bucket provisioned", BucketName: name}, nil
}

// DeleteBucket removes the bucket when the reclaim policy is Delete. It is
// idempotent and tolerates an already-deleted cluster. Access keys are removed
// separately by DeleteAccess.
func (d *Driver) DeleteBucket(ctx context.Context, cluster *v1alpha1.ObjectStore, bucket *v1alpha1.Bucket) error {
	if bucket.Spec.ReclaimPolicy != v1alpha1.BucketReclaimDelete {
		return nil
	}

	svc, ready, err := d.adminClientFor(ctx, cluster)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil // cluster secret gone: nothing to clean up
		}
		return err
	}
	if !ready {
		return nil
	}

	name := backend.BucketDisplayName(bucket)
	b, found, err := svc.getBucketByAlias(ctx, name)
	if err != nil {
		return fmt.Errorf("look up bucket %q: %w", name, err)
	}
	if !found {
		return nil
	}

	// Garage's admin DELETE /v1/bucket refuses a non-empty bucket (409
	// BucketNotEmpty), so empty it over S3 first via a short-lived owner key.
	if err := d.emptyBucket(ctx, cluster, svc, b.ID, name); err != nil {
		return err
	}

	if err := svc.deleteBucket(ctx, b.ID); err != nil {
		return fmt.Errorf("delete bucket %q: %w", name, err)
	}
	return nil
}

// emptyBucket removes all objects from the bucket using a temporary owner key
// that is revoked afterwards. The bucket has no long-lived credentials of its
// own (those belong to per-access keys, which may already be gone by the time
// the bucket is reclaimed).
func (d *Driver) emptyBucket(ctx context.Context, cluster *v1alpha1.ObjectStore, svc *adminClient, bucketID, name string) error {
	key, err := svc.createKey(ctx, "sds-object-reclaim-"+name)
	if err != nil {
		return fmt.Errorf("create temporary key to empty bucket %q: %w", name, err)
	}
	defer func() { _ = svc.deleteKey(ctx, key.AccessKeyID) }()

	if err := svc.allow(ctx, bucketID, key.AccessKeyID, permissions{Read: true, Write: true, Owner: true}); err != nil {
		return fmt.Errorf("grant temporary key on bucket %q: %w", name, err)
	}
	mc, err := s3util.NewClient(s3HostPort(cluster, d.namespace, d.clusterDomain), key.AccessKeyID, key.SecretAccessKey)
	if err != nil {
		return fmt.Errorf("build S3 client to empty bucket %q: %w", name, err)
	}
	if err := s3util.EmptyBucket(ctx, mc, name); err != nil {
		return fmt.Errorf("empty bucket %q before delete: %w", name, err)
	}
	return nil
}

// adminClientFor resolves the admin token and returns a client. ready is false
// when the admin token has not been provisioned yet.
func (d *Driver) adminClientFor(ctx context.Context, cluster *v1alpha1.ObjectStore) (*adminClient, bool, error) {
	token, err := d.adminToken(ctx, cluster)
	if err != nil {
		return nil, false, err
	}
	if token == "" {
		return nil, false, nil
	}
	return newAdminClient(adminEndpoint(cluster, d.namespace, d.clusterDomain), token), true, nil
}
