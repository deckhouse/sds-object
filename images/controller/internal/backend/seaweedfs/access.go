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
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
	"github.com/deckhouse/sds-object/images/controller/internal/backend"
)

// EnsureAccess provisions an IAM identity scoped to the bucket for the given
// access. SeaweedFS stores credentials in the filer IAM config, so the secret
// key is recoverable and always returned; mintFresh replaces it with a new
// random pair (rotation), which revokes the previous key.
func (d *Driver) EnsureAccess(ctx context.Context, cluster *v1alpha1.ObjectStorageCluster, bucket *v1alpha1.ObjectStorageBucket, access *v1alpha1.ObjectStorageBucketAccess, mintFresh bool) (backend.AccessState, error) {
	adminAK, _, err := d.adminCreds(ctx, cluster)
	if err != nil {
		return backend.AccessState{}, err
	}
	if adminAK == "" {
		return backend.AccessState{Message: "S3 admin identity is not provisioned yet"}, nil
	}

	name := backend.BucketDisplayName(bucket)
	identityName := backend.AccessResourceName(access)
	filer := newFilerClient(filerEndpoint(cluster, d.namespace, d.clusterDomain))

	cfg, err := filer.readIdentities(ctx)
	if err != nil {
		return backend.AccessState{}, fmt.Errorf("read IAM config: %w", err)
	}

	accessKey, secretKey := "", ""
	if !mintFresh {
		if cur, ok := findCredentials(cfg, identityName); ok {
			accessKey, secretKey = cur.AccessKey, cur.SecretKey
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

	identity := s3Identity{
		Name:        identityName,
		Credentials: []s3Credential{{AccessKey: accessKey, SecretKey: secretKey}},
		Actions:     bucketActions(name, access.Spec.Permission),
	}
	if cfg.upsert(identity) {
		if err := filer.writeIdentities(ctx, cfg); err != nil {
			return backend.AccessState{}, fmt.Errorf("write IAM config: %w", err)
		}
	}

	return backend.AccessState{
		Ready:           true,
		Message:         "access key provisioned",
		AccessKeyID:     accessKey,
		SecretAccessKey: secretKey,
	}, nil
}

// DeleteAccess removes the IAM identity issued for the access. Idempotent and
// tolerant of an already-deleted cluster.
func (d *Driver) DeleteAccess(ctx context.Context, cluster *v1alpha1.ObjectStorageCluster, _ *v1alpha1.ObjectStorageBucket, access *v1alpha1.ObjectStorageBucketAccess) error {
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

	filer := newFilerClient(filerEndpoint(cluster, d.namespace, d.clusterDomain))
	cfg, err := filer.readIdentities(ctx)
	if err != nil {
		return fmt.Errorf("read IAM config: %w", err)
	}
	if cfg.remove(backend.AccessResourceName(access)) {
		if err := filer.writeIdentities(ctx, cfg); err != nil {
			return fmt.Errorf("write IAM config: %w", err)
		}
	}
	return nil
}

// findCredentials returns the first credential pair of the named identity.
func findCredentials(cfg *identityConfig, name string) (s3Credential, bool) {
	for i := range cfg.Identities {
		if cfg.Identities[i].Name == name && len(cfg.Identities[i].Credentials) > 0 {
			return cfg.Identities[i].Credentials[0], true
		}
	}
	return s3Credential{}, false
}
