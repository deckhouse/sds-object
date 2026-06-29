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

// createSpecs brings up the shared ObjectStorageCluster, a bucket on it, asserts
// the standardised credentials Secret contract, and proves a real S3 round-trip
// through the generated credentials. These are the foundation specs; the
// validation and delete specs run on top of the cluster and bucket created here.
func createSpecs() {
	Describe("create", func() {
		It("creates the ObjectStorageCluster and reaches Ready", func() {
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.oscReadyTimeout+2*time.Minute)
			defer cancel()

			By("creating ObjectStorageCluster " + suiteCfg.oscName)
			Expect(createOSC(ctx, buildOSC(suiteCfg.oscName))).To(Succeed())

			By("waiting for the cluster Ready condition")
			Expect(waitOSCReady(ctx, suiteCfg.oscName)).To(Succeed())

			By("asserting the resolved status (phase, backend, endpoint)")
			phase, err := getStringField(ctx, objectStorageClusterGVR, "", suiteCfg.oscName, "status", "phase")
			Expect(err).NotTo(HaveOccurred())
			Expect(phase).To(Equal(objectv1alpha1.PhaseReady))

			backend, err := getStringField(ctx, objectStorageClusterGVR, "", suiteCfg.oscName, "status", "backend", "type")
			Expect(err).NotTo(HaveOccurred())
			Expect(backend).To(Equal(expectedBackend()), "backend type for profile %s", suiteCfg.oscType)

			endpoint, err := getStringField(ctx, objectStorageClusterGVR, "", suiteCfg.oscName, "status", "endpoint", "internal")
			Expect(err).NotTo(HaveOccurred())
			Expect(endpoint).NotTo(BeEmpty(), "status.endpoint.internal must be published")
		})

		It("creates an ObjectBucket and reaches Ready", func() {
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.obReadyTimeout+2*time.Minute)
			defer cancel()

			By("creating ObjectBucket " + suiteCfg.bucketName)
			ob := buildOB(suiteCfg.bucketName, suiteCfg.namespace, suiteCfg.oscName, objectv1alpha1.BucketReclaimDelete)
			Expect(createOB(ctx, ob)).To(Succeed())

			By("waiting for the bucket Ready condition")
			Expect(waitOBReady(ctx, suiteCfg.namespace, suiteCfg.bucketName)).To(Succeed())

			By("asserting status.bucketName and status.secretRef are populated")
			bucketName, err := getStringField(ctx, objectBucketGVR, suiteCfg.namespace, suiteCfg.bucketName, "status", "bucketName")
			Expect(err).NotTo(HaveOccurred())
			Expect(bucketName).To(Equal(suiteCfg.bucketName), "effective bucket name defaults to metadata.name")

			secretName, err := getStringField(ctx, objectBucketGVR, suiteCfg.namespace, suiteCfg.bucketName, "status", "secretRef", "name")
			Expect(err).NotTo(HaveOccurred())
			Expect(secretName).NotTo(BeEmpty(), "status.secretRef.name must be published")
		})

		It("writes a complete, owned credentials Secret", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			secretName, err := getStringField(ctx, objectBucketGVR, suiteCfg.namespace, suiteCfg.bucketName, "status", "secretRef", "name")
			Expect(err).NotTo(HaveOccurred())
			Expect(secretName).NotTo(BeEmpty())

			secret, err := suiteClientset.CoreV1().Secrets(suiteCfg.namespace).Get(ctx, secretName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "get credentials Secret %s", secretName)

			By("asserting all standardised S3 keys are present and non-empty")
			for _, key := range credsSecretKeys {
				Expect(secret.Data).To(HaveKey(key), "credentials Secret must carry %s", key)
				Expect(secret.Data[key]).NotTo(BeEmpty(), "credentials Secret %s must be non-empty", key)
			}

			By("asserting the Secret is owned by the ObjectBucket (cleaned up on delete)")
			Expect(secret.OwnerReferences).NotTo(BeEmpty(), "credentials Secret must be owned by the ObjectBucket")
			Expect(secret.OwnerReferences[0].Kind).To(Equal(objectv1alpha1.ObjectBucketKind))
			Expect(secret.OwnerReferences[0].Name).To(Equal(suiteCfg.bucketName))
		})

		It("performs an S3 write/list/read round-trip via the credentials", func() {
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.probeJobTimeout+2*time.Minute)
			defer cancel()

			secretName, err := getStringField(ctx, objectBucketGVR, suiteCfg.namespace, suiteCfg.bucketName, "status", "secretRef", "name")
			Expect(err).NotTo(HaveOccurred())
			Expect(secretName).NotTo(BeEmpty())

			By("running the mc probe Job against the bucket endpoint")
			Expect(runS3ProbeJob(ctx, "s3-probe", suiteCfg.namespace, secretName)).To(Succeed())
		})
	})
}

// expectedBackend maps the configured profile to the BackendType the cluster
// status should report.
func expectedBackend() string {
	switch suiteCfg.oscType {
	case string(objectv1alpha1.ClusterTypeSystem), string(objectv1alpha1.ClusterTypeLightweight):
		return string(objectv1alpha1.BackendGarage)
	case string(objectv1alpha1.ClusterTypeFull):
		return string(objectv1alpha1.BackendSeaweedFS)
	case string(objectv1alpha1.ClusterTypeHeavy):
		return string(objectv1alpha1.BackendCephRGW)
	default:
		return ""
	}
}
