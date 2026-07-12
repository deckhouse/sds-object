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

package garage

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

// ensureSystemLocalPVs maintains the static pool of node-sticky local PVs that
// backs the System profile's StatefulSet. For every control-plane node it
// ensures systemReplicas PVs exist (each hostPath-backed and nodeAffinity-pinned
// to that node), so the scheduler — under WaitForFirstConsumer — always has an
// available PV on whichever node it places a replica, and a bound replica
// returns to its node (hence its data) after a restart.
//
// It is create-only and idempotent: PVs have deterministic names, existing ones
// are left untouched, and the pool is owned by the ObjectStore so the PV objects
// are garbage-collected when the store is deleted (Retain keeps the on-disk data
// regardless). Over-provisioning (a full pool per node, most of it Available) is
// intentional and cheap: an Available PV allocates no directory until a pod
// mounts it.
func (d *Driver) ensureSystemLocalPVs(ctx context.Context, cluster *v1alpha1.ObjectStore) error {
	hostnames, err := d.controlPlaneHostnames(ctx)
	if err != nil {
		return err
	}

	existing := &corev1.PersistentVolumeList{}
	if err := d.apiReader.List(ctx, existing,
		client.MatchingLabels{objectStoreLabel: cluster.Name},
		client.HasLabels{labelSystemLocalNode},
	); err != nil {
		return err
	}
	have := make(map[string]struct{}, len(existing.Items))
	for i := range existing.Items {
		have[existing.Items[i].Name] = struct{}{}
	}

	for _, pv := range desiredSystemLocalPVs(cluster, hostnames) {
		if _, ok := have[pv.Name]; ok {
			continue
		}
		if err := controllerutil.SetControllerReference(cluster, pv, d.client.Scheme()); err != nil {
			return err
		}
		if err := d.client.Create(ctx, pv); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}

	return d.gcSystemLocalPVs(ctx, existing, hostnames)
}

// gcSystemLocalPVs reaps stale pool PVs: any Released PV (its PVC was recycled
// during a rebalance — Retain left the PV behind), and any Available PV pinned
// to a node that is no longer a control-plane node (a removed master). Bound PVs
// are never touched (a live replica, or a Pending replica still owning it — the
// placement reconcile recycles the latter's PVC, after which the PV goes
// Released and is reaped on a later pass).
func (d *Driver) gcSystemLocalPVs(ctx context.Context, pool *corev1.PersistentVolumeList, hostnames []string) error {
	live := make(map[string]struct{}, len(hostnames))
	for _, h := range hostnames {
		live[h] = struct{}{}
	}
	for i := range pool.Items {
		pv := &pool.Items[i]
		_, onLiveNode := live[pv.Labels[labelSystemLocalNode]]
		stale := pv.Status.Phase == corev1.VolumeReleased ||
			(pv.Status.Phase == corev1.VolumeAvailable && !onLiveNode)
		if !stale {
			continue
		}
		if err := d.client.Delete(ctx, pv); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// controlPlaneHostnames lists control-plane nodes and returns their hostnames
// (the kubernetes.io/hostname label value, falling back to the node name), which
// key both the PV nodeAffinity and its deterministic name. Read through the
// non-cached apiReader so the controller does not need a cluster-wide Node
// informer.
func (d *Driver) controlPlaneHostnames(ctx context.Context) ([]string, error) {
	nodes := &corev1.NodeList{}
	if err := d.apiReader.List(ctx, nodes, client.HasLabels{controlPlaneNodeLabel}); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(nodes.Items))
	for i := range nodes.Items {
		h := nodes.Items[i].Labels[hostnameTopologyKey]
		if h == "" {
			h = nodes.Items[i].Name
		}
		out = append(out, h)
	}
	return out, nil
}

// desiredSystemLocalPVs is the full pool the controller wants: systemReplicas PVs
// per control-plane hostname.
func desiredSystemLocalPVs(cluster *v1alpha1.ObjectStore, hostnames []string) []*corev1.PersistentVolume {
	pvs := make([]*corev1.PersistentVolume, 0, len(hostnames)*int(systemReplicas))
	for _, h := range hostnames {
		for i := int32(0); i < systemReplicas; i++ {
			pvs = append(pvs, buildSystemLocalPV(cluster, h, i))
		}
	}
	return pvs
}
