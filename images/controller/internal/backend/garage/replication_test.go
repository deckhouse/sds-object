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

func TestPinnedReplicationFactor(t *testing.T) {
	s := rfScheme(t)
	cluster := rfCluster()
	ns := "d8-sds-object"

	configMap := func(data string) *corev1.ConfigMap {
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: configName(cluster), Namespace: ns},
			Data:       map[string]string{configFileName: data},
		}
	}

	t.Run("no ConfigMap: compute initial factor", func(t *testing.T) {
		d := &Driver{client: fake.NewClientBuilder().WithScheme(s).Build(), namespace: ns}
		rf, err := d.pinnedReplicationFactor(context.Background(), cluster)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rf != initialReplicationFactor(cluster) {
			t.Errorf("rf=%d, want initial %d", rf, initialReplicationFactor(cluster))
		}
	})

	t.Run("ConfigMap with valid factor: read it back", func(t *testing.T) {
		d := &Driver{client: fake.NewClientBuilder().WithScheme(s).
			WithObjects(configMap("replication_factor = 3\n")).Build(), namespace: ns}
		rf, err := d.pinnedReplicationFactor(context.Background(), cluster)
		if err != nil || rf != 3 {
			t.Errorf("rf=%d err=%v, want 3/nil", rf, err)
		}
	})

	t.Run("ConfigMap exists but factor unparseable: fail closed", func(t *testing.T) {
		d := &Driver{client: fake.NewClientBuilder().WithScheme(s).
			WithObjects(configMap("db_engine = \"lmdb\"\n")).Build(), namespace: ns}
		_, err := d.pinnedReplicationFactor(context.Background(), cluster)
		if err == nil {
			t.Errorf("expected fail-closed error when replication_factor is absent, got nil")
		}
	})
}
