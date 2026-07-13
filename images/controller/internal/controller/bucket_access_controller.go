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
	"fmt"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
	"github.com/deckhouse/sds-object/images/controller/internal/backend"
	"github.com/deckhouse/sds-object/images/controller/pkg/config"
	"github.com/deckhouse/sds-object/images/controller/pkg/logger"
)

// BucketAccessReconciler drives every BucketAccess through a short FSM:
//
//		AccessGranted -> CredentialsReady -> Ready
//
//	  - AccessGranted gates on the referenced BucketClaim (same namespace) being
//	    Bound to a Ready Bucket, and the backend.Driver issuing an access key.
//	    Whether the claim is allowed to bind (and therefore whether access is
//	    possible) is decided upstream by the BucketClaim controller (greenfield
//	    ownership or a BucketClaimPolicy grant); this controller only reacts to the
//	    claim's Bound state. When a claim leaves Bound, any key issued for the
//	    access is revoked and its Secret removed (continuous enforcement).
//	  - CredentialsReady (re)writes the S3 credentials Secret in the access's
//	    namespace. A change to the storage.deckhouse.io/rotate annotation triggers
//	    a fresh key pair.
type BucketAccessReconciler struct {
	Client client.Client
	// APIReader is an uncached reader used for the independent deny-by-default
	// authorization re-check (a security boundary), so the decision is not
	// subject to informer-cache lag.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Log       *logger.Logger
	Cfg       *config.Options
	Registry  *backend.Registry
}

var bucketAccessStageOrder = []string{
	v1alpha1.BucketAccessConditionAccessGranted,
	v1alpha1.BucketAccessConditionCredentialsReady,
}

// accessObserved accumulates the status fields written back onto the access.
type accessObserved struct {
	bucketName       string
	endpoint         string
	accessKeyID      string
	secretName       string
	observedRotation string
	rotated          bool
	// revoked clears the credential status fields (the access's claim is no
	// longer Bound and its key/Secret were removed).
	revoked bool
}

// AddBucketAccessReconcilerToManager wires the BucketAccess reconciler. It
// watches BucketClaim so that a claim becoming (or ceasing to be) Bound
// re-reconciles every access that references it in the same namespace.
func AddBucketAccessReconcilerToManager(mgr manager.Manager, cfg *config.Options, log *logger.Logger, reg *backend.Registry) error {
	r := &BucketAccessReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		Log:       log,
		Cfg:       cfg,
		Registry:  reg,
	}

	// Reconcile on spec changes and on rotation-annotation changes.
	accessPredicate := predicate.Or(
		predicate.GenerationChangedPredicate{},
		predicate.AnnotationChangedPredicate{},
	)

	return ctrl.NewControllerManagedBy(mgr).
		Named("bucket-access").
		For(&v1alpha1.BucketAccess{}, builder.WithPredicates(accessPredicate)).
		Watches(&v1alpha1.BucketClaim{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueByClaim)).
		// Defense-in-depth: react directly to a BucketClaimPolicy change instead of
		// relying solely on the BucketClaim controller flipping Bound first, so a
		// revocation reaches the access even if that hop lags.
		Watches(&v1alpha1.BucketClaimPolicy{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueByPolicy)).
		WithOptions(controller.Options{MaxConcurrentReconciles: cfg.MaxConcurrentReconciles}).
		Complete(r)
}

