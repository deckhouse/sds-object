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

package handlers

import (
	"context"
	"fmt"

	"github.com/slok/kubewebhook/v2/pkg/model"
	kwhvalidating "github.com/slok/kubewebhook/v2/pkg/webhook/validating"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/klog/v2"
)

// ObjectStoreValidate admits ObjectStore resources. The
// schema/immutability contract is enforced by the CRD CEL rules; this validator
// adds the cross-resource checks:
//
//   - at most one cluster of type System may exist (hard deny);
//   - for type Heavy, the referenced ElasticCluster should already exist
//     (soft warning — the cluster reconciles to Pending until it does, so we do
//     not block create-before-dependency ordering).
func (v *Validator) ObjectStoreValidate(ctx context.Context, _ *model.AdmissionReview, obj metav1.Object) (*kwhvalidating.ValidatorResult, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return &kwhvalidating.ValidatorResult{Valid: true}, nil
	}

	name := u.GetName()
	clusterType, _, _ := unstructured.NestedString(u.Object, "spec", "type")
	var warnings []string

	if clusterType == "System" {
		list, err := v.dyn.Resource(objectStoreGVR).List(ctx, metav1.ListOptions{})
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("could not verify System cluster uniqueness: %v", err))
		} else {
			for i := range list.Items {
				other := &list.Items[i]
				if other.GetName() == name {
					continue
				}
				if t, _, _ := unstructured.NestedString(other.Object, "spec", "type"); t == "System" {
					return reject(fmt.Sprintf(
						"only one System ObjectStore is allowed; %q already exists", other.GetName())), nil
				}
			}
		}
	}

	if clusterType == "Heavy" {
		if ref, _, _ := unstructured.NestedString(u.Object, "spec", "elasticClusterRef"); ref != "" {
			if _, err := v.dyn.Resource(elasticClusterGVR).Get(ctx, ref, metav1.GetOptions{}); err != nil {
				warnings = append(warnings, fmt.Sprintf(
					"referenced ElasticCluster %q not found (%v); the cluster will stay pending until it exists", ref, err))
			}
		}
	}

	// Full (SeaweedFS): spec.storage.nodes overrides the volume-server count but
	// must still be able to satisfy the redundancy's replication code (which
	// spreads copies across servers), or writes would never meet replication.
	if clusterType == "Full" {
		if nodes, found, _ := unstructured.NestedInt64(u.Object, "spec", "storage", "nodes"); found && nodes > 0 {
			redundancy, _, _ := unstructured.NestedString(u.Object, "spec", "redundancy")
			if minNodes := minVolumeNodes(redundancy); nodes < minNodes {
				return reject(fmt.Sprintf(
					"spec.storage.nodes=%d is too low for redundancy %q: it needs at least %d volume server(s) to satisfy replication",
					nodes, redundancy, minNodes)), nil
			}
		}
	}

	klog.Infof("ObjectStore %s admitted (warnings: %d)", name, len(warnings))
	return &kwhvalidating.ValidatorResult{Valid: true, Warnings: warnings}, nil
}

// minVolumeNodes is the minimum SeaweedFS volume-server count that can satisfy a
// redundancy's replication code: None (000, no extra copies) needs 1, Standard
// (001, a copy on another server) needs 2, High (002, two copies on other
// servers) needs 3.
func minVolumeNodes(redundancy string) int64 {
	switch redundancy {
	case "None":
		return 1
	case "High":
		return 3
	default: // Standard or unset
		return 2
	}
}
