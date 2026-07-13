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

	"github.com/minio/minio-go/v7"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
	"github.com/deckhouse/sds-object/images/controller/internal/backend"
	"github.com/deckhouse/sds-object/images/controller/internal/backend/s3util"
)

// EnsureAccess provisions a dedicated RGW user for the access and grants it
// access to the (owner-owned) bucket via the bucket policy. Rook owns the
// user's keys; mintFresh rotates them by recreating the CephObjectStoreUser.
func (d *Driver) EnsureAccess(ctx context.Context, cluster *v1alpha1.ObjectStore, bucket *v1alpha1.Bucket, access *v1alpha1.BucketAccess, mintFresh bool) (backend.AccessState, error) {
	uid := accessUID(access)

	accessKey, secretKey, err := d.userKeys(ctx, cluster, uid)
	if err != nil {
		return backend.AccessState{}, err
	}

	if mintFresh {
		recorded := ""
		if access.Status != nil {
			recorded = access.Status.AccessKeyID
		}
		// Rotation is effected once Rook re-issues a key different from the
		// recorded one. Until then, (re)create the user to force a new key.
		if accessKey == "" || accessKey == recorded {
			if err := d.deleteUser(ctx, uid); err != nil {
				return backend.AccessState{}, err
			}
			if err := d.ensureUser(ctx, cluster, uid, nil); err != nil {
				if isNoMatch(err) {
					return backend.AccessState{Message: "CephObjectStoreUser CRD not found; is the sds-elastic module installed?"}, nil
				}
				return backend.AccessState{}, fmt.Errorf("ensure CephObjectStoreUser: %w", err)
			}
			return backend.AccessState{Message: "rotating RGW user credentials"}, nil
		}
	} else {
		if err := d.ensureUser(ctx, cluster, uid, nil); err != nil {
			if isNoMatch(err) {
				return backend.AccessState{Message: "CephObjectStoreUser CRD not found; is the sds-elastic module installed?"}, nil
			}
			return backend.AccessState{}, fmt.Errorf("ensure CephObjectStoreUser: %w", err)
		}
		accessKey, secretKey, err = d.userKeys(ctx, cluster, uid)
		if err != nil {
			return backend.AccessState{}, err
		}
	}

	if accessKey == "" || secretKey == "" {
		return backend.AccessState{Message: "waiting for Rook to issue RGW user credentials"}, nil
	}

	// Grant the access user access to the owner-owned bucket via bucket policy.
	if err := d.grantBucketAccess(ctx, cluster, bucket, uid, access.Spec.Permission); err != nil {
		return backend.AccessState{}, err
	}

	return backend.AccessState{
		Ready:           true,
		Message:         "access key provisioned",
		AccessKeyID:     accessKey,
		SecretAccessKey: secretKey,
	}, nil
}

// DeleteAccess revokes the access: drops the bucket policy statement and deletes
// the CephObjectStoreUser. Idempotent.
func (d *Driver) DeleteAccess(ctx context.Context, cluster *v1alpha1.ObjectStore, bucket *v1alpha1.Bucket, access *v1alpha1.BucketAccess) error {
	uid := accessUID(access)

	if owner, err := d.ownerClient(ctx, cluster, bucket); err == nil && owner != nil {
		name := backend.BucketDisplayName(bucket)
		if err := d.updateBucketPolicy(ctx, owner, name, uid, nil); err != nil {
			return err
		}
	}

	return d.deleteUser(ctx, uid)
}

// grantBucketAccess adds (or refreshes) the bucket-policy statement allowing the
// access user's actions on the bucket, using the bucket owner's credentials.
func (d *Driver) grantBucketAccess(ctx context.Context, cluster *v1alpha1.ObjectStore, bucket *v1alpha1.Bucket, uid string, perm v1alpha1.AccessPermission) error {
	owner, err := d.ownerClient(ctx, cluster, bucket)
	if err != nil {
		return err
	}
	if owner == nil {
		return fmt.Errorf("bucket owner credentials not available yet")
	}
	name := backend.BucketDisplayName(bucket)
	return d.updateBucketPolicy(ctx, owner, name, uid, policyActions(perm))
}

// ownerClient builds an S3 client with the per-bucket owner credentials (nil
// when they are not provisioned yet).
func (d *Driver) ownerClient(ctx context.Context, cluster *v1alpha1.ObjectStore, bucket *v1alpha1.Bucket) (*minio.Client, error) {
	ak, sk, err := d.userKeys(ctx, cluster, ownerUID(bucket))
	if err != nil {
		return nil, err
	}
	if ak == "" || sk == "" {
		return nil, nil
	}
	return s3util.NewClient(rgwHostPort(cluster, d.clusterDomain), ak, sk)
}

// updateBucketPolicy reads the current bucket policy, upserts (actions != nil)
// or removes (actions == nil) the statement for the given uid, and writes it
// back.
func (d *Driver) updateBucketPolicy(ctx context.Context, owner *minio.Client, bucketName, uid string, actions []string) error {
	raw, err := s3util.GetBucketPolicy(ctx, owner, bucketName)
	if err != nil {
		return err
	}
	doc := parsePolicy(raw)
	changed := false
	if actions == nil {
		changed = doc.remove(uid)
	} else {
		changed = doc.upsert(uid, bucketName, actions)
	}
	if !changed {
		return nil
	}
	return s3util.SetBucketPolicy(ctx, owner, bucketName, doc.marshal())
}

// policyActions maps the access permission to the S3 actions granted in the
// bucket policy.
func policyActions(perm v1alpha1.AccessPermission) []string {
	if perm == v1alpha1.AccessReadOnly {
		return []string{"s3:GetObject", "s3:ListBucket", "s3:GetBucketLocation"}
	}
	return []string{
		"s3:GetObject", "s3:PutObject", "s3:DeleteObject",
		"s3:ListBucket", "s3:GetBucketLocation",
		"s3:AbortMultipartUpload", "s3:ListMultipartUploadParts",
		"s3:ListBucketMultipartUploads",
	}
}
