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

package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return s
}

func policy(bucketRef string, names []string, patterns []string) *v1alpha1.BucketClaimPolicy {
	return &v1alpha1.BucketClaimPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec: v1alpha1.BucketClaimPolicySpec{
			BucketRef:         bucketRef,
			AllowedNamespaces: v1alpha1.NamespaceMatch{Names: names, Patterns: patterns},
		},
	}
}

func TestNamespaceAllowedForBucket(t *testing.T) {
	s := testScheme(t)

	t.Run("deny-by-default with no policy", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(s).Build()
		ok, _, err := namespaceAllowedForBucket(context.Background(), c, "shared", "team-a")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ok {
			t.Errorf("expected deny with no policy, got allow")
		}
	})

	t.Run("exact name match allows", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(policy("shared", []string{"team-a"}, nil)).Build()
		ok, _, err := namespaceAllowedForBucket(context.Background(), c, "shared", "team-a")
		if err != nil || !ok {
			t.Errorf("expected allow, got ok=%v err=%v", ok, err)
		}
	})

	t.Run("pattern match allows, anchored", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(policy("shared", nil, []string{"team-.*"})).Build()
		ok, _, err := namespaceAllowedForBucket(context.Background(), c, "shared", "team-a")
		if err != nil || !ok {
			t.Errorf("expected allow for team-a, got ok=%v err=%v", ok, err)
		}
		// Anchoring must reject a namespace that only contains the pattern.
		ok, _, err = namespaceAllowedForBucket(context.Background(), c, "shared", "x-team-a")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ok {
			t.Errorf("expected deny for x-team-a (anchored), got allow")
		}
	})

	t.Run("policy for a different bucket does not grant", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(policy("other", []string{"team-a"}, nil)).Build()
		ok, _, err := namespaceAllowedForBucket(context.Background(), c, "shared", "team-a")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ok {
			t.Errorf("expected deny (policy targets another bucket), got allow")
		}
	})
}

func TestAccessAuthorized(t *testing.T) {
	s := testScheme(t)

	greenfield := func(owner string) *v1alpha1.Bucket {
		return &v1alpha1.Bucket{ObjectMeta: metav1.ObjectMeta{
			Name: "claim-abc",
			Labels: map[string]string{
				v1alpha1.LabelBucketOrigin:          v1alpha1.BucketOriginBucketClaim,
				v1alpha1.LabelOwnedByClaimNamespace: owner,
			},
		}}
	}
	shared := &v1alpha1.Bucket{ObjectMeta: metav1.ObjectMeta{
		Name:   "shared",
		Labels: map[string]string{v1alpha1.LabelBucketOrigin: v1alpha1.BucketOriginShared},
	}}

	t.Run("greenfield: owner namespace authorized", func(t *testing.T) {
		r := &BucketAccessReconciler{APIReader: fake.NewClientBuilder().WithScheme(s).Build()}
		ok, _, err := r.accessAuthorized(context.Background(), greenfield("team-a"), "team-a")
		if err != nil || !ok {
			t.Errorf("expected allow, got ok=%v err=%v", ok, err)
		}
	})

	t.Run("greenfield: foreign namespace denied", func(t *testing.T) {
		r := &BucketAccessReconciler{APIReader: fake.NewClientBuilder().WithScheme(s).Build()}
		ok, reason, err := r.accessAuthorized(context.Background(), greenfield("team-a"), "team-b")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ok {
			t.Errorf("expected deny for foreign namespace, got allow")
		}
		if reason == "" {
			t.Errorf("expected a denial reason")
		}
	})

	t.Run("shared: gated by policy (deny-by-default)", func(t *testing.T) {
		r := &BucketAccessReconciler{APIReader: fake.NewClientBuilder().WithScheme(s).Build()}
		ok, _, err := r.accessAuthorized(context.Background(), shared, "team-a")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ok {
			t.Errorf("expected deny for shared bucket without policy, got allow")
		}
	})

	t.Run("shared: allowed with matching policy", func(t *testing.T) {
		r := &BucketAccessReconciler{APIReader: fake.NewClientBuilder().WithScheme(s).
			WithObjects(policy("shared", []string{"team-a"}, nil)).Build()}
		ok, _, err := r.accessAuthorized(context.Background(), shared, "team-a")
		if err != nil || !ok {
			t.Errorf("expected allow, got ok=%v err=%v", ok, err)
		}
	})
}
