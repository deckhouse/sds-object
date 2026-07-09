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

// Bucket is a cluster-scoped CR that represents a single S3 bucket in an
// ObjectStore. The controller creates the bucket in the backend. Credentials
// are NOT issued here: consuming namespaces request scoped access (and receive
// a credentials Secret) via namespaced BucketAccess resources.
//
// A Bucket has one of two origins (recorded in the label
// storage.deckhouse.io/bucket-origin):
//   - Shared: declared directly by an administrator; consumed cross-namespace
//     via a BucketClaim whose spec.existingBucketName points at it, gated by
//     BucketClaimPolicy (deny-by-default).
//   - BucketClaim: provisioned by the controller for a greenfield BucketClaim
//     and owned by it (see the owned-by-claim labels). Such buckets are private
//     to the owning claim's namespace and cannot be bound by other claims.
//
// +kubebuilder:resource:scope=Cluster,shortName=bkt
// +kubebuilder:subresource:status
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type Bucket struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BucketSpec    `json:"spec"`
	Status *BucketStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type BucketList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []Bucket `json:"items"`
}

// AccessPolicy controls anonymous access to the bucket.
// +kubebuilder:validation:Enum=Private;PublicRead
type AccessPolicy string

const (
	// AccessPolicyPrivate makes the bucket accessible only with issued
	// credentials.
	AccessPolicyPrivate AccessPolicy = "Private"

	// AccessPolicyPublicRead makes objects readable anonymously; writes
	// still require credentials.
	AccessPolicyPublicRead AccessPolicy = "PublicRead"
)

// BucketReclaimPolicy controls what happens to bucket data on deletion.
// +kubebuilder:validation:Enum=Retain;Delete
type BucketReclaimPolicy string

const (
	// BucketReclaimRetain keeps the bucket and its objects in the backend.
	BucketReclaimRetain BucketReclaimPolicy = "Retain"

	// BucketReclaimDelete deletes the bucket and all its objects.
	BucketReclaimDelete BucketReclaimPolicy = "Delete"
)

// +k8s:deepcopy-gen=true
type BucketSpec struct {
	// ObjectStoreRef is the name of the ObjectStore this bucket belongs to. The
	// referenced ObjectStore must exist and be in Ready phase before the bucket
	// is provisioned. Immutable after creation.
	// +kubebuilder:validation:Required
	ObjectStoreRef string `json:"objectStoreRef"`

	// BucketName is the name of the bucket in S3. Defaults to metadata.name
	// when omitted. Immutable after creation.
	// +optional
	BucketName string `json:"bucketName,omitempty"`

	// AccessPolicy selects the bucket access policy.
	// +kubebuilder:default=Private
	AccessPolicy AccessPolicy `json:"accessPolicy,omitempty"`

	// ReclaimPolicy controls what happens to bucket data on deletion.
	// +kubebuilder:default=Retain
	ReclaimPolicy BucketReclaimPolicy `json:"reclaimPolicy,omitempty"`

	// Quota sets optional usage limits for the bucket.
	// +optional
	Quota *BucketQuota `json:"quota,omitempty"`
}

// +k8s:deepcopy-gen=true
type BucketQuota struct {
	// MaxSize is the maximum total size of the bucket as a Kubernetes
	// Quantity (BinarySI), e.g. "10Gi". Omit for no size limit.
	// +optional
	MaxSize string `json:"maxSize,omitempty"`

	// MaxObjects is the maximum number of objects. 0 (default) means no
	// limit.
	// +optional
	MaxObjects int64 `json:"maxObjects,omitempty"`
}

// +k8s:deepcopy-gen=true
type BucketStatus struct {
	// ObservedGeneration is the most recent .metadata.generation reconciled
	// by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase is a coarse-grained summary derived from Conditions.
	// +kubebuilder:validation:Enum=Pending;InProgress;Ready;Error
	// +optional
	Phase string `json:"phase,omitempty"`

	// Endpoint is the in-cluster S3 endpoint URL of the backing cluster.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// BucketName is the effective bucket name created in the backend.
	// +optional
	BucketName string `json:"bucketName,omitempty"`

	// Conditions hold the latest stage states. Known types: BucketReady,
	// Ready.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// Well-known condition types for Bucket.
const (
	BucketConditionBucketReady = "BucketReady"
	BucketConditionReady       = "Ready"
)

// BucketKind is the kind constant used for OwnerReferences and
// dynamic GVK lookups.
const BucketKind = "Bucket"

// Bucket origin labels and values. LabelBucketOrigin records how a Bucket came
// to exist; the owned-by-claim labels back-reference the greenfield BucketClaim
// that provisioned it (a cluster-scoped Bucket cannot carry a namespaced
// ownerReference, so the binding is expressed with labels + a finalizer on the
// claim).
const (
	LabelBucketOrigin          = "storage.deckhouse.io/bucket-origin"
	LabelOwnedByClaimNamespace = "storage.deckhouse.io/owned-by-claim-namespace"
	LabelOwnedByClaimName      = "storage.deckhouse.io/owned-by-claim-name"

	// BucketOriginShared marks an administrator-declared shared Bucket.
	BucketOriginShared = "Shared"
	// BucketOriginBucketClaim marks a Bucket provisioned for a greenfield
	// BucketClaim and owned by it.
	BucketOriginBucketClaim = "BucketClaim"
)

// ReservedBucketNamePrefix is the metadata.name prefix reserved for
// controller-provisioned greenfield Buckets. Administrator-declared Bucket and
// BucketClaim resources must not use this prefix (enforced by the admission
// webhook), which keeps greenfield names from colliding with shared buckets.
const ReservedBucketNamePrefix = "claim-"
