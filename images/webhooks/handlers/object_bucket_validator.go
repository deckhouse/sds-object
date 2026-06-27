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

	"github.com/slok/kubewebhook/v2/pkg/model"
	kwhvalidating "github.com/slok/kubewebhook/v2/pkg/webhook/validating"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/klog/v2"
)

// ObjectBucketValidate is a placeholder admission validator for the ObjectBucket
// custom resource. It accepts every request; add the real cross-field checks
// here (e.g. clusterRef points to an existing ObjectStorageCluster) as the
// controller is implemented. Note that the schema-level CEL rules in the CRD
// already enforce clusterRef/bucketName immutability.
func ObjectBucketValidate(_ context.Context, _ *model.AdmissionReview, obj metav1.Object) (*kwhvalidating.ValidatorResult, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return &kwhvalidating.ValidatorResult{Valid: true}, nil
	}

	klog.Infof("ObjectBucket %s/%s admitted (placeholder validator)", u.GetNamespace(), u.GetName())
	return &kwhvalidating.ValidatorResult{Valid: true}, nil
}
