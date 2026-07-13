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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BucketClaim is a namespaced CR — the tenant-facing request for a
// bucket. It supports two paths:
//
//   - greenfield (spec.existingBucketName empty): the controller provisions a
//     new cluster-scoped Bucket (origin=BucketClaim) in spec.objectStoreRef,
//     owned by this claim and named under the reserved prefix so it cannot
//     collide with a Shared bucket. The bucket is private to this namespace.
//   - brownfield (spec.existingBucketName set): the claim binds to an existing
//     Shared Bucket of that name, but only when a BucketClaimPolicy grants this
//     claim's namespace (deny-by-default).
//
// Credentials are not issued here: workloads request scoped credentials with a
// BucketAccess that references this claim by name in the same namespace.
//
// +kubebuilder:resource:scope=Namespaced,shortName=bc
// +kubebuilder:subresource:status
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type BucketClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BucketClaimSpec    `json:"spec"`
	Status *BucketClaimStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type BucketClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []BucketClaim `json:"items"`
}

// +k8s:deepcopy-gen=true
type BucketClaimSpec struct {
	// ObjectStoreRef is the name of the ObjectStore in which a greenfield bucket
	// is provisioned. Required for greenfield (existingBucketName empty);
	// ignored for brownfield (the ObjectStore is taken from the bound Bucket).
	// Immutable after creation.
	// +optional
	ObjectStoreRef string `json:"objectStoreRef,omitempty"`

	// ExistingBucketName, when set, switches the claim to brownfield mode: it
	// binds to the existing Shared (administrator-declared) Bucket of this name
	// instead of provisioning a new one. Allowed only when a BucketClaimPolicy grants
	// this claim's namespace. Immutable after creation.
	// +optional
	ExistingBucketName string `json:"existingBucketName,omitempty"`

	// AccessPolicy selects the access policy for a greenfield bucket. Ignored
	// for brownfield.
	// +kubebuilder:default=Private
	AccessPolicy AccessPolicy `json:"accessPolicy,omitempty"`

	// ReclaimPolicy controls what happens to the greenfield bucket data when the
	// claim (and its owned Bucket) is deleted. Ignored for brownfield.
	// +kubebuilder:default=Retain
	ReclaimPolicy BucketReclaimPolicy `json:"reclaimPolicy,omitempty"`

	// Quota sets optional usage limits for a greenfield bucket. Ignored for
	// brownfield.
	// +optional
	Quota *BucketQuota `json:"quota,omitempty"`
}

// +k8s:deepcopy-gen=true
type BucketClaimStatus struct {
	// ObservedGeneration is the most recent .metadata.generation reconciled
	// by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase is a coarse-grained summary derived from Conditions.
	// +kubebuilder:validation:Enum=Pending;InProgress;Ready;Error
	// +optional
	Phase string `json:"phase,omitempty"`

	// BoundBucketName is the name of the cluster-scoped Bucket this claim is
	// bound to (the greenfield bucket it provisioned, or the Shared bucket it
	// bound in brownfield mode).
	// +optional
	BoundBucketName string `json:"boundBucketName,omitempty"`

	// Endpoint is the in-cluster S3 endpoint URL of the backing ObjectStore.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Conditions hold the latest stage states. Known types: Bound, BucketReady,
	// Ready.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// Well-known condition types for BucketClaim.
const (
	BucketClaimConditionBound       = "Bound"
	BucketClaimConditionBucketReady = "BucketReady"
	BucketClaimConditionReady       = "Ready"
)

// BucketClaimKind is the kind constant used for dynamic GVK lookups.
const BucketClaimKind = "BucketClaim"
