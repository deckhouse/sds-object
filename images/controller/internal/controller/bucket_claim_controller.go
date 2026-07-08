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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"

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
	"github.com/deckhouse/sds-object/images/controller/pkg/config"
	"github.com/deckhouse/sds-object/images/controller/pkg/logger"
)

// BucketClaimReconciler drives every BucketClaim through a two-stage FSM:
//
//		Bound -> BucketReady -> Ready
//
//	  - Bound establishes the binding. Greenfield (spec.existingBucketName empty)
//	    creates a cluster-scoped Bucket owned by the claim (origin=BucketClaim,
//	    reserved-prefixed name) in spec.objectStoreRef. Brownfield binds an
//	    existing Shared Bucket named spec.existingBucketName, but only when a
//	    BucketPolicy grants the claim's namespace (deny-by-default).
//	  - BucketReady gates on the bound Bucket's own Ready condition.
//
// A cluster-scoped Bucket cannot carry a namespaced ownerReference, so the
// greenfield Bucket is linked back to the claim with labels and torn down
// explicitly on claim deletion (honoring the Bucket's reclaim policy). The
// claim's finalizer is held until no BucketAccess in its namespace references
// it anymore.
type BucketClaimReconciler struct {
	Client client.Client
	Scheme *runtime.Scheme
	Log    *logger.Logger
	Cfg    *config.Options
}

var bucketClaimStageOrder = []string{
	v1alpha1.BucketClaimConditionBound,
	v1alpha1.BucketClaimConditionBucketReady,
}

// claimObserved accumulates the status fields written back onto the claim.
type claimObserved struct {
	boundBucketName string
	endpoint        string
}

// AddBucketClaimReconcilerToManager wires the BucketClaim reconciler. It watches
// Bucket (a bound bucket becoming Ready re-reconciles the claim) and
// BucketPolicy (a policy change re-evaluates brownfield claims for its bucket).
func AddBucketClaimReconcilerToManager(mgr manager.Manager, cfg *config.Options, log *logger.Logger) error {
	r := &BucketClaimReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Log:    log,
		Cfg:    cfg,
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("bucket-claim").
		For(&v1alpha1.BucketClaim{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&v1alpha1.Bucket{}, handler.EnqueueRequestsFromMapFunc(r.enqueueByBucket)).
		Watches(&v1alpha1.BucketPolicy{}, handler.EnqueueRequestsFromMapFunc(r.enqueueByPolicy)).
		WithOptions(controller.Options{MaxConcurrentReconciles: cfg.MaxConcurrentReconciles}).
		Complete(r)
}

// enqueueByBucket maps a Bucket event to the claims that reference it: the
// greenfield claim that owns it (owned-by-claim labels) and any brownfield
// claims binding it by name.
func (r *BucketClaimReconciler) enqueueByBucket(ctx context.Context, o client.Object) []reconcile.Request {
	list := &v1alpha1.BucketClaimList{}
	if err := r.Client.List(ctx, list); err != nil {
		r.Log.Error(err, "[enqueueByBucket] list failed")
		return nil
	}
	labels := o.GetLabels()
	ownerNS := labels[v1alpha1.LabelOwnedByClaimNamespace]
	ownerName := labels[v1alpha1.LabelOwnedByClaimName]
	out := make([]reconcile.Request, 0)
	for i := range list.Items {
		c := &list.Items[i]
		if c.Spec.ExistingBucketName == o.GetName() ||
			(c.Namespace == ownerNS && c.Name == ownerName) {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: c.Namespace, Name: c.Name}})
		}
	}
	return out
}

// enqueueByPolicy maps a policy event to every brownfield claim for its bucket.
func (r *BucketClaimReconciler) enqueueByPolicy(ctx context.Context, o client.Object) []reconcile.Request {
	policy, ok := o.(*v1alpha1.BucketPolicy)
	if !ok {
		return nil
	}
	list := &v1alpha1.BucketClaimList{}
	if err := r.Client.List(ctx, list); err != nil {
		r.Log.Error(err, "[enqueueByPolicy] list failed")
		return nil
	}
	out := make([]reconcile.Request, 0)
	for i := range list.Items {
		if list.Items[i].Spec.ExistingBucketName == policy.Spec.BucketRef {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: list.Items[i].Namespace, Name: list.Items[i].Name}})
		}
	}
	return out
}

