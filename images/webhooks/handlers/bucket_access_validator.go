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

// BucketAccessValidate admits BucketAccess resources. The schema/immutability
// contract is enforced by the CRD CEL rules; this validator only adds a soft
// cross-resource check: the referenced BucketClaim (same namespace) should
// exist and be Bound. It never hard-denies, so create-before-claim ordering
// stays possible; the controller keeps the access pending until the claim is
// Bound.
func (v *Validator) BucketAccessValidate(ctx context.Context, _ *model.AdmissionReview, obj metav1.Object) (*kwhvalidating.ValidatorResult, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return &kwhvalidating.ValidatorResult{Valid: true}, nil
	}

	ns := u.GetNamespace()
	name := u.GetName()
	claimName, _, _ := unstructured.NestedString(u.Object, "spec", "bucketClaimName")
	var warnings []string

	if claimName != "" {
		claim, err := v.dyn.Resource(bucketClaimGVR).Namespace(ns).Get(ctx, claimName, metav1.GetOptions{})
		if err != nil {
			warnings = append(warnings, fmt.Sprintf(
				"referenced BucketClaim %q not found in namespace %q (%v); the access will stay pending until it is Bound", claimName, ns, err))
		} else if phase, _, _ := unstructured.NestedString(claim.Object, "status", "phase"); phase != "Ready" {
			warnings = append(warnings, fmt.Sprintf(
				"BucketClaim %q is not Ready yet (phase %q); the access will stay pending until it is Bound", claimName, phase))
		}
	}

	klog.Infof("BucketAccess %s/%s admitted (warnings: %d)", ns, name, len(warnings))
	return &kwhvalidating.ValidatorResult{Valid: true, Warnings: warnings}, nil
}
