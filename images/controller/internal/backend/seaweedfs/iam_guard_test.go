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

package seaweedfs

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
	"github.com/deckhouse/sds-object/images/controller/internal/backend"
)

// filesStub is an in-memory identityFiles fake for the driver-level behaviors.
// The wire protocol itself is covered by the live test (live_filer_test.go)
// against a real filer: an in-process protocol fake previously validated our
// expectations of the filer rather than the filer, and hid a 4.39
// incompatibility through several e2e cycles.
type filesStub struct {
	files   map[string]s3Identity
	writes  int
	deletes int
}

func newFilesStub() *filesStub { return &filesStub{files: map[string]s3Identity{}} }

func (s *filesStub) readIdentity(_ context.Context, name string) (*s3Identity, bool, error) {
	id, ok := s.files[name]
	if !ok {
		return nil, false, nil
	}
	cp := id
	return &cp, true, nil
}

func (s *filesStub) writeIdentity(_ context.Context, id *s3Identity) error {
	s.writes++
	s.files[id.Name] = *id
	return nil
}

func (s *filesStub) deleteIdentity(_ context.Context, name string) error {
	s.deletes++
	delete(s.files, name)
	return nil
}

// driverWithFiles wires a Driver whose identity store is the stub and whose admin
// Secret lives in a fake client.
func driverWithFiles(t *testing.T, stub *filesStub) (*Driver, *v1alpha1.ObjectStore) {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add corev1: %v", err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add v1alpha1: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(s).Build()
	d := &Driver{client: c, apiReader: c, namespace: "d8-sds-object", identityFilesOverride: stub}
	cluster := &v1alpha1.ObjectStore{
		ObjectMeta: metav1.ObjectMeta{Name: "full"},
		Spec:       v1alpha1.ObjectStoreSpec{Type: v1alpha1.ClusterTypeFull},
	}
	return d, cluster
}

func testAccess(name string) *v1alpha1.BucketAccess {
	return &v1alpha1.BucketAccess{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: name},
		Spec:       v1alpha1.BucketAccessSpec{Permission: v1alpha1.AccessReadWrite},
	}
}

// TestEnsureAccessScopedToOwnIdentity pins the property the per-identity layout
// exists for: issuing or re-issuing one access touches only that access's file.
// The predecessor design kept every identity in one shared file, where a single
// bad read of it turned the next write into a revocation of every issued
// credential — the failure the e2e runs kept hitting as "The access key ID you
// provided does not exist in our records".
func TestEnsureAccessScopedToOwnIdentity(t *testing.T) {
	ctx := context.Background()
	stub := newFilesStub()
	d, cluster := driverWithFiles(t, stub)
	bucket := &v1alpha1.Bucket{ObjectMeta: metav1.ObjectMeta{Name: "media"}}

	if err := d.ensureAdminIdentity(ctx, cluster); err != nil {
		t.Fatalf("ensureAdminIdentity: %v", err)
	}
	otherName := backend.AccessResourceName(testAccess("other"))
	stub.files[otherName] = s3Identity{Name: otherName, Credentials: []s3Credential{{AccessKey: "oak", SecretKey: "osk"}}}

	st, err := d.EnsureAccess(ctx, cluster, bucket, testAccess("mine"), false)
	if err != nil || !st.Ready {
		t.Fatalf("EnsureAccess: ready=%v err=%v", st.Ready, err)
	}
	if st.AccessKeyID == "" || st.SecretAccessKey == "" {
		t.Fatalf("credentials must be minted, got %q/%q", st.AccessKeyID, st.SecretAccessKey)
	}

	if got := stub.files[otherName].Credentials[0].AccessKey; got != "oak" {
		t.Errorf("another identity's credentials changed: %q", got)
	}
	if _, ok := stub.files[adminIdentityName]; !ok {
		t.Errorf("the admin identity vanished")
	}
	if len(stub.files) != 3 {
		t.Errorf("files=%d, want 3 (admin, other, mine)", len(stub.files))
	}
}

