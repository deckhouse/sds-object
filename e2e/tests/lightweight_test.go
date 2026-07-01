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
			storageClass, err = resolveLightweightStorageClass(ctx)
			Expect(err).NotTo(HaveOccurred(), "resolve StorageClass for Lightweight")
			if storageClass == "" {
				Skip("no StorageClass available for Lightweight; set E2E_LIGHTWEIGHT_STORAGE_CLASS (or E2E_STORAGE_CLASS), or mark a default StorageClass")
			}
			GinkgoWriter.Printf("Lightweight profile using StorageClass %q (size %s)\n", storageClass, suiteCfg.oscSize)
		})

		It("creates a Lightweight ObjectStorageCluster (Garage on PVC) and reaches Ready", func() {
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.oscReadyTimeout+2*time.Minute)
			defer cancel()

			By("creating Lightweight ObjectStorageCluster " + oscName)
			osc := newOSC(oscName, map[string]interface{}{
				"type":       string(objectv1alpha1.ClusterTypeLightweight),
				"redundancy": string(objectv1alpha1.RedundancySingle),
				"storage": map[string]interface{}{
					"size":  suiteCfg.oscSize,
					"class": storageClass,
				},
			})
			Expect(createOSC(ctx, osc)).To(Succeed())

			By("waiting for the cluster Ready condition")
			Expect(waitOSCReady(ctx, oscName)).To(Succeed())

			backend, err := getStringField(ctx, objectStorageClusterGVR, "", oscName, "status", "backend", "type")
			Expect(err).NotTo(HaveOccurred())
			Expect(backend).To(Equal(string(objectv1alpha1.BackendGarage)), "Lightweight is backed by Garage")

			endpoint, err := getStringField(ctx, objectStorageClusterGVR, "", oscName, "status", "endpoint", "internal")
			Expect(err).NotTo(HaveOccurred())
			Expect(endpoint).NotTo(BeEmpty())
		})

		It("provisions a bucket and a complete credentials Secret", func() {
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.obReadyTimeout+2*time.Minute)
			defer cancel()

			By("creating ObjectBucket " + bucketName)
			ob := buildOB(bucketName, suiteCfg.namespace, oscName, objectv1alpha1.BucketReclaimDelete)
			Expect(createOB(ctx, ob)).To(Succeed())

			By("waiting for the bucket Ready condition")
			Expect(waitOBReady(ctx, suiteCfg.namespace, bucketName)).To(Succeed())

			var err error
			secretName, err = getStringField(ctx, objectBucketGVR, suiteCfg.namespace, bucketName, "status", "secretRef", "name")
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

		It("deletes the Lightweight bucket and cluster", func() {
			ctx, cancel := context.WithTimeout(context.Background(), resourceGoneTimeout+2*time.Minute)
			defer cancel()

			By("deleting ObjectBucket " + bucketName)
			Expect(suiteDyn.Resource(objectBucketGVR).Namespace(suiteCfg.namespace).
				Delete(ctx, bucketName, metav1.DeleteOptions{})).To(Succeed())
			Expect(waitResourceGone(ctx, objectBucketGVR, suiteCfg.namespace, bucketName, resourceGoneTimeout)).To(Succeed())
			if secretName != "" {
				Expect(waitSecretGone(ctx, suiteCfg.namespace, secretName, 2*time.Minute)).To(Succeed())
			}

			By("deleting ObjectStorageCluster " + oscName)
			Expect(suiteDyn.Resource(objectStorageClusterGVR).
				Delete(ctx, oscName, metav1.DeleteOptions{})).To(Succeed())
			Expect(waitResourceGone(ctx, objectStorageClusterGVR, "", oscName, resourceGoneTimeout)).To(Succeed())
		})
	})
}
