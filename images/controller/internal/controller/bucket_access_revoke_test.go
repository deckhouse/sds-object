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

package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
	"github.com/deckhouse/sds-object/images/controller/internal/backend"
	"github.com/deckhouse/sds-object/images/controller/pkg/config"
	"github.com/deckhouse/sds-object/images/controller/pkg/logger"
)

// recordingDriver counts the backend calls the reconciler makes. Everything it
// does not override comes from NotImplementedDriver.
type recordingDriver struct {
	backend.NotImplementedDriver
	deleteAccessCalls int
	ensureAccessCalls int
}

func (d *recordingDriver) DeleteAccess(context.Context, *v1alpha1.ObjectStore, *v1alpha1.Bucket, *v1alpha1.BucketAccess) error {
	d.deleteAccessCalls++
	return nil
}

func (d *recordingDriver) EnsureAccess(_ context.Context, _ *v1alpha1.ObjectStore, _ *v1alpha1.Bucket, _ *v1alpha1.BucketAccess, _ bool) (backend.AccessState, error) {
	d.ensureAccessCalls++
	return backend.AccessState{Ready: true, Message: "access key provisioned", AccessKeyID: "GKexisting"}, nil
}

func revokeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add v1alpha1 scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	return s
}

// accessFixture builds a granted access on a Shared bucket the namespace is
// allowed to use, with the bucket's Ready condition set to bucketReady.
func accessFixture(bucketReady metav1.ConditionStatus) (
	*v1alpha1.ObjectStore, *v1alpha1.Bucket, *v1alpha1.BucketClaim,
	*v1alpha1.BucketAccess, *v1alpha1.BucketClaimPolicy, *corev1.Secret,
) {
	store := &v1alpha1.ObjectStore{
		ObjectMeta: metav1.ObjectMeta{Name: "system"},
		Spec:       v1alpha1.ObjectStoreSpec{Type: v1alpha1.ClusterTypeSystem},
	}
	bucket := &v1alpha1.Bucket{
		ObjectMeta: metav1.ObjectMeta{Name: "shared"},
		// No LabelBucketOrigin: an administrator-declared (Shared) bucket, so
		// authorization goes through a BucketClaimPolicy.
		Spec: v1alpha1.BucketSpec{ObjectStoreRef: store.Name, BucketName: "shared"},
		Status: &v1alpha1.BucketStatus{
			BucketName: "shared",
			Conditions: []metav1.Condition{{
				Type:               v1alpha1.BucketConditionReady,
				Status:             bucketReady,
				Reason:             "Test",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
	claim := &v1alpha1.BucketClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "claim"},
		Spec:       v1alpha1.BucketClaimSpec{ExistingBucketName: bucket.Name},
		Status: &v1alpha1.BucketClaimStatus{
			BoundBucketName: bucket.Name,
			Conditions: []metav1.Condition{{
				Type:               v1alpha1.BucketClaimConditionBound,
				Status:             metav1.ConditionTrue,
				Reason:             "Ready",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
	access := &v1alpha1.BucketAccess{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  "team-a",
			Name:       "access",
			Finalizers: []string{Finalizer},
		},
		Spec: v1alpha1.BucketAccessSpec{
			BucketClaimName: claim.Name,
			Permission:      v1alpha1.AccessReadWrite,
		},
		Status: &v1alpha1.BucketAccessStatus{
			AccessKeyID: "GKexisting",
			SecretRef:   &v1alpha1.LocalSecretReference{Name: "access-creds"},
		},
	}
	pol := policy(bucket.Name, []string{claim.Namespace}, nil)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: claim.Namespace, Name: "access-creds"}}
	return store, bucket, claim, access, pol, secret
}

func newAccessReconciler(t *testing.T, objs ...client.Object) (*BucketAccessReconciler, client.Client, *recordingDriver) {
	t.Helper()
	s := revokeScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.BucketAccess{}).Build()
	log, err := logger.NewLogger("0")
	if err != nil {
		t.Fatalf("build logger: %v", err)
	}
	drv := &recordingDriver{NotImplementedDriver: backend.NotImplementedDriver{BackendType: v1alpha1.BackendGarage}}
	r := &BucketAccessReconciler{
		Client:    c,
		APIReader: c,
		Scheme:    s,
		Log:       log,
		Cfg:       &config.Options{RequeueInterval: 30 * time.Second, SecurityResyncInterval: 5 * time.Minute},
		Registry:  backend.NewRegistry(drv),
	}
	return r, c, drv
}

func getAccess(t *testing.T, c client.Client, ns, name string) *v1alpha1.BucketAccess {
	t.Helper()
	out := &v1alpha1.BucketAccess{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, out); err != nil {
		t.Fatalf("get access: %v", err)
	}
	return out
}

// TestUnreadyBucketKeepsCredentials is the regression test for a data-plane
// outage revoking working credentials. A full restart of the System store flips
// its Bucket out of Ready; the access reconciler used to read that as "the claim
// is no longer usable", delete the backend key and the credentials Secret, and
// then mint a fresh key once the store came back — silently rotating the
// caller's credentials because of an outage it should have simply waited out.
func TestUnreadyBucketKeepsCredentials(t *testing.T) {
	store, bucket, claim, access, pol, secret := accessFixture(metav1.ConditionFalse)
	r, c, drv := newAccessReconciler(t, store, bucket, claim, access, pol, secret)

	if _, err := r.reconcileNormal(context.Background(), access); err != nil {
		t.Fatalf("reconcileNormal returned an error: %v", err)
	}

	if drv.deleteAccessCalls != 0 {
		t.Errorf("the backend key was revoked %d time(s) over an unready bucket", drv.deleteAccessCalls)
	}
	if drv.ensureAccessCalls != 0 {
		t.Errorf("EnsureAccess ran %d time(s) against a bucket that is not Ready", drv.ensureAccessCalls)
	}

	got := getAccess(t, c, access.Namespace, access.Name)
	if got.Status.AccessKeyID != "GKexisting" {
		t.Errorf("status.accessKeyID = %q, want the issued key to be kept", got.Status.AccessKeyID)
	}
	if got.Status.SecretRef == nil {
		t.Error("status.secretRef was cleared; the credentials Secret must survive an outage")
	}
	err := c.Get(context.Background(), client.ObjectKey{Namespace: secret.Namespace, Name: secret.Name}, &corev1.Secret{})
	if err != nil {
		t.Errorf("credentials Secret must not be deleted over an unready bucket: %v", err)
	}

	cond := apimeta.FindStatusCondition(got.Status.Conditions, v1alpha1.BucketAccessConditionAccessGranted)
	if cond == nil {
		t.Fatal("AccessGranted condition not written")
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != reasonInProgress {
		t.Errorf("AccessGranted = %s/%s, want False/%s", cond.Status, cond.Reason, reasonInProgress)
	}
}

// TestUnauthorizedAccessStillRevoked guards the other half of the split: losing
// authorization is intent, not health, and must still revoke immediately even
// though the bucket is perfectly Ready.
func TestUnauthorizedAccessStillRevoked(t *testing.T) {
	store, bucket, claim, access, _, secret := accessFixture(metav1.ConditionTrue)
	// No BucketClaimPolicy: a Shared bucket is deny-by-default.
	r, c, drv := newAccessReconciler(t, store, bucket, claim, access, secret)

	if _, err := r.reconcileNormal(context.Background(), access); err != nil {
		t.Fatalf("reconcileNormal returned an error: %v", err)
	}

	if drv.deleteAccessCalls != 1 {
		t.Errorf("DeleteAccess called %d time(s), want 1 (authorization was withdrawn)", drv.deleteAccessCalls)
	}
	got := getAccess(t, c, access.Namespace, access.Name)
	if got.Status.AccessKeyID != "" {
		t.Errorf("status.accessKeyID = %q, want it cleared after revocation", got.Status.AccessKeyID)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, v1alpha1.BucketAccessConditionAccessGranted)
	if cond == nil || cond.Reason != "DeniedByPolicy" {
		t.Errorf("AccessGranted reason = %v, want DeniedByPolicy", cond)
	}
}

// TestUnboundClaimStillRevokes keeps the binding half honest too: a claim that no
// longer points at a bucket is a decision, so the grant goes.
func TestUnboundClaimStillRevokes(t *testing.T) {
	store, bucket, claim, access, pol, secret := accessFixture(metav1.ConditionTrue)
	apimeta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
		Type:   v1alpha1.BucketClaimConditionBound,
		Status: metav1.ConditionFalse,
		Reason: "WaitingForBucket",
	})
	r, _, drv := newAccessReconciler(t, store, bucket, claim, access, pol, secret)

	if _, err := r.reconcileNormal(context.Background(), access); err != nil {
		t.Fatalf("reconcileNormal returned an error: %v", err)
	}
	if drv.deleteAccessCalls != 1 {
		t.Errorf("DeleteAccess called %d time(s), want 1 (claim is not bound)", drv.deleteAccessCalls)
	}
}
