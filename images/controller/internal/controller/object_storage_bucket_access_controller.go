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

// ObjectStorageBucketAccessReconciler drives every ObjectStorageBucketAccess
// through a short FSM:
//
//		AccessGranted -> CredentialsReady -> Ready
//
//	  - AccessGranted gates on the referenced ObjectStorageBucket (must exist and
//	    be Ready), an ObjectStorageBucketPolicy allowing this namespace
//	    (deny-by-default), and the backend.Driver issuing an access key.
//	  - CredentialsReady (re)writes the S3 credentials Secret in the access's
//	    namespace. A change to the storage.deckhouse.io/rotate annotation triggers
//	    a fresh key pair.
type ObjectStorageBucketAccessReconciler struct {
	Client   client.Client
	Scheme   *runtime.Scheme
	Log      *logger.Logger
	Cfg      *config.Options
	Registry *backend.Registry
}

var osbaStageOrder = []string{
	v1alpha1.OSBAConditionAccessGranted,
	v1alpha1.OSBAConditionCredentialsReady,
}

// accessObserved accumulates the status fields written back onto the access.
type accessObserved struct {
	bucketName       string
	endpoint         string
	accessKeyID      string
	secretName       string
	observedRotation string
	rotated          bool
	// revoked clears the credential status fields (the access lost its policy
	// grant and its key/Secret were removed).
	revoked bool
}

// AddObjectStorageBucketAccessReconcilerToManager wires the OSBA reconciler.
// It watches ObjectStorageBucket (a bucket becoming Ready re-reconciles its
// accesses) and ObjectStorageBucketPolicy (a policy change re-evaluates the
// accesses for its bucket).
func AddObjectStorageBucketAccessReconcilerToManager(mgr manager.Manager, cfg *config.Options, log *logger.Logger, reg *backend.Registry) error {
	r := &ObjectStorageBucketAccessReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Log:      log,
		Cfg:      cfg,
		Registry: reg,
	}

	// Reconcile on spec changes and on rotation-annotation changes.
	accessPredicate := predicate.Or(
		predicate.GenerationChangedPredicate{},
		predicate.AnnotationChangedPredicate{},
	)

	return ctrl.NewControllerManagedBy(mgr).
		Named("object-storage-bucket-access").
		For(&v1alpha1.ObjectStorageBucketAccess{}, builder.WithPredicates(accessPredicate)).
		Watches(&v1alpha1.ObjectStorageBucket{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueByBucket),
			builder.WithPredicates(predicate.Funcs{
				CreateFunc:  func(_ event.CreateEvent) bool { return true },
				DeleteFunc:  func(_ event.DeleteEvent) bool { return true },
				GenericFunc: func(_ event.GenericEvent) bool { return true },
				UpdateFunc:  func(_ event.UpdateEvent) bool { return true },
			})).
		Watches(&v1alpha1.ObjectStorageBucketPolicy{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueByPolicy)).
		WithOptions(controller.Options{MaxConcurrentReconciles: cfg.MaxConcurrentReconciles}).
		Complete(r)
}

// enqueueByBucket maps a bucket event to every access that references it.
func (r *ObjectStorageBucketAccessReconciler) enqueueByBucket(ctx context.Context, o client.Object) []reconcile.Request {
	return r.enqueueByBucketRef(ctx, o.GetName())
}

// enqueueByPolicy maps a policy event to every access for the policy's bucket.
func (r *ObjectStorageBucketAccessReconciler) enqueueByPolicy(ctx context.Context, o client.Object) []reconcile.Request {
	policy, ok := o.(*v1alpha1.ObjectStorageBucketPolicy)
	if !ok {
		return nil
	}
	return r.enqueueByBucketRef(ctx, policy.Spec.BucketRef)
}

func (r *ObjectStorageBucketAccessReconciler) enqueueByBucketRef(ctx context.Context, bucketRef string) []reconcile.Request {
	list := &v1alpha1.ObjectStorageBucketAccessList{}
	if err := r.Client.List(ctx, list); err != nil {
		r.Log.Error(err, "[enqueueByBucketRef] list failed")
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].Spec.BucketRef != bucketRef {
			continue
		}
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: list.Items[i].Namespace,
			Name:      list.Items[i].Name,
		}})
	}
	return out
}

func (r *ObjectStorageBucketAccessReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.Log.Info(fmt.Sprintf("[Reconcile] start for ObjectStorageBucketAccess %s", req.NamespacedName))

	access := &v1alpha1.ObjectStorageBucketAccess{}
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

func (r *ObjectStorageBucketAccessReconciler) reconcileDelete(ctx context.Context, access *v1alpha1.ObjectStorageBucketAccess) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(access, Finalizer) {
		return ctrl.Result{}, nil
	}

	bucket, cluster, err := r.resolve(ctx, access)
	if err != nil {
		r.Log.Warning(fmt.Sprintf("[reconcileDelete] access %s/%s: %v; removing finalizer", access.Namespace, access.Name, err))
	} else if driver, derr := r.Registry.For(cluster); derr == nil {
		if derr := driver.DeleteAccess(ctx, cluster, bucket, access); derr != nil {
			return ctrl.Result{}, derr
		}
	}

	controllerutil.RemoveFinalizer(access, Finalizer)
	if err := r.Client.Update(ctx, access); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *ObjectStorageBucketAccessReconciler) reconcileNormal(ctx context.Context, access *v1alpha1.ObjectStorageBucketAccess) (ctrl.Result, error) {
	status := newStatusBuilder(access.Generation)
	observed := &accessObserved{}

	// Resolve bucket + cluster.
	bucket := &v1alpha1.ObjectStorageBucket{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: access.Spec.BucketRef}, bucket); err != nil {
		status.setCondition(v1alpha1.OSBAConditionAccessGranted, metav1.ConditionFalse, "WaitingForBucket",
			fmt.Sprintf("ObjectStorageBucket %q is not available: %v", access.Spec.BucketRef, err))
		gateAfter(status, osbaStageOrder, v1alpha1.OSBAConditionAccessGranted)
		return r.finish(ctx, access, status, observed, nil)
	}
	if bucketReadyState(bucket) != string(metav1.ConditionTrue) {
		status.setCondition(v1alpha1.OSBAConditionAccessGranted, metav1.ConditionFalse, "WaitingForBucket",
			fmt.Sprintf("ObjectStorageBucket %q is not Ready", access.Spec.BucketRef))
		gateAfter(status, osbaStageOrder, v1alpha1.OSBAConditionAccessGranted)
		return r.finish(ctx, access, status, observed, nil)
	}
	observed.bucketName = bucket.Status.BucketName

	cluster := &v1alpha1.ObjectStorageCluster{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: bucket.Spec.ClusterRef}, cluster); err != nil {
		status.setCondition(v1alpha1.OSBAConditionAccessGranted, metav1.ConditionFalse, "WaitingForCluster",
			fmt.Sprintf("ObjectStorageCluster %q is not available: %v", bucket.Spec.ClusterRef, err))
		gateAfter(status, osbaStageOrder, v1alpha1.OSBAConditionAccessGranted)
		return r.finish(ctx, access, status, observed, nil)
	}
	if cluster.Status != nil && cluster.Status.Endpoint != nil {
		observed.endpoint = cluster.Status.Endpoint.Internal
	}

	// Deny-by-default policy check.
	allowed, reason, err := namespaceAllowedForBucket(ctx, r.Client, access.Spec.BucketRef, access.Namespace)
	if err != nil {
		status.setCondition(v1alpha1.OSBAConditionAccessGranted, metav1.ConditionFalse, reasonError, err.Error())
		gateAfter(status, osbaStageOrder, v1alpha1.OSBAConditionAccessGranted)
		return r.finish(ctx, access, status, observed, err)
	}
	if !allowed {
		// Deny-by-default is enforced continuously: if this access had been
		// granted before (its key/Secret exist) and the policy no longer allows
		// the namespace, revoke the key and drop the credentials Secret.
		if access.Status != nil && access.Status.AccessKeyID != "" {
			if driver, derr := r.Registry.For(cluster); derr == nil {
				if derr := driver.DeleteAccess(ctx, cluster, bucket, access); derr != nil {
					status.setCondition(v1alpha1.OSBAConditionAccessGranted, metav1.ConditionFalse, reasonError,
						fmt.Sprintf("revoking access after policy change: %v", derr))
					gateAfter(status, osbaStageOrder, v1alpha1.OSBAConditionAccessGranted)
					return r.finish(ctx, access, status, observed, derr)
				}
			}
			if err := r.deleteCredentialsSecret(ctx, access); err != nil {
				return r.finish(ctx, access, status, observed, err)
			}
			observed.revoked = true
		}
		status.setCondition(v1alpha1.OSBAConditionAccessGranted, metav1.ConditionFalse, "DeniedByPolicy", reason)
		gateAfter(status, osbaStageOrder, v1alpha1.OSBAConditionAccessGranted)
		return r.finish(ctx, access, status, observed, nil)
	}

	driver, err := r.Registry.For(cluster)
	if err != nil {
		status.setCondition(v1alpha1.OSBAConditionAccessGranted, metav1.ConditionFalse, reasonError, err.Error())
		gateAfter(status, osbaStageOrder, v1alpha1.OSBAConditionAccessGranted)
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
	if !advance(status, osbaStageOrder, v1alpha1.OSBAConditionAccessGranted, state.Ready, state.Message, err) {
		return r.finish(ctx, access, status, observed, err)
	}

	// Write the credentials Secret. A fresh key comes with its secret key; a
	// reused key keeps the secret key already stored in the Secret.
	secretName, err := r.ensureCredentialsSecret(ctx, cluster, bucket, access, &state, existing)
	if !advance(status, osbaStageOrder, v1alpha1.OSBAConditionCredentialsReady, err == nil, "credentials Secret written", err) {
		return r.finish(ctx, access, status, observed, err)
	}
	observed.secretName = secretName
	observed.observedRotation = rotationValue
	observed.rotated = state.SecretAccessKey != ""

	status.setCondition(v1alpha1.OSBAConditionReady, metav1.ConditionTrue, reasonReady, "All stages reconciled")
	return r.finish(ctx, access, status, observed, nil)
}

