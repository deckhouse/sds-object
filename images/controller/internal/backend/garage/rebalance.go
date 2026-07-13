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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

// placementAction is the single migration step the placement reconcile decides
// to take this pass (recycle one replica's PVC), or none.
type placementAction struct {
	act     bool
	ordinal int32
	reason  string
}

// nextPlacementAction decides the one replica to relocate to converge the System
// replicas onto the target topology, given the live control-plane hostnames, the
// node each replica's local PV is pinned to (replicaNode[ordinal]; "" when the
// replica's PVC is unbound — still binding, e.g. during initial bring-up) and
// whether Garage is currently healthy. An "" (settling) replica is never
// migrated; only a replica bound to a wrong or removed node is moved. A replica
// left Pending after its master was removed keeps its PVC bound to the old PV
// (Retain), so it reads back as that removed node — not "" — and is migrated.
//
// Topology rules (the 2-master state is intentionally left untouched — a
// mid-shrink transient we neither spread across nor consolidate from):
//   - control-plane nodes >= replicas: SPREAD — one replica per master. A
//     replica that is co-located (shares its node) or pinned to a node that is
//     no longer a control-plane node is relocated onto an empty master. Gated on
//     Garage being healthy so the other replicas have re-replicated before the
//     next move (re-replication only completes once the StatefulSet is whole).
//   - exactly one control-plane node: CONSOLIDATE — every replica onto that
//     node. Replicas not already pinned there (Pending because their master was
//     removed, or on a wrong node) are relocated; the surviving replica on the
//     target is the data anchor and is never recycled. No health gate: the
//     replicas being moved hold no reachable data (their node is gone), so
//     recycling them only restores quorum.
//   - otherwise (0 or 2 nodes): no action.
//
// It returns at most one action; the caller performs it and requeues, so the
// rebalance advances one replica per reconcile.
func nextPlacementAction(cpNodes []string, replicaNode map[int32]string, replicas int32, healthy bool) placementAction {
	cpSet := make(map[string]struct{}, len(cpNodes))
	for _, n := range cpNodes {
		cpSet[n] = struct{}{}
	}

	switch {
	case int32(len(cpNodes)) >= replicas: // SPREAD
		if !healthy {
			return placementAction{} // wait: let the previous move re-replicate
		}
		perNode := map[string]int{}
		for _, n := range replicaNode {
			if _, ok := cpSet[n]; ok {
				perNode[n]++
			}
		}
		for ord := int32(0); ord < replicas; ord++ {
			n := replicaNode[ord]
			if n == "" {
				continue // PVC still binding — let it settle, do not disturb
			}
			if _, onCP := cpSet[n]; !onCP {
				return placementAction{act: true, ordinal: ord, reason: "spread: replica off a control-plane node"}
			}
			if perNode[n] > 1 {
				return placementAction{act: true, ordinal: ord, reason: "spread: replica co-located on " + n}
			}
		}
		return placementAction{}

	case len(cpNodes) == 1: // CONSOLIDATE
		target := cpNodes[0]
		for ord := int32(0); ord < replicas; ord++ {
			n := replicaNode[ord]
			if n == "" {
				continue // PVC still binding (e.g. initial bring-up) — leave it
			}
			if n != target {
				return placementAction{act: true, ordinal: ord, reason: "consolidate onto " + target}
			}
		}
		return placementAction{}

	default: // 0 or 2 control-plane nodes: ignore
		return placementAction{}
	}
}

