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
	"regexp"

	"github.com/slok/kubewebhook/v2/pkg/model"
	kwhvalidating "github.com/slok/kubewebhook/v2/pkg/webhook/validating"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/klog/v2"
)

// ObjectStorageBucketAccessValidate admits ObjectStorageBucketAccess resources.
// The schema/immutability contract is enforced by the CRD CEL rules; this
// validator adds soft cross-resource checks and does NOT hard-deny on policy:
// access is deny-by-default and enforced by the controller (no credentials are
// issued unless a policy allows the namespace), so we only warn here to keep
// create-before-policy ordering possible.
func (v *Validator) ObjectStorageBucketAccessValidate(ctx context.Context, _ *model.AdmissionReview, obj metav1.Object) (*kwhvalidating.ValidatorResult, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return &kwhvalidating.ValidatorResult{Valid: true}, nil
	}

	ns := u.GetNamespace()
	name := u.GetName()
	bucketRef, _, _ := unstructured.NestedString(u.Object, "spec", "bucketRef")
	var warnings []string

	if bucketRef != "" {
		if _, err := v.dyn.Resource(objectStorageBucketGVR).Get(ctx, bucketRef, metav1.GetOptions{}); err != nil {
			warnings = append(warnings, fmt.Sprintf(
				"referenced ObjectStorageBucket %q not found (%v); the access will stay pending until it exists", bucketRef, err))
		}
		if !v.namespaceAllowedForBucket(ctx, bucketRef, ns) {
			warnings = append(warnings, fmt.Sprintf(
				"no ObjectStorageBucketPolicy currently grants namespace %q access to bucket %q; access is deny-by-default and will stay pending until a matching policy exists", ns, bucketRef))
		}
	}

	klog.Infof("ObjectStorageBucketAccess %s/%s admitted (warnings: %d)", ns, name, len(warnings))
	return &kwhvalidating.ValidatorResult{Valid: true, Warnings: warnings}, nil
}

// namespaceAllowedForBucket reports whether any ObjectStorageBucketPolicy for
// the bucket currently allows the namespace (best-effort; returns false on
// lookup errors so the caller only warns).
func (v *Validator) namespaceAllowedForBucket(ctx context.Context, bucketRef, namespace string) bool {
	list, err := v.dyn.Resource(objectStorageBucketPolicyGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false
	}
	for i := range list.Items {
		p := &list.Items[i]
		if ref, _, _ := unstructured.NestedString(p.Object, "spec", "bucketRef"); ref != bucketRef {
			continue
		}
		names, _, _ := unstructured.NestedStringSlice(p.Object, "spec", "allowedNamespaces", "names")
		for _, n := range names {
			if n == namespace {
				return true
			}
		}
		patterns, _, _ := unstructured.NestedStringSlice(p.Object, "spec", "allowedNamespaces", "patterns")
		for _, pat := range patterns {
			if re, err := regexp.Compile("^(?:" + pat + ")$"); err == nil && re.MatchString(namespace) {
				return true
			}
		}
	}
	return false
}
