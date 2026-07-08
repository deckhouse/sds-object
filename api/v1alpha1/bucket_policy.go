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

// BucketPolicy is a cluster-scoped CR that declares which namespaces may bind a
// Shared (administrator-declared) Bucket via a brownfield BucketClaim
// (spec.existingBucketName). Binding is deny-by-default: a brownfield claim is
// only Bound when at least one policy for the target bucket matches the claim's
// namespace. Multiple policies for the same bucket are additive (their allowed
// sets are unioned). Greenfield claims provision their own private bucket and
// need no policy.
//
// +kubebuilder:resource:scope=Cluster,shortName=bp
// +kubebuilder:subresource:status
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type BucketPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BucketPolicySpec    `json:"spec"`
	Status *BucketPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type BucketPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []BucketPolicy `json:"items"`
}

// +k8s:deepcopy-gen=true
type BucketPolicySpec struct {
	// BucketRef is the name of the cluster-scoped Shared Bucket this policy
	// governs. Immutable after creation.
	// +kubebuilder:validation:Required
	BucketRef string `json:"bucketRef"`

	// AllowedNamespaces selects the namespaces permitted to bind the bucket via
	// a brownfield BucketClaim. At least one of names or patterns must be set.
	// +kubebuilder:validation:Required
	AllowedNamespaces NamespaceMatch `json:"allowedNamespaces"`
}

// NamespaceMatch selects namespaces by an exact-name list and/or a list of
// RE2 regular expressions. A namespace matches when it appears in Names or when
// it fully matches any pattern in Patterns.
// +k8s:deepcopy-gen=true
type NamespaceMatch struct {
	// Names is the list of exact namespace names allowed.
	// +optional
	// +listType=set
	Names []string `json:"names,omitempty"`

	// Patterns is the list of RE2 regular expressions matched (anchored,
	// full-string) against the namespace name.
	// +optional
	// +listType=set
	Patterns []string `json:"patterns,omitempty"`
}

// +k8s:deepcopy-gen=true
type BucketPolicyStatus struct {
	// ObservedGeneration is the most recent .metadata.generation reconciled
	// by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase is a coarse-grained summary. Ready when the policy is valid and
	// its bucket exists; Error when a pattern fails to compile.
	// +kubebuilder:validation:Enum=Pending;Ready;Error
	// +optional
	Phase string `json:"phase,omitempty"`

	// Conditions hold the latest state. Known type: Ready.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// Well-known condition types for BucketPolicy.
const (
	BucketPolicyConditionReady = "Ready"
)

// BucketPolicyKind is the kind constant used for dynamic GVK
// lookups.
const BucketPolicyKind = "BucketPolicy"
