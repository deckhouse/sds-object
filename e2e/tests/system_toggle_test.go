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

	objectv1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

// systemBucketToggleSpecs covers sdsObject.systemBucket.enabled, the switch that
// decides whether the module ships a system object storage at all. It was
// documented and never exercised, so nothing checked that turning it off unships
// the resources without destroying the stored data, or that turning it back on
// leaves a working store behind.
//
// What is asserted is what the module promises: off removes the four shipped
// objects (ObjectStore, Bucket, the d8-* claim policy and the managed local
// StorageClass) and keeps the replica PVCs, since the shipped store is Retain; on
// brings a Ready store back that serves S3. Whether the previous objects reappear
// is deliberately NOT asserted: the store is rebuilt from whatever the local
// volumes still hold, which the module does not promise either way.
//
// It runs after the delete specs (so the shared bucket is already gone) and before
// the singleReplica switch (which changes the incarnation, hence the directory the
// rebuilt store would pick up). Skipped with E2E_SKIP_SYSTEM_RECREATE together with
// the other disruptive System specs.
func systemBucketToggleSpecs() {
	Describe("system-bucket-toggle", Ordered, func() {
		const (
			systemStore  = "system"
			systemBucket = "system"
			systemPolicy = "system-d8-namespaces"
			localSC      = systemLocalStorageClassName
			testPolicy   = "system-toggle-policy"
			testClaim    = "system-toggle-claim"
			testAccess   = "system-toggle-access"
		)

		BeforeAll(func() {
			if suiteCfg.skipSystemRecreate {
				Skip("E2E_SKIP_SYSTEM_RECREATE is set; skipping the disruptive systemBucket toggle")
			}
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.oscReadyTimeout+5*time.Minute)
			defer cancel()

			if _, err := suiteDyn.Resource(objectStoreGVR).Get(ctx, systemStore, metav1.GetOptions{}); err != nil {
				if apierrors.IsNotFound(err) {
					Skip("system ObjectStore not present (sdsObject.systemBucket.enabled is already false)")
				}
				Expect(err).NotTo(HaveOccurred(), "get system ObjectStore")
			}
			Expect(waitOSCReady(ctx, systemStore)).To(Succeed())

			// Leave the module shipping its system storage again whatever happens, or
			// every later System spec would skip on a missing store.
			DeferCleanup(func() {
				bg := context.Background()
				_ = setSystemBucketEnabled(bg, true)
				_ = suiteDyn.Resource(bucketAccessGVR).Namespace(suiteCfg.namespace).Delete(bg, testAccess, metav1.DeleteOptions{})
				_ = suiteDyn.Resource(bucketClaimGVR).Namespace(suiteCfg.namespace).Delete(bg, testClaim, metav1.DeleteOptions{})
				_ = suiteDyn.Resource(bucketClaimPolicyGVR).Delete(bg, testPolicy, metav1.DeleteOptions{})
			})
		})

		It("unships the system storage but keeps its volumes when disabled", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
			defer cancel()

			By("recording the replica PVCs before disabling")
			pvcsBefore, err := suiteClientset.CoreV1().PersistentVolumeClaims(moduleNS).List(ctx, metav1.ListOptions{
				LabelSelector: "storage.deckhouse.io/object-store=" + systemStore,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(pvcsBefore.Items).NotTo(BeEmpty(), "a running System store has node-sticky PVCs")

			By("setting sdsObject.systemBucket.enabled=false in the ModuleConfig")
			Expect(setSystemBucketEnabled(ctx, false)).To(Succeed())

			By("waiting for the shipped ObjectStore, Bucket and policy to be removed")
			Expect(waitResourceGone(ctx, objectStoreGVR, "", systemStore, 10*time.Minute)).To(Succeed())
			Expect(waitResourceGone(ctx, bucketGVR, "", systemBucket, 10*time.Minute)).To(Succeed())
			Expect(waitResourceGone(ctx, bucketClaimPolicyGVR, "", systemPolicy, 5*time.Minute)).To(Succeed())

			By("waiting for the managed local StorageClass to be removed")
			Eventually(func() bool {
				_, err := suiteClientset.StorageV1().StorageClasses().Get(ctx, localSC, metav1.GetOptions{})
				return apierrors.IsNotFound(err)
			}, 5*time.Minute, pollInterval).Should(BeTrue(), "the StorageClass is shipped by the same switch")

			By("asserting the data plane workload is gone (owner-reference GC)")
			Eventually(func() bool {
				_, err := suiteClientset.AppsV1().StatefulSets(moduleNS).Get(ctx, systemStore+"-garage", metav1.GetOptions{})
				return apierrors.IsNotFound(err)
			}, 10*time.Minute, pollInterval).Should(BeTrue())

			// The shipped store is Retain, so DeleteCluster must not touch the PVCs.
			// They are the only in-cluster trace of the stored data (the hostPath
			// directories behind them are out of reach of both the controller and the
			// test), which is exactly why deleting them would be the regression.
			By("asserting the replica PVCs survived (the store is Retain)")
			Consistently(func(g Gomega) {
				for i := range pvcsBefore.Items {
					name := pvcsBefore.Items[i].Name
					_, err := suiteClientset.CoreV1().PersistentVolumeClaims(moduleNS).Get(ctx, name, metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred(), "PVC %s must not be deleted with the store", name)
				}
			}, 90*time.Second, 10*time.Second).Should(Succeed())
		})

		It("ships a working system storage again when re-enabled", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
			defer cancel()

			By("setting sdsObject.systemBucket.enabled=true in the ModuleConfig")
			Expect(setSystemBucketEnabled(ctx, true)).To(Succeed())

			By("waiting for the shipped resources to come back")
			Eventually(func() error {
				_, err := suiteDyn.Resource(objectStoreGVR).Get(ctx, systemStore, metav1.GetOptions{})
				return err
			}, 10*time.Minute, pollInterval).Should(Succeed())
			Eventually(func() error {
				_, err := suiteClientset.StorageV1().StorageClasses().Get(ctx, localSC, metav1.GetOptions{})
				return err
			}, 5*time.Minute, pollInterval).Should(Succeed())

			By("waiting for the store and its bucket to reach Ready")
			Expect(waitOSCReady(ctx, systemStore)).To(Succeed())
			Expect(waitOSBReady(ctx, systemBucket)).To(Succeed())

			By("asserting the rebuilt store serves S3 through a fresh access")
			Expect(createOSBPolicy(ctx, buildOSBPolicy(testPolicy, systemBucket, []string{suiteCfg.namespace}))).To(Succeed())
			Expect(createBucketClaim(ctx, buildBucketClaim(testClaim, suiteCfg.namespace, systemBucket))).To(Succeed())
			Expect(createOSBAccess(ctx, buildOSBAccess(testAccess, suiteCfg.namespace, testClaim, objectv1alpha1.AccessReadWrite))).To(Succeed())
			Expect(waitAccessReady(ctx, suiteCfg.namespace, testAccess)).To(Succeed())
			Expect(runS3ProbeJob(ctx, "s3-probe-reshipped", suiteCfg.namespace, credsSecretName(testAccess))).To(Succeed())
		})
	})
}
