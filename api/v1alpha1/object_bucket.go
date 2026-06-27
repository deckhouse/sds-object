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

// ObjectBucket is a namespaced CR that declares a single S3 bucket in an
// ObjectStorageCluster plus the access credentials for it. The controller
// creates the bucket in the backend, generates an access key / secret key
// scoped to it, and writes a Secret in the same namespace (status.secretRef)
// with the standard S3 connection variables so applications can consume it via
// envFrom directly.
//
// +kubebuilder:resource:scope=Namespaced,shortName=ob
// +kubebuilder:subresource:status
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ObjectBucket struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ObjectBucketSpec    `json:"spec"`
	Status *ObjectBucketStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ObjectBucketList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []ObjectBucket `json:"items"`
}

// AccessPolicy controls anonymous access to the bucket.
// +kubebuilder:validation:Enum=Private;PublicRead
type AccessPolicy string

const (
	// AccessPolicyPrivate makes the bucket accessible only with the
	// generated credentials.
	AccessPolicyPrivate AccessPolicy = "Private"

	// AccessPolicyPublicRead makes objects readable anonymously; writes
	// still require credentials.
	AccessPolicyPublicRead AccessPolicy = "PublicRead"
)

// BucketReclaimPolicy controls what happens to bucket data on deletion.
// +kubebuilder:validation:Enum=Retain;Delete
type BucketReclaimPolicy string

const (
	// BucketReclaimRetain removes the access key but keeps the bucket and
	// its objects in the backend.
	BucketReclaimRetain BucketReclaimPolicy = "Retain"

	// BucketReclaimDelete deletes the bucket and all its objects.
	BucketReclaimDelete BucketReclaimPolicy = "Delete"
)

// +k8s:deepcopy-gen=true
type ObjectBucketSpec struct {
	// ClusterRef is the name of the ObjectStorageCluster this bucket belongs
	// to. The referenced cluster must exist and be in Ready phase before the
	// bucket is provisioned. Immutable after creation.
	// +kubebuilder:validation:Required
	ClusterRef string `json:"clusterRef"`

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
type ObjectBucketStatus struct {
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

	// SecretRef references the Secret (in this ObjectBucket's namespace)
	// holding the S3 connection variables and credentials.
	// +optional
	SecretRef *LocalSecretReference `json:"secretRef,omitempty"`

	// Conditions hold the latest stage states. Known types: BucketReady,
	// CredentialsReady, Ready.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// Well-known condition types for ObjectBucket.
const (
	OBConditionBucketReady      = "BucketReady"
	OBConditionCredentialsReady = "CredentialsReady"
	OBConditionReady            = "Ready"
)

// ObjectBucketKind is the kind constant used for OwnerReferences and dynamic
// GVK lookups.
const ObjectBucketKind = "ObjectBucket"

// Keys written into the credentials Secret referenced by
// ObjectBucketStatus.SecretRef. Standardised so applications can `envFrom` it
// directly.
const (
	SecretKeyS3Endpoint     = "S3_ENDPOINT"
	SecretKeyS3Region       = "S3_REGION"
	SecretKeyS3Bucket       = "S3_BUCKET"
	SecretKeyAccessKeyID    = "AWS_ACCESS_KEY_ID"
	SecretKeySecretAccessID = "AWS_SECRET_ACCESS_KEY"
)
