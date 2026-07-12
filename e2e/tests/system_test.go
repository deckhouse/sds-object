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
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	commander "github.com/deckhouse/storage-e2e/pkg/commander"

	objectv1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

// systemBucketSpecs verifies the built-in system object storage shipped by the
// module templates (templates/system-object-storage.yaml), gated by the
// sdsObject.systemBucket.enabled config value (default true). It asserts the
// three CRs exist and carry the expected shape:
//   - a cluster-scoped `system` ObjectStore of type System, whose
//     redundancy is not set on System (it runs a fixed 3-replica Garage),
//     with reclaimPolicy Retain;
//   - a cluster-scoped `system` Bucket referencing it;
//   - a `system-d8-namespaces` BucketClaimPolicy allowing the d8-*
//     namespaces via a pattern.
//
// It asserts both the CR shape and that the system cluster + bucket are
// functional (reach Ready and serve an S3 round-trip through a test-scoped
// access). It skips when the system cluster is absent (systemBucket disabled).
func systemBucketSpecs() {
	Describe("system-bucket", func() {
		const (
			systemCluster = "system"
			systemBucket  = "system"
			systemPolicy  = "system-d8-namespaces"
		)
		// Auxiliary policy + access the suite creates to consume the shipped
		// system bucket from the (non-d8-*) test namespace.
		testPolicy := "system-e2e-" + "policy"
		testAccess := "system-e2e-access"
		testSecret := credsSecretName(testAccess)

		It("ships and runs the system ObjectStore, bucket and policy", func() {
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.oscReadyTimeout+suiteCfg.probeJobTimeout+5*time.Minute)
			defer cancel()

			osc, err := suiteDyn.Resource(objectStoreGVR).Get(ctx, systemCluster, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				Skip("system ObjectStore not present (sdsObject.systemBucket.enabled is false)")
			}
			Expect(err).NotTo(HaveOccurred(), "get system ObjectStore")

			By("asserting the system cluster is type System with reclaimPolicy Retain")
			clusterType, _, _ := unstructured.NestedString(osc.Object, "spec", "type")
			Expect(clusterType).To(Equal(string(objectv1alpha1.ClusterTypeSystem)))
			reclaim, _, _ := unstructured.NestedString(osc.Object, "spec", "reclaimPolicy")
			Expect(reclaim).To(Equal(string(objectv1alpha1.ClusterReclaimRetain)))

			By("asserting redundancy is not set on the System store (it runs a fixed 3-replica Garage)")
			_, hasRedundancy, _ := unstructured.NestedString(osc.Object, "spec", "redundancy")
			Expect(hasRedundancy).To(BeFalse(), "spec.redundancy must not be set on a System ObjectStore")

			By("asserting the system Bucket references the system cluster")
			osb, err := suiteDyn.Resource(bucketGVR).Get(ctx, systemBucket, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "get system Bucket")
			bucketObjectStoreRef, _, _ := unstructured.NestedString(osb.Object, "spec", "objectStoreRef")
			Expect(bucketObjectStoreRef).To(Equal(systemCluster))

			By("asserting the system policy grants the d8-* namespaces by pattern")
			policy, err := suiteDyn.Resource(bucketClaimPolicyGVR).Get(ctx, systemPolicy, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "get system BucketClaimPolicy")
			policyBucketRef, _, _ := unstructured.NestedString(policy.Object, "spec", "bucketRef")
			Expect(policyBucketRef).To(Equal(systemBucket))
			patterns, _, _ := unstructured.NestedStringSlice(policy.Object, "spec", "allowedNamespaces", "patterns")
			Expect(patterns).To(ContainElement("d8-.*"))

			By("waiting for the system cluster and bucket to reach Ready")
			Expect(waitOSCReady(ctx, systemCluster)).To(Succeed())
			Expect(waitOSBReady(ctx, systemBucket)).To(Succeed())

			By("asserting the replication factor is pinned to 3 (fixed, independent of the master count)")
			rf, err := garageReplicationFactor(ctx, systemCluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(rf).To(Equal(3), "System rf is pinned to 3")

			By("asserting the System StatefulSet runs a fixed 3 replicas, all Ready, with a config-hash annotation")
			sts, err := suiteClientset.AppsV1().StatefulSets(moduleNS).Get(ctx, systemCluster+"-garage", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "get Garage StatefulSet")
			Expect(sts.Spec.Replicas).NotTo(BeNil())
			Expect(*sts.Spec.Replicas).To(Equal(int32(3)), "System runs a fixed 3 replicas")
			Expect(sts.Status.ReadyReplicas).To(Equal(int32(3)), "all 3 System replicas must be Ready")
			Expect(sts.Spec.Template.Annotations).To(HaveKey("storage.deckhouse.io/config-hash"))
			Expect(sts.Spec.VolumeClaimTemplates).To(HaveLen(1), "System is PVC-backed (node-sticky local PV)")

			By("collecting the control-plane nodes (name + hostname)")
			cpNodes, err := suiteClientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{
				LabelSelector: "node-role.kubernetes.io/control-plane",
			})
			Expect(err).NotTo(HaveOccurred(), "list control-plane nodes")
			Expect(cpNodes.Items).NotTo(BeEmpty(), "cluster must have control-plane nodes")
			cpNodeNames := map[string]bool{}
			cpHostnames := map[string]bool{}
			for _, n := range cpNodes.Items {
				cpNodeNames[n.Name] = true
				if h := n.Labels["kubernetes.io/hostname"]; h != "" {
					cpHostnames[h] = true
				}
			}

			By("asserting all 3 Garage pods are Running on control-plane nodes")
			pods, err := suiteClientset.CoreV1().Pods(moduleNS).List(ctx, metav1.ListOptions{
				LabelSelector: "storage.deckhouse.io/object-store=" + systemCluster,
			})
			Expect(err).NotTo(HaveOccurred(), "list System pods")
			Expect(pods.Items).To(HaveLen(3), "3 Garage pods")
			for _, pod := range pods.Items {
				Expect(pod.Status.Phase).To(Equal(corev1.PodRunning), "pod %s must be Running", pod.Name)
				Expect(cpNodeNames).To(HaveKey(pod.Spec.NodeName), "pod %s must run on a control-plane node (got %q)", pod.Name, pod.Spec.NodeName)
			}

			By("asserting the managed local StorageClass is WaitForFirstConsumer with Retain")
			sc, err := suiteClientset.StorageV1().StorageClasses().Get(ctx, "sds-object-system-local", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "get managed local StorageClass")
			Expect(sc.VolumeBindingMode).NotTo(BeNil())
			Expect(*sc.VolumeBindingMode).To(Equal(storagev1.VolumeBindingWaitForFirstConsumer))
			Expect(sc.ReclaimPolicy).NotTo(BeNil())
			Expect(*sc.ReclaimPolicy).To(Equal(corev1.PersistentVolumeReclaimRetain))

			By("asserting the 3 replica PVCs are bound to the managed local StorageClass")
			pvcs, err := suiteClientset.CoreV1().PersistentVolumeClaims(moduleNS).List(ctx, metav1.ListOptions{
				LabelSelector: "storage.deckhouse.io/object-store=" + systemCluster,
			})
			Expect(err).NotTo(HaveOccurred(), "list System PVCs")
			Expect(pvcs.Items).To(HaveLen(3), "one node-sticky PVC per replica")
			for _, pvc := range pvcs.Items {
				Expect(pvc.Status.Phase).To(Equal(corev1.ClaimBound), "PVC %s must be bound to a local PV", pvc.Name)
				Expect(pvc.Spec.StorageClassName).NotTo(BeNil())
				Expect(*pvc.Spec.StorageClassName).To(Equal("sds-object-system-local"), "PVC %s must use the managed local StorageClass", pvc.Name)
			}

			By("asserting the node-sticky local PV pool is correctly provisioned and node-pinned")
			pvs, err := suiteClientset.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{
				LabelSelector: "storage.deckhouse.io/object-store=" + systemCluster + ",storage.deckhouse.io/system-local-node",
			})
			Expect(err).NotTo(HaveOccurred(), "list System local PVs")
			Expect(len(pvs.Items)).To(BeNumerically(">=", 3), "at least one full replica set of pool PVs per control-plane node")
			boundPVs := 0
			for _, pv := range pvs.Items {
				Expect(pv.Spec.PersistentVolumeReclaimPolicy).To(Equal(corev1.PersistentVolumeReclaimRetain), "pool PV %s must be Retain (never wipe data)", pv.Name)
				Expect(pv.Spec.StorageClassName).To(Equal("sds-object-system-local"), "pool PV %s must be on the managed local StorageClass", pv.Name)
				Expect(pv.Spec.HostPath).NotTo(BeNil(), "pool PV %s must be hostPath-backed", pv.Name)
				host := pvPinnedHostname(pv)
				Expect(host).NotTo(BeEmpty(), "pool PV %s must pin a hostname via nodeAffinity", pv.Name)
				Expect(cpHostnames).To(HaveKey(host), "pool PV %s must pin a control-plane node (got %q)", pv.Name, host)
				if pv.Status.Phase == corev1.VolumeBound {
					boundPVs++
				}
			}
			Expect(boundPVs).To(Equal(3), "exactly the 3 replica PVCs are bound to pool PVs")

			DeferCleanup(func() {
				bg := context.Background()
				_ = suiteDyn.Resource(bucketAccessGVR).Namespace(suiteCfg.namespace).Delete(bg, testAccess, metav1.DeleteOptions{})
				_ = suiteDyn.Resource(bucketClaimPolicyGVR).Delete(bg, testPolicy, metav1.DeleteOptions{})
			})

			By("granting the test namespace access to the system bucket and running an S3 round-trip")
			Expect(createOSBPolicy(ctx, buildOSBPolicy(testPolicy, systemBucket, []string{suiteCfg.namespace}))).To(Succeed())
			Expect(createBucketClaim(ctx, buildBucketClaim(claimName(systemBucket), suiteCfg.namespace, systemBucket))).To(Succeed())
			Expect(createOSBAccess(ctx, buildOSBAccess(testAccess, suiteCfg.namespace, claimName(systemBucket), objectv1alpha1.AccessReadWrite))).To(Succeed())
			Expect(waitAccessReady(ctx, suiteCfg.namespace, testAccess)).To(Succeed())
			Expect(runS3ProbeJob(ctx, "s3-probe-system", suiteCfg.namespace, testSecret)).To(Succeed())

			// Master-count transitions: grow 1->3 then shrink 3->1, asserting the
			// controller auto-rebalances the System replicas each way (spread
			// one-per-master on growth, consolidate onto the survivor on shrink)
			// while the store stays healthy. This is slow (Commander resizes the
			// control plane + Garage re-replicates per move), hence the long budget.
			if os.Getenv("E2E_COMMANDER_URL") == "" {
				By("skipping the master-count transitions (not a Commander-provisioned run)")
			} else {
				mcCtx, mcCancel := context.WithTimeout(context.Background(), 90*time.Minute)
				defer mcCancel()

				By("scaling the control plane from 1 to 3 masters via Commander")
				Expect(commander.SetMasterCount(mcCtx, 3)).To(Succeed(), "scale control plane to 3 masters")
				Eventually(func() (int, error) { return controlPlaneNodeCount(mcCtx) },
					15*time.Minute, 15*time.Second).Should(Equal(3), "control-plane node count must reach 3")

				By("confirming the controller auto-spreads the 3 replicas one-per-master")
				Eventually(func(g Gomega) {
					cp, err := controlPlaneNodeNames(mcCtx)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(cp).To(HaveLen(3))
					nodes, err := garageRunningPodNodes(mcCtx, systemCluster)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(nodes).To(HaveLen(3), "all 3 replicas must be Running")
					g.Expect(nodes).To(ConsistOf(cp), "one replica per distinct control-plane node")
				}, 40*time.Minute, 20*time.Second).Should(Succeed())
				Expect(waitOSCReady(mcCtx, systemCluster)).To(Succeed(), "System must be healthy after spread")

				By("scaling the control plane back from 3 to 1 master via Commander")
				Expect(commander.SetMasterCount(mcCtx, 1)).To(Succeed(), "scale control plane to 1 master")
				Eventually(func() (int, error) { return controlPlaneNodeCount(mcCtx) },
					25*time.Minute, 15*time.Second).Should(Equal(1), "control-plane node count must reach 1")

				By("confirming the controller consolidates all 3 replicas onto the surviving master")
				Eventually(func(g Gomega) {
					cp, err := controlPlaneNodeNames(mcCtx)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(cp).To(HaveLen(1))
					nodes, err := garageRunningPodNodes(mcCtx, systemCluster)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(nodes).To(HaveLen(3), "all 3 replicas must be Running")
					g.Expect(nodes).To(HaveEach(cp[0]), "all replicas on the surviving master")
				}, 40*time.Minute, 20*time.Second).Should(Succeed())
				Expect(waitOSCReady(mcCtx, systemCluster)).To(Succeed(), "System must be healthy after consolidation")
			}
		})
	})
}

