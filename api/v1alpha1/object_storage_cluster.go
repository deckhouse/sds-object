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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ObjectStorageCluster is a cluster-scoped CR that describes the desired state
// of an S3-compatible object storage cluster managed by the sds-object module.
// A single spec.type selects one of four turnkey profiles; the backend
// (Garage / SeaweedFS / Ceph RGW) and its low-level settings are hidden from the
// user. Buckets are declared separately in cluster-scoped ObjectStorageBucket
// resources that reference this cluster by name.
//
// +kubebuilder:resource:scope=Cluster,shortName=osc
// +kubebuilder:subresource:status
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ObjectStorageCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ObjectStorageClusterSpec    `json:"spec"`
	Status *ObjectStorageClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ObjectStorageClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []ObjectStorageCluster `json:"items"`
}

// ClusterType selects the cluster profile (backend + placement model).
// +kubebuilder:validation:Enum=System;Lightweight;Full;Heavy
type ClusterType string

const (
	// ClusterTypeSystem is Garage deployed as a DaemonSet on control-plane
	// nodes with hostPath storage. For platform/system needs.
	ClusterTypeSystem ClusterType = "System"

	// ClusterTypeLightweight is Garage deployed as a StatefulSet backed by
	// PVCs on spec.storage.class.
	ClusterTypeLightweight ClusterType = "Lightweight"

	// ClusterTypeFull is SeaweedFS (master/volume/filer + S3 gateway) backed
	// by PVCs on spec.storage.class.
	ClusterTypeFull ClusterType = "Full"

	// ClusterTypeHeavy is Ceph RADOS Gateway (CephObjectStore) on top of an
	// existing sds-elastic cluster referenced by spec.elasticClusterRef.
	ClusterTypeHeavy ClusterType = "Heavy"
)

// RedundancyMode encodes the high-level fault-tolerance intent. The controller
// maps it to backend-specific settings (replication factor / erasure coding).
// +kubebuilder:validation:Enum=Single;Replicated;HighRedundancy
type RedundancyMode string

const (
	// RedundancySingle keeps a single copy (no redundancy).
	RedundancySingle RedundancyMode = "Single"

	// RedundancyReplicated keeps replicated copies across nodes/zones.
	RedundancyReplicated RedundancyMode = "Replicated"

	// RedundancyHighRedundancy maximises durability (extra replicas or
	// erasure coding) and requires more nodes.
	RedundancyHighRedundancy RedundancyMode = "HighRedundancy"
)

// ClusterReclaimPolicy controls what happens to the backend data plane and its
// persisted data when the ObjectStorageCluster is deleted.
// +kubebuilder:validation:Enum=Retain;Delete
type ClusterReclaimPolicy string

const (
	// ClusterReclaimRetain preserves the backend's persisted data on cluster
	// deletion (for Heavy this keeps the Ceph RGW pools intact). The data
	// plane workloads are still removed, but no stored objects are destroyed.
	ClusterReclaimRetain ClusterReclaimPolicy = "Retain"

	// ClusterReclaimDelete destroys the backend's persisted data on cluster
	// deletion (PVCs, hostPath data, or Ceph RGW pools).
	ClusterReclaimDelete ClusterReclaimPolicy = "Delete"
)

// +k8s:deepcopy-gen=true
type ObjectStorageClusterSpec struct {
	// Type selects the cluster profile. Immutable after creation.
	// +kubebuilder:validation:Required
	Type ClusterType `json:"type"`

	// Storage configures capacity and backing storage. Ignored for
	// type=Heavy (capacity comes from the referenced Ceph cluster).
	// +optional
	Storage *ObjectStorageClusterStorageSpec `json:"storage,omitempty"`

	// Redundancy picks the high-level fault-tolerance intent. When omitted,
	// a sensible default is derived from Type. Immutable after creation
	// (changing the replication factor on a live cluster is not supported). For
	// System the effective factor is capped by the control-plane node count.
	// +optional
	Redundancy RedundancyMode `json:"redundancy,omitempty"`

	// Placement configures scheduling of the data plane. Ignored for
	// type=System (forced onto control-plane nodes).
	// +optional
	Placement *PlacementSpec `json:"placement,omitempty"`

	// ReclaimPolicy controls what happens to the backend data plane and its
	// stored data when the ObjectStorageCluster is deleted. Defaults to
	// Retain, which preserves persisted data (for Heavy this keeps the Ceph
	// RGW pools intact). Immutable after creation.
	// +kubebuilder:default=Retain
	ReclaimPolicy ClusterReclaimPolicy `json:"reclaimPolicy,omitempty"`

	// ElasticClusterRef is the name of the ElasticCluster (sds-elastic) the
	// CephObjectStore is provisioned on. Required and only allowed when
	// Type is Heavy. Immutable after creation.
	// +optional
	ElasticClusterRef string `json:"elasticClusterRef,omitempty"`
}

