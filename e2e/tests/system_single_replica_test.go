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
	"fmt"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commander "github.com/deckhouse/storage-e2e/pkg/commander"

	objectv1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

// systemSingleReplicaSpecs covers the sdsObject.systemBucket.singleReplica module
// setting end to end, through the path an operator actually takes: patch the
// ModuleConfig, let Deckhouse re-render the chart, and watch the controller
// converge.
//
// It asserts the whole contract of the setting:
//   - on: the system store runs ONE Garage replica with replication_factor 1, one
//     replica PVC, and a local-PV pool of one slot per control-plane node;
//   - the switch is a RECREATE, not an in-place resize: the data-plane incarnation
//     is bumped, the pool PVs are fresh objects backed by fresh directories, and a
//     marker object written before the switch is GONE afterwards;
//   - the store self-heals around the recreate: it returns to Ready, the bucket is
//     recreated and the access key re-issued, so the same credentials Secret keeps
//     serving an S3 round-trip;
//   - off: the store goes back to three replicas with factor 3, again by recreate
//     (another incarnation, data lost again).
//
// It is DESTRUCTIVE (that is the documented behaviour of the setting) and slow, so
// it runs last in the suite and can be skipped with E2E_SKIP_SYSTEM_RECREATE. It
// also skips unless the system store starts in the default 3-replica mode, so the
// on/off direction under test is unambiguous.
func systemSingleReplicaSpecs() {
	Describe("system-single-replica", Ordered, func() {
		const (
			systemStore = "system"
			// The suite drives the system bucket through its own policy + claim +
			// access, so the marker object and the round-trips do not depend on
			// fixtures other specs may have already torn down.
			testPolicy = "system-single-replica-policy"
			testAccess = "system-single-replica-access"
			// Own claim name: the system-bucket specs may leave their claim on the
			// same bucket behind, and a name collision would fail this spec on
			// AlreadyExists rather than on anything it is meant to test.
			testClaim  = "system-single-replica-claim"
			testBucket = "system"
			markerObj  = "single-replica-marker.txt"
			markerBody = "written before the singleReplica switch"
		)
		var (
			testSecret      = credsSecretName(testAccess)
			initialKeyID    string
			baseIncarnation int
		)

		// switchTimeout bounds one direction of the switch: Deckhouse re-rendering
		// the chart, the controller tearing the data plane down (pods, PVCs and
		// Retain PVs disappear only after their dependents), and the new cluster
		// meshing and reaching Ready.
		const switchTimeout = 20 * time.Minute

		BeforeAll(func() {
			if suiteCfg.skipSystemRecreate {
				Skip("E2E_SKIP_SYSTEM_RECREATE is set; skipping the destructive singleReplica switch")
			}
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.oscReadyTimeout+5*time.Minute)
			defer cancel()

			if _, err := suiteDyn.Resource(objectStoreGVR).Get(ctx, systemStore, metav1.GetOptions{}); err != nil {
				if apierrors.IsNotFound(err) {
					Skip("system ObjectStore not present (sdsObject.systemBucket.enabled is false)")
				}
				Expect(err).NotTo(HaveOccurred(), "get system ObjectStore")
			}

			_, hasRedundancy, err := storeRedundancy(ctx, systemStore)
			Expect(err).NotTo(HaveOccurred(), "read system store redundancy")
			if hasRedundancy {
				Skip("system ObjectStore is already in single-replica mode; these specs switch it on from the default profile")
			}

			By("waiting for the system store to be Ready before touching it")
			Expect(waitOSCReady(ctx, systemStore)).To(Succeed())

			baseIncarnation, err = garageSystemIncarnation(ctx, systemStore)
			Expect(err).NotTo(HaveOccurred(), "read the initial data-plane incarnation")
			GinkgoWriter.Printf("System store starts at incarnation %d\n", baseIncarnation)

			// Restore the default profile even if a spec fails midway, so the
			// remaining suite (and a preserved cluster) is left in a known mode.
			DeferCleanup(func() {
				bg := context.Background()
				_ = setSystemSingleReplica(bg, false)
				_ = suiteDyn.Resource(bucketAccessGVR).Namespace(suiteCfg.namespace).Delete(bg, testAccess, metav1.DeleteOptions{})
				_ = suiteDyn.Resource(bucketClaimGVR).Namespace(suiteCfg.namespace).Delete(bg, testClaim, metav1.DeleteOptions{})
				_ = suiteDyn.Resource(bucketClaimPolicyGVR).Delete(bg, testPolicy, metav1.DeleteOptions{})
			})
		})

		It("writes a marker object into the system bucket through its own access", func() {
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.obReadyTimeout+suiteCfg.probeJobTimeout+5*time.Minute)
			defer cancel()

			By("waiting for the shipped system Bucket to be Ready")
			Expect(waitOSBReady(ctx, testBucket)).To(Succeed())

			By("granting the test namespace access to the system bucket")
			Expect(createOSBPolicy(ctx, buildOSBPolicy(testPolicy, testBucket, []string{suiteCfg.namespace}))).To(Succeed())
			Expect(createBucketClaim(ctx, buildBucketClaim(testClaim, suiteCfg.namespace, testBucket))).To(Succeed())
			Expect(createOSBAccess(ctx, buildOSBAccess(testAccess, suiteCfg.namespace, testClaim, objectv1alpha1.AccessReadWrite))).To(Succeed())
			Expect(waitAccessReady(ctx, suiteCfg.namespace, testAccess)).To(Succeed())

			var err error
			initialKeyID, err = getStringField(ctx, bucketAccessGVR, suiteCfg.namespace, testAccess, "status", "accessKeyID")
			Expect(err).NotTo(HaveOccurred())
			Expect(initialKeyID).NotTo(BeEmpty(), "the access must record the issued key id")

			By("writing marker object " + markerObj)
			Expect(s3PutMarker(ctx, "s3-marker-write", suiteCfg.namespace, testSecret, markerObj, markerBody)).To(Succeed())
		})

		It("switches the system store to a single replica by recreating it", func() {
			ctx, cancel := context.WithTimeout(context.Background(), switchTimeout)
			defer cancel()

			By("setting sdsObject.systemBucket.singleReplica=true in the ModuleConfig")
			Expect(setSystemSingleReplica(ctx, true)).To(Succeed())

			By("waiting for Deckhouse to render spec.redundancy: None onto the system store")
			Eventually(func() (string, error) {
				value, _, err := storeRedundancy(ctx, systemStore)
				return value, err
			}, 10*time.Minute, 10*time.Second).Should(Equal(string(objectv1alpha1.RedundancyNone)),
				"the module must set redundancy None from the setting")

			By("waiting for the controller to rebuild the data plane at the next incarnation")
			Eventually(func(g Gomega) {
				incarnation, err := garageSystemIncarnation(ctx, systemStore)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(incarnation).To(Equal(baseIncarnation+1), "a replica-count switch must bump the incarnation")

				rf, err := garageReplicationFactor(ctx, systemStore)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(rf).To(Equal(1), "a single replica pins replication_factor 1")

				sts, err := suiteClientset.AppsV1().StatefulSets(moduleNS).Get(ctx, systemStore+"-garage", metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(sts.Spec.Replicas).NotTo(BeNil())
				g.Expect(*sts.Spec.Replicas).To(Equal(int32(1)))
				g.Expect(sts.Status.ReadyReplicas).To(Equal(int32(1)), "the single replica must be Ready")
			}, switchTimeout, 15*time.Second).Should(Succeed())

			By("waiting for the recreated store to report Ready")
			Expect(waitOSCReady(ctx, systemStore)).To(Succeed())

			By("asserting exactly one Garage pod runs, on a control-plane node")
			cpNodes, err := controlPlaneNodeNames(ctx)
			Expect(err).NotTo(HaveOccurred())
			nodes, err := garageRunningPodNodes(ctx, systemStore)
			Expect(err).NotTo(HaveOccurred())
			Expect(nodes).To(HaveLen(1), "single-replica System runs one pod")
			Expect(cpNodes).To(ContainElement(nodes[0]), "the replica must sit on a control-plane node")

			By("asserting a single replica PVC survived the switch, bound to the managed local StorageClass")
			pvcs, err := suiteClientset.CoreV1().PersistentVolumeClaims(moduleNS).List(ctx, metav1.ListOptions{
				LabelSelector: "storage.deckhouse.io/object-store=" + systemStore,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(pvcs.Items).To(HaveLen(1), "the previous incarnation's PVCs must be gone, one fresh PVC left")
			Expect(pvcs.Items[0].Status.Phase).To(Equal(corev1.ClaimBound))
			Expect(pvcs.Items[0].Spec.StorageClassName).NotTo(BeNil())
			Expect(*pvcs.Items[0].Spec.StorageClassName).To(Equal("sds-object-system-local"))

			By("asserting the local-PV pool is one fresh slot per control-plane node")
			pvs, err := suiteClientset.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{
				LabelSelector: "storage.deckhouse.io/object-store=" + systemStore + ",storage.deckhouse.io/system-local-node",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(pvs.Items).To(HaveLen(len(cpNodes)), "single-replica pool: one PV per master, no per-ordinal slots")
			bound := 0
			freshDir := fmt.Sprintf("/i%d/", baseIncarnation+1)
			for _, pv := range pvs.Items {
				Expect(pv.Spec.HostPath).NotTo(BeNil())
				Expect(pv.Spec.HostPath.Path).To(ContainSubstring(freshDir),
					"a recreated store must be backed by the new incarnation's directory, not the previous data")
				if pv.Status.Phase == corev1.VolumeBound {
					bound++
				}
			}
			Expect(bound).To(Equal(1), "exactly the single replica PVC is bound")
		})

		It("comes back empty: bucket recreated, key re-issued, marker gone", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()

			By("waiting for the bucket to be reprovisioned on the fresh Garage cluster")
			Expect(waitOSBReady(ctx, testBucket)).To(Succeed())

			By("waiting for the access key to be re-issued (the old key vanished with the store)")
			Eventually(func() (string, error) {
				return getStringField(ctx, bucketAccessGVR, suiteCfg.namespace, testAccess, "status", "accessKeyID")
			}, 10*time.Minute, 10*time.Second).ShouldNot(Or(BeEmpty(), Equal(initialKeyID)),
				"the controller must mint a fresh key once the recorded one is gone from the backend")
			Expect(waitAccessReady(ctx, suiteCfg.namespace, testAccess)).To(Succeed())

			By("asserting the marker object did NOT survive the recreate")
			Expect(s3AssertMarkerAbsent(ctx, "s3-marker-absent", suiteCfg.namespace, testSecret, markerObj)).To(Succeed())

			By("asserting the single-replica store serves a full S3 round-trip")
			Expect(runS3ProbeJob(ctx, "s3-probe-single-replica", suiteCfg.namespace, testSecret)).To(Succeed())
		})

		It("never migrates the single replica when masters are added", func() {
			if os.Getenv("E2E_COMMANDER_URL") == "" {
				Skip("adding a master needs Commander (E2E_COMMANDER_URL); the single-master run cannot spread anyway")
			}
			// The default profile answers new masters by spreading its replicas one per
			// node. The single-replica profile must not: there is no second copy to
			// re-replicate from, so relocating would discard the store. Growing the
			// control plane is the only way to tell "does not spread" from "had nowhere
			// to spread to", which is why this needs Commander.
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
			defer cancel()

			before, err := systemReplicaBindings(ctx, systemStore)
			Expect(err).NotTo(HaveOccurred())
			Expect(before).To(HaveLen(1))
			nodesBefore, err := garageRunningPodNodes(ctx, systemStore)
			Expect(err).NotTo(HaveOccurred())
			Expect(nodesBefore).To(HaveLen(1))
			GinkgoWriter.Printf("single replica sits on %s (%+v)\n", nodesBefore[0], before)

			DeferCleanup(func() {
				bg, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
				defer cancel()
				if err := commander.SetMasterCount(bg, 1); err != nil {
					GinkgoWriter.Printf("warning: scale control plane back to 1: %v\n", err)
					return
				}
				_ = waitOSCReady(bg, systemStore)
			})

			By("scaling the control plane from 1 to 3 masters via Commander")
			Expect(commander.SetMasterCount(ctx, 3)).To(Succeed())
			Eventually(func() (int, error) { return controlPlaneNodeCount(ctx) },
				15*time.Minute, 15*time.Second).Should(Equal(3))

			By("asserting the pool gains a slot per new master, so a move would have been possible")
			Eventually(func() (int, error) {
				pvs, err := suiteClientset.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{
					LabelSelector: "storage.deckhouse.io/object-store=" + systemStore + ",storage.deckhouse.io/system-local-node",
				})
				if err != nil {
					return 0, err
				}
				return len(pvs.Items), nil
			}, 10*time.Minute, 15*time.Second).Should(Equal(3), "one slot per control-plane node")

			By("asserting the replica stays put: same node, same volume, one pod")
			Consistently(func(g Gomega) {
				bindings, err := systemReplicaBindings(ctx, systemStore)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(bindings).To(Equal(before), "the single replica must never be re-homed")
				nodes, err := garageRunningPodNodes(ctx, systemStore)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(nodes).To(Equal(nodesBefore), "the pod must stay on its master")
			}, 5*time.Minute, 20*time.Second).Should(Succeed())
			Expect(waitOSCReady(ctx, systemStore)).To(Succeed(), "the store must stay healthy across the growth")

			By("scaling the control plane back to 1 master")
			Expect(commander.SetMasterCount(ctx, 1)).To(Succeed())
			Eventually(func() (int, error) { return controlPlaneNodeCount(ctx) },
				25*time.Minute, 15*time.Second).Should(Equal(1))
			Expect(waitOSCReady(ctx, systemStore)).To(Succeed())

			after, err := systemReplicaBindings(ctx, systemStore)
			Expect(err).NotTo(HaveOccurred())
			Expect(after).To(Equal(before), "shrinking back must not have moved the replica either")
		})

		It("switches back to three replicas, again by recreating the store", func() {
			ctx, cancel := context.WithTimeout(context.Background(), switchTimeout)
			defer cancel()

			By("setting sdsObject.systemBucket.singleReplica=false in the ModuleConfig")
			Expect(setSystemSingleReplica(ctx, false)).To(Succeed())

			By("waiting for Deckhouse to drop spec.redundancy from the system store")
			Eventually(func() (bool, error) {
				_, found, err := storeRedundancy(ctx, systemStore)
				return found, err
			}, 10*time.Minute, 10*time.Second).Should(BeFalse(),
				"clearing the setting must remove redundancy, restoring the default profile")

			// Restart the controller while the teardown is in flight. The pinned
			// factor is rewritten only after the data plane is fully gone, so a
			// restarted controller must still see the mismatch and finish the
			// recreate instead of leaving a half-switched cluster. Catching the exact
			// window is best-effort — the convergence assertions below hold either
			// way, and which case ran is logged.
			By("restarting the controller during the recreate")
			caught := false
			for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); {
				if strings.Contains(storeConditionMessage(ctx, systemStore, objectv1alpha1.ObjectStoreConditionBackendReady),
					"recreating the System store") {
					caught = true
					break
				}
				if incarnation, err := garageSystemIncarnation(ctx, systemStore); err == nil && incarnation > baseIncarnation+1 {
					break // the recreate already got past the teardown
				}
				time.Sleep(5 * time.Second)
			}
			GinkgoWriter.Printf("controller restarted with the teardown in flight: %v\n", caught)
			Expect(restartController(ctx, suiteCfg.moduleReadyTO)).To(Succeed(), "the controller must come back Ready")

			By("waiting for the controller to rebuild three replicas at the following incarnation")
			Eventually(func(g Gomega) {
				incarnation, err := garageSystemIncarnation(ctx, systemStore)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(incarnation).To(Equal(baseIncarnation+2), "switching back is another recreate")

				rf, err := garageReplicationFactor(ctx, systemStore)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(rf).To(Equal(3), "the default profile pins replication_factor 3")

				sts, err := suiteClientset.AppsV1().StatefulSets(moduleNS).Get(ctx, systemStore+"-garage", metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(sts.Spec.Replicas).NotTo(BeNil())
				g.Expect(*sts.Spec.Replicas).To(Equal(int32(3)))
				g.Expect(sts.Status.ReadyReplicas).To(Equal(int32(3)), "all three replicas must be Ready")
			}, switchTimeout, 15*time.Second).Should(Succeed())

			By("waiting for the restored store to report Ready")
			Expect(waitOSCReady(ctx, systemStore)).To(Succeed())

			By("asserting three replica PVCs on the managed local StorageClass")
			Eventually(func(g Gomega) {
				pvcs, err := suiteClientset.CoreV1().PersistentVolumeClaims(moduleNS).List(ctx, metav1.ListOptions{
					LabelSelector: "storage.deckhouse.io/object-store=" + systemStore,
				})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pvcs.Items).To(HaveLen(3))
				for _, pvc := range pvcs.Items {
					g.Expect(pvc.Status.Phase).To(Equal(corev1.ClaimBound), "PVC %s must be bound", pvc.Name)
				}
			}, 10*time.Minute, 10*time.Second).Should(Succeed())

			By("asserting the restored store serves a full S3 round-trip")
			Expect(waitOSBReady(ctx, testBucket)).To(Succeed())
			Eventually(func() error {
				return runS3ProbeJob(ctx, "s3-probe-restored", suiteCfg.namespace, testSecret)
			}, 10*time.Minute, 30*time.Second).Should(Succeed(),
				"the credentials Secret must keep working after the key is re-issued again")
		})
	})
}
