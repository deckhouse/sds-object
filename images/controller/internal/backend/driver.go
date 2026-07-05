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
// ObjectStorageCluster. The reconciler copies it into the CR status and uses
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

// BucketState is the observed state a Driver reports for an ObjectStorageBucket.
// Buckets no longer carry credentials: access keys are issued per
// ObjectStorageBucketAccess (see AccessState).
type BucketState struct {
	// Ready is true once the bucket exists in the backend.
	Ready bool

	// Message is a human-readable explanation of the current state.
	Message string

	// BucketName is the effective bucket name created in the backend.
	BucketName string
}

// AccessState is the observed state a Driver reports for an
// ObjectStorageBucketAccess. When SecretAccessKey is non-empty a fresh key was
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
	EnsureCluster(ctx context.Context, cluster *v1alpha1.ObjectStorageCluster) (ClusterState, error)

	// DeleteCluster tears down the data plane for the given cluster. It must
	// honour cluster.Spec.ReclaimPolicy: Retain preserves persisted data,
	// Delete may destroy it.
	DeleteCluster(ctx context.Context, cluster *v1alpha1.ObjectStorageCluster) error

	// EnsureBucket creates/updates the bucket in the backend (no credentials)
	// and reports the observed state.
	EnsureBucket(ctx context.Context, cluster *v1alpha1.ObjectStorageCluster, bucket *v1alpha1.ObjectStorageBucket) (BucketState, error)

	// DeleteBucket removes the bucket when the bucket's reclaim policy is
	// Delete (otherwise a no-op on the bucket data).
	DeleteBucket(ctx context.Context, cluster *v1alpha1.ObjectStorageCluster, bucket *v1alpha1.ObjectStorageBucket) error

	// EnsureAccess provisions an S3 access key scoped to the bucket for the
	// given access and reports the observed state. When mintFresh is true it
	// issues a brand-new key pair (rotating/replacing any previous key for the
	// access, revoking the old one) and returns it; when false it ensures a
	// key exists without minting a duplicate.
	EnsureAccess(ctx context.Context, cluster *v1alpha1.ObjectStorageCluster, bucket *v1alpha1.ObjectStorageBucket, access *v1alpha1.ObjectStorageBucketAccess, mintFresh bool) (AccessState, error)

	// DeleteAccess revokes the access key issued for the given access.
	DeleteAccess(ctx context.Context, cluster *v1alpha1.ObjectStorageCluster, bucket *v1alpha1.ObjectStorageBucket, access *v1alpha1.ObjectStorageBucketAccess) error
}

// AccessResourceName is the backend-facing identifier (access key display name /
// IAM identity / RGW user) for an access, unique per (namespace, name).
func AccessResourceName(access *v1alpha1.ObjectStorageBucketAccess) string {
	return fmt.Sprintf("%s.%s", access.Namespace, access.Name)
}

// BucketDisplayName is the S3 bucket name for a bucket: spec.bucketName, or
// metadata.name when unset.
func BucketDisplayName(bucket *v1alpha1.ObjectStorageBucket) string {
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
func (r *Registry) For(cluster *v1alpha1.ObjectStorageCluster) (Driver, error) {
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
