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

// Package garage implements the backend.Driver for the Garage object storage
// backend (the System and Lightweight cluster profiles).
//
// It reconciles the Garage data plane — the ServiceAccount, RPC/admin secret,
// garage.toml ConfigMap, the workload (a StatefulSet: hostPath with a fixed
// replica count on control-plane nodes for System, PVC-backed for Lightweight)
// and the S3/RPC Services — then connects the RPC peers and assigns the cluster
// layout via the Garage admin API
// (see mesh.go), and provisions buckets/access keys (buckets.go, access.go).
package garage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
	"github.com/deckhouse/sds-object/images/controller/internal/backend"
	"github.com/deckhouse/sds-object/images/controller/pkg/logger"
)

// garageVersion is the Garage release this driver targets. Keep it in sync with
// the upstream tag pinned in images/garage/werf.inc.yaml.
const garageVersion = "v1.0.1"

// Keys in the per-cluster Garage secret.
const (
	secretKeyRPC   = "rpc-secret"
	secretKeyAdmin = "admin-token"
)

// Driver reconciles Garage clusters. It is constructed once at startup with the
// manager's client; a non-cached reader (for listing pods without caching every
// pod in the cluster); the module namespace; and the Garage image reference.
type Driver struct {
	client        client.Client
	apiReader     client.Reader
	restConfig    *rest.Config
	log           *logger.Logger
	namespace     string
	image         string
	clusterDomain string
}

var _ backend.Driver = (*Driver)(nil)

// New builds a Garage Driver.
func New(c client.Client, apiReader client.Reader, restConfig *rest.Config, log *logger.Logger, namespace, image, clusterDomain string) *Driver {
	return &Driver{
		client:        c,
		apiReader:     apiReader,
		restConfig:    restConfig,
		log:           log,
		namespace:     namespace,
		image:         image,
		clusterDomain: clusterDomain,
	}
}

func (d *Driver) Type() v1alpha1.BackendType { return v1alpha1.BackendGarage }

// EnsureCluster reconciles the Garage data plane and reports its state.
func (d *Driver) EnsureCluster(ctx context.Context, cluster *v1alpha1.ObjectStore) (backend.ClusterState, error) {
	state := backend.ClusterState{
		Backend: v1alpha1.BackendStatus{Type: v1alpha1.BackendGarage, Version: garageVersion},
	}

	if d.image == "" {
		state.Message = "GARAGE_IMAGE is not configured on the controller"
		return state, nil
	}

	if err := d.ensureSecret(ctx, cluster); err != nil {
		return state, fmt.Errorf("ensure secret: %w", err)
	}
	rf, err := d.pinnedReplicationFactor(ctx, cluster)
	if err != nil {
		return state, fmt.Errorf("compute replication factor: %w", err)
	}
	cfgHash := configHash(renderConfig(rf))
	if err := d.apply(ctx, cluster, buildConfigMap(cluster, d.namespace, rf)); err != nil {
		return state, fmt.Errorf("ensure configmap: %w", err)
	}
	if err := d.apply(ctx, cluster, buildServiceAccount(cluster, d.namespace)); err != nil {
		return state, fmt.Errorf("ensure serviceaccount: %w", err)
	}
	if err := d.apply(ctx, cluster, buildS3Service(cluster, d.namespace)); err != nil {
		return state, fmt.Errorf("ensure s3 service: %w", err)
	}
	if err := d.apply(ctx, cluster, buildRPCService(cluster, d.namespace)); err != nil {
		return state, fmt.Errorf("ensure rpc service: %w", err)
	}

	state.Endpoint = v1alpha1.EndpointStatus{Internal: s3Endpoint(cluster, d.namespace, d.clusterDomain), Region: "garage"}

	// System is backed by node-sticky local PVs the controller provisions itself;
	// the pool must exist before the StatefulSet's PVCs try to bind.
	if cluster.Spec.Type == v1alpha1.ClusterTypeSystem {
		if err := d.ensureSystemLocalPVs(ctx, cluster); err != nil {
			return state, fmt.Errorf("ensure system local PVs: %w", err)
		}
		// Converge replica placement onto the target topology as the master count
		// changes (spread across 3 masters, consolidate onto 1). One health-gated
		// PVC recycle per reconcile; when it acts, report progress and requeue so
		// the move settles before the next one.
		acted, msg, err := d.reconcileSystemPlacement(ctx, cluster)
		if err != nil {
			return state, fmt.Errorf("reconcile system placement: %w", err)
		}
		if acted {
			state.Message = msg
			return state, nil
		}
	}

	workloadReady, workloadMsg, err := d.ensureWorkload(ctx, cluster, cfgHash)
	if err != nil {
		return state, err
	}
	if !workloadReady {
		state.Message = workloadMsg
		return state, nil
	}

	// Workloads are up: connect the RPC peers and assign the cluster layout.
	mesh, err := d.ensureMeshAndLayout(ctx, cluster)
	if err != nil {
		return state, err
	}
	if mesh.total != nil {
		state.Capacity = &v1alpha1.ObjectCapacityStatus{Total: *mesh.total}
	}
	state.Ready = mesh.ready
	state.Message = mesh.msg
	return state, nil
}