// enqueueByPolicy maps a BucketClaimPolicy event to every BucketAccess whose
// bound bucket is the policy's target: it finds the (brownfield) claims for the
// policy's bucket, then the accesses referencing those claims. Uses the cached
// client (enqueue is best-effort; the authoritative re-check reads APIReader).
func (r *BucketAccessReconciler) enqueueByPolicy(ctx context.Context, o client.Object) []reconcile.Request {
	policy, ok := o.(*v1alpha1.BucketClaimPolicy)
	if !ok {
		return nil
	}
	claims := &v1alpha1.BucketClaimList{}
	if err := r.Client.List(ctx, claims); err != nil {
		r.Log.Error(err, "[enqueueByPolicy] list claims failed")
		return nil
	}
	type claimKey struct{ ns, name string }
	matched := map[claimKey]bool{}
	for i := range claims.Items {
		c := &claims.Items[i]
		bound := ""
		if c.Status != nil {
			bound = c.Status.BoundBucketName
		}
		if c.Spec.ExistingBucketName == policy.Spec.BucketRef || bound == policy.Spec.BucketRef {
			matched[claimKey{c.Namespace, c.Name}] = true
		}
	}
	if len(matched) == 0 {
		return nil
	}
	accesses := &v1alpha1.BucketAccessList{}
	if err := r.Client.List(ctx, accesses); err != nil {
		r.Log.Error(err, "[enqueueByPolicy] list accesses failed")
		return nil
	}
	out := make([]reconcile.Request, 0)
	for i := range accesses.Items {
		a := &accesses.Items[i]
		if matched[claimKey{a.Namespace, a.Spec.BucketClaimName}] {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: a.Namespace, Name: a.Name}})
		}
	}
	return out
}

// enqueueByClaim maps a BucketClaim event to every access in the claim's
// namespace that references it by name.
func (r *BucketAccessReconciler) enqueueByClaim(ctx context.Context, o client.Object) []reconcile.Request {
	list := &v1alpha1.BucketAccessList{}
	if err := r.Client.List(ctx, list, client.InNamespace(o.GetNamespace())); err != nil {
		r.Log.Error(err, "[enqueueByClaim] list failed")
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].Spec.BucketClaimName != o.GetName() {
			continue
		}
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: list.Items[i].Namespace,
			Name:      list.Items[i].Name,
		}})
	}
	return out
}

func (r *BucketAccessReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.Log.Info(fmt.Sprintf("[Reconcile] start for BucketAccess %s", req.NamespacedName))

	access := &v1alpha1.BucketAccess{}
	if err := r.Client.Get(ctx, req.NamespacedName, access); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if access.DeletionTimestamp != nil {
		return r.reconcileDelete(ctx, access)
	}

	if !controllerutil.ContainsFinalizer(access, Finalizer) {
		controllerutil.AddFinalizer(access, Finalizer)
		if err := r.Client.Update(ctx, access); err != nil {
			return ctrl.Result{}, err
		}
	}

	return r.reconcileNormal(ctx, access)
}

