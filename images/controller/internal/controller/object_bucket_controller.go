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

// ObjectBucketReconciler drives every ObjectBucket through a short FSM:
//
//		BucketReady -> CredentialsReady -> Ready
//
//	  - BucketReady gates on the referenced ObjectStorageCluster (must exist and
//	    be Ready) and then on the backend.Driver creating the bucket.
//	  - CredentialsReady (re)writes the S3 credentials Secret in the bucket's
//	    namespace with the backend-issued access key.
type ObjectBucketReconciler struct {
	Client   client.Client
	Scheme   *runtime.Scheme
	Log      *logger.Logger
	Cfg      *config.Options
	Registry *backend.Registry
}

// obStageOrder lists the FSM stage condition types in execution order.
var obStageOrder = []string{
	v1alpha1.OBConditionBucketReady,
	v1alpha1.OBConditionCredentialsReady,
}

// bucketObserved accumulates the status fields the reconciler writes back onto
// the ObjectBucket (separate from backend.BucketState, which is the driver's
// result type).
type bucketObserved struct {
	bucketName string
	endpoint   string
	secretName string
}

// AddObjectBucketReconcilerToManager wires the OB reconciler into the manager.
// Besides watching ObjectBucket itself, it watches ObjectStorageCluster so that
// a cluster becoming Ready re-reconciles every bucket referencing it.
func AddObjectBucketReconcilerToManager(mgr manager.Manager, cfg *config.Options, log *logger.Logger, reg *backend.Registry) error {
	r := &ObjectBucketReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Log:      log,
		Cfg:      cfg,
		Registry: reg,
	}

	// Cluster status updates do not bump generation, so fire only on real
	// Ready transitions of the referenced cluster.
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
		Named("object-bucket").
		For(&v1alpha1.ObjectBucket{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
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
// ObjectBucket (in any namespace) that references it via spec.clusterRef.
func (r *ObjectBucketReconciler) enqueueBucketsByCluster(ctx context.Context, o client.Object) []reconcile.Request {
	list := &v1alpha1.ObjectBucketList{}
	if err := r.Client.List(ctx, list); err != nil {
		r.Log.Error(err, "[enqueueBucketsByCluster] list failed")
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].Spec.ClusterRef != o.GetName() {
			continue
		}
		out = append(out, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: list.Items[i].Namespace,
				Name:      list.Items[i].Name,
			},
		})
	}
	return out
}

