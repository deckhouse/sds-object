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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

func cluster(name string, t v1alpha1.ClusterType, r v1alpha1.RedundancyMode) *v1alpha1.ObjectStorageCluster {
	return &v1alpha1.ObjectStorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.ObjectStorageClusterSpec{Type: t, Redundancy: r},
	}
}

func TestReplicationFactor(t *testing.T) {
	cases := map[v1alpha1.RedundancyMode]int32{
		v1alpha1.RedundancySingle:         1,
		v1alpha1.RedundancyReplicated:     3,
		v1alpha1.RedundancyHighRedundancy: 5,
		v1alpha1.RedundancyMode(""):       3, // default
	}
	for r, want := range cases {
		if got := replicationFactor(cluster("c", v1alpha1.ClusterTypeLightweight, r)); got != want {
			t.Errorf("replicationFactor(%q)=%d, want %d", r, got, want)
		}
	}
}

func TestStorageSize(t *testing.T) {
	none := cluster("c", v1alpha1.ClusterTypeLightweight, "")
	if got := storageSize(none); got.Cmp(resource.MustParse("10Gi")) != 0 {
		t.Errorf("storageSize(unset)=%s, want 10Gi", got.String())
	}

	sized := cluster("c", v1alpha1.ClusterTypeLightweight, "")
	sized.Spec.Storage = &v1alpha1.ObjectStorageClusterStorageSpec{Size: "20Gi"}
	if got := storageSize(sized); got.Cmp(resource.MustParse("20Gi")) != 0 {
		t.Errorf("storageSize(20Gi)=%s, want 20Gi", got.String())
	}

	bad := cluster("c", v1alpha1.ClusterTypeLightweight, "")
	bad.Spec.Storage = &v1alpha1.ObjectStorageClusterStorageSpec{Size: "not-a-quantity"}
	if got := storageSize(bad); got.Cmp(resource.MustParse("10Gi")) != 0 {
		t.Errorf("storageSize(invalid)=%s, want 10Gi fallback", got.String())
	}
}

func TestRenderConfig(t *testing.T) {
	cfg := renderConfig(cluster("c", v1alpha1.ClusterTypeLightweight, v1alpha1.RedundancyHighRedundancy))
	for _, want := range []string{
		"replication_factor = 5",
		"[s3_api]",
		"[admin]",
		`s3_region = "garage"`,
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("renderConfig missing %q in:\n%s", want, cfg)
		}
	}
}

func TestNamesAndEndpoints(t *testing.T) {
	c := cluster("shared", v1alpha1.ClusterTypeLightweight, "")
	if got := resourceName(c); got != "shared-garage" {
		t.Errorf("resourceName=%q", got)
	}
	if got := secretName(c); got != "shared-garage-secrets" {
		t.Errorf("secretName=%q", got)
	}
	if got := s3Endpoint(c, "d8-sds-object", "internal.cluster.local"); got != "http://shared-garage.d8-sds-object.svc.internal.cluster.local:3900" {
		t.Errorf("s3Endpoint=%q", got)
	}
	if got := adminEndpoint(c, "d8-sds-object", "cluster.local"); got != "http://shared-garage.d8-sds-object.svc.cluster.local:3903" {
		t.Errorf("adminEndpoint=%q", got)
	}
	if l := commonLabels(c); l["storage.deckhouse.io/object-storage-cluster"] != "shared" {
		t.Errorf("commonLabels missing cluster label: %v", l)
	}
}

func TestBucketAndKeyNames(t *testing.T) {
	def := &v1alpha1.ObjectBucket{ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "app"}}
	if got := bucketDisplayName(def); got != "data" {
		t.Errorf("bucketDisplayName(default)=%q, want data", got)
	}
	explicit := &v1alpha1.ObjectBucket{
		ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "app"},
		Spec:       v1alpha1.ObjectBucketSpec{BucketName: "custom"},
	}
	if got := bucketDisplayName(explicit); got != "custom" {
		t.Errorf("bucketDisplayName(explicit)=%q, want custom", got)
	}
	if got := keyDisplayName(explicit, "custom"); got != "app.custom" {
		t.Errorf("keyDisplayName=%q, want app.custom", got)
	}
}

func TestLayoutTotalCapacity(t *testing.T) {
	if got := layoutTotalCapacity(&clusterLayout{}); got != nil {
		t.Errorf("layoutTotalCapacity(empty)=%v, want nil", got)
	}
	a := int64(1 << 30)
	b := int64(2 << 30)
	total := layoutTotalCapacity(&clusterLayout{Roles: []layoutRole{
		{ID: "a", Capacity: &a},
		{ID: "b", Capacity: &b},
		{ID: "c", Capacity: nil}, // gateway node, no capacity
	}})
	if total == nil || total.Value() != a+b {
		t.Errorf("layoutTotalCapacity=%v, want %d", total, a+b)
	}
}
