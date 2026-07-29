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

// Live integration test against a real SeaweedFS filer, run manually:
//
//	weed server -dir=/tmp/w -filer -s3 -ip=127.0.0.1
//	LIVE_FILER_GRPC=127.0.0.1:18888 go test ./internal/backend/seaweedfs/ -run TestLiveFiler -v
//
// Guarded by LIVE_FILER_GRPC, so CI always skips it. It exists because an
// in-process fake can only validate our expectations of the filer: the 4.39
// incompatibilities this package works around (HTTP-written entries the S3
// gateway cannot read, the gateway's migration of the legacy identity.json) were
// invisible to every unit test and cost several e2e cycles to locate.

import (
	"context"
	"os"
	"testing"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

func TestLiveFilerIdentityLifecycle(t *testing.T) {
	target := os.Getenv("LIVE_FILER_GRPC")
	if target == "" {
		t.Skip("set LIVE_FILER_GRPC (host:port of a live filer gRPC endpoint) to run")
	}
	ctx := context.Background()
	store := &filerEtcClient{target: target}

	_, found, err := store.readIdentity(ctx, "live-admin")
	if err != nil {
		t.Fatalf("initial read: %v", err)
	}
	t.Logf("initial read: found=%v", found)

	admin := &s3Identity{
		Name:        "live-admin",
		Credentials: []s3Credential{{AccessKey: "liveak", SecretKey: "livesk"}},
		Actions:     []string{actionAdmin},
	}
	if err := store.writeIdentity(ctx, admin); err != nil {
		t.Fatalf("write admin: %v", err)
	}

	app := &s3Identity{
		Name:        "app.live",
		Credentials: []s3Credential{{AccessKey: "appak", SecretKey: "appsk"}},
		Actions:     bucketActions("b", v1alpha1.AccessReadWrite),
	}
	if err := store.writeIdentity(ctx, app); err != nil {
		t.Fatalf("write app identity: %v", err)
	}

	// Overwrite (rotation shape) and read back.
	app.Credentials = []s3Credential{{AccessKey: "appak2", SecretKey: "appsk2"}}
	if err := store.writeIdentity(ctx, app); err != nil {
		t.Fatalf("rotate app identity: %v", err)
	}
	got, found, err := store.readIdentity(ctx, "app.live")
	if err != nil || !found {
		t.Fatalf("read back app: found=%v err=%v", found, err)
	}
	if got.Credentials[0].AccessKey != "appak2" {
		t.Fatalf("rotated key not read back: %+v", got.Credentials)
	}

	// The admin must be untouched by the app writes.
	gotAdmin, found, err := store.readIdentity(ctx, "live-admin")
	if err != nil || !found || !identityEqual(*gotAdmin, *admin) {
		t.Fatalf("admin identity disturbed: found=%v err=%v got=%+v", found, err, gotAdmin)
	}

	// Revocation: delete, confirm gone, idempotent.
	if err := store.deleteIdentity(ctx, "app.live"); err != nil {
		t.Fatalf("delete app identity: %v", err)
	}
	if _, found, err = store.readIdentity(ctx, "app.live"); err != nil || found {
		t.Fatalf("app identity must be gone: found=%v err=%v", found, err)
	}
	if err := store.deleteIdentity(ctx, "app.live"); err != nil {
		t.Fatalf("repeat delete: %v", err)
	}
	t.Logf("lifecycle complete: write, rotate, isolate, revoke all verified")
}
