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

// Package seaweedfs implements the backend.Driver for the SeaweedFS object
// storage backend (the Full cluster profile).
//
// Scope of the current milestone (MVP): it reconciles a single-replica
// all-in-one `weed server -s3` data plane (master + volume + filer + S3 gateway
// in one process) backed by a PVC, plus its Service, and reports readiness and
// the S3 endpoint. A distributed master/volume/filer topology with redundancy,
// and bucket/key provisioning, are follow-ups.
package seaweedfs

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
	"github.com/deckhouse/sds-object/images/controller/internal/backend"
	"github.com/deckhouse/sds-object/images/controller/pkg/logger"
)

// seaweedfsVersion is the SeaweedFS release this driver targets. Keep it in
// sync with the upstream tag pinned in images/seaweedfs/werf.inc.yaml.
const seaweedfsVersion = "3.71"

// Driver reconciles SeaweedFS clusters.
type Driver struct {
	client        client.Client
	log           *logger.Logger
	namespace     string
	image         string
	clusterDomain string
}

var _ backend.Driver = (*Driver)(nil)

// New builds a SeaweedFS Driver.
func New(c client.Client, log *logger.Logger, namespace, image, clusterDomain string) *Driver {
	return &Driver{client: c, log: log, namespace: namespace, image: image, clusterDomain: clusterDomain}
}

func (d *Driver) Type() v1alpha1.BackendType { return v1alpha1.BackendSeaweedFS }

// EnsureCluster reconciles the SeaweedFS data plane and reports its state.
func (d *Driver) EnsureCluster(ctx context.Context, cluster *v1alpha1.ObjectStorageCluster) (backend.ClusterState, error) {
	state := backend.ClusterState{
		Backend: v1alpha1.BackendStatus{Type: v1alpha1.BackendSeaweedFS, Version: seaweedfsVersion},
	}

	if d.image == "" {
		state.Message = "SEAWEEDFS_IMAGE is not configured on the controller"
		return state, nil
	}

	if err := d.apply(ctx, cluster, buildService(cluster, d.namespace)); err != nil {
		return state, fmt.Errorf("ensure service: %w", err)
	}

	sts := buildStatefulSet(cluster, d.namespace, d.image)
	if err := d.apply(ctx, cluster, sts); err != nil {
		return state, fmt.Errorf("ensure statefulset: %w", err)
	}

	desired := int32(0)
	if sts.Spec.Replicas != nil {
		desired = *sts.Spec.Replicas
	}
	if sts.Status.ReadyReplicas < desired {
		state.Message = fmt.Sprintf("SeaweedFS rolling out (%d/%d pods ready)", sts.Status.ReadyReplicas, desired)
		return state, nil
	}

	state.Ready = true
	state.Message = "SeaweedFS S3 gateway is ready"
	state.Endpoint = v1alpha1.EndpointStatus{Internal: s3Endpoint(cluster, d.namespace, d.clusterDomain), Region: s3Region}
	return state, nil
}

// DeleteCluster is a no-op: the workload and service carry an owner reference to
// the cluster and are garbage-collected when the CR is removed.
func (d *Driver) DeleteCluster(_ context.Context, _ *v1alpha1.ObjectStorageCluster) error {
	return nil
}

// EnsureBucket is not implemented in this milestone (bucket/key provisioning is
// a follow-up).
func (d *Driver) EnsureBucket(_ context.Context, _ *v1alpha1.ObjectStorageCluster, _ *v1alpha1.ObjectBucket) (backend.BucketState, error) {
	return backend.BucketState{Message: "SeaweedFS bucket provisioning is not implemented yet"}, nil
}

// DeleteBucket is not implemented in this milestone.
func (d *Driver) DeleteBucket(_ context.Context, _ *v1alpha1.ObjectStorageCluster, _ *v1alpha1.ObjectBucket) error {
	return nil
}

// apply creates or updates obj, setting the cluster as its controller owner.
func (d *Driver) apply(ctx context.Context, cluster *v1alpha1.ObjectStorageCluster, obj client.Object) error {
	desired := obj.DeepCopyObject().(client.Object)
	_, err := controllerutil.CreateOrUpdate(ctx, d.client, obj, func() error {
		mergeDesired(obj, desired)
		return controllerutil.SetControllerReference(cluster, obj, d.client.Scheme())
	})
	return err
}

// mergeDesired copies the desired spec onto the live object fetched by
// CreateOrUpdate, preserving server-managed metadata. Immutable fields
// (StatefulSet selector/volumeClaimTemplates) are left untouched once set.
func mergeDesired(live, desired client.Object) {
	switch l := live.(type) {
	case *corev1.Service:
		d := desired.(*corev1.Service)
		l.Labels = d.Labels
		l.Spec.Selector = d.Spec.Selector
		l.Spec.Ports = d.Spec.Ports
	case *appsv1.StatefulSet:
		d := desired.(*appsv1.StatefulSet)
		l.Labels = d.Labels
		l.Spec.Replicas = d.Spec.Replicas
		l.Spec.Template = d.Spec.Template
	}
}
