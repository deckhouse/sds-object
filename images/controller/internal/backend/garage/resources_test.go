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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
	"github.com/deckhouse/sds-object/images/controller/internal/backend"
)

func cluster(name string, r v1alpha1.RedundancyMode) *v1alpha1.ObjectStore {
	return &v1alpha1.ObjectStore{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.ObjectStoreSpec{Type: v1alpha1.ClusterTypeLightweight, Redundancy: r},
	}
}

func TestReplicationFactor(t *testing.T) {
	cases := map[v1alpha1.RedundancyMode]int32{
		v1alpha1.RedundancyNone:     1,
		v1alpha1.RedundancyStandard: 3,
		v1alpha1.RedundancyHigh:     5,
		v1alpha1.RedundancyMode(""): 3, // default
	}
	for r, want := range cases {
		if got := replicationFactor(cluster("c", r)); got != want {
			t.Errorf("replicationFactor(%q)=%d, want %d", r, got, want)
		}
	}
}

func TestStorageSize(t *testing.T) {
	none := cluster("c", "")
	if got := storageSize(none); got.Cmp(resource.MustParse("10Gi")) != 0 {
		t.Errorf("storageSize(unset)=%s, want 10Gi", got.String())
	}

	sized := cluster("c", "")
	sized.Spec.Storage = &v1alpha1.ObjectStoreStorageSpec{SizePerNode: "20Gi"}
	if got := storageSize(sized); got.Cmp(resource.MustParse("20Gi")) != 0 {
		t.Errorf("storageSize(20Gi)=%s, want 20Gi", got.String())
	}

	bad := cluster("c", "")
	bad.Spec.Storage = &v1alpha1.ObjectStoreStorageSpec{SizePerNode: "not-a-quantity"}
	if got := storageSize(bad); got.Cmp(resource.MustParse("10Gi")) != 0 {
		t.Errorf("storageSize(invalid)=%s, want 10Gi fallback", got.String())
	}
}

func TestRenderConfig(t *testing.T) {
	cfg := renderConfig(5)
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

func TestClampRF(t *testing.T) {
	cases := []struct{ desired, nodes, want int32 }{
		{1, 1, 1},
		{3, 1, 1}, // one master -> single copy
		{3, 2, 2}, // two nodes -> rf=2 (valid Garage mode)
		{3, 3, 3},
		{5, 3, 3}, // High on three masters
		{5, 4, 4}, // no odd rounding
		{5, 5, 5},
		{3, 0, 1}, // floor
	}
	for _, c := range cases {
		if got := clampRF(c.desired, c.nodes); got != c.want {
			t.Errorf("clampRF(%d, %d)=%d, want %d", c.desired, c.nodes, got, c.want)
		}
	}
}

func TestReplicationFactorFromConfigMap(t *testing.T) {
	cm := buildConfigMap(cluster("shared", ""), "d8-sds-object", 2)
	if got := replicationFactorFromConfigMap(cm); got != 2 {
		t.Errorf("replicationFactorFromConfigMap=%d, want 2", got)
	}
	if got := replicationFactorFromConfigMap(nil); got != 0 {
		t.Errorf("replicationFactorFromConfigMap(nil)=%d, want 0", got)
	}
}

func TestNamesAndEndpoints(t *testing.T) {
	c := cluster("shared", "")
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
	if l := commonLabels(c); l["storage.deckhouse.io/object-store"] != "shared" {
		t.Errorf("commonLabels missing cluster label: %v", l)
	}
}

func TestBucketAndKeyNames(t *testing.T) {
	def := &v1alpha1.Bucket{ObjectMeta: metav1.ObjectMeta{Name: "data"}}
	if got := backend.BucketDisplayName(def); got != "data" {
		t.Errorf("BucketDisplayName(default)=%q, want data", got)
	}
	explicit := &v1alpha1.Bucket{
		ObjectMeta: metav1.ObjectMeta{Name: "data"},
		Spec:       v1alpha1.BucketSpec{BucketName: "custom"},
	}
	if got := backend.BucketDisplayName(explicit); got != "custom" {
		t.Errorf("BucketDisplayName(explicit)=%q, want custom", got)
	}
	access := &v1alpha1.BucketAccess{ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "app"}}
	if got := backend.AccessResourceName(access); got != "app.data" {
		t.Errorf("AccessResourceName=%q, want app.data", got)
	}
}