func (r *BucketClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.Log.Info(fmt.Sprintf("[Reconcile] start for BucketClaim %s", req.NamespacedName))

	claim := &v1alpha1.BucketClaim{}
	if err := r.Client.Get(ctx, req.NamespacedName, claim); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if claim.DeletionTimestamp != nil {
		return r.reconcileDelete(ctx, claim)
	}

	if !controllerutil.ContainsFinalizer(claim, Finalizer) {
		controllerutil.AddFinalizer(claim, Finalizer)
		if err := r.Client.Update(ctx, claim); err != nil {
			return ctrl.Result{}, err
		}
	}

	return r.reconcileNormal(ctx, claim)
}

func (r *BucketClaimReconciler) reconcileDelete(ctx context.Context, claim *v1alpha1.BucketClaim) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(claim, Finalizer) {
		return ctrl.Result{}, nil
	}

	// Do not release the claim while any BucketAccess in its namespace still
	// references it.
	refs, err := r.referencingAccesses(ctx, claim)
	if err != nil {
		return ctrl.Result{}, err
	}
	if refs > 0 {
		r.Log.Info(fmt.Sprintf("[reconcileDelete] claim %s/%s still referenced by %d BucketAccess; waiting",
			claim.Namespace, claim.Name, refs))
		return ctrl.Result{RequeueAfter: r.Cfg.RequeueInterval}, nil
	}

	// Greenfield: explicitly delete the owned Bucket CR (its own reconcileDelete
	// honors the reclaim policy for backend data). Brownfield owns nothing.
	if claim.Spec.ExistingBucketName == "" {
		bucket := &v1alpha1.Bucket{ObjectMeta: metav1.ObjectMeta{Name: greenfieldBucketName(claim)}}
		if err := r.Client.Delete(ctx, bucket); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	controllerutil.RemoveFinalizer(claim, Finalizer)
	if err := r.Client.Update(ctx, claim); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *BucketClaimReconciler) reconcileNormal(ctx context.Context, claim *v1alpha1.BucketClaim) (ctrl.Result, error) {
	status := newStatusBuilder(claim.Generation)
	observed := &claimObserved{}

	boundName, gated, err := r.ensureBound(ctx, claim, status, observed)
	if err != nil {
		status.setCondition(v1alpha1.BucketClaimConditionBound, metav1.ConditionFalse, reasonError, err.Error())
		gateAfter(status, bucketClaimStageOrder, v1alpha1.BucketClaimConditionBound)
		return r.finish(ctx, claim, status, observed, err)
	}
	if gated {
		// ensureBound already recorded the Bound condition (with its specific
		// reason) and gated the remaining stages; don't overwrite it.
		return r.finish(ctx, claim, status, observed, nil)
	}
	status.setCondition(v1alpha1.BucketClaimConditionBound, metav1.ConditionTrue, reasonReady, "bucket bound")

	// BucketReady: the bound Bucket must report Ready.
	bucket := &v1alpha1.Bucket{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: boundName}, bucket); err != nil {
		status.setCondition(v1alpha1.BucketClaimConditionBucketReady, metav1.ConditionFalse, reasonInProgress,
			fmt.Sprintf("bound Bucket %q is not available yet: %v", boundName, err))
		gateAfter(status, bucketClaimStageOrder, v1alpha1.BucketClaimConditionBucketReady)
		return r.finish(ctx, claim, status, observed, nil)
	}
	if bucket.Status != nil && bucket.Status.Endpoint != "" {
		observed.endpoint = bucket.Status.Endpoint
	}
	if !advance(status, bucketClaimStageOrder, v1alpha1.BucketClaimConditionBucketReady,
		bucketReadyState(bucket) == string(metav1.ConditionTrue), "bound bucket is Ready", nil) {
		return r.finish(ctx, claim, status, observed, nil)
	}

	status.setCondition(v1alpha1.BucketClaimConditionReady, metav1.ConditionTrue, reasonReady, "All stages reconciled")
	return r.finish(ctx, claim, status, observed, nil)
}

