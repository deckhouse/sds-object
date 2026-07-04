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
	"reflect"
	"testing"

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
