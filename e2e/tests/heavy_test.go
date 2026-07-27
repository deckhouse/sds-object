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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

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
			// Tear the Ceph substrate down so it does not linger. What can still hold
			// it back is sds-elastic's own VolumesExist guard: the cluster's OSD PVs are
			// Retain and keep the guard tripped after the OSDs are gone, so the cleanup
			// reaps them while it waits. (The CephObjectStore no longer belongs on that
			// list — the module removes its own on ObjectStore deletion.)
			// Best-effort: log but do not fail.
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
			defer cancel()
			cleanupHeavyElastic(ctx, oscName, ecName)
		})

		It("creates a Heavy ObjectStore (Ceph RGW) and reaches Ready", func() {
			ctx, cancel := context.WithTimeout(context.Background(), heavyOSCReadyTimeout+2*time.Minute)
			defer cancel()

			By("creating Heavy ObjectStore " + oscName)
			osc := newOSC(oscName, map[string]interface{}{
				"type":              string(objectv1alpha1.ClusterTypeHeavy),
				"redundancy":        string(objectv1alpha1.RedundancyNone),
				"elasticClusterRef": ecName,
			})
			Expect(createOSC(ctx, osc)).To(Succeed())

			By("waiting for the cluster Ready condition (Ceph RGW)")
			Expect(waitCondition(ctx, objectStoreGVR, "", oscName,
				objectv1alpha1.ObjectStoreConditionReady, "True", heavyOSCReadyTimeout)).To(Succeed())

			backend, err := getStringField(ctx, objectStoreGVR, "", oscName, "status", "backend", "type")
			Expect(err).NotTo(HaveOccurred())
			Expect(backend).To(Equal(string(objectv1alpha1.BackendCephRGW)), "Heavy is backed by Ceph RGW")

			endpoint, err := getStringField(ctx, objectStoreGVR, "", oscName, "status", "endpoint", "internal")
			Expect(err).NotTo(HaveOccurred())
			Expect(endpoint).NotTo(BeEmpty())

			// The Ceph-side data safety of a Heavy store lives entirely in the
			// CephObjectStore the driver renders: the reclaim policy decides whether
			// Rook is allowed to destroy the RGW pools (and with them every bucket's
			// objects, regardless of any bucket's own Retain), and the redundancy
			// intent decides the pools' replication. Neither is visible on the
			// ObjectStore itself, so assert it where it is enforced.
			By("asserting the rendered CephObjectStore preserves the pools and maps redundancy to pool sizes")
			cos, err := suiteDyn.Resource(cephObjectStoreGVR).Namespace(sdsElasticNamespace).
				Get(ctx, oscName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "get CephObjectStore %s/%s", sdsElasticNamespace, oscName)

			preserve, found, err := unstructured.NestedBool(cos.Object, "spec", "preservePoolsOnDelete")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue(), "preservePoolsOnDelete must be set explicitly, not left to Rook's default")
			Expect(preserve).To(BeTrue(), "this store is Retain (the CRD default), so the pools must be preserved")

			// redundancy None: data pool size 2 with the safe-replica guard off (size 1
			// is unsafe), metadata pool always 3.
			dataSize, _, err := unstructured.NestedInt64(cos.Object, "spec", "dataPool", "replicated", "size")
			Expect(err).NotTo(HaveOccurred())
			Expect(dataSize).To(Equal(int64(2)))
			safeReplica, _, err := unstructured.NestedBool(cos.Object, "spec", "dataPool", "replicated", "requireSafeReplicaSize")
			Expect(err).NotTo(HaveOccurred())
			Expect(safeReplica).To(BeFalse(), "size 2 needs the guard disabled, or Ceph refuses the pool")
			metaSize, _, err := unstructured.NestedInt64(cos.Object, "spec", "metadataPool", "replicated", "size")
			Expect(err).NotTo(HaveOccurred())
			Expect(metaSize).To(Equal(int64(3)), "RGW metadata is always kept at three copies")
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
			Expect(runS3ProbeJob(ctx, "s3-probe-heavy", suiteCfg.namespace, secretName)).To(Succeed())
		})

		It("deletes the Heavy access, bucket and cluster", func() {
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

			// The module must take its CephObjectStore with it, under Retain as much as
			// under Delete: nobody else can remove it (sds-elastic's webhook rejects
			// vendored-Rook deletes from anyone it does not know, the garbage collector
			// included), and while it exists Rook keeps the object store alive, so the
			// ElasticCluster can never finish terminating. The pools are preserved by
			// preservePoolsOnDelete, asserted on creation above.
			By("asserting the rendered CephObjectStore was removed with the store")
			Eventually(func() bool {
				_, err := suiteDyn.Resource(cephObjectStoreGVR).Namespace(sdsElasticNamespace).
					Get(ctx, oscName, metav1.GetOptions{})
				return apierrors.IsNotFound(err) || meta.IsNoMatchError(err)
			}, resourceGoneTimeout, pollInterval).Should(BeTrue(),
				"a leftover CephObjectStore strands the Rook cluster and blocks the ElasticCluster finalizer")
		})
	})
}

