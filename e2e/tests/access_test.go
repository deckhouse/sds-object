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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	objectv1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

// accessSpecs exercises the BucketAccess / BucketClaimPolicy
// behaviours on top of the shared cluster + bucket from create_test.go:
//   - deny-by-default: an access with no matching policy stays Pending and gets
//     no Secret; adding a policy flips it to Ready; deleting the policy revokes
//     the key and garbage-collects the Secret (continuous enforcement);
//   - regexp namespace matching in a policy;
//   - key rotation via the storage.deckhouse.io/rotate annotation;
//   - ReadOnly permission (reads succeed, writes are denied).
//
// These specs create their own auxiliary buckets/accesses (except rotation,
// which acts on the shared access) and clean them up, so they must run before
// deleteSpecs tears the shared cluster down.
func accessSpecs() {
	Describe("access", func() {
		It("enforces deny-by-default and revokes on policy removal", func() {
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.obReadyTimeout+5*time.Minute)
			defer cancel()

			bucket := suiteCfg.bucketName + "-np"
			access := accessName(bucket)
			policy := policyName(bucket)
			secret := credsSecretName(access)

			claim := claimName(bucket)

			DeferCleanup(func() {
				bg := context.Background()
				_ = suiteDyn.Resource(bucketAccessGVR).Namespace(suiteCfg.namespace).Delete(bg, access, metav1.DeleteOptions{})
				_ = suiteDyn.Resource(bucketClaimGVR).Namespace(suiteCfg.namespace).Delete(bg, claim, metav1.DeleteOptions{})
				_ = suiteDyn.Resource(bucketClaimPolicyGVR).Delete(bg, policy, metav1.DeleteOptions{})
				_ = suiteDyn.Resource(bucketGVR).Delete(bg, bucket, metav1.DeleteOptions{})
			})

			By("creating a Shared bucket with NO policy: " + bucket)
			Expect(createOSB(ctx, buildOSB(bucket, suiteCfg.oscName, objectv1alpha1.BucketReclaimDelete))).To(Succeed())
			Expect(waitOSBReady(ctx, bucket)).To(Succeed())

			By("creating a brownfield claim + access in " + suiteCfg.namespace + " with no matching policy")
			Expect(createBucketClaim(ctx, buildBucketClaim(claim, suiteCfg.namespace, bucket))).To(Succeed())
			Expect(createOSBAccess(ctx, buildOSBAccess(access, suiteCfg.namespace, claim, objectv1alpha1.AccessReadWrite))).To(Succeed())

			By("asserting the claim is denied by policy, the access stays pending, and no Secret is written")
			Eventually(func() string {
				_, reason, _, _ := getCondition(ctx, bucketClaimGVR, suiteCfg.namespace, claim, objectv1alpha1.BucketClaimConditionBound)
				return reason
			}).WithTimeout(2 * time.Minute).WithPolling(pollInterval).Should(Equal("DeniedByPolicy"))
			Consistently(func() bool {
				_, err := suiteClientset.CoreV1().Secrets(suiteCfg.namespace).Get(ctx, secret, metav1.GetOptions{})
				return apierrors.IsNotFound(err)
			}).WithTimeout(15*time.Second).WithPolling(pollInterval).Should(BeTrue(), "no credentials Secret must exist while the claim is denied")

			By("creating a policy that allows the namespace: the claim binds and the access reaches Ready")
			Expect(createOSBPolicy(ctx, buildOSBPolicy(policy, bucket, []string{suiteCfg.namespace}))).To(Succeed())
			Expect(waitAccessReady(ctx, suiteCfg.namespace, access)).To(Succeed())
			Expect(secretExists(ctx, suiteCfg.namespace, secret)).To(BeTrue(), "credentials Secret must exist once allowed")

			By("deleting the policy: the claim unbinds, the access is revoked and its Secret garbage-collected")
			Expect(suiteDyn.Resource(bucketClaimPolicyGVR).Delete(ctx, policy, metav1.DeleteOptions{})).To(Succeed())
			Eventually(func() string {
				_, reason, _, _ := getCondition(ctx, bucketClaimGVR, suiteCfg.namespace, claim, objectv1alpha1.BucketClaimConditionBound)
				return reason
			}).WithTimeout(2 * time.Minute).WithPolling(pollInterval).Should(Equal("DeniedByPolicy"))
			Expect(waitSecretGone(ctx, suiteCfg.namespace, secret, 2*time.Minute)).To(Succeed())
		})

		It("matches a namespace by regexp pattern", func() {
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.obReadyTimeout+5*time.Minute)
			defer cancel()

			bucket := suiteCfg.bucketName + "-re"
			access := accessName(bucket)
			policy := policyName(bucket)

			DeferCleanup(func() {
				bg := context.Background()
				_ = suiteDyn.Resource(bucketAccessGVR).Namespace(suiteCfg.namespace).Delete(bg, access, metav1.DeleteOptions{})
				_ = suiteDyn.Resource(bucketClaimPolicyGVR).Delete(bg, policy, metav1.DeleteOptions{})
				_ = suiteDyn.Resource(bucketGVR).Delete(bg, bucket, metav1.DeleteOptions{})
			})

			By("creating bucket " + bucket)
			Expect(createOSB(ctx, buildOSB(bucket, suiteCfg.oscName, objectv1alpha1.BucketReclaimDelete))).To(Succeed())
			Expect(waitOSBReady(ctx, bucket)).To(Succeed())

			By("creating a policy whose pattern matches the namespace, not an exact name")
			pol := buildOSBPolicy(policy, bucket, nil)
			pol.Object["spec"].(map[string]interface{})["allowedNamespaces"] = map[string]interface{}{
				"patterns": []interface{}{namespacePattern(suiteCfg.namespace)},
			}
			Expect(createOSBPolicy(ctx, pol)).To(Succeed())

			By("creating the access: it must reach Ready via the pattern match")
			Expect(createBucketClaim(ctx, buildBucketClaim(claimName(bucket), suiteCfg.namespace, bucket))).To(Succeed())
			Expect(createOSBAccess(ctx, buildOSBAccess(access, suiteCfg.namespace, claimName(bucket), objectv1alpha1.AccessReadWrite))).To(Succeed())
			Expect(waitAccessReady(ctx, suiteCfg.namespace, access)).To(Succeed())
		})

		It("rotates the access key on the rotate annotation", func() {
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.obReadyTimeout+5*time.Minute)
			defer cancel()

			access := accessName(suiteCfg.bucketName)

			By("reading the current access key id and secret")
			oldKeyID, err := getStringField(ctx, bucketAccessGVR, suiteCfg.namespace, access, "status", "accessKeyID")
			Expect(err).NotTo(HaveOccurred())
			Expect(oldKeyID).NotTo(BeEmpty(), "shared access must already have an issued key")

			secretName, err := getStringField(ctx, bucketAccessGVR, suiteCfg.namespace, access, "status", "secretRef", "name")
			Expect(err).NotTo(HaveOccurred())
			oldSecretKey, err := getSecretValue(ctx, suiteCfg.namespace, secretName, objectv1alpha1.SecretKeySecretAccessID)
			Expect(err).NotTo(HaveOccurred())
			Expect(oldSecretKey).NotTo(BeEmpty())

			By("setting the rotate annotation")
			Expect(annotateAccess(ctx, suiteCfg.namespace, access, objectv1alpha1.RotateAnnotation, "1")).To(Succeed())

			By("waiting for status.accessKeyID to change to a fresh key")
			Eventually(func() string {
				v, _ := getStringField(ctx, bucketAccessGVR, suiteCfg.namespace, access, "status", "accessKeyID")
				return v
			}).WithTimeout(3 * time.Minute).WithPolling(pollInterval).ShouldNot(Or(Equal(oldKeyID), BeEmpty()))
			Expect(waitAccessReady(ctx, suiteCfg.namespace, access)).To(Succeed())

			By("asserting the credentials Secret carries the new key pair")
			newKeyID, err := getStringField(ctx, bucketAccessGVR, suiteCfg.namespace, access, "status", "accessKeyID")
			Expect(err).NotTo(HaveOccurred())
			secretKeyID, err := getSecretValue(ctx, suiteCfg.namespace, secretName, objectv1alpha1.SecretKeyAccessKeyID)
			Expect(err).NotTo(HaveOccurred())
			Expect(secretKeyID).To(Equal(newKeyID), "Secret AWS_ACCESS_KEY_ID must match the rotated key")
			newSecretKey, err := getSecretValue(ctx, suiteCfg.namespace, secretName, objectv1alpha1.SecretKeySecretAccessID)
			Expect(err).NotTo(HaveOccurred())
			Expect(newSecretKey).NotTo(Equal(oldSecretKey), "the secret key must change on rotation")

			By("confirming the rotated credentials still allow an S3 round-trip")
			Expect(runS3ProbeJob(ctx, "s3-probe-rotate", suiteCfg.namespace, secretName)).To(Succeed())
		})

		It("issues read-only credentials that cannot write", func() {
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.obReadyTimeout+5*time.Minute)
			defer cancel()

			access := suiteCfg.bucketName + "-ro"
			secret := credsSecretName(access)

			DeferCleanup(func() {
				_ = suiteDyn.Resource(bucketAccessGVR).Namespace(suiteCfg.namespace).Delete(context.Background(), access, metav1.DeleteOptions{})
			})

			By("ensuring an object exists (written earlier by the ReadWrite round-trip)")
			// The shared ReadWrite access already wrote hello.txt in create_test.go.

			By("creating a ReadOnly access against the shared bucket's existing claim")
			// The shared BucketClaim (claimName(bucketName)) was already created and
			// Bound by the create_test.go flow; reuse it here rather than re-creating.
			Expect(createOSBAccess(ctx, buildOSBAccess(access, suiteCfg.namespace, claimName(suiteCfg.bucketName), objectv1alpha1.AccessReadOnly))).To(Succeed())
			Expect(waitAccessReady(ctx, suiteCfg.namespace, access)).To(Succeed())

			By("running a probe that reads an object and asserts writes are denied")
			Expect(runS3ReadOnlyProbeJob(ctx, "s3-probe-ro", suiteCfg.namespace, secret)).To(Succeed())
		})

		It("grants independent access to one bucket from two namespaces", func() {
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.obReadyTimeout+suiteCfg.probeJobTimeout+5*time.Minute)
			defer cancel()

			ns1 := suiteCfg.namespace
			ns2 := suiteCfg.namespace + "-b"
			bucket := suiteCfg.bucketName + "-multi"
			access := accessName(bucket)
			policy := policyName(bucket)
			secret := credsSecretName(access)

			DeferCleanup(func() {
				bg := context.Background()
				_ = suiteDyn.Resource(bucketAccessGVR).Namespace(ns1).Delete(bg, access, metav1.DeleteOptions{})
				_ = suiteDyn.Resource(bucketAccessGVR).Namespace(ns2).Delete(bg, access, metav1.DeleteOptions{})
				_ = suiteDyn.Resource(bucketClaimPolicyGVR).Delete(bg, policy, metav1.DeleteOptions{})
				_ = suiteDyn.Resource(bucketGVR).Delete(bg, bucket, metav1.DeleteOptions{})
				_ = suiteClientset.CoreV1().Namespaces().Delete(bg, ns2, metav1.DeleteOptions{})
			})

			By("creating the second namespace " + ns2)
			Expect(ensureNamespace(ctx, ns2)).To(Succeed())

			By("creating a shared bucket " + bucket)
			Expect(createOSB(ctx, buildOSB(bucket, suiteCfg.oscName, objectv1alpha1.BucketReclaimDelete))).To(Succeed())
			Expect(waitOSBReady(ctx, bucket)).To(Succeed())

			By("creating a policy allowing both namespaces")
			Expect(createOSBPolicy(ctx, buildOSBPolicy(policy, bucket, []string{ns1, ns2}))).To(Succeed())

			By("creating an access in each namespace")
			Expect(createBucketClaim(ctx, buildBucketClaim(claimName(bucket), ns1, bucket))).To(Succeed())
			Expect(createOSBAccess(ctx, buildOSBAccess(access, ns1, claimName(bucket), objectv1alpha1.AccessReadWrite))).To(Succeed())
			Expect(createBucketClaim(ctx, buildBucketClaim(claimName(bucket), ns2, bucket))).To(Succeed())
			Expect(createOSBAccess(ctx, buildOSBAccess(access, ns2, claimName(bucket), objectv1alpha1.AccessReadWrite))).To(Succeed())
			Expect(waitAccessReady(ctx, ns1, access)).To(Succeed())
			Expect(waitAccessReady(ctx, ns2, access)).To(Succeed())

			By("asserting each namespace received its own distinct access key")
			keyA, err := getSecretValue(ctx, ns1, secret, objectv1alpha1.SecretKeyAccessKeyID)
			Expect(err).NotTo(HaveOccurred())
			keyB, err := getSecretValue(ctx, ns2, secret, objectv1alpha1.SecretKeyAccessKeyID)
			Expect(err).NotTo(HaveOccurred())
			Expect(keyA).NotTo(BeEmpty())
			Expect(keyB).NotTo(BeEmpty())
			Expect(keyA).NotTo(Equal(keyB), "each BucketAccess must get an independent key")

			By("revoking one namespace's access must not affect the other")
			Expect(suiteDyn.Resource(bucketAccessGVR).Namespace(ns1).Delete(ctx, access, metav1.DeleteOptions{})).To(Succeed())
			Expect(waitResourceGone(ctx, bucketAccessGVR, ns1, access, resourceGoneTimeout)).To(Succeed())
			Expect(waitSecretGone(ctx, ns1, secret, 2*time.Minute)).To(Succeed())

			By("the surviving namespace's access still works")
			Expect(waitAccessReady(ctx, ns2, access)).To(Succeed())
			Expect(secretExists(ctx, ns2, secret)).To(BeTrue())
			Expect(runS3ProbeJob(ctx, "s3-probe-multi", ns2, secret)).To(Succeed())
		})
	})
}