// reconcileSystemPlacement performs at most one placement migration for the
// System profile per reconcile: it reads the live topology, decides the next
// action (nextPlacementAction) and, when one is due, recycles that replica's PVC
// so the StatefulSet re-homes it onto the correct master. It returns a
// human-readable message and whether it acted (the caller then reports "not
// ready" and requeues so the move can settle before the next one).
func (d *Driver) reconcileSystemPlacement(ctx context.Context, cluster *v1alpha1.ObjectStore) (acted bool, msg string, err error) {
	cpNodes, err := d.controlPlaneHostnames(ctx)
	if err != nil {
		return false, "", fmt.Errorf("list control-plane nodes: %w", err)
	}
	replicaNode, err := d.systemReplicaNodes(ctx, cluster)
	if err != nil {
		return false, "", fmt.Errorf("read replica PV placement: %w", err)
	}

	// The health gate only matters for SPREAD; querying it is skipped otherwise.
	healthy := false
	if int32(len(cpNodes)) >= systemReplicas {
		healthy = d.garageHealthy(ctx, cluster)
	}

	action := nextPlacementAction(cpNodes, replicaNode, systemReplicas, healthy)
	if !action.act {
		return false, "", nil
	}

	if err := d.recycleReplica(ctx, cluster, action.ordinal); err != nil {
		return false, "", fmt.Errorf("recycle replica %d (%s): %w", action.ordinal, action.reason, err)
	}
	return true, fmt.Sprintf("rebalancing System placement: recycling %s-%d (%s)", resourceName(cluster), action.ordinal, action.reason), nil
}

// systemReplicaNodes maps each replica ordinal to the control-plane node its
// data PVC is bound to (via the bound local PV's node label), or "" when the PVC
// is unbound / the PV is missing (e.g. the replica is Pending because its master
// was removed).
func (d *Driver) systemReplicaNodes(ctx context.Context, cluster *v1alpha1.ObjectStore) (map[int32]string, error) {
	out := make(map[int32]string, systemReplicas)
	for ord := int32(0); ord < systemReplicas; ord++ {
		pvc := &corev1.PersistentVolumeClaim{}
		key := client.ObjectKey{Namespace: d.namespace, Name: fmt.Sprintf("data-%s-%d", resourceName(cluster), ord)}
		if err := d.apiReader.Get(ctx, key, pvc); err != nil {
			if apierrors.IsNotFound(err) {
				out[ord] = ""
				continue
			}
			return nil, err
		}
		if pvc.Spec.VolumeName == "" {
			out[ord] = ""
			continue
		}
		pv := &corev1.PersistentVolume{}
		if err := d.apiReader.Get(ctx, client.ObjectKey{Name: pvc.Spec.VolumeName}, pv); err != nil {
			if apierrors.IsNotFound(err) {
				out[ord] = ""
				continue
			}
			return nil, err
		}
		out[ord] = pv.Labels[labelSystemLocalNode]
	}
	return out, nil
}

// recycleReplica relocates one replica by deleting its pod and PVC: the
// StatefulSet then recreates both, and the fresh PVC (WaitForFirstConsumer)
// binds a pool PV on the master the scheduler picks — an empty master (soft
// anti-affinity) when spreading, or the sole survivor when consolidating. The
// replica boots empty and Garage re-replicates once the cluster is whole again.
// Retain means the old PV is left Released for gcSystemLocalPVs to reap.
func (d *Driver) recycleReplica(ctx context.Context, cluster *v1alpha1.ObjectStore, ordinal int32) error {
	name := fmt.Sprintf("%s-%d", resourceName(cluster), ordinal)
	pvcName := "data-" + name

	pvc := &corev1.PersistentVolumeClaim{}
	pvc.Namespace, pvc.Name = d.namespace, pvcName
	if err := d.client.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete pvc %s: %w", pvcName, err)
	}

	pod := &corev1.Pod{}
	pod.Namespace, pod.Name = d.namespace, name
	if err := d.client.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete pod %s: %w", name, err)
	}
	return nil
}

// garageHealthy reports whether the Garage admin API considers the cluster
// healthy (all storage nodes ok / data re-replicated). Any error (token missing,
// endpoint not answering, degraded) yields false, so a placement move is only
// started from a known-good state.
func (d *Driver) garageHealthy(ctx context.Context, cluster *v1alpha1.ObjectStore) bool {
	token, err := d.adminToken(ctx, cluster)
	if err != nil || token == "" {
		return false
	}
	svc := newAdminClient(adminEndpoint(cluster, d.namespace, d.clusterDomain), token)
	h, err := svc.health(ctx)
	if err != nil {
		return false
	}
	return h.Status == "healthy"
}
