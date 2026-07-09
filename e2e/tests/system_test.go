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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	objectv1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

// systemBucketSpecs verifies the built-in system object storage shipped by the
// module templates (templates/system-object-storage.yaml), gated by the
// sdsObject.systemBucket.enabled config value (default true). It asserts the
// three CRs exist and carry the expected shape:
//   - a cluster-scoped `system` ObjectStore of type System, whose
//     redundancy is not set on System (derived from control-plane node count),
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

			By("asserting redundancy is not set on the System store (derived from the control-plane node count)")
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

			By("asserting the replication factor is pinned to min(3, control-plane nodes)")
			masters, err := controlPlaneNodeCount(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(masters).To(BeNumerically(">=", 1))
			wantRF := masters
			if wantRF > 3 {
				wantRF = 3
			}
			rf, err := garageReplicationFactor(ctx, systemCluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(rf).To(Equal(wantRF), "System rf = min(3, control-plane nodes)")

			By("asserting the System DaemonSet carries a config-hash annotation")
			ds, err := suiteClientset.AppsV1().DaemonSets(moduleNS).Get(ctx, systemCluster+"-garage", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "get Garage DaemonSet")
			Expect(ds.Spec.Template.Annotations).To(HaveKey("storage.deckhouse.io/config-hash"))

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
		})
	})
}
