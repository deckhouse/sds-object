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

// Package backend defines the contract between the sds-object reconcilers and
// the concrete object storage backends (Garage, SeaweedFS, Ceph RGW). The
// reconcilers own the Kubernetes-facing FSM (conditions, phases, finalizers,
// credentials Secret); a Driver owns everything backend-specific (data-plane
// workloads, bucket/key lifecycle).
package backend

import (
	"context"
	"fmt"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

// ClusterState is the observed data-plane state a Driver reports for an
// ObjectStore. The reconciler copies it into the CR status and uses
// Ready to advance the FSM. When Ready is false, Message explains why (surfaced
// as the BackendReady condition message).
type ClusterState struct {
	// Ready is true once the data plane is up and serving S3.
	Ready bool

	// Message is a human-readable explanation of the current state, used as
	// the condition message (especially while not Ready).
	Message string

	// Backend identifies the implementation and its running version.
	Backend v1alpha1.BackendStatus

	// Endpoint is the in-cluster S3 endpoint, once known.
	Endpoint v1alpha1.EndpointStatus

	// Capacity is the latest usage probe, when available.
	Capacity *v1alpha1.ObjectCapacityStatus
}

// BucketState is the observed state a Driver reports for an Bucket.
// Buckets no longer carry credentials: access keys are issued per
// BucketAccess (see AccessState).
type BucketState struct {
	// Ready is true once the bucket exists in the backend.
	Ready bool

	// Message is a human-readable explanation of the current state.
	Message string

	// BucketName is the effective bucket name created in the backend.
	BucketName string

	// UnsupportedFeatures lists spec features that were requested on the bucket
	// but that this backend cannot enforce (e.g. a quota on a backend without
	// quota support). The reconciler surfaces them on the Bucket as a
	// FeaturesApplied=False condition so a requested feature never silently
	// no-ops. An empty slice means every requested feature was applied.
	UnsupportedFeatures []string
}

// Bucket spec features that a backend may or may not be able to enforce. Used
// as the human-readable tokens in BucketState.UnsupportedFeatures.
const (
	FeatureQuota      = "spec.quota"
	FeaturePublicRead = "spec.accessPolicy=PublicRead"
)

// RequestedFeatures returns the optional, backend-dependent features the bucket
// asks for. A backend compares this against what it can enforce to populate
// BucketState.UnsupportedFeatures.
func RequestedFeatures(bucket *v1alpha1.Bucket) []string {
	var out []string
	if bucket.Spec.Quota != nil && (bucket.Spec.Quota.MaxSize != "" || bucket.Spec.Quota.MaxObjects > 0) {
		out = append(out, FeatureQuota)
	}
	if bucket.Spec.AccessPolicy == v1alpha1.AccessPolicyPublicRead {
		out = append(out, FeaturePublicRead)
	}
	return out
}

// AccessState is the observed state a Driver reports for an
// BucketAccess. When SecretAccessKey is non-empty a fresh key was
// issued and the reconciler (re)writes the credentials Secret with it; when it
// is empty the access already had a live key and the existing Secret is
// preserved (backends other than Ceph RGW cannot recover a secret key after
// creation).
type AccessState struct {
	// Ready is true once a key scoped to the bucket is provisioned.
	Ready bool

	// Message is a human-readable explanation of the current state.
	Message string

	// AccessKeyID is the public access key id for the access.
	AccessKeyID string

	// SecretAccessKey is the secret key; populated only when a fresh key was
	// just issued (mintFresh) — see the type comment.
	SecretAccessKey string
}

// Driver is the backend-specific half of object storage reconciliation. All
// methods must be idempotent: the reconciler calls them on every reconcile.
type Driver interface {
	// Type returns the backend type this Driver implements.
	Type() v1alpha1.BackendType

	// EnsureCluster brings the data plane for the given cluster to its
	// desired state and reports the observed state.
	EnsureCluster(ctx context.Context, cluster *v1alpha1.ObjectStore) (ClusterState, error)

	// DeleteCluster tears down the data plane for the given cluster. It must
	// honour cluster.Spec.ReclaimPolicy: Retain preserves persisted data,
	// Delete may destroy it.
	DeleteCluster(ctx context.Context, cluster *v1alpha1.ObjectStore) error

	// EnsureBucket creates/updates the bucket in the backend (no credentials)
	// and reports the observed state.
	EnsureBucket(ctx context.Context, cluster *v1alpha1.ObjectStore, bucket *v1alpha1.Bucket) (BucketState, error)

	// DeleteBucket removes the bucket when the bucket's reclaim policy is
	// Delete (otherwise a no-op on the bucket data).
	DeleteBucket(ctx context.Context, cluster *v1alpha1.ObjectStore, bucket *v1alpha1.Bucket) error

	// EnsureAccess provisions an S3 access key scoped to the bucket for the
	// given access and reports the observed state. When mintFresh is true it
	// issues a brand-new key pair (rotating/replacing any previous key for the
	// access, revoking the old one) and returns it; when false it ensures a
	// key exists without minting a duplicate.
	EnsureAccess(ctx context.Context, cluster *v1alpha1.ObjectStore, bucket *v1alpha1.Bucket, access *v1alpha1.BucketAccess, mintFresh bool) (AccessState, error)

	// DeleteAccess revokes the access key issued for the given access.
	DeleteAccess(ctx context.Context, cluster *v1alpha1.ObjectStore, bucket *v1alpha1.Bucket, access *v1alpha1.BucketAccess) error
}

// AccessResourceName is the backend-facing identifier (access key display name /
// IAM identity / RGW user) for an access, unique per (namespace, name).
func AccessResourceName(access *v1alpha1.BucketAccess) string {
	return fmt.Sprintf("%s.%s", access.Namespace, access.Name)
}

// BucketDisplayName is the S3 bucket name for a bucket: spec.bucketName, or
// metadata.name when unset.
func BucketDisplayName(bucket *v1alpha1.Bucket) string {
	if bucket.Spec.BucketName != "" {
		return bucket.Spec.BucketName
	}
	return bucket.Name
}

// ResolveBackendType maps a cluster profile (spec.type) to the backend that
// implements it.
func ResolveBackendType(t v1alpha1.ClusterType) (v1alpha1.BackendType, error) {
	switch t {
	case v1alpha1.ClusterTypeSystem, v1alpha1.ClusterTypeLightweight:
		return v1alpha1.BackendGarage, nil
	case v1alpha1.ClusterTypeFull:
		return v1alpha1.BackendSeaweedFS, nil
	case v1alpha1.ClusterTypeHeavy:
		return v1alpha1.BackendCephRGW, nil
	default:
		return "", fmt.Errorf("unknown cluster type %q", t)
	}
}

// Registry resolves a Driver for a given cluster (by its spec.type → backend
// type). Drivers are registered once at startup.
type Registry struct {
	drivers map[v1alpha1.BackendType]Driver
}

// NewRegistry builds a Registry from the given Drivers, keyed by Driver.Type().
func NewRegistry(drivers ...Driver) *Registry {
	m := make(map[v1alpha1.BackendType]Driver, len(drivers))
	for _, d := range drivers {
		m[d.Type()] = d
	}
	return &Registry{drivers: m}
}

// For returns the Driver implementing the given cluster's profile.
func (r *Registry) For(cluster *v1alpha1.ObjectStore) (Driver, error) {
	bt, err := ResolveBackendType(cluster.Spec.Type)
	if err != nil {
		return nil, err
	}
	d, ok := r.drivers[bt]
	if !ok {
		return nil, fmt.Errorf("no driver registered for backend %q", bt)
	}
	return d, nil
}
