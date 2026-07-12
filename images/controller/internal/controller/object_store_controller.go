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

// ObjectStoreReconciler drives every ObjectStore through a
// short FSM:
//
//		BackendReady -> EndpointReady -> Ready
//
//	  - BackendReady covers the data-plane: the backend.Driver brings up the
//	    workloads (Garage StatefulSet, SeaweedFS, Ceph RGW) and
//	    reports readiness.
//	  - EndpointReady is satisfied once the backend reports an S3 endpoint.
//
// The backend-specific work lives in backend.Driver; this reconciler owns the
// Kubernetes-facing status machine and the finalizer.
type ObjectStoreReconciler struct {
	Client   client.Client
	Log      *logger.Logger
	Cfg      *config.Options
	Registry *backend.Registry
}

// oscStageOrder lists the FSM stage condition types in execution order.
var oscStageOrder = []string{
	v1alpha1.ObjectStoreConditionBackendReady,
	v1alpha1.ObjectStoreConditionEndpointReady,
}

// AddObjectStoreReconcilerToManager wires the OSC reconciler into the
// manager. It reconciles on spec (generation) changes only; backend-driven
// status refresh happens on the requeue interval.
func AddObjectStoreReconcilerToManager(mgr manager.Manager, cfg *config.Options, log *logger.Logger, reg *backend.Registry) error {
	r := &ObjectStoreReconciler{
		Client:   mgr.GetClient(),
		Log:      log,
		Cfg:      cfg,
		Registry: reg,
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("object-store").
		For(&v1alpha1.ObjectStore{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// Control-plane Node add/remove changes the master count, which drives
		// the System profile's replica placement (spread across masters /
		// consolidate onto one) and its local-PV pool. GenerationChangedPredicate
		// on the ObjectStore does not see that, so watch control-plane Nodes and
		// re-reconcile the System ObjectStore(s) on their create/delete.
		Watches(&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(enqueueSystemObjectStores(mgr.GetClient())),
			builder.WithPredicates(controlPlaneNodePredicate())).
		WithOptions(controller.Options{MaxConcurrentReconciles: cfg.MaxConcurrentReconciles}).
		Complete(r)
}

// controlPlaneNodePredicate matches only control-plane Nodes, so the Node watch
// ignores worker churn.
func controlPlaneNodePredicate() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(o client.Object) bool {
		_, ok := o.GetLabels()["node-role.kubernetes.io/control-plane"]
		return ok
	})
}

// enqueueSystemObjectStores maps a control-plane Node event to a reconcile of
// every System-type ObjectStore (only System depends on the master count).
func enqueueSystemObjectStores(c client.Client) handler.MapFunc {
	return func(ctx context.Context, _ client.Object) []reconcile.Request {
		list := &v1alpha1.ObjectStoreList{}
		if err := c.List(ctx, list); err != nil {
			return nil
		}
		var reqs []reconcile.Request
		for i := range list.Items {
			if list.Items[i].Spec.Type == v1alpha1.ClusterTypeSystem {
				reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKey{Name: list.Items[i].Name}})
			}
		}
		return reqs
	}
}

func (r *ObjectStoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.Log.Info(fmt.Sprintf("[Reconcile] start for ObjectStore %q", req.Name))

	cluster := &v1alpha1.ObjectStore{}
	if err := r.Client.Get(ctx, req.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if cluster.DeletionTimestamp != nil {
		return r.reconcileDelete(ctx, cluster)
	}

	if !controllerutil.ContainsFinalizer(cluster, Finalizer) {
		controllerutil.AddFinalizer(cluster, Finalizer)
		if err := r.Client.Update(ctx, cluster); err != nil {
			return ctrl.Result{}, err
		}
	}

	return r.reconcileNormal(ctx, cluster)
}

func (r *ObjectStoreReconciler) reconcileDelete(ctx context.Context, cluster *v1alpha1.ObjectStore) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(cluster, Finalizer) {
		return ctrl.Result{}, nil
	}

	driver, err := r.Registry.For(cluster)
	if err != nil {
		// Unknown backend on a CR being deleted: nothing to tear down,
		// drop the finalizer so the CR is not stuck forever.
		r.Log.Warning(fmt.Sprintf("[reconcileDelete] %q: %v; removing finalizer", cluster.Name, err))
	} else if err := driver.DeleteCluster(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}

	controllerutil.RemoveFinalizer(cluster, Finalizer)
	if err := r.Client.Update(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *ObjectStoreReconciler) reconcileNormal(ctx context.Context, cluster *v1alpha1.ObjectStore) (ctrl.Result, error) {
	status := newStatusBuilder(cluster.Generation)

	driver, err := r.Registry.For(cluster)
	if err != nil {
		// No driver for this profile is a terminal configuration error.
		status.setCondition(v1alpha1.ObjectStoreConditionBackendReady, metav1.ConditionFalse, reasonError, err.Error())
		gateAfter(status, oscStageOrder, v1alpha1.ObjectStoreConditionBackendReady)
		return r.finish(ctx, cluster, status, nil, err)
	}

	state, err := driver.EnsureCluster(ctx, cluster)
	if !advance(status, oscStageOrder, v1alpha1.ObjectStoreConditionBackendReady, state.Ready, state.Message, err) {
		return r.finish(ctx, cluster, status, &state, err)
	}

	endpointReady := state.Endpoint.Internal != ""
	endpointMsg := "S3 endpoint is available"
	if !endpointReady {
		endpointMsg = "waiting for the backend to publish an S3 endpoint"
	}
	if !advance(status, oscStageOrder, v1alpha1.ObjectStoreConditionEndpointReady, endpointReady, endpointMsg, nil) {
		return r.finish(ctx, cluster, status, &state, nil)
	}

	status.setCondition(v1alpha1.ObjectStoreConditionReady, metav1.ConditionTrue, reasonReady, "All stages reconciled")
	return r.finish(ctx, cluster, status, &state, nil)
}

// finish writes the observed backend state and the accumulated conditions onto
// the CR status, then decides whether to requeue.
func (r *ObjectStoreReconciler) finish(
	ctx context.Context,
	cluster *v1alpha1.ObjectStore,
	status *statusBuilder,
	state *backend.ClusterState,
	reconcileErr error,
) (ctrl.Result, error) {
	if err := r.updateStatus(ctx, cluster, status, state); err != nil {
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

func (r *ObjectStoreReconciler) updateStatus(
	ctx context.Context,
	cluster *v1alpha1.ObjectStore,
	sb *statusBuilder,
	state *backend.ClusterState,
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &v1alpha1.ObjectStore{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: cluster.Name}, latest); err != nil {
			return err
		}
		if latest.Status == nil {
			latest.Status = &v1alpha1.ObjectStoreStatus{}
		}
		before := latest.Status.DeepCopy()

		for _, cond := range sb.conditions {
			apimeta.SetStatusCondition(&latest.Status.Conditions, cond)
		}
		latest.Status.ObservedGeneration = cluster.Generation
		latest.Status.Phase = derivePhase(latest.Status.Conditions, oscStageOrder)

		if state != nil {
			b := state.Backend
			latest.Status.Backend = &b
			e := state.Endpoint
			latest.Status.Endpoint = &e
			if state.Capacity != nil {
				latest.Status.Capacity = state.Capacity
			}
		}

		if reflect.DeepEqual(before, latest.Status) {
			return nil
		}
		return r.Client.Status().Update(ctx, latest)
	})
}
