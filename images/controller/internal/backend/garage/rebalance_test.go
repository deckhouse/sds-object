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
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

func TestNextPlacementAction(t *testing.T) {
	const replicas = int32(3)

	cases := []struct {
		name    string
		cpNodes []string
		replica map[int32]string
		healthy bool
		wantAct bool
		wantOrd int32
	}{
		// SPREAD (>=3 masters)
		{
			name:    "spread: already 1-per-master -> no action",
			cpNodes: []string{"m0", "m1", "m2"},
			replica: map[int32]string{0: "m0", 1: "m1", 2: "m2"},
			healthy: true,
			wantAct: false,
		},
		{
			name:    "spread: co-located + healthy -> recycle the co-located replica",
			cpNodes: []string{"m0", "m1", "m2"},
			replica: map[int32]string{0: "m0", 1: "m0", 2: "m0"},
			healthy: true,
			wantAct: true,
			wantOrd: 0, // first replica sharing m0
		},
		{
			name:    "spread: co-located but NOT healthy -> gated, no action",
			cpNodes: []string{"m0", "m1", "m2"},
			replica: map[int32]string{0: "m0", 1: "m0", 2: "m0"},
			healthy: false,
			wantAct: false,
		},
		{
			name:    "spread: replica bound to a removed node + healthy -> recycle it",
			cpNodes: []string{"m0", "m1", "m2"},
			replica: map[int32]string{0: "m0", 1: "m1", 2: "gone"},
			healthy: true,
			wantAct: true,
			wantOrd: 2,
		},
		{
			name:    "spread: unbound replica (still binding) -> left alone",
			cpNodes: []string{"m0", "m1", "m2"},
			replica: map[int32]string{0: "m0", 1: "m1", 2: ""},
			healthy: true,
			wantAct: false,
		},
		// CONSOLIDATE (exactly 1 master)
		{
			name:    "consolidate: all on target -> no action",
			cpNodes: []string{"m0"},
			replica: map[int32]string{0: "m0", 1: "m0", 2: "m0"},
			healthy: false,
			wantAct: false,
		},
		{
			name:    "consolidate: replicas bound to removed masters -> recycle first (anchor untouched)",
			cpNodes: []string{"m0"},
			replica: map[int32]string{0: "m0", 1: "gone1", 2: "gone2"},
			healthy: false,
			wantAct: true,
			wantOrd: 1,
		},
		{
			name:    "consolidate: unbound replica (still binding) -> left alone",
			cpNodes: []string{"m0"},
			replica: map[int32]string{0: "m0", 1: "", 2: "m0"},
			healthy: false,
			wantAct: false,
		},
		{
			name:    "consolidate: replica on a stale node -> recycle it",
			cpNodes: []string{"m0"},
			replica: map[int32]string{0: "m0", 1: "m0", 2: "m2"},
			healthy: false,
			wantAct: true,
			wantOrd: 2,
		},
		// IGNORE (2 or 0 masters)
		{
			name:    "two masters -> ignored even when replicas are messy",
			cpNodes: []string{"m0", "m1"},
			replica: map[int32]string{0: "m0", 1: "", 2: "gone"},
			healthy: true,
			wantAct: false,
		},
		{
			name:    "zero masters -> no action",
			cpNodes: []string{},
			replica: map[int32]string{0: "", 1: "", 2: ""},
			healthy: false,
			wantAct: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nextPlacementAction(tc.cpNodes, tc.replica, replicas, tc.healthy)
			if got.act != tc.wantAct {
				t.Fatalf("act=%v, want %v (reason=%q)", got.act, tc.wantAct, got.reason)
			}
			if got.act && got.ordinal != tc.wantOrd {
				t.Errorf("ordinal=%d, want %d (reason=%q)", got.ordinal, tc.wantOrd, got.reason)
			}
		})
	}
}

// TestReconcileSystemPlacementSingleReplica pins the promise of the
// singleReplica setting: the sole replica is never relocated. The fixture is a
// consolidate case the default profile acts on without a health gate (one master
// left, the replica's PV pinned to a master that is gone), so the two modes are
// compared on identical input.
func TestReconcileSystemPlacementSingleReplica(t *testing.T) {
	ns := "d8-sds-object"

	newDriver := func(t *testing.T, cluster *v1alpha1.ObjectStore) (*Driver, client.Client) {
		t.Helper()
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name:   "m0",
			Labels: map[string]string{controlPlaneNodeLabel: "", hostnameTopologyKey: "m0"},
		}}
		pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{
			Name:   "pool-pv",
			Labels: map[string]string{objectStoreLabel: cluster.Name, labelSystemLocalNode: "gone"},
		}}
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "data-" + resourceName(cluster) + "-0", Namespace: ns},
			Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: pv.Name},
		}
		c := fake.NewClientBuilder().WithScheme(rfScheme(t)).WithObjects(node, pv, pvc).Build()
		return &Driver{client: c, apiReader: c, namespace: ns}, c
	}

	pvcGone := func(t *testing.T, c client.Client, cluster *v1alpha1.ObjectStore) bool {
		t.Helper()
		err := c.Get(context.Background(), client.ObjectKey{
			Namespace: ns, Name: "data-" + resourceName(cluster) + "-0",
		}, &corev1.PersistentVolumeClaim{})
		return apierrors.IsNotFound(err)
	}

	t.Run("single replica: never migrates", func(t *testing.T) {
		cluster := singleReplicaSystemCluster()
		d, c := newDriver(t, cluster)
		acted, msg, err := d.reconcileSystemPlacement(context.Background(), cluster)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if acted {
			t.Errorf("acted=true (%q), want no placement action for a single replica", msg)
		}
		if pvcGone(t, c, cluster) {
			t.Errorf("the replica's PVC was recycled; single-replica data must never be discarded")
		}
	})

	t.Run("default profile: consolidates onto the surviving master", func(t *testing.T) {
		cluster := systemCluster()
		d, c := newDriver(t, cluster)
		acted, _, err := d.reconcileSystemPlacement(context.Background(), cluster)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !acted {
			t.Fatalf("acted=false, want the default profile to recycle the stale replica")
		}
		if !pvcGone(t, c, cluster) {
			t.Errorf("expected the replica's PVC to be recycled")
		}
	})
}