// pinnedReplicationFactor returns the replication_factor to bake into
// garage.toml. Garage cannot change replication_factor on a live cluster, so it
// is decided ONCE at cluster init and then pinned: the value is read back from
// the running garage.toml on every subsequent reconcile and never recomputed
// from the current node count. This keeps the factor stable across control-plane
// node-count changes (e.g. masters going 3->1->3) — every node keeps the same
// factor, no data-losing rf change is ever attempted, and the cluster merely
// degrades to read-only (data intact) if the live node count drops below it.
func (d *Driver) pinnedReplicationFactor(ctx context.Context, cluster *v1alpha1.ObjectStore) (int32, error) {
	existing := &corev1.ConfigMap{}
	err := d.client.Get(ctx, client.ObjectKey{Namespace: d.namespace, Name: configName(cluster)}, existing)
	switch {
	case err == nil:
		if rf := replicationFactorFromConfigMap(existing); rf > 0 {
			return rf, nil // already initialized: keep the pinned factor
		}
	case !apierrors.IsNotFound(err):
		return 0, err
	}
	return initialReplicationFactor(cluster), nil
}

// initialReplicationFactor computes the factor to pin at first init: the
// redundancy intent (for System always Standard=3, since redundancy is not
// settable there) clamped to the desired replica count. For System the count is
// fixed (systemReplicas), so the factor is independent of the master count.
func initialReplicationFactor(cluster *v1alpha1.ObjectStore) int32 {
	return clampRF(replicationFactor(cluster), desiredReplicas(cluster))
}

// DeleteCluster relies on owner-reference GC for the workloads and Services.
// The StatefulSet's PVCs (Lightweight) are NOT garbage-collected by Kubernetes,
// so they persist by default (Retain). Only when the cluster reclaim policy is
// Delete are they removed; System (hostPath) data is left to node cleanup.
func (d *Driver) DeleteCluster(ctx context.Context, cluster *v1alpha1.ObjectStore) error {
	if cluster.Spec.ReclaimPolicy != v1alpha1.ClusterReclaimDelete {
		return nil
	}
	return backend.DeleteClusterPVCs(ctx, d.client, d.namespace, commonLabels(cluster))
}

// EnsureBucket and DeleteBucket are implemented in buckets.go.

// ensureSecret creates the per-cluster Garage secret (rpc secret + admin token)
// on first reconcile and never overwrites existing values.
func (d *Driver) ensureSecret(ctx context.Context, cluster *v1alpha1.ObjectStore) error {
	key := client.ObjectKey{Namespace: d.namespace, Name: secretName(cluster)}
	existing := &corev1.Secret{}
	err := d.client.Get(ctx, key, existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	rpcSecret, err := randomHex(32)
	if err != nil {
		return err
	}
	adminToken, err := randomHex(32)
	if err != nil {
		return err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName(cluster),
			Namespace: d.namespace,
			Labels:    commonLabels(cluster),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			secretKeyRPC:   []byte(rpcSecret),
			secretKeyAdmin: []byte(adminToken),
		},
	}
	if err := controllerutil.SetControllerReference(cluster, secret, d.client.Scheme()); err != nil {
		return err
	}
	return d.client.Create(ctx, secret)
}

// ensureWorkload creates/updates the profile StatefulSet (hostPath, fixed
// replicas for System; PVC-backed for Lightweight/Full) and reports whether its
// pods are ready.
func (d *Driver) ensureWorkload(ctx context.Context, cluster *v1alpha1.ObjectStore, cfgHash string) (bool, string, error) {
	sts := buildStatefulSet(cluster, d.namespace, d.image, cfgHash)
	if cluster.Spec.Type == v1alpha1.ClusterTypeSystem {
		sts = buildSystemStatefulSet(cluster, d.namespace, d.image, cfgHash)
	}
	if err := d.apply(ctx, cluster, sts); err != nil {
		return false, "", fmt.Errorf("ensure statefulset: %w", err)
	}
	desired := int32(0)
	if sts.Spec.Replicas != nil {
		desired = *sts.Spec.Replicas
	}
	if sts.Status.ReadyReplicas < desired {
		return false, fmt.Sprintf("Garage StatefulSet rolling out (%d/%d pods ready)", sts.Status.ReadyReplicas, desired), nil
	}
	return true, "", nil
}

// apply creates or updates obj, setting the cluster as its controller owner.
// On update it overwrites the spec-bearing fields with the desired object.
func (d *Driver) apply(ctx context.Context, cluster *v1alpha1.ObjectStore, obj client.Object) error {
	desired := obj.DeepCopyObject().(client.Object)
	_, err := controllerutil.CreateOrUpdate(ctx, d.client, obj, func() error {
		mergeDesired(obj, desired)
		return controllerutil.SetControllerReference(cluster, obj, d.client.Scheme())
	})
	return err
}

// mergeDesired copies the desired spec/data onto the live object fetched by
// CreateOrUpdate, preserving server-managed metadata. Immutable fields
// (StatefulSet selector/volumeClaimTemplates, headless ClusterIP) are not
// changed once set.
func mergeDesired(live, desired client.Object) {
	switch l := live.(type) {
	case *corev1.ConfigMap:
		l.Data = desired.(*corev1.ConfigMap).Data
		l.Labels = desired.GetLabels()
	case *corev1.ServiceAccount:
		l.AutomountServiceAccountToken = desired.(*corev1.ServiceAccount).AutomountServiceAccountToken
		l.Labels = desired.GetLabels()
	case *corev1.Service:
		d := desired.(*corev1.Service)
		l.Labels = d.Labels
		l.Spec.Selector = d.Spec.Selector
		l.Spec.Ports = d.Spec.Ports
		l.Spec.PublishNotReadyAddresses = d.Spec.PublishNotReadyAddresses
	case *appsv1.StatefulSet:
		d := desired.(*appsv1.StatefulSet)
		l.Labels = d.Labels
		l.Spec.Replicas = d.Spec.Replicas
		l.Spec.Template = d.Spec.Template
	}
}

// randomHex returns n random bytes hex-encoded.
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
