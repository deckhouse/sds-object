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
	"regexp"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
	"github.com/deckhouse/sds-object/images/controller/pkg/config"
	"github.com/deckhouse/sds-object/images/controller/pkg/logger"
)

// BucketClaimPolicyReconciler validates an BucketClaimPolicy:
// its regexp patterns must compile and the referenced bucket should exist. It
// carries no backend state (enforcement happens in the access reconciler and
// the admission webhook), so it only maintains the Ready condition / phase.
type BucketClaimPolicyReconciler struct {
	Client client.Client
	Log    *logger.Logger
	Cfg    *config.Options
}

func AddBucketClaimPolicyReconcilerToManager(mgr manager.Manager, cfg *config.Options, log *logger.Logger) error {
	r := &BucketClaimPolicyReconciler{
		Client: mgr.GetClient(),
		Log:    log,
		Cfg:    cfg,
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("bucket-policy").
		For(&v1alpha1.BucketClaimPolicy{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		WithOptions(controller.Options{MaxConcurrentReconciles: cfg.MaxConcurrentReconciles}).
		Complete(r)
}

func (r *BucketClaimPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	policy := &v1alpha1.BucketClaimPolicy{}
	if err := r.Client.Get(ctx, req.NamespacedName, policy); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	phase := v1alpha1.PhaseReady
	condStatus := metav1.ConditionTrue
	reason := reasonReady
	message := "policy is valid"

	if badPattern, err := firstInvalidPattern(policy.Spec.AllowedNamespaces.Patterns); err != nil {
		phase, condStatus, reason = v1alpha1.PhaseError, metav1.ConditionFalse, reasonError
		message = fmt.Sprintf("invalid namespace pattern %q: %v", badPattern, err)
	} else if !r.bucketExists(ctx, policy.Spec.BucketRef) {
		phase, condStatus, reason = v1alpha1.PhasePending, metav1.ConditionFalse, "WaitingForBucket"
		message = fmt.Sprintf("referenced Bucket %q not found", policy.Spec.BucketRef)
	}

	return ctrl.Result{}, r.updateStatus(ctx, policy, phase, condStatus, reason, message)
}

func (r *BucketClaimPolicyReconciler) bucketExists(ctx context.Context, name string) bool {
	bucket := &v1alpha1.Bucket{}
	return r.Client.Get(ctx, client.ObjectKey{Name: name}, bucket) == nil
}

// firstInvalidPattern returns the first pattern that fails to compile.
func firstInvalidPattern(patterns []string) (string, error) {
	for _, p := range patterns {
		if _, err := regexp.Compile(anchor(p)); err != nil {
			return p, err
		}
	}
	return "", nil
}

func (r *BucketClaimPolicyReconciler) updateStatus(
	ctx context.Context,
	policy *v1alpha1.BucketClaimPolicy,
	phase string,
	condStatus metav1.ConditionStatus,
	reason, message string,
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &v1alpha1.BucketClaimPolicy{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: policy.Name}, latest); err != nil {
			return err
		}
		if latest.Status == nil {
			latest.Status = &v1alpha1.BucketClaimPolicyStatus{}
		}
		before := latest.Status.DeepCopy()

		apimeta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.BucketClaimPolicyConditionReady,
			Status:             condStatus,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: policy.Generation,
		})
		latest.Status.ObservedGeneration = policy.Generation
		latest.Status.Phase = phase

		if reflect.DeepEqual(before, latest.Status) {
			return nil
		}
		return r.Client.Status().Update(ctx, latest)
	})
}
