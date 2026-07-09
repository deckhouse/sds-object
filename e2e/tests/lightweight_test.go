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

// lightweightSpecs exercises the Lightweight profile (Garage backed by a PVC on
// a StorageClass) on its own cluster, alongside the primary (default: System)
// flow: create → bucket + creds Secret → S3 round-trip → delete. It needs a
// StorageClass (E2E_LIGHTWEIGHT_STORAGE_CLASS / E2E_STORAGE_CLASS / the cluster
// default); when none is available it skips. Skipped, too, when the primary
// profile is already Lightweight (the create/delete specs cover it then).
func lightweightSpecs() {
	Describe("lightweight", Ordered, func() {
		const oscName = "e2e-osc-light"
		const bucketName = "e2e-light-bucket"

		var (
			storageClass string
			secretName   string
		)

		BeforeAll(func() {
			if suiteCfg.oscType == string(objectv1alpha1.ClusterTypeLightweight) {
				Skip("primary profile is already Lightweight; dedicated Lightweight specs would duplicate it")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			var err error
			storageClass, err = resolvePVCStorageClass(ctx)
			Expect(err).NotTo(HaveOccurred(), "resolve StorageClass for Lightweight")
			if storageClass == "" {
				Skip("no StorageClass available for Lightweight; set E2E_PVC_STORAGE_CLASS (or E2E_STORAGE_CLASS), or mark a default StorageClass")
			}
			GinkgoWriter.Printf("Lightweight profile using StorageClass %q (size %s)\n", storageClass, suiteCfg.oscSize)
		})

		It("creates a Lightweight ObjectStore (Garage on PVC) and reaches Ready", func() {
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.oscReadyTimeout+2*time.Minute)
			defer cancel()

			By("creating Lightweight ObjectStore " + oscName)
			osc := newOSC(oscName, map[string]interface{}{
				"type":       string(objectv1alpha1.ClusterTypeLightweight),
				"redundancy": string(objectv1alpha1.RedundancyNone),
				"storage": map[string]interface{}{
					"sizePerNode": suiteCfg.oscSize,
					"class":       storageClass,
				},
			})
			Expect(createOSC(ctx, osc)).To(Succeed())

			By("waiting for the cluster Ready condition")
			Expect(waitOSCReady(ctx, oscName)).To(Succeed())

			backend, err := getStringField(ctx, objectStoreGVR, "", oscName, "status", "backend", "type")
			Expect(err).NotTo(HaveOccurred())
			Expect(backend).To(Equal(string(objectv1alpha1.BackendGarage)), "Lightweight is backed by Garage")

			endpoint, err := getStringField(ctx, objectStoreGVR, "", oscName, "status", "endpoint", "internal")
			Expect(err).NotTo(HaveOccurred())
			Expect(endpoint).NotTo(BeEmpty())
		})

		It("provisions a bucket, access + policy and a complete credentials Secret", func() {
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.obReadyTimeout+2*time.Minute)
			defer cancel()

			By("creating Bucket " + bucketName)
			Expect(createOSB(ctx, buildOSB(bucketName, oscName, objectv1alpha1.BucketReclaimDelete))).To(Succeed())
			Expect(waitOSBReady(ctx, bucketName)).To(Succeed())

			By("creating policy + BucketAccess " + accessName(bucketName))
			Expect(createOSBPolicy(ctx, buildOSBPolicy(policyName(bucketName), bucketName, []string{suiteCfg.namespace}))).To(Succeed())
			Expect(createBucketClaim(ctx, buildBucketClaim(claimName(bucketName), suiteCfg.namespace, bucketName))).To(Succeed())
			Expect(createOSBAccess(ctx, buildOSBAccess(accessName(bucketName), suiteCfg.namespace, claimName(bucketName), objectv1alpha1.AccessReadWrite))).To(Succeed())
			Expect(waitAccessReady(ctx, suiteCfg.namespace, accessName(bucketName))).To(Succeed())

			var err error
			secretName, err = getStringField(ctx, bucketAccessGVR, suiteCfg.namespace, accessName(bucketName), "status", "secretRef", "name")
			Expect(err).NotTo(HaveOccurred())
			Expect(secretName).NotTo(BeEmpty())

			secret, err := suiteClientset.CoreV1().Secrets(suiteCfg.namespace).Get(ctx, secretName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "get credentials Secret %s", secretName)
			for _, key := range credsSecretKeys {
				Expect(secret.Data).To(HaveKey(key))
				Expect(secret.Data[key]).NotTo(BeEmpty(), "credentials Secret %s must be non-empty", key)
			}
		})

		It("performs an S3 write/list/read round-trip via the credentials", func() {
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.probeJobTimeout+2*time.Minute)
			defer cancel()

			Expect(secretName).NotTo(BeEmpty())
			Expect(runS3ProbeJob(ctx, "s3-probe-light", suiteCfg.namespace, secretName)).To(Succeed())
		})

		It("deletes the Lightweight access, bucket and cluster", func() {
			ctx, cancel := context.WithTimeout(context.Background(), resourceGoneTimeout+2*time.Minute)
			defer cancel()

			By("deleting BucketAccess " + accessName(bucketName))
			Expect(suiteDyn.Resource(bucketAccessGVR).Namespace(suiteCfg.namespace).
				Delete(ctx, accessName(bucketName), metav1.DeleteOptions{})).To(Succeed())
			Expect(waitResourceGone(ctx, bucketAccessGVR, suiteCfg.namespace, accessName(bucketName), resourceGoneTimeout)).To(Succeed())
			if secretName != "" {
				Expect(waitSecretGone(ctx, suiteCfg.namespace, secretName, 2*time.Minute)).To(Succeed())
			}

			By("deleting BucketClaimPolicy + Bucket " + bucketName)
			_ = suiteDyn.Resource(bucketClaimPolicyGVR).Delete(ctx, policyName(bucketName), metav1.DeleteOptions{})
			Expect(suiteDyn.Resource(bucketGVR).
				Delete(ctx, bucketName, metav1.DeleteOptions{})).To(Succeed())
			Expect(waitResourceGone(ctx, bucketGVR, "", bucketName, resourceGoneTimeout)).To(Succeed())

			By("deleting ObjectStore " + oscName)
			Expect(suiteDyn.Resource(objectStoreGVR).
				Delete(ctx, oscName, metav1.DeleteOptions{})).To(Succeed())
			Expect(waitResourceGone(ctx, objectStoreGVR, "", oscName, resourceGoneTimeout)).To(Succeed())
		})
	})
}
