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

package backend

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// errReader stands in for a reader whose List cannot serve the request — the
// shape of a cached client with no watch permission, which is exactly what
// DeleteClusterPVCs must not be handed.
type errReader struct {
	client.Reader
	err error
}

func (r errReader) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return r.err
}

func pvcScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return s
}

func pvc(name, namespace string, labels map[string]string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
	}
}

func TestDeleteClusterPVCs(t *testing.T) {
	ctx := context.Background()
	ns := "d8-sds-object"
	labels := map[string]string{"storage.deckhouse.io/object-store": "lw"}
	other := map[string]string{"storage.deckhouse.io/object-store": "another"}

	c := fake.NewClientBuilder().WithScheme(pvcScheme(t)).WithObjects(
		pvc("data-lw-garage-0", ns, labels),
		pvc("data-lw-garage-1", ns, labels),
		pvc("data-another-garage-0", ns, other),
		pvc("data-lw-garage-0", "other-ns", labels),
	).Build()

	if err := DeleteClusterPVCs(ctx, c, c, ns, labels); err != nil {
		t.Fatalf("DeleteClusterPVCs: %v", err)
	}

	gone := func(name, namespace string) bool {
		err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &corev1.PersistentVolumeClaim{})
		return apierrors.IsNotFound(err)
	}
	for _, name := range []string{"data-lw-garage-0", "data-lw-garage-1"} {
		if !gone(name, ns) {
			t.Errorf("PVC %s must be deleted", name)
		}
	}
	if gone("data-another-garage-0", ns) {
		t.Errorf("another cluster's PVC must be left alone")
	}
	if gone("data-lw-garage-0", "other-ns") {
		t.Errorf("a PVC in another namespace must be left alone")
	}

	// Idempotent: nothing left to delete is not an error.
	if err := DeleteClusterPVCs(ctx, c, c, ns, labels); err != nil {
		t.Errorf("second pass: %v", err)
	}
}

// TestDeleteClusterPVCsListError pins the reader contract: the listing must go
// through the reader passed in (the drivers hand it the non-cached API reader,
// because a cached List of PVCs would block forever on a cache that can never
// sync without the watch verb), and a failure there must surface as an error
// rather than be swallowed into "nothing to delete".
func TestDeleteClusterPVCsListError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(pvcScheme(t)).Build()
	want := errors.New("forbidden")

	err := DeleteClusterPVCs(context.Background(), errReader{err: want}, c, "d8-sds-object", nil)
	if !errors.Is(err, want) {
		t.Fatalf("error=%v, want it to wrap %v", err, want)
	}
}
