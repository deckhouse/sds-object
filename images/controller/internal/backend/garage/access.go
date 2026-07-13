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
)

// EnsureAccess provisions a Garage access key scoped to the bucket for the
// given access. mintFresh forces a brand-new key (the previous one, recorded in
// access.Status.AccessKeyID, is revoked) — the reconciler passes it on rotation
// or when the credentials Secret is missing. Otherwise the recorded key is
// reused (its secret cannot be recovered from Garage, so it is not returned).
func (d *Driver) EnsureAccess(ctx context.Context, cluster *v1alpha1.ObjectStore, bucket *v1alpha1.Bucket, access *v1alpha1.BucketAccess, mintFresh bool) (backend.AccessState, error) {
	svc, ready, err := d.adminClientFor(ctx, cluster)
	if err != nil {
		return backend.AccessState{}, err
	}
	if !ready {
		return backend.AccessState{Message: "Garage admin token is not provisioned yet"}, nil
	}

	name := backend.BucketDisplayName(bucket)
	b, found, err := svc.getBucketByAlias(ctx, name)
	if err != nil {
		return backend.AccessState{}, fmt.Errorf("look up bucket %q: %w", name, err)
	}
	if !found {
		return backend.AccessState{Message: fmt.Sprintf("bucket %q does not exist yet", name)}, nil
	}

	prevKeyID := accessKeyID(access)
	accessKey, secretKey := prevKeyID, ""

	if !mintFresh && accessKey != "" {
		exists, err := svc.keyExists(ctx, accessKey)
		if err != nil {
			return backend.AccessState{}, fmt.Errorf("check access key: %w", err)
		}
		if !exists {
			mintFresh = true // recorded key vanished; re-issue
		}
	}
	minted := false
	if mintFresh || accessKey == "" {
		key, err := svc.createKey(ctx, backend.AccessResourceName(access))
		if err != nil {
			return backend.AccessState{}, fmt.Errorf("create access key: %w", err)
		}
		accessKey, secretKey = key.AccessKeyID, key.SecretAccessKey
		minted = true
	}

	if err := svc.allow(ctx, b.ID, accessKey, garagePermissions(access)); err != nil {
		if minted {
			// Roll back the key we just created: its id/secret were not persisted
			// to status, so leaving it would leak an orphan key and the next
			// reconcile would mint yet another one.
			_ = svc.deleteKey(ctx, accessKey)
		}
		return backend.AccessState{}, fmt.Errorf("grant key on bucket: %w", err)
	}

	// Revoke the superseded key after the new one is granted.
	if mintFresh && prevKeyID != "" && prevKeyID != accessKey {
		if err := svc.deleteKey(ctx, prevKeyID); err != nil {
			return backend.AccessState{}, fmt.Errorf("revoke previous access key: %w", err)
		}
	}

	return backend.AccessState{
		Ready:           true,
		Message:         "access key provisioned",
		AccessKeyID:     accessKey,
		SecretAccessKey: secretKey,
	}, nil
}

// DeleteAccess revokes the access key recorded for the access. Idempotent and
// tolerant of an already-deleted cluster.
func (d *Driver) DeleteAccess(ctx context.Context, cluster *v1alpha1.ObjectStore, _ *v1alpha1.Bucket, access *v1alpha1.BucketAccess) error {
	keyID := accessKeyID(access)
	if keyID == "" {
		return nil
	}
	svc, ready, err := d.adminClientFor(ctx, cluster)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !ready {
		return nil
	}
	if err := svc.deleteKey(ctx, keyID); err != nil {
		return fmt.Errorf("delete access key: %w", err)
	}
	return nil
}

// accessKeyID returns the access key id recorded in the access status (empty
// when not yet issued).
func accessKeyID(access *v1alpha1.BucketAccess) string {
	if access.Status == nil {
		return ""
	}
	return access.Status.AccessKeyID
}

// garagePermissions maps the access permission to Garage bucket permissions.
func garagePermissions(access *v1alpha1.BucketAccess) permissions {
	if access.Spec.Permission == v1alpha1.AccessReadOnly {
		return permissions{Read: true}
	}
	return permissions{Read: true, Write: true}
}
