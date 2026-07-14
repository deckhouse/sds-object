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
		It("denies a System ObjectStore not named 'system' (CEL)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			// A System store must be named `system`; this both keeps a single
			// System store and makes any second one collide on the name.
			second := newOSC("e2e-extra-system", map[string]interface{}{
				"type": string(objectv1alpha1.ClusterTypeSystem),
			})
			err := createOSC(ctx, second)
			// Best-effort cleanup in case the guard ever regresses and admits it.
			defer func() {
				_ = suiteDyn.Resource(objectStoreGVR).Delete(context.Background(), "e2e-extra-system", metav1.DeleteOptions{})
			}()
			expectDenied(err, "must be named 'system'")
		})

		It("denies spec.redundancy on a System ObjectStore (CEL)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			// Use name 'system' so the name rule passes and the redundancy rule is
			// the sole CEL failure. CEL runs on the submitted object at admission
			// (before persistence), so nothing is ever created — no cleanup, and no
			// risk of touching the shipped `system` store.
			bad := newOSC("system", map[string]interface{}{
				"type":       string(objectv1alpha1.ClusterTypeSystem),
				"redundancy": string(objectv1alpha1.RedundancyStandard),
			})
			err := createOSC(ctx, bad)
			expectDenied(err, "redundancy must not be set")
		})

		It("denies spec.storage.sizePerNode on a System ObjectStore (CEL)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			// name 'system' isolates the sizePerNode rule (see the redundancy case).
			bad := newOSC("system", map[string]interface{}{
				"type":    string(objectv1alpha1.ClusterTypeSystem),
				"storage": map[string]interface{}{"sizePerNode": "10Gi"},
			})
			err := createOSC(ctx, bad)
			expectDenied(err, "sizePerNode must not be set")
		})

		It("denies an administrator Bucket using the reserved claim- name prefix", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			// The reserved prefix is only for controller-provisioned greenfield
			// buckets; an administrator-declared Bucket may not use it.
			bad := buildOSB("claim-e2e-reserved", suiteCfg.oscName, objectv1alpha1.BucketReclaimRetain)
			err := createOSB(ctx, bad)
			defer func() {
				_ = suiteDyn.Resource(bucketGVR).Delete(context.Background(), "claim-e2e-reserved", metav1.DeleteOptions{})
			}()
			expectDenied(err, "reserved prefix")
		})

		It("denies a duplicate effective bucket name on the same cluster", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			// Distinct metadata.name, but spec.bucketName collides with the shared
			// cluster-scoped bucket on the same clusterRef -> the webhook must
			// reject it (bucket-name uniqueness per clusterRef is a hard deny).
			dup := buildOSB("e2e-bucket-dup", suiteCfg.oscName, objectv1alpha1.BucketReclaimRetain)
			spec := dup.Object["spec"].(map[string]interface{})
			spec["bucketName"] = suiteCfg.bucketName

			err := createOSB(ctx, dup)
			defer func() {
				_ = suiteDyn.Resource(bucketGVR).Delete(context.Background(), "e2e-bucket-dup", metav1.DeleteOptions{})
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
			_, err := suiteDyn.Resource(objectStoreGVR).Patch(ctx, suiteCfg.oscName, types.MergePatchType, patch, metav1.PatchOptions{})
			expectDenied(err, "spec.type is immutable")
		})

		It("rejects a Heavy cluster without elasticClusterRef (CEL)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			bad := newOSC("e2e-heavy-noref", map[string]interface{}{
				"type":       string(objectv1alpha1.ClusterTypeHeavy),
				"redundancy": string(objectv1alpha1.RedundancyNone),
			})
			err := createOSC(ctx, bad)
			defer func() {
				_ = suiteDyn.Resource(objectStoreGVR).Delete(context.Background(), "e2e-heavy-noref", metav1.DeleteOptions{})
			}()
			expectDenied(err, "elasticClusterRef is required")
		})

		It("rejects elasticClusterRef on a non-Heavy cluster (CEL)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			bad := newOSC("e2e-sys-ref", map[string]interface{}{
				"type":              string(objectv1alpha1.ClusterTypeSystem),
				"elasticClusterRef": "some-cluster",
			})
			err := createOSC(ctx, bad)
			defer func() {
				_ = suiteDyn.Resource(objectStoreGVR).Delete(context.Background(), "e2e-sys-ref", metav1.DeleteOptions{})
			}()
			expectDenied(err, "elasticClusterRef is only allowed")
		})

		It("rejects a Lightweight cluster without storage.class (CEL)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			bad := newOSC("e2e-light-noclass", map[string]interface{}{
				"type":       string(objectv1alpha1.ClusterTypeLightweight),
				"redundancy": string(objectv1alpha1.RedundancyNone),
			})
			err := createOSC(ctx, bad)
			defer func() {
				_ = suiteDyn.Resource(objectStoreGVR).Delete(context.Background(), "e2e-light-noclass", metav1.DeleteOptions{})
			}()
			expectDenied(err, "storage.class is required")
		})

		It("denies an BucketClaimPolicy with an invalid regex pattern", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			// The policy validator warns on most inputs, but rejects patterns that
			// fail to compile as RE2 (an unclosed group is invalid).
			bad := buildOSBPolicy("e2e-bad-pattern", suiteCfg.bucketName, nil)
			spec := bad.Object["spec"].(map[string]interface{})
			spec["allowedNamespaces"] = map[string]interface{}{
				"patterns": []interface{}{"("},
			}
			err := createOSBPolicy(ctx, bad)
			defer func() {
				_ = suiteDyn.Resource(bucketClaimPolicyGVR).Delete(context.Background(), "e2e-bad-pattern", metav1.DeleteOptions{})
			}()
			expectDenied(err, "pattern")
		})

		It("denies a BucketClaim setting both objectStoreRef and existingBucketName (CEL)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			// greenfield (objectStoreRef) and brownfield (existingBucketName) are
			// mutually exclusive; setting both must be rejected, not silently
			// treated as brownfield.
			claim := buildGreenfieldClaim("e2e-both-fields", suiteCfg.namespace, suiteCfg.oscName, objectv1alpha1.BucketReclaimDelete)
			claim.Object["spec"].(map[string]interface{})["existingBucketName"] = suiteCfg.bucketName
			err := createBucketClaim(ctx, claim)
			defer func() {
				_ = suiteDyn.Resource(bucketClaimGVR).Namespace(suiteCfg.namespace).Delete(context.Background(), "e2e-both-fields", metav1.DeleteOptions{})
			}()
			expectDenied(err, "mutually exclusive")
		})

		It("denies a Full ObjectStore whose storage.nodes cannot satisfy replication", func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			// Full + High needs >=3 volume servers (replication 002); nodes=1 can
			// never satisfy replication, so admission must reject it.
			osc := newOSC("e2e-full-nodes", map[string]interface{}{
				"type":       "Full",
				"redundancy": "High",
				"storage": map[string]interface{}{
					"class": "any-storage-class",
					"nodes": int64(1),
				},
			})
			err := createOSC(ctx, osc)
			defer func() {
				_ = suiteDyn.Resource(objectStoreGVR).Delete(context.Background(), "e2e-full-nodes", metav1.DeleteOptions{})
			}()
			expectDenied(err, "replication")
		})
	})
}

// newOSC builds an ObjectStore with an explicit spec map (for negative
// cases where buildOSC's profile-aware defaults would mask the rule under test).
func newOSC(name string, spec map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: apiGroup, Version: apiVersion, Kind: objectv1alpha1.ObjectStoreKind})
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
