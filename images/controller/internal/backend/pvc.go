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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DeleteClusterPVCs deletes every PVC in the given namespace carrying the given
// labels. StatefulSet volumeClaimTemplates PVCs are not garbage-collected by
// Kubernetes, so PVC-backed drivers call this to honour a Delete reclaim
// policy. Idempotent.
func DeleteClusterPVCs(ctx context.Context, c client.Client, namespace string, labels map[string]string) error {
	list := &corev1.PersistentVolumeClaimList{}
	if err := c.List(ctx, list, client.InNamespace(namespace), client.MatchingLabels(labels)); err != nil {
		return fmt.Errorf("list cluster PVCs: %w", err)
	}
	for i := range list.Items {
		if err := c.Delete(ctx, &list.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete PVC %s/%s: %w", list.Items[i].Namespace, list.Items[i].Name, err)
		}
	}
	return nil
}
