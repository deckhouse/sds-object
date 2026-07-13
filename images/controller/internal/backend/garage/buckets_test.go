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
	"testing"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
	"github.com/deckhouse/sds-object/images/controller/internal/backend"
)

func TestGarageBucketQuotas(t *testing.T) {
	t.Run("nil quota clears limits", func(t *testing.T) {
		q, err := garageBucketQuotas(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if q.MaxSize != nil || q.MaxObjects != nil {
			t.Errorf("expected both limits nil, got %+v", q)
		}
	})

	t.Run("maxSize converts binary Quantity to bytes", func(t *testing.T) {
		q, err := garageBucketQuotas(&v1alpha1.BucketQuota{MaxSize: "10Gi"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if q.MaxSize == nil || *q.MaxSize != 10*1024*1024*1024 {
			t.Errorf("MaxSize=%v, want %d", q.MaxSize, 10*1024*1024*1024)
		}
		if q.MaxObjects != nil {
			t.Errorf("MaxObjects=%v, want nil", q.MaxObjects)
		}
	})

	t.Run("maxObjects passes through", func(t *testing.T) {
		q, err := garageBucketQuotas(&v1alpha1.BucketQuota{MaxObjects: 1000})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if q.MaxObjects == nil || *q.MaxObjects != 1000 {
			t.Errorf("MaxObjects=%v, want 1000", q.MaxObjects)
		}
	})

	t.Run("invalid maxSize errors", func(t *testing.T) {
		if _, err := garageBucketQuotas(&v1alpha1.BucketQuota{MaxSize: "not-a-size"}); err == nil {
			t.Errorf("expected error for invalid maxSize")
		}
	})
}

func TestGarageUnsupported(t *testing.T) {
	t.Run("quota is enforced, not reported", func(t *testing.T) {
		b := &v1alpha1.Bucket{Spec: v1alpha1.BucketSpec{Quota: &v1alpha1.BucketQuota{MaxSize: "1Gi"}}}
		if got := garageUnsupported(b); len(got) != 0 {
			t.Errorf("unsupported=%v, want empty", got)
		}
	})

	t.Run("PublicRead is reported unsupported", func(t *testing.T) {
		b := &v1alpha1.Bucket{Spec: v1alpha1.BucketSpec{AccessPolicy: v1alpha1.AccessPolicyPublicRead}}
		got := garageUnsupported(b)
		if len(got) != 1 || got[0] != backend.FeaturePublicRead {
			t.Errorf("unsupported=%v, want [%s]", got, backend.FeaturePublicRead)
		}
	})
}
