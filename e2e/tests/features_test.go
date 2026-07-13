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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	objectv1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

// featuresSpecs exercises the fail-loud feature-parity contract on top of the
// shared cluster from create_test.go: optional Bucket spec fields (quota,
// accessPolicy=PublicRead) that a backend cannot enforce must NOT silently
// no-op. The controller reports them on the Bucket via the FeaturesApplied
// condition (informational, does not gate Ready).
//
// These specs create their own buckets and clean them up, so they run before
// deleteSpecs tears the shared cluster down.
func featuresSpecs() {
	Describe("features", func() {
		It("reports PublicRead as unsupported (fail-loud) while keeping the bucket Ready", func() {
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.obReadyTimeout+3*time.Minute)
			defer cancel()

			bucket := suiteCfg.bucketName + "-pub"

			DeferCleanup(func() {
				_ = suiteDyn.Resource(bucketGVR).Delete(context.Background(), bucket, metav1.DeleteOptions{})
			})

			By("creating a bucket requesting accessPolicy=PublicRead")
			osb := buildOSBFeatures(bucket, suiteCfg.oscName, objectv1alpha1.BucketReclaimDelete,
				objectv1alpha1.AccessPolicyPublicRead, nil)
			Expect(createOSB(ctx, osb)).To(Succeed())

			By("the bucket still reaches Ready (unsupported features do not gate readiness)")
			Expect(waitOSBReady(ctx, bucket)).To(Succeed())

			By("FeaturesApplied is False with reason Unsupported (no silent no-op)")
			Expect(waitCondition(ctx, bucketGVR, "", bucket,
				objectv1alpha1.BucketConditionFeaturesApplied, "False", 2*time.Minute)).To(Succeed())
			_, reason, found, err := getCondition(ctx, bucketGVR, "", bucket, objectv1alpha1.BucketConditionFeaturesApplied)
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue(), "FeaturesApplied condition must be present")
			Expect(reason).To(Equal("Unsupported"))
		})

		It("reflects backend quota support in the FeaturesApplied condition", func() {
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.obReadyTimeout+3*time.Minute)
			defer cancel()

			bucket := suiteCfg.bucketName + "-quota"

			DeferCleanup(func() {
				_ = suiteDyn.Resource(bucketGVR).Delete(context.Background(), bucket, metav1.DeleteOptions{})
			})

			By("creating a bucket requesting a quota")
			osb := buildOSBFeatures(bucket, suiteCfg.oscName, objectv1alpha1.BucketReclaimDelete, "",
				map[string]interface{}{"maxSize": "1Gi", "maxObjects": int64(1000)})
			Expect(createOSB(ctx, osb)).To(Succeed())

			By("the bucket reaches Ready")
			Expect(waitOSBReady(ctx, bucket)).To(Succeed())

			// Garage / Ceph RGW enforce the quota (FeaturesApplied=True); SeaweedFS
			// cannot (fail-loud, FeaturesApplied=False).
			want := "False"
			if quotaEnforcedByBackend() {
				want = "True"
			}
			By("FeaturesApplied reflects whether the backend enforces the quota: want " + want)
			Expect(waitCondition(ctx, bucketGVR, "", bucket,
				objectv1alpha1.BucketConditionFeaturesApplied, want, 2*time.Minute)).To(Succeed())
		})
	})
}
