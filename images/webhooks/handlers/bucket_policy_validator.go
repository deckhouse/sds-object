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

// BucketPolicyValidate admits BucketPolicy resources.
// It hard-denies patterns that fail to compile (CEL cannot validate RE2
// compilation) and warns when the referenced bucket does not exist yet.
func (v *Validator) BucketPolicyValidate(ctx context.Context, _ *model.AdmissionReview, obj metav1.Object) (*kwhvalidating.ValidatorResult, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return &kwhvalidating.ValidatorResult{Valid: true}, nil
	}

	name := u.GetName()
	bucketRef, _, _ := unstructured.NestedString(u.Object, "spec", "bucketRef")
	patterns, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "allowedNamespaces", "patterns")
	var warnings []string

	for _, pat := range patterns {
		if _, err := regexp.Compile("^(?:" + pat + ")$"); err != nil {
			return reject(fmt.Sprintf("invalid namespace pattern %q: %v", pat, err)), nil
		}
	}

	if bucketRef != "" {
		if _, err := v.dyn.Resource(bucketGVR).Get(ctx, bucketRef, metav1.GetOptions{}); err != nil {
			warnings = append(warnings, fmt.Sprintf(
				"referenced Bucket %q not found (%v); the policy takes effect once it exists", bucketRef, err))
		}
	}

	klog.Infof("BucketPolicy %s admitted (warnings: %d)", name, len(warnings))
	return &kwhvalidating.ValidatorResult{Valid: true, Warnings: warnings}, nil
}
