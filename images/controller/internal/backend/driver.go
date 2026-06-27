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

// BucketState is the observed state a Driver reports for an ObjectBucket. When
// Ready is true the credentials fields are populated and the reconciler
// (re)writes the credentials Secret in the bucket's namespace.
type BucketState struct {
	// Ready is true once the bucket exists and credentials are valid.
	Ready bool

	// Message is a human-readable explanation of the current state.
	Message string

	// BucketName is the effective bucket name created in the backend.
	BucketName string

	// AccessKeyID / SecretAccessKey are the S3 credentials scoped to the
	// bucket. Populated only when Ready is true.
	AccessKeyID     string
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

	// DeleteCluster tears down the data plane for the given cluster.
	DeleteCluster(ctx context.Context, cluster *v1alpha1.ObjectStorageCluster) error

	// EnsureBucket creates/updates the bucket and its access key in the
	// backend and reports the observed state (including credentials).
	EnsureBucket(ctx context.Context, cluster *v1alpha1.ObjectStorageCluster, bucket *v1alpha1.ObjectBucket) (BucketState, error)

	// DeleteBucket removes the access key and, depending on the bucket's
	// reclaim policy, the bucket itself.
	DeleteBucket(ctx context.Context, cluster *v1alpha1.ObjectStorageCluster, bucket *v1alpha1.ObjectBucket) error
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
