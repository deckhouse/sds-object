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
)

// deleteSpecs tears down the shared bucket and cluster and asserts the
// finalizer-driven cleanup: the bucket's credentials Secret is removed with it,
// and the cluster object disappears once its finalizer releases. These specs run
// LAST in the Ordered container.
func deleteSpecs() {
	Describe("delete", func() {
		It("deletes the ObjectBucket and its credentials Secret", func() {
			ctx, cancel := context.WithTimeout(context.Background(), resourceGoneTimeout+2*time.Minute)
			defer cancel()

			secretName, err := getStringField(ctx, objectBucketGVR, suiteCfg.namespace, suiteCfg.bucketName, "status", "secretRef", "name")
			Expect(err).NotTo(HaveOccurred())
			Expect(secretName).NotTo(BeEmpty())

			By("deleting ObjectBucket " + suiteCfg.bucketName)
			Expect(suiteDyn.Resource(objectBucketGVR).Namespace(suiteCfg.namespace).
				Delete(ctx, suiteCfg.bucketName, metav1.DeleteOptions{})).To(Succeed())

			By("waiting for the bucket object to be gone (finalizer released)")
			Expect(waitResourceGone(ctx, objectBucketGVR, suiteCfg.namespace, suiteCfg.bucketName, resourceGoneTimeout)).To(Succeed())

			By("waiting for the owned credentials Secret to be garbage-collected")
			Expect(waitSecretGone(ctx, suiteCfg.namespace, secretName, 2*time.Minute)).To(Succeed())
		})

		It("deletes the ObjectStorageCluster", func() {
			ctx, cancel := context.WithTimeout(context.Background(), resourceGoneTimeout+2*time.Minute)
			defer cancel()

			By("deleting ObjectStorageCluster " + suiteCfg.oscName)
			Expect(suiteDyn.Resource(objectStorageClusterGVR).
				Delete(ctx, suiteCfg.oscName, metav1.DeleteOptions{})).To(Succeed())

			By("waiting for the cluster object to be gone (finalizer released)")
			Expect(waitResourceGone(ctx, objectStorageClusterGVR, "", suiteCfg.oscName, resourceGoneTimeout)).To(Succeed())
		})
	})
}