func (r *BucketAccessReconciler) reconcileDelete(ctx context.Context, access *v1alpha1.BucketAccess) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(access, Finalizer) {
		return ctrl.Result{}, nil
	}

	// Resolve the bound bucket + cluster to revoke the backend key. Distinguish a
	// transient failure (retry, keep the finalizer — do not orphan the key) from
	// the claim being genuinely gone (its cluster teardown took the key with it,
	// so release).
	claim := &v1alpha1.BucketClaim{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: access.Namespace, Name: access.Spec.BucketClaimName}, claim)
	switch {
	case err == nil:
		if claim.Status != nil && claim.Status.BoundBucketName != "" {
			bucket, cluster := r.resolveBound(ctx, claim.Status.BoundBucketName)
			if bucket != nil && cluster != nil {
				driver, derr := r.Registry.For(cluster)
				if derr != nil {
					return ctrl.Result{}, derr
				}
				if derr := driver.DeleteAccess(ctx, cluster, bucket, access); derr != nil {
					return ctrl.Result{}, derr
				}
			}
			// bucket/cluster no longer resolvable: the backend (and its key) is
			// gone; fall through to release best-effort.
		}
	case apierrors.IsNotFound(err):
		// Claim gone: the key was removed with the claim's cluster; release.
		r.Log.Info(fmt.Sprintf("[reconcileDelete] access %s/%s: claim %q not found; releasing", access.Namespace, access.Name, access.Spec.BucketClaimName))
	default:
		// Transient error reading the claim: retry rather than orphan the key.
		return ctrl.Result{}, err
	}

	controllerutil.RemoveFinalizer(access, Finalizer)
	if err := r.Client.Update(ctx, access); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *BucketAccessReconciler) reconcileNormal(ctx context.Context, access *v1alpha1.BucketAccess) (ctrl.Result, error) {
	status := newStatusBuilder(access.Generation)
	observed := &accessObserved{}

	// Resolve the BucketClaim in this access's namespace.
	claim := &v1alpha1.BucketClaim{}
	claimKey := client.ObjectKey{Namespace: access.Namespace, Name: access.Spec.BucketClaimName}
	if err := r.Client.Get(ctx, claimKey, claim); err != nil {
		if !apierrors.IsNotFound(err) {
			return r.finish(ctx, access, status, observed, err)
		}
		// Claim is gone entirely: revoke best-effort and drop the Secret.
		r.revokeIfIssued(ctx, access, observed)
		status.setCondition(v1alpha1.BucketAccessConditionAccessGranted, metav1.ConditionFalse, "WaitingForClaim",
			fmt.Sprintf("BucketClaim %q not found", access.Spec.BucketClaimName))
		gateAfter(status, bucketAccessStageOrder, v1alpha1.BucketAccessConditionAccessGranted)
		return r.finish(ctx, access, status, observed, nil)
	}

	// A claim exposes the bucket it is (or was) bound to via
	// status.boundBucketName; it stays populated even when the claim leaves
	// Bound, so a revocation can still resolve the backend.
	boundName := ""
	if claim.Status != nil {
		boundName = claim.Status.BoundBucketName
		observed.endpoint = claim.Status.Endpoint
	}

	// Resolve the bound Bucket + ObjectStore when known (used by both the
	// issue and the revoke paths).
	var bucket *v1alpha1.Bucket
	var cluster *v1alpha1.ObjectStore
	if boundName != "" {
		bucket, cluster = r.resolveBound(ctx, boundName)
	}

	// Independently re-check authorization (defense-in-depth): do not rely solely
	// on the claim's Bound condition, which is written by another reconciler and
	// may be stale. For a Shared bucket the access's namespace must be granted by
	// a BucketClaimPolicy (deny-by-default, read via the uncached APIReader); a
	// greenfield bucket is private to its owning claim's namespace.
	authorized, authzReason := true, ""
	if bucket != nil {
		ok, reason, aerr := r.accessAuthorized(ctx, bucket, access.Namespace)
		if aerr != nil {
			status.setCondition(v1alpha1.BucketAccessConditionAccessGranted, metav1.ConditionFalse, reasonError, aerr.Error())
			gateAfter(status, bucketAccessStageOrder, v1alpha1.BucketAccessConditionAccessGranted)
			return r.finish(ctx, access, status, observed, aerr)
		}
		authorized, authzReason = ok, reason
	}

	// Gate on the claim being Bound to a Ready bucket AND on the independent
	// authorization check.
	if !claimBound(claim) || bucket == nil || cluster == nil || bucketReadyState(bucket) != string(metav1.ConditionTrue) || !authorized {
		// The claim is not usable (or access is no longer authorized): enforce
		// revocation of any prior grant.
		if access.Status != nil && access.Status.AccessKeyID != "" {
			if bucket != nil && cluster != nil {
				if driver, derr := r.Registry.For(cluster); derr == nil {
					if derr := driver.DeleteAccess(ctx, cluster, bucket, access); derr != nil {
						status.setCondition(v1alpha1.BucketAccessConditionAccessGranted, metav1.ConditionFalse, reasonError,
							fmt.Sprintf("revoking access after claim became unbound: %v", derr))
						gateAfter(status, bucketAccessStageOrder, v1alpha1.BucketAccessConditionAccessGranted)
						return r.finish(ctx, access, status, observed, derr)
					}
				}
			}
			if err := r.deleteCredentialsSecret(ctx, access); err != nil {
				return r.finish(ctx, access, status, observed, err)
			}
			observed.revoked = true
		}
		reason, message := "WaitingForClaim", fmt.Sprintf("BucketClaim %q is not Bound to a Ready bucket", access.Spec.BucketClaimName)
		if !authorized && bucket != nil && cluster != nil && bucketReadyState(bucket) == string(metav1.ConditionTrue) && claimBound(claim) {
			// The only failing gate is authorization: report it explicitly.
			reason, message = "DeniedByPolicy", authzReason
		}
		status.setCondition(v1alpha1.BucketAccessConditionAccessGranted, metav1.ConditionFalse, reason, message)
		gateAfter(status, bucketAccessStageOrder, v1alpha1.BucketAccessConditionAccessGranted)
		return r.finish(ctx, access, status, observed, nil)
	}
	observed.bucketName = bucket.Status.BucketName

	driver, err := r.Registry.For(cluster)
	if err != nil {
		status.setCondition(v1alpha1.BucketAccessConditionAccessGranted, metav1.ConditionFalse, reasonError, err.Error())
		gateAfter(status, bucketAccessStageOrder, v1alpha1.BucketAccessConditionAccessGranted)
		return r.finish(ctx, access, status, observed, err)
	}

	// Decide whether to issue a fresh key: on a rotation-annotation change, when
	// no key has been issued yet, or when the credentials Secret is missing.
	rotationValue := access.Annotations[v1alpha1.RotateAnnotation]
	observedRotation := ""
	recordedKey := ""
	if access.Status != nil {
		observedRotation = access.Status.ObservedRotation
		recordedKey = access.Status.AccessKeyID
	}
	existing, secretExists, err := r.getSecret(ctx, access)
	if err != nil {
		return r.finish(ctx, access, status, observed, err)
	}
	mintFresh := rotationValue != observedRotation || recordedKey == "" || !secretExists

	state, err := driver.EnsureAccess(ctx, cluster, bucket, access, mintFresh)
	if state.AccessKeyID != "" {
		observed.accessKeyID = state.AccessKeyID
	} else {
		observed.accessKeyID = recordedKey
	}
	if !advance(status, bucketAccessStageOrder, v1alpha1.BucketAccessConditionAccessGranted, state.Ready, state.Message, err) {
		return r.finish(ctx, access, status, observed, err)
	}

	// Write the credentials Secret. A fresh key comes with its secret key; a
	// reused key keeps the secret key already stored in the Secret.
	secretName, err := r.ensureCredentialsSecret(ctx, cluster, bucket, access, &state, existing)
	if !advance(status, bucketAccessStageOrder, v1alpha1.BucketAccessConditionCredentialsReady, err == nil, "credentials Secret written", err) {
		return r.finish(ctx, access, status, observed, err)
	}
	observed.secretName = secretName
	observed.observedRotation = rotationValue
	observed.rotated = state.SecretAccessKey != ""

	status.setCondition(v1alpha1.BucketAccessConditionReady, metav1.ConditionTrue, reasonReady, "All stages reconciled")
	return r.finish(ctx, access, status, observed, nil)
}

