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

	objectv1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

// orphanSpecs audits the backend's own state after a teardown, which no Kubernetes
// object reflects: the specs elsewhere assert that CRs and Secrets disappear, but a
// key or bucket left behind in Garage is invisible to them — and an S3 credential
// that outlives its BucketAccess is exactly what must not happen.
//
// It also covers the one credential the module creates behind the user's back: to
// delete a non-empty bucket the driver mints a temporary owner key, empties the
// bucket over S3 and revokes the key in a deferred call. A revocation that silently
// failed would leave a full-access key on a bucket nobody is watching.
//
// Garage only (the audit runs the `garage` CLI inside a data-plane pod), so it skips
// on the SeaweedFS and Ceph RGW profiles.
func orphanSpecs() {
	Describe("orphans", Ordered, func() {
		const bucket = "e2e-orphan-bucket"
		var (
			access   = accessName(bucket)
			keyID    string
			nonEmpty = "orphan-payload.txt"
		)

		BeforeAll(func() {
			if expectedBackend() != string(objectv1alpha1.BackendGarage) {
				Skip("the backend audit uses the garage CLI; only the Garage-backed profiles are covered")
			}
			DeferCleanup(func() {
				bg := context.Background()
				_ = suiteDyn.Resource(bucketAccessGVR).Namespace(suiteCfg.namespace).Delete(bg, access, metav1.DeleteOptions{})
				_ = suiteDyn.Resource(bucketClaimGVR).Namespace(suiteCfg.namespace).Delete(bg, claimName(bucket), metav1.DeleteOptions{})
				_ = suiteDyn.Resource(bucketClaimPolicyGVR).Delete(bg, policyName(bucket), metav1.DeleteOptions{})
				_ = suiteDyn.Resource(bucketGVR).Delete(bg, bucket, metav1.DeleteOptions{})
			})
		})

		It("issues a key the backend really has", func() {
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.obReadyTimeout+suiteCfg.probeJobTimeout+5*time.Minute)
			defer cancel()

			By("creating bucket " + bucket + " with an access, and filling it")
			Expect(createOSB(ctx, buildOSB(bucket, suiteCfg.oscName, objectv1alpha1.BucketReclaimDelete))).To(Succeed())
			Expect(waitOSBReady(ctx, bucket)).To(Succeed())
			Expect(createOSBPolicy(ctx, buildOSBPolicy(policyName(bucket), bucket, []string{suiteCfg.namespace}))).To(Succeed())
			Expect(createBucketClaim(ctx, buildBucketClaim(claimName(bucket), suiteCfg.namespace, bucket))).To(Succeed())
			Expect(createOSBAccess(ctx, buildOSBAccess(access, suiteCfg.namespace, claimName(bucket), objectv1alpha1.AccessReadWrite))).To(Succeed())
			Expect(waitAccessReady(ctx, suiteCfg.namespace, access)).To(Succeed())

			var err error
			keyID, err = getStringField(ctx, bucketAccessGVR, suiteCfg.namespace, access, "status", "accessKeyID")
			Expect(err).NotTo(HaveOccurred())
			Expect(keyID).NotTo(BeEmpty())

			// A non-empty bucket is the interesting case for deletion: Garage refuses to
			// delete one, so the driver has to empty it over S3 first.
			Expect(s3PutMarker(ctx, "s3-orphan-write", suiteCfg.namespace, credsSecretName(access), nonEmpty, "payload")).To(Succeed())

			By("asserting the issued key is present in the backend (the audit itself works)")
			keys, err := garageCLI(ctx, suiteCfg.oscName, "key", "list")
			Expect(err).NotTo(HaveOccurred(), "garage key list")
			GinkgoWriter.Printf("garage key list:\n%s\n", keys)
			Expect(keys).To(ContainSubstring(keyID),
				"the key recorded in status must exist in Garage, or the rest of this audit proves nothing")
		})

		It("leaves no key behind when the access is deleted", func() {
			ctx, cancel := context.WithTimeout(context.Background(), resourceGoneTimeout+5*time.Minute)
			defer cancel()

			By("deleting BucketAccess " + access)
			Expect(suiteDyn.Resource(bucketAccessGVR).Namespace(suiteCfg.namespace).
				Delete(ctx, access, metav1.DeleteOptions{})).To(Succeed())
			Expect(waitResourceGone(ctx, bucketAccessGVR, suiteCfg.namespace, access, resourceGoneTimeout)).To(Succeed())

			By("asserting the key is gone from Garage, not just from the CR")
			Eventually(func() (string, error) {
				return garageCLI(ctx, suiteCfg.oscName, "key", "list")
			}, 5*time.Minute, pollInterval).ShouldNot(ContainSubstring(keyID),
				"the revoked key must be deleted in the backend")
		})

		It("leaves no bucket and no temporary owner key behind when the bucket is deleted", func() {
			ctx, cancel := context.WithTimeout(context.Background(), resourceGoneTimeout+5*time.Minute)
			defer cancel()

			By("deleting the claim and the non-empty Bucket (reclaimPolicy Delete)")
			Expect(suiteDyn.Resource(bucketClaimGVR).Namespace(suiteCfg.namespace).
				Delete(ctx, claimName(bucket), metav1.DeleteOptions{})).To(Succeed())
			Expect(waitResourceGone(ctx, bucketClaimGVR, suiteCfg.namespace, claimName(bucket), resourceGoneTimeout)).To(Succeed())
			Expect(suiteDyn.Resource(bucketGVR).Delete(ctx, bucket, metav1.DeleteOptions{})).To(Succeed())
			Expect(waitResourceGone(ctx, bucketGVR, "", bucket, resourceGoneTimeout)).To(Succeed())

			By("asserting the bucket is gone from Garage")
			Eventually(func() (string, error) {
				return garageCLI(ctx, suiteCfg.oscName, "bucket", "list")
			}, 5*time.Minute, pollInterval).ShouldNot(ContainSubstring(bucket),
				"reclaimPolicy Delete must empty and remove the backend bucket")

			By("asserting the temporary owner key used to empty it was revoked")
			keys, err := garageCLI(ctx, suiteCfg.oscName, "key", "list")
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("garage key list after teardown:\n%s\n", keys)
			Expect(strings.ToLower(keys)).NotTo(ContainSubstring("sds-object-reclaim-"),
				"the short-lived owner key minted to empty the bucket must not survive it")
		})
	})
}