func systemCluster(name string) *v1alpha1.ObjectStore {
	return &v1alpha1.ObjectStore{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.ObjectStoreSpec{Type: v1alpha1.ClusterTypeSystem},
	}
}

func TestDesiredReplicas(t *testing.T) {
	if got := desiredReplicas(systemCluster("system")); got != systemReplicas {
		t.Errorf("desiredReplicas(System)=%d, want %d (fixed, master-count independent)", got, systemReplicas)
	}
	if got := desiredReplicas(cluster("lw", v1alpha1.RedundancyStandard)); got != 3 {
		t.Errorf("desiredReplicas(Lightweight Standard)=%d, want 3", got)
	}
	if got := desiredReplicas(cluster("lw", v1alpha1.RedundancyNone)); got != 1 {
		t.Errorf("desiredReplicas(Lightweight None)=%d, want 1", got)
	}
	nodes := int32(4)
	over := cluster("lw", v1alpha1.RedundancyStandard)
	over.Spec.Storage = &v1alpha1.ObjectStoreStorageSpec{Nodes: &nodes}
	if got := desiredReplicas(over); got != 4 {
		t.Errorf("desiredReplicas(Lightweight nodes=4)=%d, want 4", got)
	}
}

func TestBuildSystemStatefulSet(t *testing.T) {
	sts := buildSystemStatefulSet(systemCluster("system"), "d8-sds-object", "garage:v1", "hash")

	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != systemReplicas {
		t.Fatalf("replicas=%v, want %d (fixed, independent of master count)", sts.Spec.Replicas, systemReplicas)
	}
	if sts.Spec.PodManagementPolicy != appsv1.ParallelPodManagement {
		t.Errorf("PodManagementPolicy=%q, want Parallel", sts.Spec.PodManagementPolicy)
	}
	if sts.Spec.ServiceName != rpcSvcName(systemCluster("system")) {
		t.Errorf("ServiceName=%q, want %q", sts.Spec.ServiceName, rpcSvcName(systemCluster("system")))
	}

	// Node-sticky storage: a per-ordinal PVC on the managed local StorageClass,
	// no inline hostPath volume.
	if len(sts.Spec.VolumeClaimTemplates) != 1 {
		t.Fatalf("System must use one volumeClaimTemplate (local PV), got %d", len(sts.Spec.VolumeClaimTemplates))
	}
	vct := sts.Spec.VolumeClaimTemplates[0]
	if vct.Name != "data" {
		t.Errorf("volumeClaimTemplate name=%q, want data", vct.Name)
	}
	if vct.Spec.StorageClassName == nil || *vct.Spec.StorageClassName != systemLocalStorageClass {
		t.Errorf("volumeClaimTemplate storageClass=%v, want %q", vct.Spec.StorageClassName, systemLocalStorageClass)
	}

	spec := sts.Spec.Template.Spec
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == "data" {
			t.Errorf("System must not carry an inline data volume (PVC-backed): %+v", spec.Volumes[i])
		}
	}

	// Forced onto control-plane, tolerating master taints.
	if _, ok := spec.NodeSelector[controlPlaneNodeLabel]; !ok {
		t.Errorf("nodeSelector missing %q: %v", controlPlaneNodeLabel, spec.NodeSelector)
	}
	if len(spec.Tolerations) == 0 {
		t.Errorf("expected control-plane tolerations")
	}

	// Soft (preferred, not required) anti-affinity so all replicas still run on a
	// single master.
	if spec.Affinity == nil || spec.Affinity.PodAntiAffinity == nil {
		t.Fatalf("expected pod anti-affinity")
	}
	if len(spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 0 {
		t.Errorf("anti-affinity must be soft, found required terms (would block single-master scheduling)")
	}
	pref := spec.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution
	if len(pref) != 1 || pref[0].PodAffinityTerm.TopologyKey != hostnameTopologyKey {
		t.Errorf("preferred anti-affinity term=%+v, want topologyKey %q", pref, hostnameTopologyKey)
	}

	// Stable node identity: an optional identity-Secret volume plus a
	// restore-node-key initContainer that mounts both the data volume and the
	// identity Secret (so it can restore node_key before Garage starts).
	var idVol *corev1.Volume
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == "node-identity" {
			idVol = &spec.Volumes[i]
		}
	}
	if idVol == nil {
		t.Fatalf("expected a node-identity volume")
	}
	if idVol.Secret == nil || idVol.Secret.SecretName != nodeIdentitySecretName(systemCluster("system")) {
		t.Errorf("node-identity volume=%+v, want secret %q", idVol.VolumeSource, nodeIdentitySecretName(systemCluster("system")))
	}
	if idVol.Secret.Optional == nil || !*idVol.Secret.Optional {
		t.Errorf("node-identity secret must be optional (first boot has no identity yet)")
	}
	if len(spec.InitContainers) != 1 || spec.InitContainers[0].Name != "restore-node-key" {
		t.Fatalf("expected one restore-node-key initContainer, got %+v", spec.InitContainers)
	}
	init := spec.InitContainers[0]
	if init.Image != "garage:v1" {
		t.Errorf("initContainer image=%q, want garage:v1", init.Image)
	}
	mounts := map[string]string{}
	for _, m := range init.VolumeMounts {
		mounts[m.Name] = m.MountPath
	}
	if mounts["data"] != dataMountPath {
		t.Errorf("initContainer must mount data at %q, got %q", dataMountPath, mounts["data"])
	}
	if mounts["node-identity"] != nodeIdentityMountPath {
		t.Errorf("initContainer must mount node-identity at %q, got %q", nodeIdentityMountPath, mounts["node-identity"])
	}
}

