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
	"testing"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

func TestResolveBackendType(t *testing.T) {
	cases := []struct {
		in      v1alpha1.ClusterType
		want    v1alpha1.BackendType
		wantErr bool
	}{
		{v1alpha1.ClusterTypeSystem, v1alpha1.BackendGarage, false},
		{v1alpha1.ClusterTypeLightweight, v1alpha1.BackendGarage, false},
		{v1alpha1.ClusterTypeFull, v1alpha1.BackendSeaweedFS, false},
		{v1alpha1.ClusterTypeHeavy, v1alpha1.BackendCephRGW, false},
		{v1alpha1.ClusterType("Bogus"), "", true},
	}
	for _, c := range cases {
		got, err := ResolveBackendType(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ResolveBackendType(%q): err=%v, wantErr=%v", c.in, err, c.wantErr)
		}
		if got != c.want {
			t.Errorf("ResolveBackendType(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

func TestRegistryFor(t *testing.T) {
	reg := DefaultRegistry()

	cluster := &v1alpha1.ObjectStore{
		Spec: v1alpha1.ObjectStoreSpec{Type: v1alpha1.ClusterTypeLightweight},
	}
	d, err := reg.For(cluster)
	if err != nil {
		t.Fatalf("For(Lightweight): unexpected error %v", err)
	}
	if d.Type() != v1alpha1.BackendGarage {
		t.Errorf("For(Lightweight).Type()=%q, want %q", d.Type(), v1alpha1.BackendGarage)
	}

	bad := &v1alpha1.ObjectStore{
		Spec: v1alpha1.ObjectStoreSpec{Type: v1alpha1.ClusterType("Bogus")},
	}
	if _, err := reg.For(bad); err == nil {
		t.Errorf("For(Bogus): expected error, got nil")
	}
}

func TestRequestedFeatures(t *testing.T) {
	cases := []struct {
		name string
		spec v1alpha1.BucketSpec
		want []string
	}{
		{"none", v1alpha1.BucketSpec{}, nil},
		{"empty quota is not a request", v1alpha1.BucketSpec{Quota: &v1alpha1.BucketQuota{}}, nil},
		{"quota maxSize", v1alpha1.BucketSpec{Quota: &v1alpha1.BucketQuota{MaxSize: "1Gi"}}, []string{FeatureQuota}},
		{"quota maxObjects", v1alpha1.BucketSpec{Quota: &v1alpha1.BucketQuota{MaxObjects: 10}}, []string{FeatureQuota}},
		{"public read", v1alpha1.BucketSpec{AccessPolicy: v1alpha1.AccessPolicyPublicRead}, []string{FeaturePublicRead}},
		{"private is not a request", v1alpha1.BucketSpec{AccessPolicy: v1alpha1.AccessPolicyPrivate}, nil},
		{"both", v1alpha1.BucketSpec{
			Quota:        &v1alpha1.BucketQuota{MaxSize: "1Gi"},
			AccessPolicy: v1alpha1.AccessPolicyPublicRead,
		}, []string{FeatureQuota, FeaturePublicRead}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := RequestedFeatures(&v1alpha1.Bucket{Spec: c.spec})
			if len(got) != len(c.want) {
				t.Fatalf("RequestedFeatures=%v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("RequestedFeatures[%d]=%q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestNotImplementedDriver(t *testing.T) {
	d := NotImplementedDriver{BackendType: v1alpha1.BackendSeaweedFS}
	if d.Type() != v1alpha1.BackendSeaweedFS {
		t.Errorf("Type()=%q, want %q", d.Type(), v1alpha1.BackendSeaweedFS)
	}

	st, err := d.EnsureCluster(context.Background(), &v1alpha1.ObjectStore{})
	if err != nil {
		t.Fatalf("EnsureCluster: unexpected error %v", err)
	}
	if st.Ready {
		t.Errorf("EnsureCluster: stub must not report Ready")
	}
	if st.Message == "" {
		t.Errorf("EnsureCluster: stub must explain why it is not ready")
	}

	bs, err := d.EnsureBucket(context.Background(), &v1alpha1.ObjectStore{}, &v1alpha1.Bucket{})
	if err != nil {
		t.Fatalf("EnsureBucket: unexpected error %v", err)
	}
	if bs.Ready {
		t.Errorf("EnsureBucket: stub must not report Ready")
	}
}
