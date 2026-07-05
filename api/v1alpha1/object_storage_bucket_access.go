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

// ObjectStorageBucketAccess is a namespaced CR that requests scoped S3 access
// to a cluster-scoped ObjectStorageBucket from a consuming namespace. The
// controller mints a dedicated access key / secret key for this access,
// writes a Secret in the access's namespace (status.secretRef) with the
// standard S3 connection variables, and revokes the key when the access is
// deleted. Whether the access is allowed is governed by ObjectStorageBucket
// Policy resources (deny-by-default: an access with no matching policy stays
// Pending/Denied).
//
// Key rotation: add or change the annotation
// storage.deckhouse.io/rotate on the ObjectStorageBucketAccess to trigger
// issuance of a fresh key pair (the Secret is updated and the previous key is
// revoked). status.observedRotation records the last processed value.
//
// +kubebuilder:resource:scope=Namespaced,shortName=osba
// +kubebuilder:subresource:status
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ObjectStorageBucketAccess struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ObjectStorageBucketAccessSpec    `json:"spec"`
	Status *ObjectStorageBucketAccessStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ObjectStorageBucketAccessList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []ObjectStorageBucketAccess `json:"items"`
}

// AccessPermission is the level of access granted to the credentials issued for
// an ObjectStorageBucketAccess.
// +kubebuilder:validation:Enum=ReadWrite;ReadOnly
type AccessPermission string

const (
	// AccessReadWrite grants read and write access to the bucket.
	AccessReadWrite AccessPermission = "ReadWrite"

	// AccessReadOnly grants read-only access to the bucket.
	AccessReadOnly AccessPermission = "ReadOnly"
)

// +k8s:deepcopy-gen=true
type ObjectStorageBucketAccessSpec struct {
	// BucketRef is the name of the cluster-scoped ObjectStorageBucket this
	// access targets. Immutable after creation.
	// +kubebuilder:validation:Required
	BucketRef string `json:"bucketRef"`

	// Permission is the access level granted to the issued credentials.
	// +kubebuilder:default=ReadWrite
	Permission AccessPermission `json:"permission,omitempty"`

	// SecretName overrides the name of the credentials Secret written in this
	// access's namespace. Defaults to <metadata.name>-s3-credentials.
	// +optional
	SecretName string `json:"secretName,omitempty"`
}

// +k8s:deepcopy-gen=true
type ObjectStorageBucketAccessStatus struct {
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

	// BucketName is the effective bucket name the access is scoped to.
	// +optional
	BucketName string `json:"bucketName,omitempty"`

	// AccessKeyID is the public access key id issued for this access (the
	// secret key is only written into the credentials Secret).
	// +optional
	AccessKeyID string `json:"accessKeyID,omitempty"`

	// SecretRef references the Secret (in this access's namespace) holding
	// the S3 connection variables and credentials.
	// +optional
	SecretRef *LocalSecretReference `json:"secretRef,omitempty"`

	// ObservedRotation is the last value of the rotation annotation processed
	// by the controller. When it differs from the current annotation value a
	// new key pair is issued.
	// +optional
	ObservedRotation string `json:"observedRotation,omitempty"`

	// LastRotationTime is the timestamp of the most recent key issuance.
	// +optional
	LastRotationTime *metav1.Time `json:"lastRotationTime,omitempty"`

	// Conditions hold the latest stage states. Known types: AccessGranted,
	// CredentialsReady, Ready.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// Well-known condition types for ObjectStorageBucketAccess.
const (
	OSBAConditionAccessGranted    = "AccessGranted"
	OSBAConditionCredentialsReady = "CredentialsReady"
	OSBAConditionReady            = "Ready"
)

// ObjectStorageBucketAccessKind is the kind constant used for OwnerReferences
// and dynamic GVK lookups.
const ObjectStorageBucketAccessKind = "ObjectStorageBucketAccess"

// RotateAnnotation, when its value changes, triggers issuance of a fresh access
// key pair for the ObjectStorageBucketAccess.
const RotateAnnotation = "storage.deckhouse.io/rotate"

// Keys written into the credentials Secret referenced by
// ObjectStorageBucketAccessStatus.SecretRef. Standardised so applications can
// `envFrom` it directly.
const (
	SecretKeyS3Endpoint     = "S3_ENDPOINT"
	SecretKeyS3Region       = "S3_REGION"
	SecretKeyS3Bucket       = "S3_BUCKET"
	SecretKeyAccessKeyID    = "AWS_ACCESS_KEY_ID"
	SecretKeySecretAccessID = "AWS_SECRET_ACCESS_KEY"
)
