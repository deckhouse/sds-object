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

package tests

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	objectv1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

// validationSpecs exercises the admission guards that protect the API: the
// validating webhooks (single System cluster, unique bucket name per cluster)
// and the CRD CEL rules (immutability + profile-conditional requirements). They
// run on top of the shared cluster/bucket from create_test.go.
func validationSpecs() {
	Describe("validation", func() {
		It("denies a second System ObjectStorageCluster", func() {
			if !suiteCfg.isSystem() {
				Skip("shared cluster is not type System; the single-System webhook guard is not armed")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			second := buildOSC(suiteCfg.oscName + "-2")
			err := createOSC(ctx, second)
			// Best-effort cleanup in case the guard ever regresses and admits it.
			defer func() {
				_ = suiteDyn.Resource(objectStorageClusterGVR).Delete(context.Background(), suiteCfg.oscName+"-2", metav1.DeleteOptions{})
			}()
			expectDenied(err, "only one System ObjectStorageCluster is allowed")
		})

		It("denies a duplicate effective bucket name on the same cluster", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			// Distinct metadata.name, but spec.bucketName collides with the shared
			// bucket on the same clusterRef -> the webhook must reject it.
			dup := buildOB("e2e-bucket-dup", suiteCfg.namespace, suiteCfg.oscName, objectv1alpha1.BucketReclaimRetain)
			spec := dup.Object["spec"].(map[string]interface{})
			spec["bucketName"] = suiteCfg.bucketName

			err := createOB(ctx, dup)
			defer func() {
				_ = suiteDyn.Resource(objectBucketGVR).Namespace(suiteCfg.namespace).Delete(context.Background(), "e2e-bucket-dup", metav1.DeleteOptions{})
			}()
			expectDenied(err, "already claimed by")
		})

		It("rejects changing the immutable spec.type", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			// Patch the shared cluster's type to a different valid enum value.
			newType := string(objectv1alpha1.ClusterTypeLightweight)
			if suiteCfg.oscType == newType {
				newType = string(objectv1alpha1.ClusterTypeSystem)
			}
			patch := []byte(`{"spec":{"type":"` + newType + `"}}`)
			_, err := suiteDyn.Resource(objectStorageClusterGVR).Patch(ctx, suiteCfg.oscName, types.MergePatchType, patch, metav1.PatchOptions{})
			expectDenied(err, "spec.type is immutable")
		})

		It("rejects a Heavy cluster without elasticClusterRef (CEL)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			bad := newOSC("e2e-heavy-noref", map[string]interface{}{
				"type":       string(objectv1alpha1.ClusterTypeHeavy),
				"redundancy": string(objectv1alpha1.RedundancySingle),
			})
			err := createOSC(ctx, bad)
			defer func() {
				_ = suiteDyn.Resource(objectStorageClusterGVR).Delete(context.Background(), "e2e-heavy-noref", metav1.DeleteOptions{})
			}()
			expectDenied(err, "elasticClusterRef is required")
		})

		It("rejects elasticClusterRef on a non-Heavy cluster (CEL)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			bad := newOSC("e2e-sys-ref", map[string]interface{}{
				"type":              string(objectv1alpha1.ClusterTypeSystem),
				"redundancy":        string(objectv1alpha1.RedundancySingle),
				"elasticClusterRef": "some-cluster",
			})
			err := createOSC(ctx, bad)
			defer func() {
				_ = suiteDyn.Resource(objectStorageClusterGVR).Delete(context.Background(), "e2e-sys-ref", metav1.DeleteOptions{})
			}()
			expectDenied(err, "elasticClusterRef is only allowed")
		})

		It("rejects a Lightweight cluster without storage.class (CEL)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			bad := newOSC("e2e-light-noclass", map[string]interface{}{
				"type":       string(objectv1alpha1.ClusterTypeLightweight),
				"redundancy": string(objectv1alpha1.RedundancySingle),
			})
			err := createOSC(ctx, bad)
			defer func() {
				_ = suiteDyn.Resource(objectStorageClusterGVR).Delete(context.Background(), "e2e-light-noclass", metav1.DeleteOptions{})
			}()
			expectDenied(err, "storage.class is required")
		})
	})
}

// newOSC builds an ObjectStorageCluster with an explicit spec map (for negative
// cases where buildOSC's profile-aware defaults would mask the rule under test).
func newOSC(name string, spec map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: apiGroup, Version: apiVersion, Kind: objectv1alpha1.ObjectStorageClusterKind})
	u.SetName(name)
	u.Object["spec"] = spec
	return u
}

// expectDenied asserts the admission attempt was rejected and that the denial
// message names the expected rule (so we know the RIGHT guard fired, not an
// unrelated error).
func expectDenied(err error, wantSubstr string) {
	GinkgoHelper()
	Expect(err).To(HaveOccurred(), "expected admission to deny the request, but it was accepted")
	Expect(strings.ToLower(err.Error())).To(ContainSubstring(strings.ToLower(wantSubstr)),
		"denied, but not by the expected guard: %v", err)
}