// revokeIfIssued best-effort deletes the credentials Secret when the referenced
// claim has vanished and the access had previously been granted. The backend
// key cannot be revoked here (the bucket/store are no longer resolvable), so
// this only drops the in-cluster Secret and marks the status for clearing.
func (r *BucketAccessReconciler) revokeIfIssued(ctx context.Context, access *v1alpha1.BucketAccess, observed *accessObserved) {
	if access.Status == nil || access.Status.AccessKeyID == "" {
		return
	}
	if err := r.deleteCredentialsSecret(ctx, access); err != nil {
		r.Log.Error(err, "[revokeIfIssued] deleting credentials Secret")
		return
	}
	observed.revoked = true
}

// resolveBound loads a cluster-scoped Bucket by name and its ObjectStore. It
// returns nil pointers (not an error) when either is missing, so callers can
// gate rather than fail.
func (r *BucketAccessReconciler) resolveBound(ctx context.Context, bucketName string) (*v1alpha1.Bucket, *v1alpha1.ObjectStore) {
	bucket := &v1alpha1.Bucket{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: bucketName}, bucket); err != nil {
		return nil, nil
	}
	cluster := &v1alpha1.ObjectStore{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: bucket.Spec.ObjectStoreRef}, cluster); err != nil {
		return bucket, nil
	}
	return bucket, cluster
}

