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

// deleteSpecs tears down the shared access, bucket policy, bucket and cluster
// and asserts the finalizer-driven cleanup: deleting the ObjectStorageBucket
// Access revokes credentials and garbage-collects its owned Secret; deleting the
// cluster-scoped ObjectStorageBucket (reclaimPolicy=Delete) removes the bucket;
// and the cluster object disappears once its finalizer releases. These specs run
// LAST in the Ordered container.
func deleteSpecs() {
	Describe("delete", func() {
		It("deletes the ObjectStorageBucketAccess and its credentials Secret", func() {
			ctx, cancel := context.WithTimeout(context.Background(), resourceGoneTimeout+2*time.Minute)
			defer cancel()

			access := accessName(suiteCfg.bucketName)
			secretName, err := getStringField(ctx, objectStorageBucketAccessGVR, suiteCfg.namespace, access, "status", "secretRef", "name")
			Expect(err).NotTo(HaveOccurred())
			Expect(secretName).NotTo(BeEmpty())

			By("deleting ObjectStorageBucketAccess " + access)
			Expect(suiteDyn.Resource(objectStorageBucketAccessGVR).Namespace(suiteCfg.namespace).
				Delete(ctx, access, metav1.DeleteOptions{})).To(Succeed())

			By("waiting for the access object to be gone (finalizer released)")
			Expect(waitResourceGone(ctx, objectStorageBucketAccessGVR, suiteCfg.namespace, access, resourceGoneTimeout)).To(Succeed())

			By("waiting for the owned credentials Secret to be garbage-collected")
			Expect(waitSecretGone(ctx, suiteCfg.namespace, secretName, 2*time.Minute)).To(Succeed())
		})

		It("deletes the ObjectStorageBucketPolicy and the ObjectStorageBucket", func() {
			ctx, cancel := context.WithTimeout(context.Background(), resourceGoneTimeout+2*time.Minute)
			defer cancel()

			By("deleting ObjectStorageBucketPolicy " + policyName(suiteCfg.bucketName))
			Expect(suiteDyn.Resource(objectStorageBucketPolicyGVR).
				Delete(ctx, policyName(suiteCfg.bucketName), metav1.DeleteOptions{})).To(Succeed())
			Expect(waitResourceGone(ctx, objectStorageBucketPolicyGVR, "", policyName(suiteCfg.bucketName), resourceGoneTimeout)).To(Succeed())

			By("deleting ObjectStorageBucket " + suiteCfg.bucketName + " (reclaimPolicy=Delete)")
			Expect(suiteDyn.Resource(objectStorageBucketGVR).
				Delete(ctx, suiteCfg.bucketName, metav1.DeleteOptions{})).To(Succeed())

			By("waiting for the bucket object to be gone (finalizer released)")
			Expect(waitResourceGone(ctx, objectStorageBucketGVR, "", suiteCfg.bucketName, resourceGoneTimeout)).To(Succeed())
		})

		It("deletes the ObjectStorageCluster", func() {
			if !oscCreatedBySuite {
				Skip("primary ObjectStorageCluster " + suiteCfg.oscName + " is module-managed (adopted); not deleting it")
			}
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