// resolve returns the bucket and cluster referenced by the access.
func (r *ObjectStorageBucketAccessReconciler) resolve(ctx context.Context, access *v1alpha1.ObjectStorageBucketAccess) (*v1alpha1.ObjectStorageBucket, *v1alpha1.ObjectStorageCluster, error) {
	bucket := &v1alpha1.ObjectStorageBucket{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: access.Spec.BucketRef}, bucket); err != nil {
		return nil, nil, fmt.Errorf("bucket %q: %w", access.Spec.BucketRef, err)
	}
	cluster := &v1alpha1.ObjectStorageCluster{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: bucket.Spec.ClusterRef}, cluster); err != nil {
		return nil, nil, fmt.Errorf("cluster %q: %w", bucket.Spec.ClusterRef, err)
	}
	return bucket, cluster, nil
}

// credentialsSecretName is spec.secretName or <access>-s3-credentials.
func credentialsSecretName(access *v1alpha1.ObjectStorageBucketAccess) string {
	if access.Spec.SecretName != "" {
		return access.Spec.SecretName
	}
	return access.Name + "-s3-credentials"
}

// deleteCredentialsSecret removes the access's credentials Secret (idempotent).
func (r *ObjectStorageBucketAccessReconciler) deleteCredentialsSecret(ctx context.Context, access *v1alpha1.ObjectStorageBucketAccess) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: credentialsSecretName(access), Namespace: access.Namespace},
	}
	if err := r.Client.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// getSecret reads the credentials Secret (nil, false when it does not exist).
func (r *ObjectStorageBucketAccessReconciler) getSecret(ctx context.Context, access *v1alpha1.ObjectStorageBucketAccess) (*corev1.Secret, bool, error) {
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: access.Namespace, Name: credentialsSecretName(access)}
	if err := r.Client.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return secret, true, nil
}

// ensureCredentialsSecret creates or updates the credentials Secret owned by the
// access and returns its name.
func (r *ObjectStorageBucketAccessReconciler) ensureCredentialsSecret(
	ctx context.Context,
	cluster *v1alpha1.ObjectStorageCluster,
	bucket *v1alpha1.ObjectStorageBucket,
	access *v1alpha1.ObjectStorageBucketAccess,
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
		secret.Labels["storage.deckhouse.io/object-storage-bucket-access"] = access.Name
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

// bucketReadyState returns the bucket's Ready condition status (or "").
func bucketReadyState(b *v1alpha1.ObjectStorageBucket) string {
	if b == nil || b.Status == nil {
		return ""
	}
	cond := apimeta.FindStatusCondition(b.Status.Conditions, v1alpha1.OSBConditionReady)
	if cond == nil {
		return ""
	}
	return string(cond.Status)
}

func (r *ObjectStorageBucketAccessReconciler) finish(
	ctx context.Context,
	access *v1alpha1.ObjectStorageBucketAccess,
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
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: r.Cfg.RequeueInterval}, nil
}

func (r *ObjectStorageBucketAccessReconciler) updateStatus(
	ctx context.Context,
	access *v1alpha1.ObjectStorageBucketAccess,
	sb *statusBuilder,
	observed *accessObserved,
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &v1alpha1.ObjectStorageBucketAccess{}
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: access.Namespace, Name: access.Name}, latest); err != nil {
			return err
		}
		if latest.Status == nil {
			latest.Status = &v1alpha1.ObjectStorageBucketAccessStatus{}
		}
		before := latest.Status.DeepCopy()

		for _, cond := range sb.conditions {
			apimeta.SetStatusCondition(&latest.Status.Conditions, cond)
		}
		latest.Status.ObservedGeneration = access.Generation
		latest.Status.Phase = derivePhase(latest.Status.Conditions, osbaStageOrder)

		if observed.endpoint != "" {
			latest.Status.Endpoint = observed.endpoint
		}
		if observed.bucketName != "" {
			latest.Status.BucketName = observed.bucketName
		}
		if observed.revoked {
			// Access lost its policy grant: clear the credential status.
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