// ensureBound establishes the binding and returns the bound bucket name. When it
// cannot bind, it writes the Bound condition itself (with a specific reason),
// gates the remaining stages, and returns gated=true so the caller finishes
// without overwriting the reason. On success it returns (name, false, nil); a
// non-nil error signals a transient failure the caller reports as Error.
func (r *BucketClaimReconciler) ensureBound(
	ctx context.Context,
	claim *v1alpha1.BucketClaim,
	status *statusBuilder,
	observed *claimObserved,
) (string, bool, error) {
	if claim.Spec.ExistingBucketName != "" {
		return r.ensureBrownfield(ctx, claim, status, observed)
	}
	return r.ensureGreenfield(ctx, claim, status, observed)
}

// ensureBrownfield binds an existing Shared Bucket, gated by BucketPolicy.
func (r *BucketClaimReconciler) ensureBrownfield(
	ctx context.Context,
	claim *v1alpha1.BucketClaim,
	status *statusBuilder,
	observed *claimObserved,
) (string, bool, error) {
	name := claim.Spec.ExistingBucketName

	bucket := &v1alpha1.Bucket{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: name}, bucket); err != nil {
		if apierrors.IsNotFound(err) {
			status.setCondition(v1alpha1.BucketClaimConditionBound, metav1.ConditionFalse, "WaitingForBucket",
				fmt.Sprintf("Shared Bucket %q not found", name))
			gateAfter(status, bucketClaimStageOrder, v1alpha1.BucketClaimConditionBound)
			return "", true, nil
		}
		return "", false, err
	}
	// A greenfield bucket owned by another claim is private and never bindable.
	if bucket.Labels[v1alpha1.LabelBucketOrigin] == v1alpha1.BucketOriginBucketClaim {
		status.setCondition(v1alpha1.BucketClaimConditionBound, metav1.ConditionFalse, "NotShared",
			fmt.Sprintf("Bucket %q is owned by another claim and cannot be bound", name))
		gateAfter(status, bucketClaimStageOrder, v1alpha1.BucketClaimConditionBound)
		return "", true, nil
	}

	allowed, reason, err := namespaceAllowedForBucket(ctx, r.Client, name, claim.Namespace)
	if err != nil {
		return "", false, err
	}
	if !allowed {
		status.setCondition(v1alpha1.BucketClaimConditionBound, metav1.ConditionFalse, "DeniedByPolicy", reason)
		gateAfter(status, bucketClaimStageOrder, v1alpha1.BucketClaimConditionBound)
		return "", true, nil
	}

	observed.boundBucketName = name
	return name, false, nil
}

// ensureGreenfield creates (idempotently) the claim-owned Bucket and binds it.
func (r *BucketClaimReconciler) ensureGreenfield(
	ctx context.Context,
	claim *v1alpha1.BucketClaim,
	status *statusBuilder,
	observed *claimObserved,
) (string, bool, error) {
	if claim.Spec.ObjectStoreRef == "" {
		status.setCondition(v1alpha1.BucketClaimConditionBound, metav1.ConditionFalse, "MissingObjectStoreRef",
			"spec.objectStoreRef is required for a greenfield claim")
		gateAfter(status, bucketClaimStageOrder, v1alpha1.BucketClaimConditionBound)
		return "", true, nil
	}

	name := greenfieldBucketName(claim)
	bucket := &v1alpha1.Bucket{ObjectMeta: metav1.ObjectMeta{Name: name}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, bucket, func() error {
		if bucket.Labels == nil {
			bucket.Labels = map[string]string{}
		}
		bucket.Labels[v1alpha1.LabelBucketOrigin] = v1alpha1.BucketOriginBucketClaim
		bucket.Labels[v1alpha1.LabelOwnedByClaimNamespace] = claim.Namespace
		bucket.Labels[v1alpha1.LabelOwnedByClaimName] = claim.Name
		bucket.Spec.ObjectStoreRef = claim.Spec.ObjectStoreRef
		bucket.Spec.AccessPolicy = claim.Spec.AccessPolicy
		bucket.Spec.ReclaimPolicy = claim.Spec.ReclaimPolicy
		bucket.Spec.Quota = claim.Spec.Quota
		return nil
	})
	if err != nil {
		return "", false, err
	}

	observed.boundBucketName = name
	return name, false, nil
}

