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
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

func TestBucketActions(t *testing.T) {
	got := bucketActions("media", v1alpha1.AccessReadWrite)
	want := []string{"Read:media", "Write:media", "List:media", "Tagging:media"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bucketActions=%v, want %v", got, want)
	}
}

func TestIdentityConfigUpsert(t *testing.T) {
	cfg := &identityConfig{}

	id := s3Identity{
		Name:        "ns.bucket",
		Credentials: []s3Credential{{AccessKey: "ak", SecretKey: "sk"}},
		Actions:     bucketActions("bucket", v1alpha1.AccessReadWrite),
	}

	if !cfg.upsert(id) {
		t.Fatal("upsert(new) should report a change")
	}
	if len(cfg.Identities) != 1 {
		t.Fatalf("expected 1 identity, got %d", len(cfg.Identities))
	}

	// Same content -> no change.
	if cfg.upsert(id) {
		t.Error("upsert(identical) should report no change")
	}
	if len(cfg.Identities) != 1 {
		t.Errorf("identity count must stay 1, got %d", len(cfg.Identities))
	}

	// Rotated secret -> change, still one entry (replaced by name).
	rotated := s3Identity{
		Name:        "ns.bucket",
		Credentials: []s3Credential{{AccessKey: "ak", SecretKey: "sk2"}},
		Actions:     bucketActions("bucket", v1alpha1.AccessReadWrite),
	}
	if !cfg.upsert(rotated) {
		t.Error("upsert(changed) should report a change")
	}
	if len(cfg.Identities) != 1 {
		t.Errorf("identity count must stay 1 after replace, got %d", len(cfg.Identities))
	}
	if cfg.Identities[0].Credentials[0].SecretKey != "sk2" {
		t.Errorf("secret key not updated: %q", cfg.Identities[0].Credentials[0].SecretKey)
	}
}

func TestIdentityConfigRemove(t *testing.T) {
	cfg := &identityConfig{Identities: []s3Identity{
		{Name: "admin", Actions: []string{actionAdmin}},
		{Name: "ns.bucket", Actions: bucketActions("bucket", v1alpha1.AccessReadWrite)},
	}}

	if !cfg.remove("ns.bucket") {
		t.Error("remove(existing) should report a change")
	}
	if len(cfg.Identities) != 1 || cfg.Identities[0].Name != "admin" {
		t.Errorf("admin identity must remain, got %+v", cfg.Identities)
	}
	if cfg.remove("ns.bucket") {
		t.Error("remove(absent) should report no change")
	}
}

func TestIdentityLockPerCluster(t *testing.T) {
	d := &Driver{}
	a := &v1alpha1.ObjectStore{ObjectMeta: metav1.ObjectMeta{Name: "a"}}
	b := &v1alpha1.ObjectStore{ObjectMeta: metav1.ObjectMeta{Name: "b"}}
	if d.identityLock(a) != d.identityLock(a) {
		t.Errorf("same cluster must return the same mutex")
	}
	if d.identityLock(a) == d.identityLock(b) {
		t.Errorf("different clusters must return different mutexes")
	}
}

// fakeFiler is an in-memory stand-in for the SeaweedFS filer identity.json
// endpoint used to exercise the read/write round-trip and the serialized
// mutateIdentities path.
func TestFilerIdentitiesRoundTrip(t *testing.T) {
	var stored []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if stored == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write(stored)
		case http.MethodPost:
			// The real filer reads the multipart "file" field; for the test we
			// just capture the raw body's JSON payload.
			b, _ := io.ReadAll(r.Body)
			// Extract the JSON object from the multipart body (between the first
			// '{' and the last '}').
			start, end := indexByte(b, '{'), lastIndexByte(b, '}')
			if start >= 0 && end >= start {
				stored = b[start : end+1]
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	f := newFilerClient(srv.URL)
	ctx := context.Background()

	cfg, err := f.readIdentities(ctx)
	if err != nil || len(cfg.Identities) != 0 {
		t.Fatalf("missing file must yield empty config: cfg=%v err=%v", cfg, err)
	}
	cfg.upsert(s3Identity{Name: "u", Credentials: []s3Credential{{AccessKey: "ak", SecretKey: "sk"}}, Actions: []string{"Read:b"}})
	if err := f.writeIdentities(ctx, cfg); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := f.readIdentities(ctx)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got.Identities) != 1 || got.Identities[0].Name != "u" || len(got.Identities[0].Credentials) != 1 || got.Identities[0].Credentials[0].AccessKey != "ak" {
		t.Errorf("round-trip mismatch: %+v", got.Identities)
	}
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func lastIndexByte(b []byte, c byte) int {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == c {
			return i
		}
	}
	return -1
}