// controlPlaneNodeNames returns the names of the control-plane nodes.
func controlPlaneNodeNames(ctx context.Context) ([]string, error) {
	nodes, err := suiteClientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: "node-role.kubernetes.io/control-plane",
	})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(nodes.Items))
	for i := range nodes.Items {
		names = append(names, nodes.Items[i].Name)
	}
	return names, nil
}

// controlPlaneNodeCount counts the control-plane nodes.
func controlPlaneNodeCount(ctx context.Context) (int, error) {
	names, err := controlPlaneNodeNames(ctx)
	return len(names), err
}

// garageRunningPodNodes returns the node names hosting Running Garage pods of the
// given ObjectStore (one entry per Running pod).
func garageRunningPodNodes(ctx context.Context, oscName string) ([]string, error) {
	pods, err := suiteClientset.CoreV1().Pods(moduleNS).List(ctx, metav1.ListOptions{
		LabelSelector: "storage.deckhouse.io/object-store=" + oscName,
	})
	if err != nil {
		return nil, err
	}
	var nodes []string
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodRunning && p.Spec.NodeName != "" {
			nodes = append(nodes, p.Spec.NodeName)
		}
	}
	return nodes, nil
}

// pvPinnedHostname returns the kubernetes.io/hostname value a System local PV is
// pinned to via its required nodeAffinity, or "" when it is not pinned by
// hostname.
func pvPinnedHostname(pv corev1.PersistentVolume) string {
	if pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil {
		return ""
	}
	for _, term := range pv.Spec.NodeAffinity.Required.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key == "kubernetes.io/hostname" && expr.Operator == corev1.NodeSelectorOpIn && len(expr.Values) > 0 {
				return expr.Values[0]
			}
		}
	}
	return ""
}
