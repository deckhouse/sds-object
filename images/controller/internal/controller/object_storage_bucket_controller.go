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
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
	"github.com/deckhouse/sds-object/images/controller/internal/backend"
	"github.com/deckhouse/sds-object/images/controller/pkg/config"
	"github.com/deckhouse/sds-object/images/controller/pkg/logger"
)

// ObjectStorageBucketReconciler drives every ObjectStorageBucket through a
// single-stage FSM:
//
//	BucketReady -> Ready
//
// BucketReady gates on the referenced ObjectStorageCluster (must exist and be
// Ready) and then on the backend.Driver creating the bucket. Credentials are
// issued separately per ObjectStorageBucketAccess.
type ObjectStorageBucketReconciler struct {
	Client   client.Client
	Scheme   *runtime.Scheme
	Log      *logger.Logger
	Cfg      *config.Options
	Registry *backend.Registry
}

// osbStageOrder lists the FSM stage condition types in execution order.
var osbStageOrder = []string{
	v1alpha1.OSBConditionBucketReady,
}

// bucketObserved accumulates the status fields the reconciler writes back onto
// the ObjectStorageBucket.
type bucketObserved struct {
	bucketName string
	endpoint   string
}

// AddObjectStorageBucketReconcilerToManager wires the OSB reconciler into the
// manager. Besides watching ObjectStorageBucket itself, it watches
// ObjectStorageCluster so that a cluster becoming Ready re-reconciles every
// bucket referencing it.
func AddObjectStorageBucketReconcilerToManager(mgr manager.Manager, cfg *config.Options, log *logger.Logger, reg *backend.Registry) error {
	r := &ObjectStorageBucketReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Log:      log,
		Cfg:      cfg,
		Registry: reg,
	}

	clusterReadyPredicate := predicate.Funcs{
		CreateFunc:  func(_ event.CreateEvent) bool { return true },
		DeleteFunc:  func(_ event.DeleteEvent) bool { return true },
		GenericFunc: func(_ event.GenericEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldC, _ := e.ObjectOld.(*v1alpha1.ObjectStorageCluster)
			newC, _ := e.ObjectNew.(*v1alpha1.ObjectStorageCluster)
			return clusterReadyState(oldC) != clusterReadyState(newC)
		},
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("object-storage-bucket").
		For(&v1alpha1.ObjectStorageBucket{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&v1alpha1.ObjectStorageCluster{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueBucketsByCluster),
			builder.WithPredicates(clusterReadyPredicate)).
		WithOptions(controller.Options{MaxConcurrentReconciles: cfg.MaxConcurrentReconciles}).
		Complete(r)
}

// clusterReadyState returns the cluster's Ready condition status (or "").
func clusterReadyState(c *v1alpha1.ObjectStorageCluster) string {
	if c == nil || c.Status == nil {
		return ""
	}
	cond := apimeta.FindStatusCondition(c.Status.Conditions, v1alpha1.OSCConditionReady)
	if cond == nil {
		return ""
	}
	return string(cond.Status)
}

// enqueueBucketsByCluster maps an ObjectStorageCluster event to every
// ObjectStorageBucket that references it via spec.clusterRef.
func (r *ObjectStorageBucketReconciler) enqueueBucketsByCluster(ctx context.Context, o client.Object) []reconcile.Request {
	list := &v1alpha1.ObjectStorageBucketList{}
	if err := r.Client.List(ctx, list); err != nil {
		r.Log.Error(err, "[enqueueBucketsByCluster] list failed")
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].Spec.ClusterRef != o.GetName() {
			continue
		}
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: list.Items[i].Name}})
	}
	return out
}

func (r *ObjectStorageBucketReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.Log.Info(fmt.Sprintf("[Reconcile] start for ObjectStorageBucket %q", req.Name))

	bucket := &v1alpha1.ObjectStorageBucket{}
	if err := r.Client.Get(ctx, req.NamespacedName, bucket); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if bucket.DeletionTimestamp != nil {
		return r.reconcileDelete(ctx, bucket)
	}

	if !controllerutil.ContainsFinalizer(bucket, Finalizer) {
		controllerutil.AddFinalizer(bucket, Finalizer)
		if err := r.Client.Update(ctx, bucket); err != nil {
			return ctrl.Result{}, err
		}
	}

	return r.reconcileNormal(ctx, bucket)
}