// sdsElasticNamespace is where sds-elastic runs the Rook Ceph substrate.
const sdsElasticNamespace = "d8-sds-elastic"

// cephObjectStoreGVR is the sds-elastic-internal CephObjectStore the Heavy
// backend provisions for a Ceph RGW ObjectStore (same group/version as
// sdsElasticRookGroupVersion).
var cephObjectStoreGVR = schema.GroupVersionResource{
	Group: "internal.sdselastic.deckhouse.io", Version: "v1", Resource: "cephobjectstores",
}

// cleanupHeavyElastic removes the Ceph substrate created for the Heavy specs:
//
//  1. reap the cluster's Retain OSD PVs in the background as Rook releases them
//     (they persist after the OSDs go and keep sds-elastic's VolumesExist guard
//     tripped, blocking the ElasticCluster finalizer);
//  2. delete the ElasticCluster and wait for it to be gone.
//
// It no longer tries to delete a leftover CephObjectStore. That step could never
// work — sds-elastic's validating webhook rejects requests to vendored Rook
// resources from anyone it does not know, so the suite's admin user was denied
// ("Direct modifications to Rook Ceph resources are not allowed") — and it is no
// longer needed: the module removes its own CephObjectStore on ObjectStore
// deletion under both reclaim policies, keeping the pools via
// preservePoolsOnDelete.
//
// Best-effort throughout: it logs and never fails the suite.
func cleanupHeavyElastic(ctx context.Context, oscName, ecName string) {
	// The module's own teardown must have removed the CephObjectStore already; if
	// one is still there, the ElasticCluster wait below will not finish, so say so
	// rather than letting the timeout look like an sds-elastic problem.
	if _, err := suiteDyn.Resource(cephObjectStoreGVR).Namespace(sdsElasticNamespace).
		Get(ctx, oscName, metav1.GetOptions{}); err == nil {
		GinkgoWriter.Printf("warning: CephObjectStore %s/%s still exists after the ObjectStore was deleted; "+
			"it pins the Rook cluster and will block the ElasticCluster finalizer\n", sdsElasticNamespace, oscName)
	} else if !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
		GinkgoWriter.Printf("warning: check CephObjectStore %s/%s: %v\n", sdsElasticNamespace, oscName, err)
	}

	osdPVPrefix := "sds-elastic-" + ecName + "-osd-"
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			reapReleasedPVs(ctx, osdPVPrefix)
			select {
			case <-stop:
				return
			case <-time.After(15 * time.Second):
			}
		}
	}()

	if err := testkit.TeardownElasticCluster(ctx, suiteRestCfg, ecName, 20*time.Minute); err != nil {
		GinkgoWriter.Printf("warning: ElasticCluster %s teardown failed: %v\n", ecName, err)
	}
	close(stop)
	<-done
}

// reapReleasedPVs deletes PVs whose name starts with prefix and are no longer
// Bound (Released/Available/Failed). Bound PVs are skipped so a still-running OSD
// is never yanked out from under Ceph.
func reapReleasedPVs(ctx context.Context, prefix string) {
	pvs, err := suiteClientset.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	for i := range pvs.Items {
		pv := &pvs.Items[i]
		if !strings.HasPrefix(pv.Name, prefix) || pv.Status.Phase == corev1.VolumeBound {
			continue
		}
		_ = suiteClientset.CoreV1().PersistentVolumes().Delete(ctx, pv.Name, metav1.DeleteOptions{})
	}
}