// namespacePattern builds a non-trivial RE2 pattern that still matches ns (using
// its first label as a prefix), so the policy exercises pattern matching rather
// than the exact-name path.
func namespacePattern(ns string) string {
	prefix := ns
	if i := strings.IndexByte(ns, '-'); i > 0 {
		prefix = ns[:i]
	}
	return prefix + ".*"
}

// annotateAccess sets a single annotation on an BucketAccess via a
// merge patch.
func annotateAccess(ctx context.Context, ns, name, key, value string) error {
	patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, key, value))
	_, err := suiteDyn.Resource(bucketAccessGVR).Namespace(ns).
		Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// getSecretValue returns a single key from a Secret as a string.
func getSecretValue(ctx context.Context, ns, name, key string) (string, error) {
	s, err := suiteClientset.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return string(s.Data[key]), nil
}

// secretExists reports whether a Secret is present.
func secretExists(ctx context.Context, ns, name string) (bool, error) {
	_, err := suiteClientset.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// runS3ReadOnlyProbeJob runs a one-shot Job that asserts the credentials can
// READ an existing object but that a WRITE is denied (proving the ReadOnly
// permission is enforced by the backend).
func runS3ReadOnlyProbeJob(ctx context.Context, jobName, ns, secretName string) error {
	script := strings.Join([]string{
		"set -e",
		fmt.Sprintf("mc alias set %s \"$%s\" \"$%s\" \"$%s\"", probeAlias, objectv1alpha1.SecretKeyS3Endpoint, objectv1alpha1.SecretKeyAccessKeyID, objectv1alpha1.SecretKeySecretAccessID),
		fmt.Sprintf("mc cat \"%s/$%s/hello.txt\" >/dev/null", probeAlias, objectv1alpha1.SecretKeyS3Bucket),
		"echo '--- read OK ---'",
		fmt.Sprintf("if echo deny-me | mc pipe \"%s/$%s/should-fail.txt\" 2>/dev/null; then echo 'WRITE SHOULD HAVE BEEN DENIED'; exit 1; fi", probeAlias, objectv1alpha1.SecretKeyS3Bucket),
		"echo 'RO OK'",
	}, "\n")

	backoff := int32(6)
	var ttl int32 = 600
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ns},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "mc",
						Image:   suiteCfg.probeImage,
						Command: []string{"/bin/sh", "-c"},
						Args:    []string{script},
						EnvFrom: []corev1.EnvFromSource{{
							SecretRef: &corev1.SecretEnvSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
							},
						}},
					}},
				},
			},
		},
	}

	_ = suiteClientset.BatchV1().Jobs(ns).Delete(ctx, jobName, metav1.DeleteOptions{PropagationPolicy: ptr(metav1.DeletePropagationForeground)})
	if _, err := suiteClientset.BatchV1().Jobs(ns).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create read-only probe job %s: %w", formatRef(ns, jobName), err)
	}

	deadline := time.Now().Add(suiteCfg.probeJobTimeout)
	for {
		j, err := suiteClientset.BatchV1().Jobs(ns).Get(ctx, jobName, metav1.GetOptions{})
		if err == nil {
			if j.Status.Succeeded > 0 {
				return nil
			}
			if j.Status.Failed >= backoff {
				return fmt.Errorf("read-only probe job %s failed (%d attempts); inspect `kubectl -n %s logs job/%s`", formatRef(ns, jobName), j.Status.Failed, ns, jobName)
			}
		}
		if time.Now().After(deadline) {
			s, f := jobStatus(j)
			return fmt.Errorf("timeout waiting for read-only probe job %s (succeeded=%d failed=%d)", formatRef(ns, jobName), s, f)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}
