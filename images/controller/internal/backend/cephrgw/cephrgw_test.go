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
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

func cephScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(cephObjectStoreGVK, &unstructured.Unstructured{})
	list := cephObjectStoreGVK
	list.Kind += "List"
	s.AddKnownTypeWithName(list, &unstructured.UnstructuredList{})
	return s
}

func existingStore(cluster *v1alpha1.ObjectStore) *unstructured.Unstructured {
	obj := newUnstructured(cephObjectStoreGVK)
	ns, name := objectStoreKey(cluster)
	obj.SetNamespace(ns)
	obj.SetName(name)
	return obj
}

// TestDeleteClusterRemovesStoreUnderBothPolicies pins the teardown contract. The
// reclaim policy is carried by the CR's preservePoolsOnDelete field, not by whether
// the CR survives, so Retain must delete it too: a CephObjectStore left behind is
// stranded — sds-elastic's webhook rejects vendored-Rook deletes from anyone it
// does not know, the garbage collector included — and while it exists Rook keeps
// the object store alive, so the ElasticCluster's finalizer never completes.
func TestDeleteClusterRemovesStoreUnderBothPolicies(t *testing.T) {
	for _, policy := range []v1alpha1.ClusterReclaimPolicy{
		v1alpha1.ClusterReclaimRetain,
		v1alpha1.ClusterReclaimDelete,
	} {
		cluster := heavy("main", v1alpha1.RedundancyNone)
		cluster.Spec.ReclaimPolicy = policy
		cluster.Spec.ElasticClusterRef = "ec"

		store := existingStore(cluster)
		c := fake.NewClientBuilder().WithScheme(cephScheme(t)).WithObjects(store).Build()
		d := &Driver{client: c, apiReader: c}

		if err := d.DeleteCluster(context.Background(), cluster); err != nil {
			t.Fatalf("%s: DeleteCluster: %v", policy, err)
		}

		ns, name := objectStoreKey(cluster)
		err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, newUnstructured(cephObjectStoreGVK))
		if !apierrors.IsNotFound(err) {
			t.Errorf("%s: CephObjectStore must be deleted, got err=%v", policy, err)
		}
	}
}

// TestDeleteClusterIsIdempotent covers the repeat pass: reconcileDelete may run
// again before the finalizer is released, and an already-absent store is not an
// error.
func TestDeleteClusterIsIdempotent(t *testing.T) {
	cluster := heavy("main", v1alpha1.RedundancyNone)
	cluster.Spec.ReclaimPolicy = v1alpha1.ClusterReclaimRetain
	c := fake.NewClientBuilder().WithScheme(cephScheme(t)).Build()
	d := &Driver{client: c, apiReader: c}

	if err := d.DeleteCluster(context.Background(), cluster); err != nil {
		t.Errorf("deleting an absent store must succeed, got %v", err)
	}
}

// TestPreservePoolsFollowsReclaimPolicy is the other half of the contract: with the
// CR now always deleted, preservePoolsOnDelete is the only thing standing between
// Retain and the destruction of every bucket's data.
func TestPreservePoolsFollowsReclaimPolicy(t *testing.T) {
	cases := map[v1alpha1.ClusterReclaimPolicy]bool{
		v1alpha1.ClusterReclaimRetain: true,
		v1alpha1.ClusterReclaimDelete: false,
		"":                            true, // unset behaves as the CRD default, Retain
	}
	for policy, want := range cases {
		cluster := heavy("main", v1alpha1.RedundancyNone)
		cluster.Spec.ReclaimPolicy = policy

		obj := buildCephObjectStore(cluster)
		got, found, err := unstructured.NestedBool(obj.Object, "spec", "preservePoolsOnDelete")
		if err != nil || !found {
			t.Fatalf("%q: preservePoolsOnDelete must be set explicitly (found=%v err=%v)", policy, found, err)
		}
		if got != want {
			t.Errorf("%q: preservePoolsOnDelete=%v, want %v", policy, got, want)
		}
	}
}
