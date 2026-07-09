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

// greenfieldSpecs exercises the greenfield BucketClaim path (the tenant provisions
// its own private bucket, without spec.existingBucketName) against the shared
// primary ObjectStore from create_test.go: provisioning + ownership labels, an
// S3 round-trip, the capture guard (a private bucket cannot be bound by another
// claim), and the finalizer gate (a claim is not released while a BucketAccess
// still references it). Runs Ordered so the claim created first is reused/deleted
// by later specs.
func greenfieldSpecs() {
	Describe("greenfield", Ordered, func() {
		const claim = "e2e-greenfield"
		access := accessName(claim)
		var boundBucket string

		It("provisions a private Bucket owned by the greenfield claim and reaches Ready", func() {
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.obReadyTimeout+2*time.Minute)
			defer cancel()

			By("creating a greenfield BucketClaim (no existingBucketName)")
			Expect(createBucketClaim(ctx, buildGreenfieldClaim(claim, suiteCfg.namespace, suiteCfg.oscName, objectv1alpha1.BucketReclaimDelete))).To(Succeed())

			By("waiting for the claim Ready condition")
			Expect(waitCondition(ctx, bucketClaimGVR, suiteCfg.namespace, claim, objectv1alpha1.BucketClaimConditionReady, string(metav1.ConditionTrue), suiteCfg.obReadyTimeout)).To(Succeed())

			var err error
			boundBucket, err = getStringField(ctx, bucketClaimGVR, suiteCfg.namespace, claim, "status", "boundBucketName")
			Expect(err).NotTo(HaveOccurred())
			Expect(boundBucket).To(HavePrefix("claim-"), "greenfield bucket uses the reserved prefix")

			By("asserting the provisioned Bucket is owned by the claim")
			b, err := suiteDyn.Resource(bucketGVR).Get(ctx, boundBucket, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "get greenfield Bucket %s", boundBucket)
			labels := b.GetLabels()
			Expect(labels["storage.deckhouse.io/bucket-origin"]).To(Equal("BucketClaim"))
			Expect(labels["storage.deckhouse.io/owned-by-claim-namespace"]).To(Equal(suiteCfg.namespace))
			Expect(labels["storage.deckhouse.io/owned-by-claim-name"]).To(Equal(claim))
		})

		It("serves an S3 round-trip through a BucketAccess on the greenfield claim", func() {
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.obReadyTimeout+suiteCfg.probeJobTimeout+2*time.Minute)
			defer cancel()

			By("creating BucketAccess " + access + " referencing the greenfield claim")
			Expect(createOSBAccess(ctx, buildOSBAccess(access, suiteCfg.namespace, claim, objectv1alpha1.AccessReadWrite))).To(Succeed())
			Expect(waitAccessReady(ctx, suiteCfg.namespace, access)).To(Succeed())

			By("running an S3 write/list/read round-trip")
			Expect(runS3ProbeJob(ctx, "s3-probe-greenfield", suiteCfg.namespace, credsSecretName(access))).To(Succeed())
		})

		It("refuses to bind the private greenfield bucket from another claim (NotShared)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()

			const poach = "e2e-greenfield-poach"
			DeferCleanup(func() {
				_ = suiteDyn.Resource(bucketClaimGVR).Namespace(suiteCfg.namespace).Delete(context.Background(), poach, metav1.DeleteOptions{})
			})

			By("creating a brownfield claim targeting the private greenfield bucket")
			Expect(createBucketClaim(ctx, buildBucketClaim(poach, suiteCfg.namespace, boundBucket))).To(Succeed())

			By("asserting it stays unbound with reason NotShared")
			Eventually(func() string {
				_, reason, _, _ := getCondition(ctx, bucketClaimGVR, suiteCfg.namespace, poach, objectv1alpha1.BucketClaimConditionBound)
				return reason
			}).WithTimeout(2 * time.Minute).WithPolling(pollInterval).Should(Equal("NotShared"))
		})

		It("blocks claim deletion while a BucketAccess still references it, then GCs the owned bucket", func() {
			ctx, cancel := context.WithTimeout(context.Background(), resourceGoneTimeout+2*time.Minute)
			defer cancel()

			By("deleting the claim while its access still exists")
			Expect(suiteDyn.Resource(bucketClaimGVR).Namespace(suiteCfg.namespace).Delete(ctx, claim, metav1.DeleteOptions{})).To(Succeed())

			By("asserting the claim is held (finalizer) while the access references it")
			Consistently(func() bool {
				_, err := suiteDyn.Resource(bucketClaimGVR).Namespace(suiteCfg.namespace).Get(ctx, claim, metav1.GetOptions{})
				return err == nil // still present
			}).WithTimeout(20*time.Second).WithPolling(pollInterval).Should(BeTrue(), "claim must not be released until its BucketAccess is deleted")

			By("deleting the access: the claim is released and its owned Bucket is garbage-collected")
			Expect(suiteDyn.Resource(bucketAccessGVR).Namespace(suiteCfg.namespace).Delete(ctx, access, metav1.DeleteOptions{})).To(Succeed())
			Expect(waitResourceGone(ctx, bucketClaimGVR, suiteCfg.namespace, claim, resourceGoneTimeout)).To(Succeed())
			Expect(waitResourceGone(ctx, bucketGVR, "", boundBucket, resourceGoneTimeout)).To(Succeed())
		})
	})
}