func (r *ObjectStorageBucketReconciler) reconcileDelete(ctx context.Context, bucket *v1alpha1.ObjectStorageBucket) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(bucket, Finalizer) {
		return ctrl.Result{}, nil
	}

	cluster, err := r.getCluster(ctx, bucket.Spec.ClusterRef)
	switch {
	case err != nil:
		r.Log.Warning(fmt.Sprintf("[reconcileDelete] bucket %q: cluster %q unavailable (%v); removing finalizer",
			bucket.Name, bucket.Spec.ClusterRef, err))
	default:
		if driver, derr := r.Registry.For(cluster); derr == nil {
			if derr := driver.DeleteBucket(ctx, cluster, bucket); derr != nil {
				return ctrl.Result{}, derr
			}
		}
	}

	controllerutil.RemoveFinalizer(bucket, Finalizer)
	if err := r.Client.Update(ctx, bucket); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *ObjectStorageBucketReconciler) reconcileNormal(ctx context.Context, bucket *v1alpha1.ObjectStorageBucket) (ctrl.Result, error) {
	status := newStatusBuilder(bucket.Generation)
	observed := &bucketObserved{}

	cluster, err := r.getCluster(ctx, bucket.Spec.ClusterRef)
	if err != nil {
		status.setCondition(v1alpha1.OSBConditionBucketReady, metav1.ConditionFalse, "WaitingForCluster",
			fmt.Sprintf("ObjectStorageCluster %q is not available: %v", bucket.Spec.ClusterRef, err))
		gateAfter(status, osbStageOrder, v1alpha1.OSBConditionBucketReady)
		return r.finish(ctx, bucket, status, observed, nil)
	}
	if cluster.Status != nil && cluster.Status.Endpoint != nil {
		observed.endpoint = cluster.Status.Endpoint.Internal
	}
	if clusterReadyState(cluster) != string(metav1.ConditionTrue) {
		status.setCondition(v1alpha1.OSBConditionBucketReady, metav1.ConditionFalse, "WaitingForCluster",
			fmt.Sprintf("ObjectStorageCluster %q is not Ready", bucket.Spec.ClusterRef))
		gateAfter(status, osbStageOrder, v1alpha1.OSBConditionBucketReady)
		return r.finish(ctx, bucket, status, observed, nil)
	}

	driver, err := r.Registry.For(cluster)
	if err != nil {
		status.setCondition(v1alpha1.OSBConditionBucketReady, metav1.ConditionFalse, reasonError, err.Error())
		gateAfter(status, osbStageOrder, v1alpha1.OSBConditionBucketReady)
		return r.finish(ctx, bucket, status, observed, err)
	}

	state, err := driver.EnsureBucket(ctx, cluster, bucket)
	observed.bucketName = state.BucketName
	if !advance(status, osbStageOrder, v1alpha1.OSBConditionBucketReady, state.Ready, state.Message, err) {
		return r.finish(ctx, bucket, status, observed, err)
	}

	status.setCondition(v1alpha1.OSBConditionReady, metav1.ConditionTrue, reasonReady, "All stages reconciled")
	return r.finish(ctx, bucket, status, observed, nil)
}

// getCluster fetches the cluster-scoped ObjectStorageCluster by name.
func (r *ObjectStorageBucketReconciler) getCluster(ctx context.Context, name string) (*v1alpha1.ObjectStorageCluster, error) {
	if name == "" {
		return nil, fmt.Errorf("spec.clusterRef is empty")
	}
	cluster := &v1alpha1.ObjectStorageCluster{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: name}, cluster); err != nil {
		return nil, err
	}
	return cluster, nil
}

func (r *ObjectStorageBucketReconciler) finish(
	ctx context.Context,
	bucket *v1alpha1.ObjectStorageBucket,
	status *statusBuilder,
	observed *bucketObserved,
	reconcileErr error,
) (ctrl.Result, error) {
	if err := r.updateStatus(ctx, bucket, status, observed); err != nil {
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

func (r *ObjectStorageBucketReconciler) updateStatus(
	ctx context.Context,
	bucket *v1alpha1.ObjectStorageBucket,
	sb *statusBuilder,
	observed *bucketObserved,
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &v1alpha1.ObjectStorageBucket{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: bucket.Name}, latest); err != nil {
			return err
		}
		if latest.Status == nil {
			latest.Status = &v1alpha1.ObjectStorageBucketStatus{}
		}
		before := latest.Status.DeepCopy()

		for _, cond := range sb.conditions {
			apimeta.SetStatusCondition(&latest.Status.Conditions, cond)
		}
		latest.Status.ObservedGeneration = bucket.Generation
		latest.Status.Phase = derivePhase(latest.Status.Conditions, osbStageOrder)

		if observed.endpoint != "" {
			latest.Status.Endpoint = observed.endpoint
		}
		if observed.bucketName != "" {
			latest.Status.BucketName = observed.bucketName
		}

		if reflect.DeepEqual(before, latest.Status) {
			return nil
		}
		return r.Client.Status().Update(ctx, latest)
	})
}
