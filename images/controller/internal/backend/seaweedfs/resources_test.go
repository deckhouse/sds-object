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
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

func cluster(name string) *v1alpha1.ObjectStorageCluster {
	return &v1alpha1.ObjectStorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.ObjectStorageClusterSpec{Type: v1alpha1.ClusterTypeFull},
	}
}

func TestNamesAndEndpoint(t *testing.T) {
	c := cluster("media")
	if got := resourceName(c); got != "media-seaweedfs" {
		t.Errorf("resourceName=%q", got)
	}
	if got := s3Endpoint(c, "d8-sds-object", "internal.cluster.local"); got != "http://media-seaweedfs.d8-sds-object.svc.internal.cluster.local:8333" {
		t.Errorf("s3Endpoint=%q", got)
	}
	if l := commonLabels(c); l["app.kubernetes.io/name"] != "seaweedfs" {
		t.Errorf("commonLabels: %v", l)
	}
}

func TestStorageSizeAndClass(t *testing.T) {
	c := cluster("media")
	if got := storageSize(c); got.Cmp(resource.MustParse("10Gi")) != 0 {
		t.Errorf("storageSize(unset)=%s, want 10Gi", got.String())
	}
	c.Spec.Storage = &v1alpha1.ObjectStorageClusterStorageSpec{Size: "2Ti", Class: "fast"}
	if got := storageSize(c); got.Cmp(resource.MustParse("2Ti")) != 0 {
		t.Errorf("storageSize=%s, want 2Ti", got.String())
	}
	if got := storageClass(c); got != "fast" {
		t.Errorf("storageClass=%q, want fast", got)
	}
}

func TestBuildStatefulSet(t *testing.T) {
	c := cluster("media")
	c.Spec.Storage = &v1alpha1.ObjectStorageClusterStorageSpec{Size: "20Gi", Class: "fast"}
	sts := buildStatefulSet(c, "d8-sds-object", "registry/seaweedfs:test")

	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 1 {
		t.Errorf("MVP must be single-replica, got %v", sts.Spec.Replicas)
	}
	if len(sts.Spec.VolumeClaimTemplates) != 1 {
		t.Fatalf("expected one PVC template, got %d", len(sts.Spec.VolumeClaimTemplates))
	}
	if sc := sts.Spec.VolumeClaimTemplates[0].Spec.StorageClassName; sc == nil || *sc != "fast" {
		t.Errorf("PVC storageClass=%v, want fast", sc)
	}
	sec := sts.Spec.Template.Spec.SecurityContext
	if sec == nil || sec.RunAsUser == nil || *sec.RunAsUser != 0 {
		t.Errorf("data plane must run as root, got %+v", sec)
	}
}
