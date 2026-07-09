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
	"strings"

	"github.com/slok/kubewebhook/v2/pkg/model"
	kwhvalidating "github.com/slok/kubewebhook/v2/pkg/webhook/validating"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/klog/v2"
)

// BucketValidate admits Bucket resources. The schema/immutability contract is
// enforced by the CRD CEL rules; this validator adds the cross-resource checks:
//
//   - the reserved name prefix is off-limits to administrators: only the
//     controller may create greenfield buckets under it (identified by the
//     origin=BucketClaim label), which keeps greenfield names from colliding
//     with Shared buckets (hard deny);
//   - the effective bucket name must be unique within an ObjectStore: two
//     Buckets pointing at the same backend bucket on the same store would
//     collide (hard deny);
//   - the referenced ObjectStore should already exist (soft warning — the
//     bucket reconciles to Pending until it does).
func (v *Validator) BucketValidate(ctx context.Context, _ *model.AdmissionReview, obj metav1.Object) (*kwhvalidating.ValidatorResult, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return &kwhvalidating.ValidatorResult{Valid: true}, nil
	}

	name := u.GetName()
	objectStoreRef, _, _ := unstructured.NestedString(u.Object, "spec", "objectStoreRef")
	bucketName := effectiveBucketName(u)
	var warnings []string

	// Reserved-prefix guard: administrators may not use the greenfield prefix.
	if strings.HasPrefix(name, reservedBucketNamePrefix) &&
		u.GetLabels()[labelBucketOrigin] != bucketOriginBucketClaim {
		return reject(fmt.Sprintf(
			"Bucket name %q uses the reserved prefix %q, which is reserved for controller-provisioned greenfield buckets",
			name, reservedBucketNamePrefix)), nil
	}

	list, err := v.dyn.Resource(bucketGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("could not verify bucket name uniqueness: %v", err))
	} else {
		for i := range list.Items {
			other := &list.Items[i]
			if other.GetName() == name {
				continue
			}
			if ref, _, _ := unstructured.NestedString(other.Object, "spec", "objectStoreRef"); ref != objectStoreRef {
				continue
			}
			if effectiveBucketName(other) == bucketName {
				return reject(fmt.Sprintf(
					"bucket name %q on object store %q is already claimed by Bucket %q",
					bucketName, objectStoreRef, other.GetName())), nil
			}
		}
	}

	if objectStoreRef != "" {
		if _, err := v.dyn.Resource(objectStoreGVR).Get(ctx, objectStoreRef, metav1.GetOptions{}); err != nil {
			warnings = append(warnings, fmt.Sprintf(
				"referenced ObjectStore %q not found (%v); the bucket will stay pending until it exists", objectStoreRef, err))
		}
	}

	klog.Infof("Bucket %s admitted (warnings: %d)", name, len(warnings))
	return &kwhvalidating.ValidatorResult{Valid: true, Warnings: warnings}, nil
}
