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

func peersFrom(ids ...string) []nodePeer {
	ps := make([]nodePeer, 0, len(ids))
	for _, id := range ids {
		ps = append(ps, nodePeer{id: id, ip: "10.0.0.1"})
	}
	return ps
}

func layoutFrom(ids ...string) *clusterLayout {
	l := &clusterLayout{}
	for _, id := range ids {
		l.Roles = append(l.Roles, layoutRole{ID: id, Zone: layoutZone})
	}
	return l
}

func classify(changes []roleChange) (assign, remove []string) {
	for _, c := range changes {
		if c.Remove {
			remove = append(remove, c.ID)
		} else {
			assign = append(assign, c.ID)
		}
	}
	return assign, remove
}

func TestLayoutRoleChanges(t *testing.T) {
	const capBytes = int64(10 << 30)

	t.Run("assigns new peers", func(t *testing.T) {
		changes := layoutRoleChanges(layoutFrom("a"), peersFrom("a", "b", "c"), 3, capBytes)
		assign, remove := classify(changes)
		if len(assign) != 2 || len(remove) != 0 {
			t.Fatalf("assign=%v remove=%v, want assign {b,c}", assign, remove)
		}
		for _, c := range changes {
			if c.Zone != layoutZone || c.Capacity == nil || *c.Capacity != capBytes {
				t.Errorf("assign change %+v missing zone/capacity", c)
			}
		}
	})

	t.Run("prunes stale role when full complement is live", func(t *testing.T) {
		// Old node "a" is gone; its replacement "a2" rejoined over an empty
		// hostPath under a new identity. All 3 replicas answer.
		changes := layoutRoleChanges(layoutFrom("a", "b", "c"), peersFrom("a2", "b", "c"), 3, capBytes)
		assign, remove := classify(changes)
		if len(assign) != 1 || assign[0] != "a2" {
			t.Errorf("assign=%v, want {a2}", assign)
		}
		if len(remove) != 1 || remove[0] != "a" {
			t.Errorf("remove=%v, want {a} (stale node dropped)", remove)
		}
	})

	t.Run("does not prune during a transient reschedule", func(t *testing.T) {
		// Only 2 of 3 replicas answer (one is mid-reschedule): keep the stale
		// role, do not churn the layout / trigger a rebalance.
		changes := layoutRoleChanges(layoutFrom("a", "b", "c"), peersFrom("b", "c"), 3, capBytes)
		_, remove := classify(changes)
		if len(remove) != 0 {
			t.Errorf("remove=%v, want none while under full complement", remove)
		}
	})

	t.Run("no changes when layout already matches", func(t *testing.T) {
		changes := layoutRoleChanges(layoutFrom("a", "b", "c"), peersFrom("a", "b", "c"), 3, capBytes)
		if len(changes) != 0 {
			t.Errorf("changes=%v, want none", changes)
		}
	})
}