// TestEnsureAccessReusesAndRotates covers the mint decision: an existing pair is
// reused (the Secret's contract is a stable key until rotation), and mintFresh
// replaces it.
func TestEnsureAccessReusesAndRotates(t *testing.T) {
	ctx := context.Background()
	stub := newFilesStub()
	d, cluster := driverWithFiles(t, stub)
	bucket := &v1alpha1.Bucket{ObjectMeta: metav1.ObjectMeta{Name: "media"}}
	access := testAccess("mine")

	if err := d.ensureAdminIdentity(ctx, cluster); err != nil {
		t.Fatalf("ensureAdminIdentity: %v", err)
	}
	first, err := d.EnsureAccess(ctx, cluster, bucket, access, false)
	if err != nil {
		t.Fatalf("first EnsureAccess: %v", err)
	}
	again, err := d.EnsureAccess(ctx, cluster, bucket, access, false)
	if err != nil {
		t.Fatalf("second EnsureAccess: %v", err)
	}
	if again.AccessKeyID != first.AccessKeyID || again.SecretAccessKey != first.SecretAccessKey {
		t.Errorf("an existing pair must be reused, got %q -> %q", first.AccessKeyID, again.AccessKeyID)
	}

	rotated, err := d.EnsureAccess(ctx, cluster, bucket, access, true)
	if err != nil {
		t.Fatalf("rotate EnsureAccess: %v", err)
	}
	if rotated.AccessKeyID == first.AccessKeyID {
		t.Errorf("mintFresh must issue a new key")
	}
	if got := stub.files[backend.AccessResourceName(access)].Credentials[0].AccessKey; got != rotated.AccessKeyID {
		t.Errorf("the stored identity must carry the rotated key, got %q", got)
	}
}

// TestDeleteAccessRemovesOnlyOwnFile pins revocation: the access's file goes, the
// rest stay. On the legacy single-file layout revocation was broken outright —
// the gateway had already migrated the identity to its own file, which a legacy
// rewrite did not touch, so a "revoked" key kept working.
func TestDeleteAccessRemovesOnlyOwnFile(t *testing.T) {
	ctx := context.Background()
	stub := newFilesStub()
	d, cluster := driverWithFiles(t, stub)
	bucket := &v1alpha1.Bucket{ObjectMeta: metav1.ObjectMeta{Name: "media"}}
	access := testAccess("mine")

	if err := d.ensureAdminIdentity(ctx, cluster); err != nil {
		t.Fatalf("ensureAdminIdentity: %v", err)
	}
	if _, err := d.EnsureAccess(ctx, cluster, bucket, access, false); err != nil {
		t.Fatalf("EnsureAccess: %v", err)
	}

	if err := d.DeleteAccess(ctx, cluster, bucket, access); err != nil {
		t.Fatalf("DeleteAccess: %v", err)
	}
	if _, ok := stub.files[backend.AccessResourceName(access)]; ok {
		t.Errorf("the access's identity file must be deleted")
	}
	if _, ok := stub.files[adminIdentityName]; !ok {
		t.Errorf("the admin identity must survive a revocation")
	}

	// Idempotent.
	if err := d.DeleteAccess(ctx, cluster, bucket, access); err != nil {
		t.Errorf("repeat DeleteAccess: %v", err)
	}
}

// TestIdentityFileJSONMatchesGateway pins the on-disk encoding: the gateway
// decodes per-identity files with encoding/json against the generated
// iam_pb.Identity tags (snake_case). camelCase keys — the legacy identity.json
// convention — would silently read back as an identity with empty credentials.
func TestIdentityFileJSONMatchesGateway(t *testing.T) {
	raw, err := json.Marshal(s3Identity{
		Name:        "app.mine",
		Credentials: []s3Credential{{AccessKey: "ak", SecretKey: "sk"}},
		Actions:     []string{"Read:media"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var gatewayView struct {
		Name        string `json:"name"`
		Credentials []struct {
			AccessKey string `json:"access_key"`
			SecretKey string `json:"secret_key"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal(raw, &gatewayView); err != nil {
		t.Fatalf("unmarshal as the gateway: %v", err)
	}
	if len(gatewayView.Credentials) != 1 || gatewayView.Credentials[0].AccessKey != "ak" || gatewayView.Credentials[0].SecretKey != "sk" {
		t.Fatalf("the gateway would not see the credentials: %s", raw)
	}
}
