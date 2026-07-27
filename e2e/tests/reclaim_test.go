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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	objectv1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

// reclaimSpecs covers the reclaim-policy contracts, i.e. exactly when the module
// is allowed to destroy data. Only the Delete side of Bucket reclaim had any
// coverage (via the shared bucket in delete_test.go); Retain — the DEFAULT, and
// the one whose regression silently loses data — had none, and neither did the
// cluster-level policy.
//
//   - Bucket reclaimPolicy Retain: deleting the Bucket must leave the backend
//     bucket and its objects untouched, so re-declaring the same Bucket adopts the
//     data back.
//   - ObjectStore reclaimPolicy Retain vs Delete: deleting the store keeps or
//     removes the data-plane PVCs accordingly (Kubernetes never garbage-collects a
//     StatefulSet's PVCs, so this is entirely the controller's call).
//
// The Bucket specs run on the shared store; the cluster specs need a real
// StorageClass for two throwaway Lightweight stores and skip when none is
// available.
func reclaimSpecs() {
	Describe("reclaim", Ordered, func() {
		const (
			retainBucket = "e2e-retain-bucket"
			markerObj    = "reclaim-marker.txt"
			markerBody   = "written before the Bucket was deleted"
		)

		Describe("bucket-retain", Ordered, func() {
			var secretName string

			BeforeAll(func() {
				// Best effort: leave no bucket behind if a spec fails midway.
				DeferCleanup(func() {
					bg := context.Background()
					_ = suiteDyn.Resource(bucketAccessGVR).Namespace(suiteCfg.namespace).Delete(bg, accessName(retainBucket), metav1.DeleteOptions{})
					_ = suiteDyn.Resource(bucketClaimGVR).Namespace(suiteCfg.namespace).Delete(bg, claimName(retainBucket), metav1.DeleteOptions{})
					_ = suiteDyn.Resource(bucketClaimPolicyGVR).Delete(bg, policyName(retainBucket), metav1.DeleteOptions{})
					_ = suiteDyn.Resource(bucketGVR).Delete(bg, retainBucket, metav1.DeleteOptions{})
				})
			})

			It("keeps the backend bucket and its objects when a Retain Bucket is deleted", func() {
				ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.obReadyTimeout+suiteCfg.probeJobTimeout+resourceGoneTimeout+5*time.Minute)
				defer cancel()

				By("declaring Bucket " + retainBucket + " with reclaimPolicy Retain")
				Expect(createOSB(ctx, buildOSB(retainBucket, suiteCfg.oscName, objectv1alpha1.BucketReclaimRetain))).To(Succeed())
				Expect(waitOSBReady(ctx, retainBucket)).To(Succeed())

				By("granting access and writing marker object " + markerObj)
				Expect(createOSBPolicy(ctx, buildOSBPolicy(policyName(retainBucket), retainBucket, []string{suiteCfg.namespace}))).To(Succeed())
				Expect(createBucketClaim(ctx, buildBucketClaim(claimName(retainBucket), suiteCfg.namespace, retainBucket))).To(Succeed())
				Expect(createOSBAccess(ctx, buildOSBAccess(accessName(retainBucket), suiteCfg.namespace, claimName(retainBucket), objectv1alpha1.AccessReadWrite))).To(Succeed())
				Expect(waitAccessReady(ctx, suiteCfg.namespace, accessName(retainBucket))).To(Succeed())

				var err error
				secretName, err = getStringField(ctx, bucketAccessGVR, suiteCfg.namespace, accessName(retainBucket), "status", "secretRef", "name")
				Expect(err).NotTo(HaveOccurred())
				Expect(s3PutMarker(ctx, "s3-reclaim-write", suiteCfg.namespace, secretName, markerObj, markerBody)).To(Succeed())

				By("tearing down the access and the claim (a claim blocks Bucket deletion)")
				Expect(suiteDyn.Resource(bucketAccessGVR).Namespace(suiteCfg.namespace).
					Delete(ctx, accessName(retainBucket), metav1.DeleteOptions{})).To(Succeed())
				Expect(waitResourceGone(ctx, bucketAccessGVR, suiteCfg.namespace, accessName(retainBucket), resourceGoneTimeout)).To(Succeed())
				Expect(suiteDyn.Resource(bucketClaimGVR).Namespace(suiteCfg.namespace).
					Delete(ctx, claimName(retainBucket), metav1.DeleteOptions{})).To(Succeed())
				Expect(waitResourceGone(ctx, bucketClaimGVR, suiteCfg.namespace, claimName(retainBucket), resourceGoneTimeout)).To(Succeed())

				By("deleting Bucket " + retainBucket + " (reclaimPolicy Retain: the backend bucket must survive)")
				Expect(suiteDyn.Resource(bucketGVR).Delete(ctx, retainBucket, metav1.DeleteOptions{})).To(Succeed())
				Expect(waitResourceGone(ctx, bucketGVR, "", retainBucket, resourceGoneTimeout)).To(Succeed())
			})

			It("adopts the retained bucket, data and all, when it is re-declared", func() {
				ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.obReadyTimeout+suiteCfg.probeJobTimeout+5*time.Minute)
				defer cancel()

				By("re-declaring the same Bucket " + retainBucket)
				Expect(createOSB(ctx, buildOSB(retainBucket, suiteCfg.oscName, objectv1alpha1.BucketReclaimDelete))).To(Succeed())
				Expect(waitOSBReady(ctx, retainBucket)).To(Succeed(),
					"the reconciler must adopt the existing backend bucket instead of failing on it")

				By("granting access again and reading the marker back")
				// The policy from the previous spec is still there (it is not tied to the
				// Bucket's lifecycle), so tolerate it rather than recreate it.
				Expect(ensureOSBPolicy(ctx, buildOSBPolicy(policyName(retainBucket), retainBucket, []string{suiteCfg.namespace}))).To(Succeed())
				Expect(createBucketClaim(ctx, buildBucketClaim(claimName(retainBucket), suiteCfg.namespace, retainBucket))).To(Succeed())
				Expect(createOSBAccess(ctx, buildOSBAccess(accessName(retainBucket), suiteCfg.namespace, claimName(retainBucket), objectv1alpha1.AccessReadWrite))).To(Succeed())
				Expect(waitAccessReady(ctx, suiteCfg.namespace, accessName(retainBucket))).To(Succeed())

				newSecret, err := getStringField(ctx, bucketAccessGVR, suiteCfg.namespace, accessName(retainBucket), "status", "secretRef", "name")
				Expect(err).NotTo(HaveOccurred())
				Expect(s3AssertMarkerContent(ctx, "s3-reclaim-read", suiteCfg.namespace, newSecret, markerObj, markerBody)).To(Succeed(),
					"Retain must have kept the objects, so the re-declared bucket still has them")

				By("cleaning up: this time with reclaimPolicy Delete, the bucket must go")
				Expect(suiteDyn.Resource(bucketAccessGVR).Namespace(suiteCfg.namespace).
					Delete(ctx, accessName(retainBucket), metav1.DeleteOptions{})).To(Succeed())
				Expect(waitResourceGone(ctx, bucketAccessGVR, suiteCfg.namespace, accessName(retainBucket), resourceGoneTimeout)).To(Succeed())
				Expect(suiteDyn.Resource(bucketClaimGVR).Namespace(suiteCfg.namespace).
					Delete(ctx, claimName(retainBucket), metav1.DeleteOptions{})).To(Succeed())
				Expect(waitResourceGone(ctx, bucketClaimGVR, suiteCfg.namespace, claimName(retainBucket), resourceGoneTimeout)).To(Succeed())
				_ = suiteDyn.Resource(bucketClaimPolicyGVR).Delete(ctx, policyName(retainBucket), metav1.DeleteOptions{})
				Expect(suiteDyn.Resource(bucketGVR).Delete(ctx, retainBucket, metav1.DeleteOptions{})).To(Succeed())
				Expect(waitResourceGone(ctx, bucketGVR, "", retainBucket, resourceGoneTimeout)).To(Succeed(),
					"a non-empty bucket must still be emptied and deleted under reclaimPolicy Delete")
			})
		})

		Describe("cluster-pvcs", Ordered, func() {
			var storageClass string

			BeforeAll(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()

				var err error
				storageClass, err = resolvePVCStorageClass(ctx)
				Expect(err).NotTo(HaveOccurred(), "resolve StorageClass")
				if storageClass == "" {
					Skip("no StorageClass available for the throwaway Lightweight stores; set E2E_PVC_STORAGE_CLASS (or E2E_STORAGE_CLASS), or mark a default StorageClass")
				}
			})

			// Both cases use a single-node Lightweight store: the data plane shape does
			// not matter here, only what happens to its PVCs on deletion.
			DescribeTable("honours the cluster reclaim policy on the data-plane PVCs",
				func(name string, reclaim objectv1alpha1.ClusterReclaimPolicy, pvcsMustSurvive bool) {
					ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.oscReadyTimeout+resourceGoneTimeout+5*time.Minute)
					defer cancel()

					DeferCleanup(func() {
						bg, cancel := context.WithTimeout(context.Background(), resourceGoneTimeout+2*time.Minute)
						defer cancel()
						_ = suiteDyn.Resource(objectStoreGVR).Delete(bg, name, metav1.DeleteOptions{})
						// Retained PVCs are the operator's to remove; the suite must not
						// leave them behind either.
						pvcs, err := suiteClientset.CoreV1().PersistentVolumeClaims(moduleNS).List(bg, metav1.ListOptions{
							LabelSelector: "storage.deckhouse.io/object-store=" + name,
						})
						if err != nil {
							return
						}
						for i := range pvcs.Items {
							_ = suiteClientset.CoreV1().PersistentVolumeClaims(moduleNS).Delete(bg, pvcs.Items[i].Name, metav1.DeleteOptions{})
						}
					})

					By("creating Lightweight ObjectStore " + name + " with reclaimPolicy " + string(reclaim))
					Expect(createOSC(ctx, newOSC(name, map[string]interface{}{
						"type":          string(objectv1alpha1.ClusterTypeLightweight),
						"redundancy":    string(objectv1alpha1.RedundancyNone),
						"reclaimPolicy": string(reclaim),
						"storage": map[string]interface{}{
							"sizePerNode": suiteCfg.oscSize,
							"class":       storageClass,
							"nodes":       int64(1),
						},
					}))).To(Succeed())
					Expect(waitOSCReady(ctx, name)).To(Succeed())

					By("recording the data-plane PVCs")
					pvcs, err := suiteClientset.CoreV1().PersistentVolumeClaims(moduleNS).List(ctx, metav1.ListOptions{
						LabelSelector: "storage.deckhouse.io/object-store=" + name,
					})
					Expect(err).NotTo(HaveOccurred())
					Expect(pvcs.Items).To(HaveLen(1), "one PVC for a single-node Lightweight store")
					pvcName := pvcs.Items[0].Name
					Expect(pvcs.Items[0].Status.Phase).To(Equal(corev1.ClaimBound))

					By("deleting the ObjectStore")
					Expect(suiteDyn.Resource(objectStoreGVR).Delete(ctx, name, metav1.DeleteOptions{})).To(Succeed())
					Expect(waitResourceGone(ctx, objectStoreGVR, "", name, resourceGoneTimeout)).To(Succeed())

					if pvcsMustSurvive {
						By("asserting the PVC survived (Retain keeps the stored data)")
						// Give the controller room to get it wrong before concluding it did
						// the right thing: assert the PVC is still there consistently.
						Consistently(func() error {
							_, err := suiteClientset.CoreV1().PersistentVolumeClaims(moduleNS).Get(ctx, pvcName, metav1.GetOptions{})
							return err
						}, 90*time.Second, 10*time.Second).Should(Succeed(),
							"reclaimPolicy Retain must not delete the data-plane PVC")
						return
					}

					By("asserting the PVC was removed (Delete destroys the stored data)")
					Eventually(func() bool {
						_, err := suiteClientset.CoreV1().PersistentVolumeClaims(moduleNS).Get(ctx, pvcName, metav1.GetOptions{})
						return apierrors.IsNotFound(err)
					}, 5*time.Minute, 5*time.Second).Should(BeTrue(),
						"reclaimPolicy Delete must remove the data-plane PVC")
				},
				Entry("Retain keeps them", "e2e-reclaim-retain", objectv1alpha1.ClusterReclaimRetain, true),
				Entry("Delete removes them", "e2e-reclaim-delete", objectv1alpha1.ClusterReclaimDelete, false),
			)
		})
	})
}
