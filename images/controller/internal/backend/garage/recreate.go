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
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

// teardownSystemDataPlane removes everything that binds the System cluster to its
// current storage and Garage identities — the StatefulSet (hence its pods), the
// per-replica PVCs, the node-identity Secret and the local-PV pool — so the next
// reconcile can rebuild the cluster from scratch at the following incarnation. It
// is the only sanctioned way out of a pinned replication factor, used when the
// System replica count switches (see EnsureCluster).
//
// Deletion is asynchronous — a PVC keeps its protection finalizer until its pod
// is gone, and a Retain PV lingers until its PVC is gone — so every pass issues
// the deletes again and done is reported only once nothing is left. Being
// idempotent, an interrupted teardown simply continues on the next reconcile: the
// pinned factor in the ConfigMap is rewritten only after this returns done, so a
// controller restart mid-teardown still sees the mismatch and finishes the job.
//
// What it deliberately does NOT do: wipe the data on disk. hostPath directories
// are out of the controller's reach (no node agent) and Retain never destroys
// data, so the previous incarnation's directories stay behind as garbage while
// the new incarnation starts on fresh ones (see systemLocalPVPath). The
// rebuilt cluster is therefore EMPTY: buckets are recreated by the Bucket
// reconciler and access keys are re-issued (BucketAccess re-mints a key that
// vanished from the backend), but the stored objects are gone.
func (d *Driver) teardownSystemDataPlane(ctx context.Context, cluster *v1alpha1.ObjectStore) (bool, string, error) {
	remaining := 0

	sts := &appsv1.StatefulSet{}
	err := d.client.Get(ctx, client.ObjectKey{Namespace: d.namespace, Name: resourceName(cluster)}, sts)
	switch {
	case err == nil:
		remaining++
		// Background propagation explicitly: the pods must go for the replica PVCs
		// to lose their protection finalizer, so orphaning them would stall the
		// teardown.
		if err := d.deleteIfNotDeleting(ctx, sts, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
			return false, "", fmt.Errorf("delete statefulset: %w", err)
		}
	case !apierrors.IsNotFound(err):
		return false, "", fmt.Errorf("get statefulset: %w", err)
	}

	// The StatefulSet's volumeClaimTemplates carry the cluster labels, so the
	// replica PVCs are selectable (they outlive the StatefulSet by design).
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := d.apiReader.List(ctx, pvcs,
		client.InNamespace(d.namespace),
		client.MatchingLabels(commonLabels(cluster)),
	); err != nil {
		return false, "", fmt.Errorf("list replica pvcs: %w", err)
	}
	for i := range pvcs.Items {
		remaining++
		if err := d.deleteIfNotDeleting(ctx, &pvcs.Items[i]); err != nil {
			return false, "", fmt.Errorf("delete pvc %s: %w", pvcs.Items[i].Name, err)
		}
	}

	// The persisted Garage node identities belong to the old cluster: a fresh one
	// must generate its own, or its replicas would come up claiming node IDs of a
	// layout that no longer exists.
	identity := &corev1.Secret{}
	err = d.client.Get(ctx, client.ObjectKey{Namespace: d.namespace, Name: nodeIdentitySecretName(cluster)}, identity)
	switch {
	case err == nil:
		remaining++
		if err := d.deleteIfNotDeleting(ctx, identity); err != nil {
			return false, "", fmt.Errorf("delete node identity secret: %w", err)
		}
	case !apierrors.IsNotFound(err):
		return false, "", fmt.Errorf("get node identity secret: %w", err)
	}

	pvs := &corev1.PersistentVolumeList{}
	if err := d.apiReader.List(ctx, pvs,
		client.MatchingLabels{objectStoreLabel: cluster.Name},
		client.HasLabels{labelSystemLocalNode},
	); err != nil {
		return false, "", fmt.Errorf("list pool pvs: %w", err)
	}
	for i := range pvs.Items {
		remaining++
		if err := d.deleteIfNotDeleting(ctx, &pvs.Items[i]); err != nil {
			return false, "", fmt.Errorf("delete pool pv %s: %w", pvs.Items[i].Name, err)
		}
	}

	if remaining > 0 {
		return false, fmt.Sprintf(
			"recreating the System store for a replica-count switch: waiting for %d object(s) of the previous data plane to be removed",
			remaining), nil
	}
	return true, "", nil
}

// deleteIfNotDeleting issues a delete unless the object is already terminating,
// tolerating a concurrent removal. Re-issuing a delete on a terminating object is
// harmless but pointless: the teardown just waits for it to disappear.
func (d *Driver) deleteIfNotDeleting(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if obj.GetDeletionTimestamp() != nil {
		return nil
	}
	if err := d.client.Delete(ctx, obj, opts...); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
