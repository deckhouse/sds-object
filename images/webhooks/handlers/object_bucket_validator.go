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

// ObjectBucketValidate admits ObjectBucket resources. The schema/immutability
// contract is enforced by the CRD CEL rules; this validator adds the
// cross-resource checks:
//
//   - the effective bucket name must be unique within a cluster: two
//     ObjectBuckets pointing at the same backend bucket on the same cluster
//     would fight over its credentials (hard deny);
//   - the referenced ObjectStorageCluster should already exist (soft warning —
//     the bucket reconciles to Pending until it does).
func (v *Validator) ObjectBucketValidate(ctx context.Context, _ *model.AdmissionReview, obj metav1.Object) (*kwhvalidating.ValidatorResult, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return &kwhvalidating.ValidatorResult{Valid: true}, nil
	}

	ns := u.GetNamespace()
	name := u.GetName()
	clusterRef, _, _ := unstructured.NestedString(u.Object, "spec", "clusterRef")
	bucketName := effectiveBucketName(u)
	var warnings []string

	list, err := v.dyn.Resource(objectBucketGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("could not verify bucket name uniqueness: %v", err))
	} else {
		for i := range list.Items {
			other := &list.Items[i]
			if other.GetNamespace() == ns && other.GetName() == name {
				continue
			}
			if ref, _, _ := unstructured.NestedString(other.Object, "spec", "clusterRef"); ref != clusterRef {
				continue
			}
			if effectiveBucketName(other) == bucketName {
				return reject(fmt.Sprintf(
					"bucket name %q on cluster %q is already claimed by ObjectBucket %s/%s",
					bucketName, clusterRef, other.GetNamespace(), other.GetName())), nil
			}
		}
	}

	if clusterRef != "" {
		if _, err := v.dyn.Resource(objectStorageClusterGVR).Get(ctx, clusterRef, metav1.GetOptions{}); err != nil {
			warnings = append(warnings, fmt.Sprintf(
				"referenced ObjectStorageCluster %q not found (%v); the bucket will stay pending until it exists", clusterRef, err))
		}
	}

	klog.Infof("ObjectBucket %s/%s admitted (warnings: %d)", ns, name, len(warnings))
	return &kwhvalidating.ValidatorResult{Valid: true, Warnings: warnings}, nil
}
