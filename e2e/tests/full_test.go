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

// fullOSCReadyTimeout is generous: the Full profile brings up a distributed
// SeaweedFS (master/volume/filer), which takes longer than a Garage cluster.
const fullOSCReadyTimeout = 30 * time.Minute

// fullHRReadyTimeout covers the HighRedundancy Full cluster, which additionally
// waits for managed-postgres to provision the shared filer database and runs a
// multi-replica master/volume/filer topology.
const fullHRReadyTimeout = 40 * time.Minute

// postgresGroupVersion is the managed-postgres API the SeaweedFS filer uses for
// its shared metadata store in HighRedundancy (multi-filer HA). Single/
// Replicated use the built-in leveldb store and do NOT require it.
const postgresGroupVersion = "managed-services.deckhouse.io/v1alpha1"

// fullSpecs exercises the Full profile (SeaweedFS) on its own cluster, alongside
// the primary flow: create → bucket + creds Secret → S3 round-trip → delete.
// This spec uses redundancy Single, which stores filer metadata in the built-in
// leveldb store on a local PVC — so it needs a StorageClass but NOT
// managed-postgres (that is only required for HighRedundancy multi-filer HA). It
// skips when no StorageClass is available, or when the primary profile is
// already Full.
func fullSpecs() {
	Describe("full", Ordered, func() {
		const oscName = "e2e-osc-full"
		const bucketName = "e2e-full-bucket"

		var (
			storageClass string
			secretName   string
		)

		BeforeAll(func() {
			if suiteCfg.oscType == string(objectv1alpha1.ClusterTypeFull) {
				Skip("primary profile is already Full; dedicated Full specs would duplicate it")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			var err error
			storageClass, err = resolvePVCStorageClass(ctx)
			Expect(err).NotTo(HaveOccurred(), "resolve StorageClass for Full")
			if storageClass == "" {
				Skip("no StorageClass available for Full; set E2E_PVC_STORAGE_CLASS (or E2E_STORAGE_CLASS), or mark a default StorageClass")
			}
			GinkgoWriter.Printf("Full profile using StorageClass %q (size %s), leveldb metadata store (Single)\n", storageClass, suiteCfg.oscSize)
		})

		It("creates a Full ObjectStore (SeaweedFS) and reaches Ready", func() {
			ctx, cancel := context.WithTimeout(context.Background(), fullOSCReadyTimeout+2*time.Minute)
			defer cancel()

			By("creating Full ObjectStore " + oscName)
			osc := newOSC(oscName, map[string]interface{}{
				"type":       string(objectv1alpha1.ClusterTypeFull),
				"redundancy": string(objectv1alpha1.RedundancySingle),
				"storage": map[string]interface{}{
					"size":  suiteCfg.oscSize,
					"class": storageClass,
				},
			})
			Expect(createOSC(ctx, osc)).To(Succeed())

			By("waiting for the cluster Ready condition (SeaweedFS, leveldb store)")
			Expect(waitCondition(ctx, objectStoreGVR, "", oscName,
				objectv1alpha1.ObjectStoreConditionReady, "True", fullOSCReadyTimeout)).To(Succeed())

			backend, err := getStringField(ctx, objectStoreGVR, "", oscName, "status", "backend", "type")
			Expect(err).NotTo(HaveOccurred())
			Expect(backend).To(Equal(string(objectv1alpha1.BackendSeaweedFS)), "Full is backed by SeaweedFS")

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
			Expect(runS3ProbeJob(ctx, "s3-probe-full", suiteCfg.namespace, secretName)).To(Succeed())
		})

		It("deletes the Full access, bucket and cluster", func() {
			ctx, cancel := context.WithTimeout(context.Background(), resourceGoneTimeout+2*time.Minute)
			defer cancel()

			By("deleting BucketAccess " + accessName(bucketName))
			Expect(suiteDyn.Resource(bucketAccessGVR).Namespace(suiteCfg.namespace).
				Delete(ctx, accessName(bucketName), metav1.DeleteOptions{})).To(Succeed())
			Expect(waitResourceGone(ctx, bucketAccessGVR, suiteCfg.namespace, accessName(bucketName), resourceGoneTimeout)).To(Succeed())
			if secretName != "" {
				Expect(waitSecretGone(ctx, suiteCfg.namespace, secretName, 2*time.Minute)).To(Succeed())
			}

			By("deleting BucketPolicy + Bucket " + bucketName)
			_ = suiteDyn.Resource(bucketPolicyGVR).Delete(ctx, policyName(bucketName), metav1.DeleteOptions{})
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

// fullHighRedundancySpecs exercises the Full profile at redundancy
// HighRedundancy, which runs a multi-replica master/volume/filer topology whose
// filer metadata lives in a SHARED PostgreSQL provisioned via the
// managed-postgres module (contrast fullSpecs, which uses Single/leveldb). It
// needs a StorageClass AND the managed-postgres CRD, and skips when either is
// missing.
func fullHighRedundancySpecs() {
	Describe("full-highredundancy", Ordered, func() {
		const oscName = "e2e-osc-full-hr"
		const bucketName = "e2e-full-hr-bucket"

		var (
			storageClass string
			secretName   string
		)

		BeforeAll(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			// HighRedundancy is the mode that requires managed-postgres.
			served, err := groupVersionServed(postgresGroupVersion)
			Expect(err).NotTo(HaveOccurred(), "discover %s", postgresGroupVersion)
			if !served {
				Skip("managed-postgres is not installed (" + postgresGroupVersion + " not served); HighRedundancy Full needs it for the shared filer metadata store")
			}

			storageClass, err = resolvePVCStorageClass(ctx)
			Expect(err).NotTo(HaveOccurred(), "resolve StorageClass for HighRedundancy Full")
			if storageClass == "" {
				Skip("no StorageClass available for Full; set E2E_PVC_STORAGE_CLASS (or E2E_STORAGE_CLASS), or mark a default StorageClass")
			}
			GinkgoWriter.Printf("HighRedundancy Full using StorageClass %q, shared managed-postgres metadata store\n", storageClass)
		})

		It("creates a HighRedundancy Full cluster (SeaweedFS + managed-postgres) and reaches Ready", func() {
			ctx, cancel := context.WithTimeout(context.Background(), fullHRReadyTimeout+2*time.Minute)
			defer cancel()

			osc := newOSC(oscName, map[string]interface{}{
				"type":       string(objectv1alpha1.ClusterTypeFull),
				"redundancy": string(objectv1alpha1.RedundancyHighRedundancy),
				"storage": map[string]interface{}{
					"size":  suiteCfg.oscSize,
					"class": storageClass,
				},
			})
			Expect(createOSC(ctx, osc)).To(Succeed())

			By("waiting for the cluster Ready condition (multi-filer HA on PostgreSQL)")
			Expect(waitCondition(ctx, objectStoreGVR, "", oscName,
				objectv1alpha1.ObjectStoreConditionReady, "True", fullHRReadyTimeout)).To(Succeed())

			backend, err := getStringField(ctx, objectStoreGVR, "", oscName, "status", "backend", "type")
			Expect(err).NotTo(HaveOccurred())
			Expect(backend).To(Equal(string(objectv1alpha1.BackendSeaweedFS)))
		})

		It("provisions a bucket, access + policy and performs an S3 round-trip", func() {
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.obReadyTimeout+suiteCfg.probeJobTimeout+3*time.Minute)
			defer cancel()

			Expect(createOSB(ctx, buildOSB(bucketName, oscName, objectv1alpha1.BucketReclaimDelete))).To(Succeed())
			Expect(waitOSBReady(ctx, bucketName)).To(Succeed())

			Expect(createOSBPolicy(ctx, buildOSBPolicy(policyName(bucketName), bucketName, []string{suiteCfg.namespace}))).To(Succeed())
			Expect(createBucketClaim(ctx, buildBucketClaim(claimName(bucketName), suiteCfg.namespace, bucketName))).To(Succeed())
			Expect(createOSBAccess(ctx, buildOSBAccess(accessName(bucketName), suiteCfg.namespace, claimName(bucketName), objectv1alpha1.AccessReadWrite))).To(Succeed())
			Expect(waitAccessReady(ctx, suiteCfg.namespace, accessName(bucketName))).To(Succeed())

			var err error
			secretName, err = getStringField(ctx, bucketAccessGVR, suiteCfg.namespace, accessName(bucketName), "status", "secretRef", "name")
			Expect(err).NotTo(HaveOccurred())
			Expect(secretName).NotTo(BeEmpty())

			Expect(runS3ProbeJob(ctx, "s3-probe-full-hr", suiteCfg.namespace, secretName)).To(Succeed())
		})

		It("deletes the HighRedundancy Full access, bucket and cluster", func() {
			ctx, cancel := context.WithTimeout(context.Background(), resourceGoneTimeout+2*time.Minute)
			defer cancel()

			Expect(suiteDyn.Resource(bucketAccessGVR).Namespace(suiteCfg.namespace).
				Delete(ctx, accessName(bucketName), metav1.DeleteOptions{})).To(Succeed())
			Expect(waitResourceGone(ctx, bucketAccessGVR, suiteCfg.namespace, accessName(bucketName), resourceGoneTimeout)).To(Succeed())
			if secretName != "" {
				Expect(waitSecretGone(ctx, suiteCfg.namespace, secretName, 2*time.Minute)).To(Succeed())
			}

			_ = suiteDyn.Resource(bucketPolicyGVR).Delete(ctx, policyName(bucketName), metav1.DeleteOptions{})
			Expect(suiteDyn.Resource(bucketGVR).Delete(ctx, bucketName, metav1.DeleteOptions{})).To(Succeed())
			Expect(waitResourceGone(ctx, bucketGVR, "", bucketName, resourceGoneTimeout)).To(Succeed())

			Expect(suiteDyn.Resource(objectStoreGVR).Delete(ctx, oscName, metav1.DeleteOptions{})).To(Succeed())
			Expect(waitResourceGone(ctx, objectStoreGVR, "", oscName, resourceGoneTimeout)).To(Succeed())
		})
	})
}
