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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
	"github.com/deckhouse/sds-object/images/controller/internal/backend"
	"github.com/deckhouse/sds-object/images/controller/pkg/config"
	"github.com/deckhouse/sds-object/images/controller/pkg/logger"
)

func statusScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return s
}

// TestUpdateStatusAdminSecretRef covers status.adminSecretRef: the CRD documents it
// as the pointer to the Secret holding the backend admin credentials, so a driver
// that reports one must have it published — and a driver that keeps its credentials
// elsewhere (Ceph RGW's live in the sds-elastic namespace, owned by Rook) must not
// have a stale value written or cleared.
func TestUpdateStatusAdminSecretRef(t *testing.T) {
	ctx := context.Background()
	store := &v1alpha1.ObjectStore{
		ObjectMeta: metav1.ObjectMeta{Name: "system", Generation: 2},
		Spec:       v1alpha1.ObjectStoreSpec{Type: v1alpha1.ClusterTypeSystem},
	}
	c := fake.NewClientBuilder().WithScheme(statusScheme(t)).
		WithObjects(store).WithStatusSubresource(store).Build()
	log, err := logger.NewLogger("0")
	if err != nil {
		t.Fatalf("build logger: %v", err)
	}
	r := &ObjectStoreReconciler{Client: c, Log: log}

	get := func() *v1alpha1.ObjectStore {
		t.Helper()
		out := &v1alpha1.ObjectStore{}
		if err := c.Get(ctx, client.ObjectKey{Name: store.Name}, out); err != nil {
			t.Fatalf("get store: %v", err)
		}
		return out
	}

	// A driver that reports its admin Secret gets it published.
	sb := newStatusBuilder(store.Generation)
	state := &backend.ClusterState{AdminSecretName: "system-garage-secrets"}
	if err := r.updateStatus(ctx, store, sb, state); err != nil {
		t.Fatalf("updateStatus: %v", err)
	}
	got := get()
	if got.Status == nil || got.Status.AdminSecretRef == nil {
		t.Fatalf("status.adminSecretRef was not published: %+v", got.Status)
	}
	if got.Status.AdminSecretRef.Name != "system-garage-secrets" {
		t.Errorf("adminSecretRef.name=%q, want system-garage-secrets", got.Status.AdminSecretRef.Name)
	}

	// A later pass with no name reported (a driver that keeps none, or a state
	// observed before the Secret existed) must not clear what was published.
	if err := r.updateStatus(ctx, store, newStatusBuilder(store.Generation), &backend.ClusterState{}); err != nil {
		t.Fatalf("updateStatus (empty): %v", err)
	}
	if ref := get().Status.AdminSecretRef; ref == nil || ref.Name != "system-garage-secrets" {
		t.Errorf("adminSecretRef=%+v, want it left in place", ref)
	}
}

// TestFinishRequeuesReadyCluster pins the resync: a Ready cluster must still be
// requeued. Its data plane changes without any API event the controller watches —
// restarted pods come back with new IPs and the Garage RPC mesh needs
// reconnecting, a recycled replica needs a fresh pool PV — so a Ready store that
// stops being reconciled sits at Ready while being unusable.
func TestFinishRequeuesReadyCluster(t *testing.T) {
	ctx := context.Background()
	store := &v1alpha1.ObjectStore{
		ObjectMeta: metav1.ObjectMeta{Name: "system", Generation: 1},
		Spec:       v1alpha1.ObjectStoreSpec{Type: v1alpha1.ClusterTypeSystem},
	}
	c := fake.NewClientBuilder().WithScheme(statusScheme(t)).
		WithObjects(store).WithStatusSubresource(store).Build()
	log, err := logger.NewLogger("0")
	if err != nil {
		t.Fatalf("build logger: %v", err)
	}
	r := &ObjectStoreReconciler{
		Client: c,
		Log:    log,
		Cfg:    &config.Options{RequeueInterval: 30 * time.Second},
	}

	sb := newStatusBuilder(store.Generation)
	for _, stage := range oscStageOrder {
		sb.setCondition(stage, metav1.ConditionTrue, reasonReady, "ready")
	}
	sb.setCondition(v1alpha1.ObjectStoreConditionReady, metav1.ConditionTrue, reasonReady, "All stages reconciled")

	res, err := r.finish(ctx, store, sb, &backend.ClusterState{Ready: true}, nil)
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if res.RequeueAfter != 30*time.Second {
		t.Errorf("RequeueAfter=%v, want the configured 30s resync even though the cluster is Ready", res.RequeueAfter)
	}
}
