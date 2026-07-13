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
	"github.com/deckhouse/sds-object/images/controller/internal/backend"
)

func heavy(name string, r v1alpha1.RedundancyMode) *v1alpha1.ObjectStore {
	return &v1alpha1.ObjectStore{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.ObjectStoreSpec{Type: v1alpha1.ClusterTypeHeavy, Redundancy: r},
	}
}

func TestReplicatedPool(t *testing.T) {
	cases := []struct {
		r    v1alpha1.RedundancyMode
		size int64
		safe bool
	}{
		{v1alpha1.RedundancyNone, 2, false},
		{v1alpha1.RedundancyStandard, 3, true},
		{v1alpha1.RedundancyMode(""), 3, true},
		{v1alpha1.RedundancyHigh, 4, true},
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

func TestUserAndSecretNames(t *testing.T) {
	c := heavy("main", "")
	b := &v1alpha1.Bucket{ObjectMeta: metav1.ObjectMeta{Name: "data"}}
	access := &v1alpha1.BucketAccess{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "reader"}}

	if got := ownerUID(b); got != "data-owner" {
		t.Errorf("ownerUID=%q, want data-owner", got)
	}
	if got := accessUID(access); got != "app.reader" {
		t.Errorf("accessUID=%q, want app.reader", got)
	}
	if got := rgwUserSecretName(c, "app.reader"); got != "rook-ceph-object-user-main-app.reader" {
		t.Errorf("rgwUserSecretName=%q", got)
	}
	if got := rgwHostPort(c, "internal.cluster.local"); got != "rook-ceph-rgw-main.d8-sds-elastic.svc.internal.cluster.local:80" {
		t.Errorf("rgwHostPort=%q", got)
	}
	if got := backend.BucketDisplayName(b); got != "data" {
		t.Errorf("BucketDisplayName=%q, want data", got)
	}

	user := buildCephObjectStoreUser(c, "data-owner", nil)
	if user.GetNamespace() != elasticNamespace {
		t.Errorf("user namespace=%q", user.GetNamespace())
	}
	spec, _ := user.Object["spec"].(map[string]interface{})
	if spec["store"] != "main" {
		t.Errorf("user spec.store=%v, want main", spec["store"])
	}
}

func TestBuildCephObjectStore(t *testing.T) {
	c := heavy("main", v1alpha1.RedundancyHigh)
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

func TestBuildCephObjectStoreUserQuotas(t *testing.T) {
	c := heavy("main", v1alpha1.RedundancyStandard)

	t.Run("no quota omits the block", func(t *testing.T) {
		user := buildCephObjectStoreUser(c, "data-owner", nil)
		spec, _ := user.Object["spec"].(map[string]interface{})
		if _, ok := spec["quotas"]; ok {
			t.Errorf("expected no quotas block, got %v", spec["quotas"])
		}
	})

	t.Run("quota maps to spec.quotas", func(t *testing.T) {
		q := &v1alpha1.BucketQuota{MaxSize: "10Gi", MaxObjects: 1000}
		user := buildCephObjectStoreUser(c, "data-owner", q)
		spec, _ := user.Object["spec"].(map[string]interface{})
		quotas, ok := spec["quotas"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected quotas block, spec=%v", spec)
		}
		if quotas["maxSize"] != "10Gi" {
			t.Errorf("maxSize=%v, want 10Gi", quotas["maxSize"])
		}
		if quotas["maxObjects"] != int64(1000) {
			t.Errorf("maxObjects=%v, want 1000", quotas["maxObjects"])
		}
	})

	t.Run("empty quota fields omit the block", func(t *testing.T) {
		user := buildCephObjectStoreUser(c, "data-owner", &v1alpha1.BucketQuota{})
		spec, _ := user.Object["spec"].(map[string]interface{})
		if _, ok := spec["quotas"]; ok {
			t.Errorf("expected no quotas block for empty quota, got %v", spec["quotas"])
		}
	})
}

func TestCephRGWUnsupported(t *testing.T) {
	t.Run("quota is enforced, not reported", func(t *testing.T) {
		b := &v1alpha1.Bucket{Spec: v1alpha1.BucketSpec{Quota: &v1alpha1.BucketQuota{MaxObjects: 5}}}
		if got := cephRGWUnsupported(b); len(got) != 0 {
			t.Errorf("unsupported=%v, want empty", got)
		}
	})

	t.Run("PublicRead is reported unsupported", func(t *testing.T) {
		b := &v1alpha1.Bucket{Spec: v1alpha1.BucketSpec{AccessPolicy: v1alpha1.AccessPolicyPublicRead}}
		got := cephRGWUnsupported(b)
		if len(got) != 1 || got[0] != backend.FeaturePublicRead {
			t.Errorf("unsupported=%v, want [%s]", got, backend.FeaturePublicRead)
		}
	})
}

func TestUnstructuredManagedFieldsChanged(t *testing.T) {
	c := heavy("main", v1alpha1.RedundancyStandard)
	a := buildCephObjectStore(c)
	b := buildCephObjectStore(c)
	if unstructuredManagedFieldsChanged(a, b) {
		t.Errorf("identical objects must report no change (would churn Rook)")
	}
	b.Object["spec"].(map[string]interface{})["gateway"] = map[string]interface{}{"instances": int64(2)}
	if !unstructuredManagedFieldsChanged(a, b) {
		t.Errorf("differing spec must report a change")
	}
	c2 := buildCephObjectStore(c)
	c2.SetLabels(map[string]string{"extra": "x"})
	if !unstructuredManagedFieldsChanged(a, c2) {
		t.Errorf("differing labels must report a change")
	}
}
