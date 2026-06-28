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

// Package cephrgw implements the backend.Driver for the Heavy cluster profile:
// a Ceph RADOS Gateway (S3) provisioned on top of an existing sds-elastic
// cluster, referenced by spec.elasticClusterRef.
//
// It creates a Rook CephObjectStore in the sds-elastic namespace
// (d8-sds-elastic); Rook attaches it to the CephCluster running there and
// deploys the RGW. sds-elastic vendors Rook under the renamed API group
// internal.sdselastic.deckhouse.io, which is what this driver addresses.
//
// Scope of the current milestone: EnsureCluster (the CephObjectStore + endpoint
// reporting), gated on the referenced ElasticCluster being Ready. Bucket/user
// provisioning (CephObjectStoreUser) is a follow-up.
package cephrgw

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
	"github.com/deckhouse/sds-object/images/controller/internal/backend"
	"github.com/deckhouse/sds-object/images/controller/pkg/logger"
)

// Driver reconciles Heavy (Ceph RGW) clusters.
type Driver struct {
	client        client.Client
	apiReader     client.Reader
	log           *logger.Logger
	clusterDomain string
}

var _ backend.Driver = (*Driver)(nil)

// New builds a Ceph RGW Driver. Rook CR reads go through the non-cached
// apiReader so a missing Rook CRD (sds-elastic not installed) does not make the
// manager cache watch an absent type.
func New(c client.Client, apiReader client.Reader, log *logger.Logger, clusterDomain string) *Driver {
	return &Driver{client: c, apiReader: apiReader, log: log, clusterDomain: clusterDomain}
}

func (d *Driver) Type() v1alpha1.BackendType { return v1alpha1.BackendCephRGW }

// isNoMatch reports whether err is a "no matches for kind" RESTMapper error,
// which means the Rook CRD is absent (sds-elastic not installed).
func isNoMatch(err error) bool { return apimeta.IsNoMatchError(err) }

// EnsureCluster gates on the referenced ElasticCluster, then ensures the
// CephObjectStore and reports readiness from its status.
func (d *Driver) EnsureCluster(ctx context.Context, cluster *v1alpha1.ObjectStorageCluster) (backend.ClusterState, error) {
	state := backend.ClusterState{
		Backend: v1alpha1.BackendStatus{Type: v1alpha1.BackendCephRGW},
	}

	ref := cluster.Spec.ElasticClusterRef
	if ref == "" {
		state.Message = "spec.elasticClusterRef is required for type Heavy"
		return state, nil
	}

	// Gate on the referenced ElasticCluster being Ready.
	ec := newUnstructured(elasticClusterGVK)
	if err := d.apiReader.Get(ctx, client.ObjectKey{Name: ref}, ec); err != nil {
		if apierrors.IsNotFound(err) {
			state.Message = fmt.Sprintf("ElasticCluster %q not found", ref)
			return state, nil
		}
		if apimeta.IsNoMatchError(err) {
			state.Message = "ElasticCluster CRD not found; is the sds-elastic module installed?"
			return state, nil
		}
		return state, fmt.Errorf("get ElasticCluster %q: %w", ref, err)
	}
	if phase, _, _ := unstructured.NestedString(ec.Object, "status", "phase"); phase != v1alpha1.PhaseReady {
		state.Message = fmt.Sprintf("ElasticCluster %q is not Ready (phase=%q)", ref, phase)
		return state, nil
	}
	if v, _, _ := unstructured.NestedString(ec.Object, "status", "cephVersion", "running"); v != "" {
		state.Backend.Version = v
	}

	// Ensure the CephObjectStore.
	if err := d.ensureObjectStore(ctx, cluster); err != nil {
		if apimeta.IsNoMatchError(err) {
			state.Message = "CephObjectStore CRD not found; is the sds-elastic module installed?"
			return state, nil
		}
		return state, fmt.Errorf("ensure CephObjectStore: %w", err)
	}

	// Read back its status for readiness + endpoint.
	store := newUnstructured(cephObjectStoreGVK)
	ns, name := objectStoreKey(cluster)
	if err := d.apiReader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, store); err != nil {
		return state, fmt.Errorf("get CephObjectStore: %w", err)
	}
	phase, _, _ := unstructured.NestedString(store.Object, "status", "phase")
	if phase != "Ready" {
		state.Message = fmt.Sprintf("waiting for CephObjectStore to be Ready (phase=%q)", phase)
		return state, nil
	}

	state.Ready = true
	state.Message = "Ceph RGW is ready"
	state.Endpoint = v1alpha1.EndpointStatus{Internal: rgwEndpoint(cluster, d.clusterDomain), Region: s3Region}
	return state, nil
}

// DeleteCluster removes the CephObjectStore (idempotent).
func (d *Driver) DeleteCluster(ctx context.Context, cluster *v1alpha1.ObjectStorageCluster) error {
	store := newUnstructured(cephObjectStoreGVK)
	ns, name := objectStoreKey(cluster)
	store.SetNamespace(ns)
	store.SetName(name)
	if err := d.client.Delete(ctx, store); err != nil {
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			return nil
		}
		return err
	}
	return nil
}

// EnsureBucket and DeleteBucket are implemented in buckets.go.

// ensureObjectStore creates or updates the CephObjectStore. Reads go through
// the non-cached apiReader; writes go straight to the API server.
func (d *Driver) ensureObjectStore(ctx context.Context, cluster *v1alpha1.ObjectStorageCluster) error {
	desired := buildCephObjectStore(cluster)
	if err := controllerutil.SetControllerReference(cluster, desired, d.client.Scheme()); err != nil {
		return err
	}

	existing := newUnstructured(cephObjectStoreGVK)
	ns, name := objectStoreKey(cluster)
	err := d.apiReader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, existing)
	if apierrors.IsNotFound(err) {
		return d.client.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	existing.Object["spec"] = desired.Object["spec"]
	existing.SetLabels(desired.GetLabels())
	existing.SetOwnerReferences(desired.GetOwnerReferences())
	return d.client.Update(ctx, existing)
}