// referencingAccesses counts the BucketAccess objects in the claim's namespace
// that reference it.
func (r *BucketClaimReconciler) referencingAccesses(ctx context.Context, claim *v1alpha1.BucketClaim) (int, error) {
	list := &v1alpha1.BucketAccessList{}
	if err := r.Client.List(ctx, list, client.InNamespace(claim.Namespace)); err != nil {
		return 0, err
	}
	n := 0
	for i := range list.Items {
		if list.Items[i].Spec.BucketClaimName == claim.Name {
			n++
		}
	}
	return n, nil
}

// greenfieldBucketName derives a DNS-1123 / S3-safe, collision-proof bucket name
// for a greenfield claim under the reserved prefix. It is deterministic per
// (namespace, name) so repeated reconciles target the same Bucket.
func greenfieldBucketName(claim *v1alpha1.BucketClaim) string {
	sum := sha256.Sum256([]byte(claim.Namespace + "/" + claim.Name))
	short := hex.EncodeToString(sum[:])[:10]
	name := v1alpha1.ReservedBucketNamePrefix + short
	if base := sanitizeDNSLabel(claim.Namespace + "-" + claim.Name); base != "" {
		name = name + "-" + base
	}
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.Trim(name, "-")
}

// sanitizeDNSLabel lowercases and replaces every character outside [a-z0-9-]
// with '-', collapsing runs and trimming leading/trailing dashes.
func sanitizeDNSLabel(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func (r *BucketClaimReconciler) finish(
	ctx context.Context,
	claim *v1alpha1.BucketClaim,
	status *statusBuilder,
	observed *claimObserved,
	reconcileErr error,
) (ctrl.Result, error) {
	if err := r.updateStatus(ctx, claim, status, observed); err != nil {
		r.Log.Error(err, "[finish] unable to update status")
		if reconcileErr == nil {
			reconcileErr = err
		}
	}
	if reconcileErr != nil {
		return ctrl.Result{}, reconcileErr
	}
	if aggregateReady(status) {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: r.Cfg.RequeueInterval}, nil
}

func (r *BucketClaimReconciler) updateStatus(
	ctx context.Context,
	claim *v1alpha1.BucketClaim,
	sb *statusBuilder,
	observed *claimObserved,
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &v1alpha1.BucketClaim{}
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: claim.Namespace, Name: claim.Name}, latest); err != nil {
			return err
		}
		if latest.Status == nil {
			latest.Status = &v1alpha1.BucketClaimStatus{}
		}
		before := latest.Status.DeepCopy()

		for _, cond := range sb.conditions {
			apimeta.SetStatusCondition(&latest.Status.Conditions, cond)
		}
		latest.Status.ObservedGeneration = claim.Generation
		latest.Status.Phase = derivePhase(latest.Status.Conditions, bucketClaimStageOrder)

		// boundBucketName is only ever set (never cleared here) so that a claim
		// that later loses its binding still lets a BucketAccess resolve the
		// backend to revoke its key.
		if observed.boundBucketName != "" {
			latest.Status.BoundBucketName = observed.boundBucketName
		}
		if observed.endpoint != "" {
			latest.Status.Endpoint = observed.endpoint
		}

		if reflect.DeepEqual(before, latest.Status) {
			return nil
		}
		return r.Client.Status().Update(ctx, latest)
	})
}
