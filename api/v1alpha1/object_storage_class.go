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

// ObjectStorageClass is a placeholder cluster-scoped custom resource that wires
// the api -> crds -> openapi -> rbac -> webhook plumbing together. Replace its
// Spec/Status with the real sds-object contract as the module is implemented.
type ObjectStorageClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ObjectStorageClassSpec   `json:"spec,omitempty"`
	Status ObjectStorageClassStatus `json:"status,omitempty"`
}

// ObjectStorageClassList contains a list of ObjectStorageClass.
type ObjectStorageClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []ObjectStorageClass `json:"items"`
}

// ObjectStorageClassSpec defines the desired state of an ObjectStorageClass.
type ObjectStorageClassSpec struct {
	// ReclaimPolicy is a placeholder field. Define the real desired-state
	// fields here.
	ReclaimPolicy string `json:"reclaimPolicy,omitempty"`
}

// ObjectStorageClassStatus defines the observed state of an ObjectStorageClass.
type ObjectStorageClassStatus struct {
	// Phase is a placeholder field reflecting the observed lifecycle stage.
	Phase string `json:"phase,omitempty"`
}
