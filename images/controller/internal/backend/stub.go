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
	"fmt"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

// NotImplementedDriver is a placeholder Driver for a backend whose data-plane
// logic has not landed yet. EnsureCluster / EnsureBucket report a non-Ready
// state with an explanatory message (so the CR sits in InProgress with a clear
// reason rather than erroring), and the Delete methods are no-ops. Real drivers
// replace these one backend at a time.
type NotImplementedDriver struct {
	BackendType v1alpha1.BackendType
}

var _ Driver = NotImplementedDriver{}

func (d NotImplementedDriver) Type() v1alpha1.BackendType {
	return d.BackendType
}

func (d NotImplementedDriver) EnsureCluster(_ context.Context, _ *v1alpha1.ObjectStorageCluster) (ClusterState, error) {
	return ClusterState{
		Ready:   false,
		Message: fmt.Sprintf("backend %q is not implemented yet", d.BackendType),
		Backend: v1alpha1.BackendStatus{Type: d.BackendType},
	}, nil
}

func (d NotImplementedDriver) DeleteCluster(_ context.Context, _ *v1alpha1.ObjectStorageCluster) error {
	return nil
}

func (d NotImplementedDriver) EnsureBucket(_ context.Context, _ *v1alpha1.ObjectStorageCluster, _ *v1alpha1.ObjectBucket) (BucketState, error) {
	return BucketState{
		Ready:   false,
		Message: fmt.Sprintf("backend %q is not implemented yet", d.BackendType),
	}, nil
}

func (d NotImplementedDriver) DeleteBucket(_ context.Context, _ *v1alpha1.ObjectStorageCluster, _ *v1alpha1.ObjectBucket) error {
	return nil
}

// DefaultRegistry returns a Registry with a NotImplementedDriver for every
// backend type. As real drivers land they are passed to NewRegistry instead.
func DefaultRegistry() *Registry {
	return NewRegistry(
		NotImplementedDriver{BackendType: v1alpha1.BackendGarage},
		NotImplementedDriver{BackendType: v1alpha1.BackendSeaweedFS},
		NotImplementedDriver{BackendType: v1alpha1.BackendCephRGW},
	)
}
