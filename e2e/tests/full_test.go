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

// postgresGroupVersion is the managed-postgres API that the SeaweedFS (Full)
// filer stores its metadata in; the Full specs require it to be served.
const postgresGroupVersion = "managed-services.deckhouse.io/v1alpha1"

// fullOSCReadyTimeout is generous: the Full profile brings up a distributed
// SeaweedFS (master/volume/filer) AND waits for managed-postgres to provision
// the shared filer database, which takes longer than a Garage cluster.
const fullOSCReadyTimeout = 30 * time.Minute

// fullSpecs exercises the Full profile (distributed SeaweedFS whose filer
// metadata lives in a shared PostgreSQL from the managed-postgres module) on its
// own cluster, alongside the primary flow: create → bucket + creds Secret → S3
// round-trip → delete. It needs a StorageClass and the managed-postgres CRD
// (enabled via cluster_config); it skips when either is missing, or when the
// primary profile is already Full.
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

			// Full requires the managed-postgres Postgres CRD (enabled via
			// cluster_config's modules list). Skip if it is not served.
			served, err := groupVersionServed(postgresGroupVersion)
			Expect(err).NotTo(HaveOccurred(), "discover %s", postgresGroupVersion)
			if !served {
				Skip("managed-postgres is not installed (" + postgresGroupVersion + " not served); Full needs it for the SeaweedFS filer metadata store")
			}

			storageClass, err = resolvePVCStorageClass(ctx)
			Expect(err).NotTo(HaveOccurred(), "resolve StorageClass for Full")
			if storageClass == "" {
				Skip("no StorageClass available for Full; set E2E_PVC_STORAGE_CLASS (or E2E_STORAGE_CLASS), or mark a default StorageClass")
			}
			GinkgoWriter.Printf("Full profile using StorageClass %q (size %s)\n", storageClass, suiteCfg.oscSize)
		})

		It("creates a Full ObjectStorageCluster (SeaweedFS) and reaches Ready", func() {
			ctx, cancel := context.WithTimeout(context.Background(), fullOSCReadyTimeout+2*time.Minute)
			defer cancel()

			By("creating Full ObjectStorageCluster " + oscName)
			osc := newOSC(oscName, map[string]interface{}{
				"type":       string(objectv1alpha1.ClusterTypeFull),
				"redundancy": string(objectv1alpha1.RedundancySingle),
				"storage": map[string]interface{}{
					"size":  suiteCfg.oscSize,
					"class": storageClass,
				},
			})
			Expect(createOSC(ctx, osc)).To(Succeed())

			By("waiting for the cluster Ready condition (SeaweedFS + managed-postgres)")
			Expect(waitCondition(ctx, objectStorageClusterGVR, "", oscName,
				objectv1alpha1.OSCConditionReady, "True", fullOSCReadyTimeout)).To(Succeed())

			backend, err := getStringField(ctx, objectStorageClusterGVR, "", oscName, "status", "backend", "type")
			Expect(err).NotTo(HaveOccurred())
			Expect(backend).To(Equal(string(objectv1alpha1.BackendSeaweedFS)), "Full is backed by SeaweedFS")

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
			Expect(runS3ProbeJob(ctx, "s3-probe-full", suiteCfg.namespace, secretName)).To(Succeed())
		})

		It("deletes the Full bucket and cluster", func() {
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
