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
	"github.com/deckhouse/storage-e2e/pkg/testkit"
)

const (
	// sdsElasticRookGroupVersion is the Rook API group sds-elastic vendors under a
	// renamed group; it is served only when the sds-elastic module is installed,
	// so the Heavy specs use it to detect sds-elastic and skip otherwise. (The
	// ElasticCluster kind lives under storage.deckhouse.io, which sds-object also
	// serves, so it cannot be used as the presence signal.)
	sdsElasticRookGroupVersion = "internal.sdselastic.deckhouse.io/v1"

	// heavyECReadyTimeout covers sds-elastic bringing up the full Rook Ceph
	// cluster (mon/mgr/osd + csi-ceph wiring) behind the ElasticCluster.
	heavyECReadyTimeout = 30 * time.Minute
	// heavyOSCReadyTimeout covers the CephObjectStore (RGW) provisioning once the
	// ElasticCluster is Ready.
	heavyOSCReadyTimeout = 20 * time.Minute

	// OSD selector labels applied to storage nodes and consumable BlockDevices,
	// matched by the ElasticCluster storage selectors. sds-object-specific so they
	// never collide with sds-elastic's own e2e labels.
	heavyNodeLabelKey = "sds-object-e2e.storage.deckhouse.io/storage-node"
	heavyNodeLabelVal = "true"
	heavyOSDLabelKey  = "sds-object-e2e.storage.deckhouse.io/osd"
	heavyOSDLabelVal  = "true"
	// heavyMinOSDBlockDevices is the floor of consumable OSD BlockDevices that must
	// surface on the storage nodes before the ElasticCluster can come up.
	heavyMinOSDBlockDevices = 1
)

// heavySpecs exercises the Heavy profile (Ceph RADOS Gateway on top of an
// sds-elastic ElasticCluster) on its own cluster, alongside the primary flow:
// bring up the ElasticCluster (Rook Ceph) → create Heavy OSC → bucket + creds
// Secret → S3 round-trip → delete. It needs the sds-elastic module (enabled via
// cluster_config) and spare block devices for Ceph OSDs; it skips when
// sds-elastic is not installed, or when the primary profile is already Heavy.
func heavySpecs() {
	Describe("heavy", Ordered, func() {
		const oscName = "e2e-osc-heavy"
		const bucketName = "e2e-heavy-bucket"
		const ecName = "e2e-osc-heavy-ec" // cluster-scoped, <=30 chars, DNS-1123

		var secretName string

		BeforeAll(func() {
			if suiteCfg.oscType == string(objectv1alpha1.ClusterTypeHeavy) {
				Skip("primary profile is already Heavy; dedicated Heavy specs would duplicate it")
			}

			// sds-elastic provides the Ceph substrate; skip when it is not installed.
			served, err := groupVersionServed(sdsElasticRookGroupVersion)
			Expect(err).NotTo(HaveOccurred(), "discover %s", sdsElasticRookGroupVersion)
			if !served {
				Skip("sds-elastic is not installed (" + sdsElasticRookGroupVersion + " not served); Heavy needs it for the Ceph RGW substrate")
			}

			ctx, cancel := context.WithTimeout(context.Background(), heavyECReadyTimeout+10*time.Minute)
			defer cancel()

			By("labelling storage nodes and consumable OSD BlockDevices for the ElasticCluster")
			_, err = testkit.EnsureElasticOSDBlockDevices(ctx, suiteRestCfg, testkit.ElasticOSDBlockDevicesConfig{
				NodeLabelKey:          heavyNodeLabelKey,
				NodeLabelValue:        heavyNodeLabelVal,
				BlockDeviceLabelKey:   heavyOSDLabelKey,
				BlockDeviceLabelValue: heavyOSDLabelVal,
				MinBlockDevices:       heavyMinOSDBlockDevices,
			})
			Expect(err).NotTo(HaveOccurred(), "prepare OSD BlockDevices for the ElasticCluster")

			By("creating the ElasticCluster " + ecName + " and waiting for Ready (Rook Ceph)")
			_, err = testkit.EnsureElasticCluster(ctx, suiteRestCfg, testkit.ElasticClusterConfig{
				Name:                           ecName,
				NodeSelectorMatchLabels:        map[string]string{heavyNodeLabelKey: heavyNodeLabelVal},
				BlockDeviceSelectorMatchLabels: map[string]string{heavyOSDLabelKey: heavyOSDLabelVal},
				ReadyTimeout:                   heavyECReadyTimeout,
			})
			Expect(err).NotTo(HaveOccurred(), "ElasticCluster %s did not reach Ready", ecName)
		})

		AfterAll(func() {
			// Tear the ElasticCluster down so the Ceph substrate does not linger for
			// the rest of the suite / teardown. Best-effort: log but do not fail.
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			if err := testkit.TeardownElasticCluster(ctx, suiteRestCfg, ecName, 15*time.Minute); err != nil {
				GinkgoWriter.Printf("warning: ElasticCluster %s teardown failed: %v\n", ecName, err)
			}
		})

		It("creates a Heavy ObjectStorageCluster (Ceph RGW) and reaches Ready", func() {
			ctx, cancel := context.WithTimeout(context.Background(), heavyOSCReadyTimeout+2*time.Minute)
			defer cancel()

			By("creating Heavy ObjectStorageCluster " + oscName)
			osc := newOSC(oscName, map[string]interface{}{
				"type":              string(objectv1alpha1.ClusterTypeHeavy),
				"redundancy":        string(objectv1alpha1.RedundancySingle),
				"elasticClusterRef": ecName,
			})
			Expect(createOSC(ctx, osc)).To(Succeed())

			By("waiting for the cluster Ready condition (Ceph RGW)")
			Expect(waitCondition(ctx, objectStorageClusterGVR, "", oscName,
				objectv1alpha1.OSCConditionReady, "True", heavyOSCReadyTimeout)).To(Succeed())

			backend, err := getStringField(ctx, objectStorageClusterGVR, "", oscName, "status", "backend", "type")
			Expect(err).NotTo(HaveOccurred())
			Expect(backend).To(Equal(string(objectv1alpha1.BackendCephRGW)), "Heavy is backed by Ceph RGW")

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
			Expect(runS3ProbeJob(ctx, "s3-probe-heavy", suiteCfg.namespace, secretName)).To(Succeed())
		})

		It("deletes the Heavy bucket and cluster", func() {
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
