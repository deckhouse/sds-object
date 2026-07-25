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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

// teardownScheme carries the object kinds the System teardown touches.
func teardownScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add corev1: %v", err)
	}
	if err := appsv1.AddToScheme(s); err != nil {
		t.Fatalf("add appsv1: %v", err)
	}
	return s
}

// systemDataPlane is the full set of objects a running System cluster owns that
// the teardown has to remove.
func systemDataPlane(cluster *v1alpha1.ObjectStore, ns string) []client.Object {
	return []client.Object{
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
			Name: resourceName(cluster), Namespace: ns, Labels: commonLabels(cluster),
		}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: "data-" + resourceName(cluster) + "-0", Namespace: ns, Labels: commonLabels(cluster),
		}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: "data-" + resourceName(cluster) + "-1", Namespace: ns, Labels: commonLabels(cluster),
		}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: nodeIdentitySecretName(cluster), Namespace: ns, Labels: commonLabels(cluster),
		}},
		&corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{
			Name:   systemLocalPVName(cluster, "m0", firstSystemIncarnation, 0),
			Labels: map[string]string{objectStoreLabel: cluster.Name, labelSystemLocalNode: "m0"},
		}},
	}
}

func TestTeardownSystemDataPlane(t *testing.T) {
	ns := "d8-sds-object"
	cluster := systemCluster()
	ctx := context.Background()

	c := fake.NewClientBuilder().WithScheme(teardownScheme(t)).
		WithObjects(systemDataPlane(cluster, ns)...).Build()
	d := &Driver{client: c, apiReader: c, namespace: ns}

	// First pass: everything is still there, so it reports "not done" and says what
	// it is waiting for (the caller surfaces this as the cluster message).
	done, msg, err := d.teardownSystemDataPlane(ctx, cluster)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if done {
		t.Errorf("done=true on the pass that issued the deletes, want false")
	}
	if msg == "" {
		t.Errorf("expected a progress message while objects remain")
	}

	// The fake client deletes synchronously (no pods, no protection finalizers), so
	// the next pass finds an empty data plane.
	done, _, err = d.teardownSystemDataPlane(ctx, cluster)
	if err != nil {
		t.Fatalf("unexpected error on the second pass: %v", err)
	}
	if !done {
		t.Fatalf("done=false, want the teardown to complete once nothing is left")
	}

	for _, obj := range systemDataPlane(cluster, ns) {
		key := client.ObjectKeyFromObject(obj)
		if err := c.Get(ctx, key, obj); err == nil {
			t.Errorf("%T %q survived the teardown", obj, key.Name)
		}
	}

	// Idempotent on an already-empty data plane.
	if done, _, err := d.teardownSystemDataPlane(ctx, cluster); err != nil || !done {
		t.Errorf("teardown on an empty data plane: done=%v err=%v, want true/nil", done, err)
	}
}

// TestTeardownSystemDataPlaneKeepsOtherClusters guards the label selectors: a
// Lightweight cluster's objects must not be swept up by the System recreate.
func TestTeardownSystemDataPlaneKeepsOtherClusters(t *testing.T) {
	ns := "d8-sds-object"
	system := systemCluster()
	other := cluster("lw", v1alpha1.RedundancyStandard)
	ctx := context.Background()

	objs := append(systemDataPlane(system, ns), &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "data-" + resourceName(other) + "-0", Namespace: ns, Labels: commonLabels(other),
		},
	})
	c := fake.NewClientBuilder().WithScheme(teardownScheme(t)).WithObjects(objs...).Build()
	d := &Driver{client: c, apiReader: c, namespace: ns}

	if _, _, err := d.teardownSystemDataPlane(ctx, system); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	key := client.ObjectKey{Namespace: ns, Name: "data-" + resourceName(other) + "-0"}
	if err := c.Get(ctx, key, &corev1.PersistentVolumeClaim{}); err != nil {
		t.Errorf("the Lightweight cluster's PVC was deleted: %v", err)
	}
}
