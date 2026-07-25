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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

func rfScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return s
}

func rfCluster() *v1alpha1.ObjectStore {
	return &v1alpha1.ObjectStore{
		ObjectMeta: metav1.ObjectMeta{Name: "lw"},
		Spec:       v1alpha1.ObjectStoreSpec{Type: v1alpha1.ClusterTypeLightweight, Redundancy: v1alpha1.RedundancyStandard},
	}
}

func TestPinnedState(t *testing.T) {
	s := rfScheme(t)
	cluster := rfCluster()
	ns := "d8-sds-object"

	configMap := func(data map[string]string) *corev1.ConfigMap {
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: configName(cluster), Namespace: ns},
			Data:       data,
		}
	}

	t.Run("no ConfigMap: compute initial factor, cluster not initialized yet", func(t *testing.T) {
		d := &Driver{client: fake.NewClientBuilder().WithScheme(s).Build(), namespace: ns}
		got, err := d.pinnedState(context.Background(), cluster)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.rf != initialReplicationFactor(cluster) {
			t.Errorf("rf=%d, want initial %d", got.rf, initialReplicationFactor(cluster))
		}
		if got.exists {
			t.Errorf("exists=true, want false (no ConfigMap means never initialized)")
		}
		if got.incarnation != firstSystemIncarnation {
			t.Errorf("incarnation=%d, want %d", got.incarnation, firstSystemIncarnation)
		}
	})

	t.Run("ConfigMap with valid factor: read it back", func(t *testing.T) {
		d := &Driver{client: fake.NewClientBuilder().WithScheme(s).
			WithObjects(configMap(map[string]string{configFileName: "replication_factor = 3\n"})).Build(), namespace: ns}
		got, err := d.pinnedState(context.Background(), cluster)
		if err != nil || got.rf != 3 || !got.exists {
			t.Errorf("state=%+v err=%v, want rf 3/exists true/nil", got, err)
		}
	})

	t.Run("ConfigMap exists but factor unparseable: fail closed", func(t *testing.T) {
		d := &Driver{client: fake.NewClientBuilder().WithScheme(s).
			WithObjects(configMap(map[string]string{configFileName: "db_engine = \"lmdb\"\n"})).Build(), namespace: ns}
		_, err := d.pinnedState(context.Background(), cluster)
		if err == nil {
			t.Errorf("expected fail-closed error when replication_factor is absent, got nil")
		}
	})

	t.Run("incarnation is read back", func(t *testing.T) {
		d := &Driver{client: fake.NewClientBuilder().WithScheme(s).WithObjects(configMap(map[string]string{
			configFileName:       "replication_factor = 1\n",
			systemIncarnationKey: "4",
		})).Build(), namespace: ns}
		got, err := d.pinnedState(context.Background(), cluster)
		if err != nil || got.incarnation != 4 {
			t.Errorf("incarnation=%d err=%v, want 4/nil", got.incarnation, err)
		}
	})
}

func TestSystemIncarnationFromConfigMap(t *testing.T) {
	sys := systemCluster()
	cases := map[string]struct {
		cm   *corev1.ConfigMap
		want int32
	}{
		"nil": {nil, firstSystemIncarnation},
		"absent (legacy)": {&corev1.ConfigMap{Data: map[string]string{configFileName: "replication_factor = 3\n"}},
			firstSystemIncarnation},
		"unparseable": {&corev1.ConfigMap{Data: map[string]string{systemIncarnationKey: "later"}}, firstSystemIncarnation},
		"below floor": {&corev1.ConfigMap{Data: map[string]string{systemIncarnationKey: "0"}}, firstSystemIncarnation},
		"recorded":    {&corev1.ConfigMap{Data: map[string]string{systemIncarnationKey: "7"}}, 7},
	}
	for name, c := range cases {
		if got := systemIncarnationFromConfigMap(c.cm); got != c.want {
			t.Errorf("systemIncarnationFromConfigMap(%s)=%d, want %d", name, got, c.want)
		}
	}

	// System records the counter; the PVC-backed profiles have no use for it.
	if cm := buildConfigMap(sys, "d8-sds-object", 1, 3); cm.Data[systemIncarnationKey] != "3" {
		t.Errorf("System ConfigMap incarnation=%q, want 3", cm.Data[systemIncarnationKey])
	}
	if cm := buildConfigMap(rfCluster(), "d8-sds-object", 3, 3); cm.Data[systemIncarnationKey] != "" {
		t.Errorf("Lightweight ConfigMap must not carry an incarnation, got %q", cm.Data[systemIncarnationKey])
	}
}
