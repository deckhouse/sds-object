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
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

// layoutZone is the Garage layout zone assigned to every node. A single zone is
// deliberate: the System profile must keep a fixed replica count even on a
// single master (replicas co-located there), which a per-host zone would forbid.
// The trade-off — co-located replicas share a physical disk, so redundancy is at
// the process level until masters allow spreading — is documented in DESIGN.
const layoutZone = "dc1"

// meshResult is the outcome of a meshing+layout reconcile pass.
type meshResult struct {
	ready bool
	msg   string
	total *resource.Quantity
}

// nodePeer is a discovered Garage pod with its node identity.
type nodePeer struct {
	id string
	ip string
}

// ensureMeshAndLayout connects the Garage RPC peers and assigns the cluster
// layout via the admin API, returning whether the cluster is healthy and
// serving. It is idempotent: connect is a no-op for already-connected peers and
// layout is only staged for nodes not yet present in it.
func (d *Driver) ensureMeshAndLayout(ctx context.Context, cluster *v1alpha1.ObjectStore) (meshResult, error) {
	token, err := d.adminToken(ctx, cluster)
	if err != nil {
		return meshResult{}, err
	}
	if token == "" {
		return meshResult{msg: "Garage admin token is not provisioned yet"}, nil
	}

	peers, err := d.discoverPeers(ctx, cluster, token)
	if err != nil {
		return meshResult{}, err
	}
	if len(peers) == 0 {
		return meshResult{msg: "waiting for Garage pods to report a node identity"}, nil
	}

	svc := newAdminClient(adminEndpoint(cluster, d.namespace, d.clusterDomain), token)

	// 1. Connect peers (idempotent gossip seed).
	peerSpecs := make([]string, 0, len(peers))
	for _, p := range peers {
		peerSpecs = append(peerSpecs, fmt.Sprintf("%s@%s:%d", p.id, p.ip, rpcPort))
	}
	if err := svc.connect(ctx, peerSpecs); err != nil {
		return meshResult{msg: fmt.Sprintf("connecting Garage peers: %v", err)}, nil
	}

	// 2. Reconcile the layout onto the live peers: assign roles for new nodes
	// and drop roles for nodes that are gone (once the full replica complement
	// is live, so a transient reschedule does not churn the layout).
	layout, err := svc.layout(ctx)
	if err != nil {
		return meshResult{msg: fmt.Sprintf("reading layout: %v", err)}, nil
	}

	size := storageSize(cluster)
	changes := layoutRoleChanges(layout, peers, desiredReplicas(cluster), size.Value())
	if len(changes) > 0 {
		if err := svc.stageLayout(ctx, changes); err != nil {
			return meshResult{msg: fmt.Sprintf("staging layout: %v", err)}, nil
		}
		cur, err := svc.layout(ctx)
		if err != nil {
			return meshResult{msg: fmt.Sprintf("reading staged layout: %v", err)}, nil
		}
		if err := svc.applyLayout(ctx, cur.Version+1); err != nil {
			return meshResult{msg: fmt.Sprintf("applying layout: %v", err)}, nil
		}
	}

	// 3. Health gate.
	health, err := svc.health(ctx)
	if err != nil {
		return meshResult{msg: fmt.Sprintf("reading health: %v", err)}, nil
	}
	total := layoutTotalCapacity(layout)
	if health.Status != "healthy" {
		return meshResult{msg: fmt.Sprintf("Garage cluster health is %q (%d/%d storage nodes ok)", health.Status, health.StorageNodesOk, health.StorageNodes), total: total}, nil
	}

	return meshResult{ready: true, msg: "Garage cluster is healthy", total: total}, nil
}

// adminToken reads the admin token from the per-cluster secret.
func (d *Driver) adminToken(ctx context.Context, cluster *v1alpha1.ObjectStore) (string, error) {
	secret := &corev1.Secret{}
	if err := d.client.Get(ctx, client.ObjectKey{Namespace: d.namespace, Name: secretName(cluster)}, secret); err != nil {
		return "", err
	}
	return string(secret.Data[secretKeyAdmin]), nil
}

// discoverPeers lists the Garage pods and queries each pod's admin API for its
// node identity. Pods without an IP or not yet answering are skipped.
func (d *Driver) discoverPeers(ctx context.Context, cluster *v1alpha1.ObjectStore, token string) ([]nodePeer, error) {
	pods := &corev1.PodList{}
	if err := d.apiReader.List(ctx, pods,
		client.InNamespace(d.namespace),
		client.MatchingLabels(commonLabels(cluster)),
	); err != nil {
		return nil, err
	}

	peers := make([]nodePeer, 0, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" {
			continue
		}
		pc := newAdminClient(fmt.Sprintf("http://%s:%d", pod.Status.PodIP, adminPort), token)
		st, err := pc.status(ctx)
		if err != nil || st.Node == "" {
			// Pod not answering yet; it will be picked up on requeue.
			continue
		}
		peers = append(peers, nodePeer{id: st.Node, ip: pod.Status.PodIP})
	}
	return peers, nil
}

// layoutRoleChanges computes the layout mutations converging the assigned roles
// onto the currently-live Garage peers:
//
//   - every live peer not yet in the layout is assigned a role (single zone,
//     per-node capacity), so a rejoining or freshly-scheduled pod starts holding
//     data and Garage re-replicates onto it;
//   - a role whose node is no longer live is removed — but only once the full
//     replica complement (expected) is live. A System pod that moves to another
//     master comes back under a NEW node identity over an empty hostPath; the
//     stale role must be dropped so Garage stops expecting the vanished node and
//     the layout returns to healthy. Gating removal on a full complement avoids
//     churning the layout (and triggering rebalances) during a transient
//     reschedule when a pod is briefly absent.
//
// capacity is the per-node storage capacity in bytes.
func layoutRoleChanges(layout *clusterLayout, peers []nodePeer, expected int32, capacity int64) []roleChange {
	assigned := map[string]struct{}{}
	for _, r := range layout.Roles {
		assigned[r.ID] = struct{}{}
	}
	live := make(map[string]struct{}, len(peers))
	for _, p := range peers {
		live[p.id] = struct{}{}
	}

	var changes []roleChange
	for _, p := range peers {
		if _, ok := assigned[p.id]; ok {
			continue
		}
		c := capacity
		changes = append(changes, roleChange{ID: p.id, Zone: layoutZone, Capacity: &c, Tags: []string{}})
	}
	if int32(len(peers)) >= expected {
		for _, r := range layout.Roles {
			if _, ok := live[r.ID]; ok {
				continue
			}
			changes = append(changes, roleChange{ID: r.ID, Remove: true, Tags: []string{}})
		}
	}
	return changes
}

// layoutTotalCapacity sums the assigned role capacities (bytes) into a Quantity.
func layoutTotalCapacity(layout *clusterLayout) *resource.Quantity {
	var sum int64
	for _, r := range layout.Roles {
		if r.Capacity != nil {
			sum += *r.Capacity
		}
	}
	if sum == 0 {
		return nil
	}
	q := resource.NewQuantity(sum, resource.BinarySI)
	return q
}
