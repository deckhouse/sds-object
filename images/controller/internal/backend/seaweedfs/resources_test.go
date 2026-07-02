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

package seaweedfs

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

func cluster(name string, r v1alpha1.RedundancyMode) *v1alpha1.ObjectStorageCluster {
	return &v1alpha1.ObjectStorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.ObjectStorageClusterSpec{Type: v1alpha1.ClusterTypeFull, Redundancy: r},
	}
}

func TestNamesAndEndpoint(t *testing.T) {
	c := cluster("media", "")
	if got := resourceName(c); got != "media-seaweedfs" {
		t.Errorf("resourceName=%q", got)
	}
	if got := componentName(c, compMaster); got != "media-seaweedfs-master" {
		t.Errorf("componentName=%q", got)
	}
	if got := s3Endpoint(c, "d8-sds-object", "internal.cluster.local"); got != "http://media-seaweedfs.d8-sds-object.svc.internal.cluster.local:8333" {
		t.Errorf("s3Endpoint=%q", got)
	}
}

func TestTopology(t *testing.T) {
	cases := []struct {
		r           v1alpha1.RedundancyMode
		masters     int32
		volumes     int32
		replication string
	}{
		{v1alpha1.RedundancySingle, 1, 1, "000"},
		{v1alpha1.RedundancyReplicated, 3, 3, "001"},
		{v1alpha1.RedundancyMode(""), 3, 3, "001"},
		{v1alpha1.RedundancyHighRedundancy, 3, 4, "002"},
	}
	for _, c := range cases {
		m, v, repl := topology(cluster("c", c.r))
		if m != c.masters || v != c.volumes || repl != c.replication {
			t.Errorf("topology(%q)=(%d,%d,%q), want (%d,%d,%q)", c.r, m, v, repl, c.masters, c.volumes, c.replication)
		}
	}
}

func TestMasterServers(t *testing.T) {
	// Single -> one peer.
	if got := masterServers(cluster("c", v1alpha1.RedundancySingle), "d8-sds-object"); got != "c-seaweedfs-master-0.c-seaweedfs-master.d8-sds-object:9333" {
		t.Errorf("single masterServers=%q", got)
	}
	// Replicated -> three comma-separated peers.
	got := masterServers(cluster("c", v1alpha1.RedundancyReplicated), "d8-sds-object")
	if n := strings.Count(got, ","); n != 2 {
		t.Errorf("replicated masterServers should have 3 peers, got %q", got)
	}
	if !strings.Contains(got, "c-seaweedfs-master-2.c-seaweedfs-master.d8-sds-object:9333") {
		t.Errorf("missing 3rd peer in %q", got)
	}
}

func TestBuildStatefulSets(t *testing.T) {
	c := cluster("media", v1alpha1.RedundancyHighRedundancy)
	c.Spec.Storage = &v1alpha1.ObjectStorageClusterStorageSpec{Size: "100Gi", Class: "fast"}

	master := buildMasterStatefulSet(c, "d8-sds-object", "img")
	if master.Spec.Replicas == nil || *master.Spec.Replicas != 3 {
		t.Errorf("master replicas=%v, want 3", master.Spec.Replicas)
	}
	if !argsContain(master, "-defaultReplication=002") {
		t.Errorf("master args missing defaultReplication: %v", master.Spec.Template.Spec.Containers[0].Args)
	}

	volume := buildVolumeStatefulSet(c, "d8-sds-object", "img")
	if volume.Spec.Replicas == nil || *volume.Spec.Replicas != 4 {
		t.Errorf("volume replicas=%v, want 4", volume.Spec.Replicas)
	}
	if sc := volume.Spec.VolumeClaimTemplates[0].Spec.StorageClassName; sc == nil || *sc != "fast" {
		t.Errorf("volume PVC class=%v, want fast", sc)
	}
	if got := volume.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(resource.MustParse("100Gi")) != 0 {
		t.Errorf("volume PVC size=%s, want 100Gi", got.String())
	}

	filer := buildFilerStatefulSet(c, "d8-sds-object", "img")
	// HighRedundancy -> 3 filer replicas (HA, shared Postgres metadata store).
	if filer.Spec.Replicas == nil || *filer.Spec.Replicas != 3 {
		t.Errorf("filer replicas=%v, want 3", filer.Spec.Replicas)
	}
	if argsContain(filer, "-s3.config=/etc/sw/seaweedfs_s3_config") {
		t.Errorf("filer must NOT use a static -s3.config (uses filer-stored IAM): %v", filer.Spec.Template.Spec.Containers[0].Args)
	}
	if !argsContain(filer, "-s3") {
		t.Errorf("filer must enable -s3")
	}
	// Stateless filer: no local data PVC; metadata lives in Postgres.
	if len(filer.Spec.VolumeClaimTemplates) != 0 {
		t.Errorf("filer must have no VolumeClaimTemplates, got %d", len(filer.Spec.VolumeClaimTemplates))
	}
	// The filer config must be mounted from a Secret (carries the DB password).
	var cfgFromSecret bool
	for _, v := range filer.Spec.Template.Spec.Volumes {
		if v.Name == "config" && v.Secret != nil && v.Secret.SecretName == filerConfigName(c) {
			cfgFromSecret = true
		}
	}
	if !cfgFromSecret {
		t.Errorf("filer config must be mounted from Secret %q: %+v", filerConfigName(c), filer.Spec.Template.Spec.Volumes)
	}
	if sc := master.Spec.Template.Spec.SecurityContext; sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != 0 {
		t.Errorf("data plane must run as root")
	}
}

