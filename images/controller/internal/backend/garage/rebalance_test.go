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

import "testing"

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
