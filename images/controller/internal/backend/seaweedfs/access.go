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

package seaweedfs

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
	"github.com/deckhouse/sds-object/images/controller/internal/backend"
)

// EnsureAccess provisions an IAM identity scoped to the bucket for the given
// access — one per-identity file on the filer, touching no other identity's
// credentials. SeaweedFS stores the secret key retrievably, so it is always
// returned; mintFresh replaces the pair with a new random one (rotation), which
// revokes the previous key.
func (d *Driver) EnsureAccess(ctx context.Context, cluster *v1alpha1.ObjectStore, bucket *v1alpha1.Bucket, access *v1alpha1.BucketAccess, mintFresh bool) (backend.AccessState, error) {
	adminAK, _, err := d.adminCreds(ctx, cluster)
	if err != nil {
		return backend.AccessState{}, err
	}
	if adminAK == "" {
		return backend.AccessState{Message: "S3 admin identity is not provisioned yet"}, nil
	}

	name := backend.BucketDisplayName(bucket)
	identityName := backend.AccessResourceName(access)
	files := d.identityFilesFor(cluster)

	var accessKey, secretKey string
	if !mintFresh {
		existing, found, err := files.readIdentity(ctx, identityName)
		if err != nil {
			return backend.AccessState{}, err
		}
		if found && len(existing.Credentials) > 0 {
			accessKey, secretKey = existing.Credentials[0].AccessKey, existing.Credentials[0].SecretKey
		}
	}
	if accessKey == "" || secretKey == "" {
		if accessKey, err = randomHex(16); err != nil {
			return backend.AccessState{}, err
		}
		if secretKey, err = randomHex(32); err != nil {
			return backend.AccessState{}, err
		}
	}

	if err := files.writeIdentity(ctx, &s3Identity{
		Name:        identityName,
		Credentials: []s3Credential{{AccessKey: accessKey, SecretKey: secretKey}},
		Actions:     bucketActions(name, access.Spec.Permission),
	}); err != nil {
		return backend.AccessState{}, err
	}

	return backend.AccessState{
		Ready:           true,
		Message:         "access key provisioned",
		AccessKeyID:     accessKey,
		SecretAccessKey: secretKey,
	}, nil
}

// DeleteAccess removes the IAM identity issued for the access. Idempotent and
// tolerant of an already-deleted cluster; an identity file that cannot be
// confirmed gone is an error, so the access keeps its finalizer and retries
// rather than releasing while a live credential may remain.
func (d *Driver) DeleteAccess(ctx context.Context, cluster *v1alpha1.ObjectStore, _ *v1alpha1.Bucket, access *v1alpha1.BucketAccess) error {
	adminAK, _, err := d.adminCreds(ctx, cluster)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if adminAK == "" {
		return nil
	}

	return d.identityFilesFor(cluster).deleteIdentity(ctx, backend.AccessResourceName(access))
}