func TestFilerReplicas(t *testing.T) {
	cases := []struct {
		r    v1alpha1.RedundancyMode
		want int32
	}{
		{v1alpha1.RedundancySingle, 1},
		{v1alpha1.RedundancyReplicated, 2},
		{v1alpha1.RedundancyMode(""), 2},
		{v1alpha1.RedundancyHighRedundancy, 3},
	}
	for _, c := range cases {
		if got := filerReplicas(cluster("c", c.r)); got != c.want {
			t.Errorf("filerReplicas(%q)=%d, want %d", c.r, got, c.want)
		}
	}
}

func TestBuildPostgres(t *testing.T) {
	c := cluster("media", v1alpha1.RedundancyHighRedundancy)
	c.Spec.Storage = &v1alpha1.ObjectStorageClusterStorageSpec{Class: "fast"}

	pg := buildPostgres(c, "d8-sds-object")
	if pg.GetName() != "media-seaweedfs-pg" {
		t.Errorf("pg name=%q", pg.GetName())
	}
	if pg.GroupVersionKind() != postgresGVK {
		t.Errorf("pg gvk=%v", pg.GroupVersionKind())
	}
	spec, _ := pg.Object["spec"].(map[string]interface{})
	if spec["type"] != "Cluster" {
		t.Errorf("HighRedundancy pg type=%v, want Cluster", spec["type"])
	}
	if cl, _ := spec["cluster"].(map[string]interface{}); cl["replication"] != "ConsistencyAndAvailability" {
		t.Errorf("HighRedundancy replication=%v", cl["replication"])
	}
	users, _ := spec["users"].([]interface{})
	u0, _ := users[0].(map[string]interface{})
	if u0["storeCredsToSecret"] != pgCredsSecretName(c) {
		t.Errorf("storeCredsToSecret=%v, want %q", u0["storeCredsToSecret"], pgCredsSecretName(c))
	}

	// Single -> a standalone instance.
	single := buildPostgres(cluster("c", v1alpha1.RedundancySingle), "d8-sds-object")
	if s, _ := single.Object["spec"].(map[string]interface{}); s["type"] != "Standalone" {
		t.Errorf("Single pg type=%v, want Standalone", s["type"])
	}
}

func TestRenderFilerToml(t *testing.T) {
	toml := renderFilerToml("d8ms-pg-x-rw", pgPort, "seaweedfs", "s3cr3t", "seaweedfs")
	for _, want := range []string{
		"[postgres2]",
		"enabled = true",
		`hostname = "d8ms-pg-x-rw"`,
		"port = 5432",
		`username = "seaweedfs"`,
		`password = "s3cr3t"`,
		`database = "seaweedfs"`,
		`sslmode = "require"`,
		// createTable is required by the postgres2 store and must keep a LITERAL
		// %s placeholder for SeaweedFS to fill with each table name at runtime.
		"createTable =",
		`CREATE TABLE IF NOT EXISTS "%s"`,
	} {
		if !strings.Contains(toml, want) {
			t.Errorf("filer.toml missing %q:\n%s", want, toml)
		}
	}
	// A stray %! means our Sprintf consumed SeaweedFS's %s placeholder (the exact
	// bug that made the filer send `%!(EXTRA ...)` to Postgres).
	if strings.Contains(toml, "%!") {
		t.Errorf("filer.toml contains a botched format verb:\n%s", toml)
	}
}

func TestBuildFilerConfigSecret(t *testing.T) {
	c := cluster("media", v1alpha1.RedundancyReplicated)
	s := buildFilerConfigSecret(c, "d8-sds-object", "toml-body")
	if s.Name != filerConfigName(c) {
		t.Errorf("secret name=%q, want %q", s.Name, filerConfigName(c))
	}
	if s.Type != corev1.SecretTypeOpaque {
		t.Errorf("secret type=%q", s.Type)
	}
	if s.StringData["filer.toml"] != "toml-body" {
		t.Errorf("filer.toml=%q", s.StringData["filer.toml"])
	}
}

func argsContain(sts *appsv1.StatefulSet, want string) bool {
	for _, a := range sts.Spec.Template.Spec.Containers[0].Args {
		if a == want {
			return true
		}
	}
	return false
}
