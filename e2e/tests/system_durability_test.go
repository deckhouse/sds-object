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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	objectv1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

// systemDurabilitySpecs covers the two mechanisms the System profile's data
// safety rests on, neither of which needs a Commander-driven master-count change:
//
//   - node-sticky local PVs: a full data-plane restart must bring every replica
//     back to ITS volume on ITS node, so the stored objects survive. This is the
//     failure class the profile was designed against — with plain hostPath the
//     scheduler could permute the pods across nodes and each would come up over an
//     empty directory, silently orphaning the data.
//   - stable Garage node identity: a recycled replica (the step a rebalance is made
//     of) comes back with a FRESH, empty volume but its ORIGINAL node ID, restored
//     from the identity Secret by the restore-node-key initContainer, and
//     re-replicates from the survivors. Without it the old ID would linger in the
//     layout as a dead node and the cluster would hang in `degraded`.
//
// Both specs are non-destructive: they assert the data is still there afterwards.
// They run against the shipped system store in whatever replica mode the suite is
// in; the recycle spec needs a surviving copy, so it skips on a single replica.
func systemDurabilitySpecs() {
	Describe("system-durability", Ordered, func() {
		const (
			systemStore = "system"
			testBucket  = "system"
			testPolicy  = "system-durability-policy"
			testClaim   = "system-durability-claim"
			testAccess  = "system-durability-access"
			markerObj   = "durability-marker.txt"
			markerBody  = "written before the data-plane restart"
		)
		var (
			testSecret = credsSecretName(testAccess)
			keyID      string
			replicas   int32
		)

		BeforeAll(func() {
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.oscReadyTimeout+suiteCfg.obReadyTimeout+5*time.Minute)
			defer cancel()

			if _, err := suiteDyn.Resource(objectStoreGVR).Get(ctx, systemStore, metav1.GetOptions{}); err != nil {
				if apierrors.IsNotFound(err) {
					Skip("system ObjectStore not present (sdsObject.systemBucket.enabled is false)")
				}
				Expect(err).NotTo(HaveOccurred(), "get system ObjectStore")
			}

			By("waiting for the system store and bucket to be Ready")
			Expect(waitOSCReady(ctx, systemStore)).To(Succeed())
			Expect(waitOSBReady(ctx, testBucket)).To(Succeed())

			var err error
			replicas, _, err = statefulSetReadyReplicas(ctx, systemStore)
			Expect(err).NotTo(HaveOccurred(), "read the System StatefulSet")
			GinkgoWriter.Printf("System store runs %d replica(s)\n", replicas)

			By("granting the test namespace access to the system bucket")
			Expect(createOSBPolicy(ctx, buildOSBPolicy(testPolicy, testBucket, []string{suiteCfg.namespace}))).To(Succeed())
			Expect(createBucketClaim(ctx, buildBucketClaim(testClaim, suiteCfg.namespace, testBucket))).To(Succeed())
			Expect(createOSBAccess(ctx, buildOSBAccess(testAccess, suiteCfg.namespace, testClaim, objectv1alpha1.AccessReadWrite))).To(Succeed())
			Expect(waitAccessReady(ctx, suiteCfg.namespace, testAccess)).To(Succeed())

			keyID, err = getStringField(ctx, bucketAccessGVR, suiteCfg.namespace, testAccess, "status", "accessKeyID")
			Expect(err).NotTo(HaveOccurred())
			Expect(keyID).NotTo(BeEmpty())

			DeferCleanup(func() {
				bg := context.Background()
				_ = suiteDyn.Resource(bucketAccessGVR).Namespace(suiteCfg.namespace).Delete(bg, testAccess, metav1.DeleteOptions{})
				_ = suiteDyn.Resource(bucketClaimGVR).Namespace(suiteCfg.namespace).Delete(bg, testClaim, metav1.DeleteOptions{})
				_ = suiteDyn.Resource(bucketClaimPolicyGVR).Delete(bg, testPolicy, metav1.DeleteOptions{})
			})
		})

		It("keeps every replica on its own volume and node across a full data-plane restart", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
			defer cancel()

			By("writing marker object " + markerObj)
			Expect(s3PutMarker(ctx, "s3-durability-write", suiteCfg.namespace, testSecret, markerObj, markerBody)).To(Succeed())

			By("recording the PVC -> PV -> node bindings of every replica")
			before, err := systemReplicaBindings(ctx, systemStore)
			Expect(err).NotTo(HaveOccurred())
			Expect(before).To(HaveLen(int(replicas)), "one node-sticky PVC per replica")
			for pvc, binding := range before {
				Expect(binding.pvName).NotTo(BeEmpty(), "PVC %s must be bound to a pool PV", pvc)
				Expect(binding.node).NotTo(BeEmpty(), "PV %s must pin a node", binding.pvName)
				GinkgoWriter.Printf("  %s -> %s on %s\n", pvc, binding.pvName, binding.node)
			}

			By("deleting every Garage pod at once (the permutation risk a plain hostPath would lose data to)")
			deleted, err := deleteGaragePods(ctx, systemStore)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(HaveLen(int(replicas)))

			By("waiting for the data plane to come back whole")
			Eventually(func(g Gomega) {
				pods, err := garagePods(ctx, systemStore)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pods.Items).To(HaveLen(int(replicas)))
				for i := range pods.Items {
					p := &pods.Items[i]
					g.Expect(deleted[p.Name]).NotTo(Equal(p.UID), "pod %s is still the pre-restart instance", p.Name)
					g.Expect(p.Status.Phase).To(Equal(corev1.PodRunning), "pod %s must be Running", p.Name)
				}
				desired, ready, err := statefulSetReadyReplicas(ctx, systemStore)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(ready).To(Equal(desired), "all replicas must be Ready again")
			}, 15*time.Minute, 10*time.Second).Should(Succeed())
			Expect(waitOSCReady(ctx, systemStore)).To(Succeed(), "the store must return to Ready (Garage healthy)")

			By("asserting the bindings are untouched: same PVC -> same PV -> same node")
			after, err := systemReplicaBindings(ctx, systemStore)
			Expect(err).NotTo(HaveOccurred())
			Expect(after).To(Equal(before), "a restart must never re-home a replica onto another volume")

			By("asserting every pod runs on the node its own volume is pinned to")
			pods, err := garagePods(ctx, systemStore)
			Expect(err).NotTo(HaveOccurred())
			for i := range pods.Items {
				p := &pods.Items[i]
				binding, ok := after["data-"+p.Name]
				Expect(ok).To(BeTrue(), "no PVC recorded for pod %s", p.Name)
				Expect(nodeHostname(ctx, p.Spec.NodeName)).To(Equal(binding.node),
					"pod %s must run on the node its PV pins (%s)", p.Name, binding.node)
			}

			By("asserting the stored object survived, on the same credentials")
			Expect(s3AssertMarkerContent(ctx, "s3-durability-read", suiteCfg.namespace, testSecret, markerObj, markerBody)).To(Succeed())
			currentKey, err := getStringField(ctx, bucketAccessGVR, suiteCfg.namespace, testAccess, "status", "accessKeyID")
			Expect(err).NotTo(HaveOccurred())
			Expect(currentKey).To(Equal(keyID),
				"a restart must not look like a recreate: the backend keys must still be there")
		})

		It("rebinds a recycled replica and brings it back with its original identity", func() {
			if replicas < 2 {
				Skip("a recycle relies on the surviving copies to serve and re-replicate; a single-replica store has none")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()

			const ordinal = 0
			podName := fmt.Sprintf("%s-garage-%d", systemStore, ordinal)
			pvcName := "data-" + podName

			By("reading the persisted Garage identity of replica " + podName)
			identity, err := suiteClientset.CoreV1().Secrets(moduleNS).Get(ctx, systemStore+"-garage-node-identity", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "the controller must persist the replica identities")
			keyName := fmt.Sprintf("node-%d", ordinal)
			Expect(identity.Data).To(HaveKey(keyName), "identity Secret must carry the key of ordinal %d", ordinal)
			Expect(identity.Data).To(HaveKey(keyName + ".pub"))
			identityBefore := append([]byte(nil), identity.Data[keyName]...)

			before, err := systemReplicaBindings(ctx, systemStore)
			Expect(err).NotTo(HaveOccurred())
			oldPV := before[pvcName]
			Expect(oldPV.pvName).NotTo(BeEmpty())
			Expect(oldPV.pvUID).NotTo(BeEmpty())

			By("recycling the replica: deleting its PVC and pod, exactly as a rebalance does")
			Expect(suiteClientset.CoreV1().PersistentVolumeClaims(moduleNS).
				Delete(ctx, pvcName, metav1.DeleteOptions{})).To(Succeed())
			Expect(suiteClientset.CoreV1().Pods(moduleNS).
				Delete(ctx, podName, metav1.DeleteOptions{})).To(Succeed())

			// The replacement PVC binds a pool PV wherever the scheduler places the
			// pod: another master when one is free, or the same slot on the only
			// master (pool PV names are deterministic per node and slot, so the name
			// can repeat — the OBJECT is always a new one, since a Released PV is
			// reaped rather than rebound).
			By("waiting for the StatefulSet to recreate the replica and bind it to a fresh pool PV")
			Eventually(func(g Gomega) {
				bindings, err := systemReplicaBindings(ctx, systemStore)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(bindings).To(HaveKey(pvcName), "the StatefulSet must recreate the replica PVC")
				g.Expect(bindings[pvcName].pvUID).NotTo(BeEmpty(), "the fresh PVC must bind a pool PV")
				g.Expect(bindings[pvcName].pvUID).NotTo(Equal(oldPV.pvUID), "the previous PV object must not be rebound")

				pod, err := suiteClientset.CoreV1().Pods(moduleNS).Get(ctx, podName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning))
			}, 15*time.Minute, 10*time.Second).Should(Succeed())

			By("asserting the replica restored its persisted identity instead of generating a new one")
			log, err := containerLog(ctx, moduleNS, podName, "restore-node-key")
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("restore-node-key: %s\n", strings.TrimSpace(log))
			Expect(log).To(ContainSubstring(fmt.Sprintf("restored garage node identity for ordinal %d", ordinal)),
				"the initContainer must restore node_key from the identity Secret; a generated ID would strand the old one as a dead node in the layout")

			By("asserting the controller did not overwrite the stored identity")
			identityAfter, err := suiteClientset.CoreV1().Secrets(moduleNS).Get(ctx, systemStore+"-garage-node-identity", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(identityAfter.Data[keyName]).To(Equal(identityBefore), "a persisted identity is stable for the life of the cluster")

			By("waiting for the layout to settle and the store to report healthy again")
			Expect(waitOSCReady(ctx, systemStore)).To(Succeed())

			By("asserting the store still serves the stored object after the recycle")
			Expect(s3AssertMarkerContent(ctx, "s3-recycle-read", suiteCfg.namespace, testSecret, markerObj, markerBody)).To(Succeed())

			By("asserting the Released PV left behind by the recycle is reaped")
			Eventually(func(g Gomega) {
				// Gone, or replaced by a new object under the same deterministic name
				// — either way the Released one must not linger.
				pv, err := suiteClientset.CoreV1().PersistentVolumes().Get(ctx, oldPV.pvName, metav1.GetOptions{})
				if apierrors.IsNotFound(err) {
					return
				}
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pv.UID).NotTo(Equal(oldPV.pvUID),
					"gcSystemLocalPVs must reap the Released pool PV left behind by the recycle")
			}, 10*time.Minute, 10*time.Second).Should(Succeed())
		})

		It("adopts a store provisioned before the incarnation counter, instead of rebuilding it", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
			defer cancel()

			// The local-PV names and on-node directories of a never-recreated store
			// deliberately carry no incarnation, so that a controller which learns
			// about the counter finds the cluster it is already running rather than a
			// pool it has to provision from scratch. A store created before the
			// counter existed is exactly a ConfigMap without the key — so strip it,
			// restart the controller, and require the cluster to be adopted untouched.
			// (A real cross-version upgrade needs the harness to install two module
			// versions; this covers the invariant that upgrade depends on.)
			cm, err := suiteClientset.CoreV1().ConfigMaps(moduleNS).Get(ctx, systemStore+"-garage-config", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(cm.Data).To(HaveKey(systemIncarnationKey), "the running controller must record an incarnation")

			rfBefore, err := garageReplicationFactor(ctx, systemStore)
			Expect(err).NotTo(HaveOccurred())
			bindingsBefore, err := systemReplicaBindings(ctx, systemStore)
			Expect(err).NotTo(HaveOccurred())
			podsBefore, err := garagePods(ctx, systemStore)
			Expect(err).NotTo(HaveOccurred())
			podUIDs := map[string]types.UID{}
			for i := range podsBefore.Items {
				podUIDs[podsBefore.Items[i].Name] = podsBefore.Items[i].UID
			}

			By("removing the incarnation key, making the store look pre-upgrade")
			_, err = suiteClientset.CoreV1().ConfigMaps(moduleNS).Patch(ctx, cm.Name, types.MergePatchType,
				[]byte(fmt.Sprintf(`{"data":{%q:null}}`, systemIncarnationKey)), metav1.PatchOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("restarting the controller so it reconciles the store from scratch")
			Expect(restartController(ctx, suiteCfg.moduleReadyTO)).To(Succeed())

			By("waiting for the controller to record the first incarnation again")
			Eventually(func() (int, error) {
				return garageSystemIncarnation(ctx, systemStore)
			}, 10*time.Minute, 10*time.Second).Should(Equal(1),
				"a missing counter reads back as the first incarnation, never as a new one")

			By("asserting nothing was rebuilt: same pinned factor, same volumes, same pods")
			rfAfter, err := garageReplicationFactor(ctx, systemStore)
			Expect(err).NotTo(HaveOccurred())
			Expect(rfAfter).To(Equal(rfBefore), "the pinned replication factor must be read back, not recomputed")

			bindingsAfter, err := systemReplicaBindings(ctx, systemStore)
			Expect(err).NotTo(HaveOccurred())
			Expect(bindingsAfter).To(Equal(bindingsBefore), "adoption must not re-home any replica")

			podsAfter, err := garagePods(ctx, systemStore)
			Expect(err).NotTo(HaveOccurred())
			Expect(podsAfter.Items).To(HaveLen(len(podUIDs)))
			for i := range podsAfter.Items {
				p := &podsAfter.Items[i]
				Expect(p.UID).To(Equal(podUIDs[p.Name]), "pod %s must not be restarted by adoption", p.Name)
			}

			Expect(waitOSCReady(ctx, systemStore)).To(Succeed())

			By("asserting the data is still served")
			Expect(s3AssertMarkerContent(ctx, "s3-adoption-read", suiteCfg.namespace, testSecret, markerObj, markerBody)).To(Succeed())
		})
	})
}

// nodeHostname resolves a node's kubernetes.io/hostname label (the key the System
// local PVs pin), falling back to the node name.
func nodeHostname(ctx context.Context, nodeName string) string {
	node, err := suiteClientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nodeName
	}
	if h := node.Labels["kubernetes.io/hostname"]; h != "" {
		return h
	}
	return node.Name
}
