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

package cephrgw

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

func heavy(name string, r v1alpha1.RedundancyMode) *v1alpha1.ObjectStorageCluster {
	return &v1alpha1.ObjectStorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.ObjectStorageClusterSpec{Type: v1alpha1.ClusterTypeHeavy, Redundancy: r},
	}
}

func TestReplicatedPool(t *testing.T) {
	cases := []struct {
		r    v1alpha1.RedundancyMode
		size int64
		safe bool
	}{
		{v1alpha1.RedundancySingle, 2, false},
		{v1alpha1.RedundancyReplicated, 3, true},
		{v1alpha1.RedundancyMode(""), 3, true},
		{v1alpha1.RedundancyHighRedundancy, 4, true},
	}
	for _, c := range cases {
		p := replicatedPool(heavy("h", c.r))
		if p["size"] != c.size {
			t.Errorf("redundancy %q: size=%v, want %d", c.r, p["size"], c.size)
		}
		if p["requireSafeReplicaSize"] != c.safe {
			t.Errorf("redundancy %q: safe=%v, want %v", c.r, p["requireSafeReplicaSize"], c.safe)
		}
	}
}

func TestRGWEndpointAndStore(t *testing.T) {
	c := heavy("main", "")
	if got := storeName(c); got != "main" {
		t.Errorf("storeName=%q", got)
	}
	want := "http://rook-ceph-rgw-main.d8-sds-elastic.svc.internal.cluster.local:80"
	if got := rgwEndpoint(c, "internal.cluster.local"); got != want {
		t.Errorf("rgwEndpoint=%q, want %q", got, want)
	}
}

func TestBuildCephObjectStore(t *testing.T) {
	c := heavy("main", v1alpha1.RedundancyHighRedundancy)
	obj := buildCephObjectStore(c)

	if obj.GetNamespace() != elasticNamespace {
		t.Errorf("namespace=%q, want %q", obj.GetNamespace(), elasticNamespace)
	}
	if gvk := obj.GroupVersionKind(); gvk.Group != "internal.sdselastic.deckhouse.io" || gvk.Kind != "CephObjectStore" {
		t.Errorf("unexpected GVK %v", gvk)
	}
	spec, _ := obj.Object["spec"].(map[string]interface{})
	dataPool, _ := spec["dataPool"].(map[string]interface{})
	repl, _ := dataPool["replicated"].(map[string]interface{})
	if repl["size"] != int64(4) {
		t.Errorf("HighRedundancy dataPool size=%v, want 4", repl["size"])
	}
}