func TestPodOrdinal(t *testing.T) {
	cases := []struct {
		name string
		want int32
		ok   bool
	}{
		{"system-garage-0", 0, true},
		{"system-garage-12", 12, true},
		{"system-garage", 0, false},
		{"system-garage-", 0, false},
		{"system-garage-x", 0, false},
	}
	for _, c := range cases {
		got, ok := podOrdinal(c.name)
		if ok != c.ok || got != c.want {
			t.Errorf("podOrdinal(%q)=(%d,%v), want (%d,%v)", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestBuildSystemLocalPV(t *testing.T) {
	pv := buildSystemLocalPV(systemCluster("system"), "master-0", 2)

	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		t.Errorf("reclaimPolicy=%q, want Retain (never wipe data)", pv.Spec.PersistentVolumeReclaimPolicy)
	}
	if pv.Spec.StorageClassName != systemLocalStorageClass {
		t.Errorf("storageClass=%q, want %q", pv.Spec.StorageClassName, systemLocalStorageClass)
	}
	if pv.Spec.HostPath == nil || pv.Spec.HostPath.Type == nil || *pv.Spec.HostPath.Type != corev1.HostPathDirectoryOrCreate {
		t.Fatalf("expected hostPath source with DirectoryOrCreate: %+v", pv.Spec.HostPath)
	}
	if pv.Labels[labelSystemLocalNode] != "master-0" {
		t.Errorf("node label=%q, want master-0", pv.Labels[labelSystemLocalNode])
	}

	// nodeAffinity pins the PV (hence the replica) to its node.
	na := pv.Spec.NodeAffinity
	if na == nil || na.Required == nil || len(na.Required.NodeSelectorTerms) != 1 {
		t.Fatalf("expected required nodeAffinity: %+v", na)
	}
	me := na.Required.NodeSelectorTerms[0].MatchExpressions
	if len(me) != 1 || me[0].Key != hostnameTopologyKey || me[0].Operator != corev1.NodeSelectorOpIn ||
		len(me[0].Values) != 1 || me[0].Values[0] != "master-0" {
		t.Errorf("nodeAffinity term=%+v, want %s In [master-0]", me, hostnameTopologyKey)
	}
}

func TestDesiredSystemLocalPVs(t *testing.T) {
	hostnames := []string{"master-0", "master-1", "master-2"}
	pvs := desiredSystemLocalPVs(systemCluster("system"), hostnames)

	want := len(hostnames) * int(systemReplicas)
	if len(pvs) != want {
		t.Fatalf("pool size=%d, want %d (systemReplicas per node)", len(pvs), want)
	}
	// Names are unique and deterministic.
	names := map[string]struct{}{}
	for _, pv := range pvs {
		if _, dup := names[pv.Name]; dup {
			t.Errorf("duplicate PV name %q", pv.Name)
		}
		names[pv.Name] = struct{}{}
	}
	// Every node carries systemReplicas PVs.
	perNode := map[string]int{}
	for _, pv := range pvs {
		perNode[pv.Labels[labelSystemLocalNode]]++
	}
	for _, h := range hostnames {
		if perNode[h] != int(systemReplicas) {
			t.Errorf("node %q has %d PVs, want %d", h, perNode[h], systemReplicas)
		}
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