// credentialsSecretName is spec.credentialsSecretName or <access>-s3-credentials.
func credentialsSecretName(access *v1alpha1.BucketAccess) string {
	if access.Spec.CredentialsSecretName != "" {
		return access.Spec.CredentialsSecretName
	}
	return access.Name + "-s3-credentials"
}

// deleteCredentialsSecret removes the access's credentials Secret (idempotent).
func (r *BucketAccessReconciler) deleteCredentialsSecret(ctx context.Context, access *v1alpha1.BucketAccess) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: credentialsSecretName(access), Namespace: access.Namespace},
	}
	if err := r.Client.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// getSecret reads the credentials Secret (nil, false when it does not exist).
// It reads through the uncached APIReader: the result drives the mintFresh
// decision, and a stale cached read (Secret just written but not yet in the
// informer cache) would make it look missing and trigger an unnecessary key
// rotation — revoking a live key.
func (r *BucketAccessReconciler) getSecret(ctx context.Context, access *v1alpha1.BucketAccess) (*corev1.Secret, bool, error) {
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: access.Namespace, Name: credentialsSecretName(access)}
	if err := r.APIReader.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return secret, true, nil
}

// ensureCredentialsSecret creates or updates the credentials Secret owned by the
// access and returns its name.
func (r *BucketAccessReconciler) ensureCredentialsSecret(
	ctx context.Context,
	cluster *v1alpha1.ObjectStore,
	bucket *v1alpha1.Bucket,
	access *v1alpha1.BucketAccess,
	state *backend.AccessState,
	existing *corev1.Secret,
) (string, error) {
	endpoint, region := "", ""
	if cluster.Status != nil && cluster.Status.Endpoint != nil {
		endpoint = cluster.Status.Endpoint.Internal
		region = cluster.Status.Endpoint.Region
	}
	bucketName := bucket.Status.BucketName
	if bucketName == "" {
		bucketName = backend.BucketDisplayName(bucket)
	}

	accessKeyID := state.AccessKeyID
	secretKey := state.SecretAccessKey
	if secretKey == "" && existing != nil {
		// Reused key: keep the secret key already stored.
		secretKey = string(existing.Data[v1alpha1.SecretKeySecretAccessID])
		if accessKeyID == "" {
			accessKeyID = string(existing.Data[v1alpha1.SecretKeyAccessKeyID])
		}
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      credentialsSecretName(access),
			Namespace: access.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.Labels == nil {
			secret.Labels = map[string]string{}
		}
		secret.Labels["storage.deckhouse.io/bucket-access"] = access.Name
		secret.Type = corev1.SecretTypeOpaque
		secret.StringData = map[string]string{
			v1alpha1.SecretKeyS3Endpoint:     endpoint,
			v1alpha1.SecretKeyS3Region:       region,
			v1alpha1.SecretKeyS3Bucket:       bucketName,
			v1alpha1.SecretKeyAccessKeyID:    accessKeyID,
			v1alpha1.SecretKeySecretAccessID: secretKey,
		}
		return controllerutil.SetControllerReference(access, secret, r.Scheme)
	})
	if err != nil {
		return "", err
	}
	return secret.Name, nil
}

// accessAuthorized independently re-checks whether the access's namespace may
// use the bound bucket, so enforcement does not rely solely on the claim's
// Bound condition (defense-in-depth). A greenfield bucket (origin=BucketClaim)
// is private to its owning claim's namespace; any other bucket is treated as
// Shared and gated by BucketClaimPolicy (deny-by-default), read through the
// uncached APIReader so a just-revoked policy is not masked by cache lag. The
// returned string explains a denial for the access status.
func (r *BucketAccessReconciler) accessAuthorized(ctx context.Context, bucket *v1alpha1.Bucket, namespace string) (bool, string, error) {
	if bucket.Labels[v1alpha1.LabelBucketOrigin] == v1alpha1.BucketOriginBucketClaim {
		owner := bucket.Labels[v1alpha1.LabelOwnedByClaimNamespace]
		if owner != namespace {
			return false, fmt.Sprintf("bucket %q is private to namespace %q", bucket.Name, owner), nil
		}
		return true, "", nil
	}
	return namespaceAllowedForBucket(ctx, r.APIReader, bucket.Name, namespace)
}