// +k8s:deepcopy-gen=true
type ObjectStorageClusterStorageSpec struct {
	// Size is the total usable capacity as a Kubernetes Quantity (BinarySI),
	// e.g. "50Gi" or "2Ti". For type=System it is the capacity contributed
	// per control-plane node.
	// +optional
	Size string `json:"size,omitempty"`

	// Class is the Kubernetes StorageClass used to provision PVCs. Required
	// for Lightweight and Full; ignored for System (hostPath) and Heavy.
	// +optional
	Class string `json:"class,omitempty"`
}

// +k8s:deepcopy-gen=true
type PlacementSpec struct {
	// NodeSelector is the set of node labels the data-plane Pods must match.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations are applied to the data-plane Pods.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

// +k8s:deepcopy-gen=true
type ObjectStorageClusterStatus struct {
	// ObservedGeneration is the most recent .metadata.generation reconciled
	// by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase is a coarse-grained summary derived from Conditions.
	// +kubebuilder:validation:Enum=Pending;InProgress;Ready;Error
	// +optional
	Phase string `json:"phase,omitempty"`

	// Backend describes the resolved backend implementation behind spec.type.
	// +optional
	Backend *BackendStatus `json:"backend,omitempty"`

	// Endpoint is the S3 endpoint clients use to reach this cluster.
	// +optional
	Endpoint *EndpointStatus `json:"endpoint,omitempty"`

	// Capacity reports cluster-wide storage usage as reported by the backend.
	// +optional
	Capacity *ObjectCapacityStatus `json:"capacity,omitempty"`

	// AdminSecretRef references the Secret (in the module namespace) holding
	// the backend admin credentials used by the controller to manage buckets
	// and access keys.
	// +optional
	AdminSecretRef *LocalSecretReference `json:"adminSecretRef,omitempty"`

	// Conditions hold the latest stage states. Known types: BackendReady,
	// EndpointReady, Ready.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// BackendType identifies the concrete backend implementing a cluster profile.
// +kubebuilder:validation:Enum=Garage;SeaweedFS;CephRGW
type BackendType string

const (
	BackendGarage    BackendType = "Garage"
	BackendSeaweedFS BackendType = "SeaweedFS"
	BackendCephRGW   BackendType = "CephRGW"
)

// +k8s:deepcopy-gen=true
type BackendStatus struct {
	// Type is the backend implementing the selected profile.
	// +optional
	Type BackendType `json:"type,omitempty"`

	// Version is the running backend version.
	// +optional
	Version string `json:"version,omitempty"`
}

// +k8s:deepcopy-gen=true
type EndpointStatus struct {
	// Internal is the in-cluster S3 endpoint URL (Service DNS).
	// +optional
	Internal string `json:"internal,omitempty"`

	// Region is the default S3 region advertised by the endpoint.
	// +optional
	Region string `json:"region,omitempty"`
}

// +k8s:deepcopy-gen=true
type ObjectCapacityStatus struct {
	// Total is the total usable capacity (Kubernetes Quantity, BinarySI).
	// +optional
	Total resource.Quantity `json:"total,omitempty"`

	// Used is the consumed capacity (Kubernetes Quantity, BinarySI).
	// +optional
	Used resource.Quantity `json:"used,omitempty"`

	// Available is the free capacity (Kubernetes Quantity, BinarySI).
	// +optional
	Available resource.Quantity `json:"available,omitempty"`

	// UsedPercent is Used / Total * 100, formatted with two decimals.
	// +optional
	UsedPercent string `json:"usedPercent,omitempty"`

	// LastUpdated is the timestamp of the latest capacity probe.
	// +optional
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`
}

// LocalSecretReference references a Secret by name within a known namespace.
// +k8s:deepcopy-gen=true
type LocalSecretReference struct {
	// Name of the referenced Secret.
	Name string `json:"name"`
}

// Well-known condition types for ObjectStorageCluster.
const (
	OSCConditionBackendReady  = "BackendReady"
	OSCConditionEndpointReady = "EndpointReady"
	OSCConditionReady         = "Ready"
)

// ObjectStorageClusterKind is the kind constant used for OwnerReferences and
// dynamic GVK lookups.
const ObjectStorageClusterKind = "ObjectStorageCluster"

// Status.phase values shared across the module's CRs.
const (
	PhasePending    = "Pending"
	PhaseInProgress = "InProgress"
	PhaseReady      = "Ready"
	PhaseError      = "Error"
)