func (r *ObjectBucketReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.Log.Info(fmt.Sprintf("[Reconcile] start for ObjectBucket %s", req.NamespacedName))

	bucket := &v1alpha1.ObjectBucket{}
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

func (r *ObjectBucketReconciler) reconcileDelete(ctx context.Context, bucket *v1alpha1.ObjectBucket) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(bucket, Finalizer) {
		return ctrl.Result{}, nil
	}

	cluster, err := r.getCluster(ctx, bucket.Spec.ClusterRef)
	switch {
	case err != nil:
		// Cluster gone or unresolved: nothing to clean up in a backend we
		// cannot reach. Drop the finalizer to avoid a stuck bucket.
		r.Log.Warning(fmt.Sprintf("[reconcileDelete] %s/%s: cluster %q unavailable (%v); removing finalizer",
			bucket.Namespace, bucket.Name, bucket.Spec.ClusterRef, err))
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

func (r *ObjectBucketReconciler) reconcileNormal(ctx context.Context, bucket *v1alpha1.ObjectBucket) (ctrl.Result, error) {
	status := newStatusBuilder(bucket.Generation)
	observed := &bucketObserved{}

	// Stage 1: BucketReady — gate on the cluster, then create the bucket.
	cluster, err := r.getCluster(ctx, bucket.Spec.ClusterRef)
	if err != nil {
		status.setCondition(v1alpha1.OBConditionBucketReady, metav1.ConditionFalse, "WaitingForCluster",
			fmt.Sprintf("ObjectStorageCluster %q is not available: %v", bucket.Spec.ClusterRef, err))
		gateAfter(status, obStageOrder, v1alpha1.OBConditionBucketReady)
		return r.finish(ctx, bucket, status, observed, nil)
	}
	if cluster.Status != nil && cluster.Status.Endpoint != nil {
		observed.endpoint = cluster.Status.Endpoint.Internal
	}
	if clusterReadyState(cluster) != string(metav1.ConditionTrue) {
		status.setCondition(v1alpha1.OBConditionBucketReady, metav1.ConditionFalse, "WaitingForCluster",
			fmt.Sprintf("ObjectStorageCluster %q is not Ready", bucket.Spec.ClusterRef))
		gateAfter(status, obStageOrder, v1alpha1.OBConditionBucketReady)
		return r.finish(ctx, bucket, status, observed, nil)
	}

	driver, err := r.Registry.For(cluster)
	if err != nil {
		status.setCondition(v1alpha1.OBConditionBucketReady, metav1.ConditionFalse, reasonError, err.Error())
		gateAfter(status, obStageOrder, v1alpha1.OBConditionBucketReady)
		return r.finish(ctx, bucket, status, observed, err)
	}

	state, err := driver.EnsureBucket(ctx, cluster, bucket)
	observed.bucketName = state.BucketName
	if !advance(status, obStageOrder, v1alpha1.OBConditionBucketReady, state.Ready, state.Message, err) {
		return r.finish(ctx, bucket, status, observed, err)
	}

	// Stage 2: CredentialsReady — (re)write the credentials Secret.
	secretName, err := r.ensureCredentialsSecret(ctx, cluster, bucket, &state)
	if !advance(status, obStageOrder, v1alpha1.OBConditionCredentialsReady, err == nil,
		"credentials Secret written", err) {
		return r.finish(ctx, bucket, status, observed, err)
	}
	observed.secretName = secretName

	status.setCondition(v1alpha1.OBConditionReady, metav1.ConditionTrue, reasonReady, "All stages reconciled")
	return r.finish(ctx, bucket, status, observed, nil)
}

// getCluster fetches the cluster-scoped ObjectStorageCluster by name.
func (r *ObjectBucketReconciler) getCluster(ctx context.Context, name string) (*v1alpha1.ObjectStorageCluster, error) {
	if name == "" {
		return nil, fmt.Errorf("spec.clusterRef is empty")
	}
	cluster := &v1alpha1.ObjectStorageCluster{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: name}, cluster); err != nil {
		return nil, err
	}
	return cluster, nil
}

// credentialsSecretName returns the name of the Secret holding the bucket's S3
// credentials, in the bucket's namespace.
func credentialsSecretName(bucket *v1alpha1.ObjectBucket) string {
	return bucket.Name + "-s3-credentials"
}

// ensureCredentialsSecret creates or updates the credentials Secret owned by the
// bucket and returns its name.
func (r *ObjectBucketReconciler) ensureCredentialsSecret(
	ctx context.Context,
	cluster *v1alpha1.ObjectStorageCluster,
	bucket *v1alpha1.ObjectBucket,
	state *backend.BucketState,
) (string, error) {
	endpoint := ""
	region := ""
	if cluster.Status != nil && cluster.Status.Endpoint != nil {
		endpoint = cluster.Status.Endpoint.Internal
		region = cluster.Status.Endpoint.Region
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      credentialsSecretName(bucket),
			Namespace: bucket.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.Labels == nil {
			secret.Labels = map[string]string{}
		}
		secret.Labels["storage.deckhouse.io/object-bucket"] = bucket.Name
		secret.Type = corev1.SecretTypeOpaque
		secret.StringData = map[string]string{
			v1alpha1.SecretKeyS3Endpoint:     endpoint,
			v1alpha1.SecretKeyS3Region:       region,
			v1alpha1.SecretKeyS3Bucket:       state.BucketName,
			v1alpha1.SecretKeyAccessKeyID:    state.AccessKeyID,
			v1alpha1.SecretKeySecretAccessID: state.SecretAccessKey,
		}
		return controllerutil.SetControllerReference(bucket, secret, r.Scheme)
	})
	if err != nil {
		return "", err
	}
	return secret.Name, nil
}

func (r *ObjectBucketReconciler) finish(
	ctx context.Context,
	bucket *v1alpha1.ObjectBucket,
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

func (r *ObjectBucketReconciler) updateStatus(
	ctx context.Context,
	bucket *v1alpha1.ObjectBucket,
	sb *statusBuilder,
	observed *bucketObserved,
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &v1alpha1.ObjectBucket{}
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: bucket.Namespace, Name: bucket.Name}, latest); err != nil {
			return err
		}
		if latest.Status == nil {
			latest.Status = &v1alpha1.ObjectBucketStatus{}
		}
		before := latest.Status.DeepCopy()

		for _, cond := range sb.conditions {
			apimeta.SetStatusCondition(&latest.Status.Conditions, cond)
		}
		latest.Status.ObservedGeneration = bucket.Generation
		latest.Status.Phase = derivePhase(latest.Status.Conditions, obStageOrder)

		if observed.endpoint != "" {
			latest.Status.Endpoint = observed.endpoint
		}
		if observed.bucketName != "" {
			latest.Status.BucketName = observed.bucketName
		}
		if observed.secretName != "" {
			latest.Status.SecretRef = &v1alpha1.LocalSecretReference{Name: observed.secretName}
		}

		if reflect.DeepEqual(before, latest.Status) {
			return nil
		}
		return r.Client.Status().Update(ctx, latest)
	})
}