// claimBound reports whether the claim's Bound condition is True.
func claimBound(c *v1alpha1.BucketClaim) bool {
	if c == nil || c.Status == nil {
		return false
	}
	cond := apimeta.FindStatusCondition(c.Status.Conditions, v1alpha1.BucketClaimConditionBound)
	return cond != nil && cond.Status == metav1.ConditionTrue
}

// bucketReadyState returns the bucket's Ready condition status (or "").
func bucketReadyState(b *v1alpha1.Bucket) string {
	if b == nil || b.Status == nil {
		return ""
	}
	cond := apimeta.FindStatusCondition(b.Status.Conditions, v1alpha1.BucketConditionReady)
	if cond == nil {
		return ""
	}
	return string(cond.Status)
}

func (r *BucketAccessReconciler) finish(
	ctx context.Context,
	access *v1alpha1.BucketAccess,
	status *statusBuilder,
	observed *accessObserved,
	reconcileErr error,
) (ctrl.Result, error) {
	if err := r.updateStatus(ctx, access, status, observed); err != nil {
		r.Log.Error(err, "[finish] unable to update status")
		if reconcileErr == nil {
			reconcileErr = err
		}
	}
	if reconcileErr != nil {
		return ctrl.Result{}, reconcileErr
	}
	if aggregateReady(status) {
		// Re-drive even when Ready so a granted access periodically re-checks its
		// authorization (deny-by-default). A missed policy/claim watch event thus
		// self-heals within minutes instead of persisting as a dangling grant
		// until the ~10h informer resync.
		return ctrl.Result{RequeueAfter: r.Cfg.SecurityResyncInterval}, nil
	}
	return ctrl.Result{RequeueAfter: r.Cfg.RequeueInterval}, nil
}

func (r *BucketAccessReconciler) updateStatus(
	ctx context.Context,
	access *v1alpha1.BucketAccess,
	sb *statusBuilder,
	observed *accessObserved,
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &v1alpha1.BucketAccess{}
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: access.Namespace, Name: access.Name}, latest); err != nil {
			return err
		}
		if latest.Status == nil {
			latest.Status = &v1alpha1.BucketAccessStatus{}
		}
		before := latest.Status.DeepCopy()

		for _, cond := range sb.conditions {
			apimeta.SetStatusCondition(&latest.Status.Conditions, cond)
		}
		latest.Status.ObservedGeneration = access.Generation
		latest.Status.Phase = derivePhase(latest.Status.Conditions, bucketAccessStageOrder)

		if observed.endpoint != "" {
			latest.Status.Endpoint = observed.endpoint
		}
		if observed.bucketName != "" {
			latest.Status.BucketName = observed.bucketName
		}
		if observed.revoked {
			// Access lost its claim binding: clear the credential status.
			latest.Status.AccessKeyID = ""
			latest.Status.SecretRef = nil
			latest.Status.ObservedRotation = ""
			latest.Status.LastRotationTime = nil
			if reflect.DeepEqual(before, latest.Status) {
				return nil
			}
			return r.Client.Status().Update(ctx, latest)
		}
		if observed.accessKeyID != "" {
			latest.Status.AccessKeyID = observed.accessKeyID
		}
		if observed.secretName != "" {
			latest.Status.SecretRef = &v1alpha1.LocalSecretReference{Name: observed.secretName}
			latest.Status.ObservedRotation = observed.observedRotation
			if observed.rotated {
				now := metav1.Now()
				latest.Status.LastRotationTime = &now
			}
		}

		if reflect.DeepEqual(before, latest.Status) {
			return nil
		}
		return r.Client.Status().Update(ctx, latest)
	})
}
