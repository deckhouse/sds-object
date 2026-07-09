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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// Validator carries the cross-resource lookup client the admission validators
// need (schema-level immutability / shape checks are already enforced by the
// CEL rules on the CRDs; the validators here add the checks CEL cannot express,
// namely ones that look at other resources).
type Validator struct {
	dyn dynamic.Interface
}

// NewValidator builds a Validator backed by the given dynamic client.
func NewValidator(dyn dynamic.Interface) *Validator {
	return &Validator{dyn: dyn}
}

// Contract constants mirrored from api/v1alpha1 (kept as local literals to avoid
// a build dependency on the controller's API module).
const (
	reservedBucketNamePrefix = "claim-"
	labelBucketOrigin        = "storage.deckhouse.io/bucket-origin"
	bucketOriginBucketClaim  = "BucketClaim"
)

// GroupVersionResources the validators query.
var (
	objectStoreGVR = schema.GroupVersionResource{
		Group: "storage.deckhouse.io", Version: "v1alpha1", Resource: "objectstores",
	}
	bucketGVR = schema.GroupVersionResource{
		Group: "storage.deckhouse.io", Version: "v1alpha1", Resource: "buckets",
	}
	bucketClaimGVR = schema.GroupVersionResource{
		Group: "storage.deckhouse.io", Version: "v1alpha1", Resource: "bucketclaims",
	}
	bucketClaimPolicyGVR = schema.GroupVersionResource{
		Group: "storage.deckhouse.io", Version: "v1alpha1", Resource: "bucketclaimpolicies",
	}
	elasticClusterGVR = schema.GroupVersionResource{
		Group: "storage.deckhouse.io", Version: "v1alpha1", Resource: "elasticclusters",
	}
)

// effectiveBucketName is the S3 bucket name a Bucket maps to:
// spec.bucketName when set, otherwise metadata.name.
func effectiveBucketName(u *unstructured.Unstructured) string {
	if name, _, _ := unstructured.NestedString(u.Object, "spec", "bucketName"); name != "" {
		return name
	}
	return u.GetName()
}
