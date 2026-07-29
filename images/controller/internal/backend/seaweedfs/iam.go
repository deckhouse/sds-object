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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

// SeaweedFS 4.39 stores its S3 IAM state as one file PER IDENTITY under the
// filer's /etc/iam/identities/ directory; the S3 gateway watches that directory
// and reloads on every change. The single legacy /etc/iam/identity.json is
// read-once compatibility input: the first gateway that sees it MIGRATES it (each
// identity rewritten as its own file, the legacy file renamed to
// identity.json.old), and the multi-file layout takes priority from then on.
//
// This driver therefore manages per-identity files directly — the same layout and
// JSON encoding the gateway's own credential manager writes. Managing the legacy
// file instead cannot work on 4.39:
//
//   - every write races the gateway's migration, which renames the file away;
//   - revocation is broken outright: an identity removed from the legacy file has
//     already been migrated, and the per-identity copy keeps working;
//   - and the shared file made every mutation a read-modify-write, where one bad
//     read could clobber every issued credential. One file per identity removes
//     that hazard structurally: the driver only ever touches the identity it owns.
//
// Transport is the filer gRPC API with the payload in the entry's inline
// `content` field — the only representation the gateway reads (its loader uses
// ReadInsideFiler, which returns Entry.Content and never file chunks; both filer
// HTTP upload paths on 4.39 produce entries the gateway or the driver itself
// cannot read back). Verified against a stock 4.39 `weed server -filer -s3`; see
// live_filer_test.go.
const (
	iamIdentitiesDir = "/etc/iam/identities"
)

// SeaweedFS S3 actions.
const (
	actionAdmin   = "Admin"
	actionRead    = "Read"
	actionWrite   = "Write"
	actionList    = "List"
	actionTagging = "Tagging"
)

// s3Credential is one access key / secret key pair. The JSON field names are the
// generated iam_pb.Identity ones (snake_case): the gateway's credential manager
// decodes per-identity files with encoding/json against those tags, so camelCase
// keys would silently read back as empty credentials.
type s3Credential struct {
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

// s3Identity is one IAM identity (user), stored as its own file
// <name>.json under iamIdentitiesDir.
type s3Identity struct {
	Name        string         `json:"name"`
	Credentials []s3Credential `json:"credentials,omitempty"`
	Actions     []string       `json:"actions"`
}

func identityEqual(a, b s3Identity) bool {
	if a.Name != b.Name || len(a.Credentials) != len(b.Credentials) || len(a.Actions) != len(b.Actions) {
		return false
	}
	for i := range a.Credentials {
		if a.Credentials[i] != b.Credentials[i] {
			return false
		}
	}
	for i := range a.Actions {
		if a.Actions[i] != b.Actions[i] {
			return false
		}
	}
	return true
}

// bucketActions returns the per-bucket action set granted to a bucket user for
// the given permission. ReadOnly grants read/list only; ReadWrite adds write
// and tagging.
func bucketActions(bucket string, perm v1alpha1.AccessPermission) []string {
	if perm == v1alpha1.AccessReadOnly {
		return []string{
			actionRead + ":" + bucket,
			actionList + ":" + bucket,
		}
	}
	return []string{
		actionRead + ":" + bucket,
		actionWrite + ":" + bucket,
		actionList + ":" + bucket,
		actionTagging + ":" + bucket,
	}
}

// identityFiles is the per-identity file store, one implementation talking to the
// filer and one in-memory fake for the driver-level tests. All operations are
// scoped to a single identity: nothing here can affect another identity's file.
type identityFiles interface {
	readIdentity(ctx context.Context, name string) (*s3Identity, bool, error)
	writeIdentity(ctx context.Context, id *s3Identity) error
	deleteIdentity(ctx context.Context, name string) error
}

// identityFilesFor returns the identity store for the cluster's filer.
func (d *Driver) identityFilesFor(cluster *v1alpha1.ObjectStore) identityFiles {
	if d.identityFilesOverride != nil {
		return d.identityFilesOverride
	}
	return &filerEtcClient{target: filerGRPCTarget(cluster, d.namespace, d.clusterDomain)}
}

// filerEtcClient implements identityFiles over the filer gRPC API. The raw-codec
// protowire approach (and rawCodec itself) is shared with the quota code in
// quota.go, for the same reason documented there: a handful of hand-encoded
// fields is not worth vendoring the SeaweedFS module.
type filerEtcClient struct {
	target string // host:port of the filer gRPC endpoint
}

// SeaweedFiler methods and protobuf field numbers used here (Lookup* constants
// are shared with quota.go).
const (
	filerSvcCreateEntry = "/filer_pb.SeaweedFiler/CreateEntry"
	filerSvcDeleteEntry = "/filer_pb.SeaweedFiler/DeleteEntry"

	fieldEntryName       = 1 // Entry.name
	fieldEntryAttributes = 4 // Entry.attributes (FuseAttributes)
	fieldEntryContent    = 9 // Entry.content (inline file content)

	fieldAttrFileSize = 1 // FuseAttributes.file_size
	fieldAttrMtime    = 2 // FuseAttributes.mtime (unix seconds)
	fieldAttrFileMode = 3 // FuseAttributes.file_mode
	fieldAttrCrtime   = 6 // FuseAttributes.crtime (unix seconds)

	fieldCreateReqDirectory = 1 // CreateEntryRequest.directory (same in UpdateEntryRequest)
	fieldCreateReqEntry     = 2 // CreateEntryRequest.entry (same in UpdateEntryRequest)
	fieldCreateRespError    = 1 // CreateEntryResponse.error (in-band, string)

	fieldDeleteReqDirectory = 1 // DeleteEntryRequest.directory
	fieldDeleteReqName      = 2 // DeleteEntryRequest.name
	fieldDeleteRespError    = 1 // DeleteEntryResponse.error (in-band, string)
	// UpdateEntryResponse carries no in-band error (its field 1 is a metadata
	// event); UpdateEntry failures arrive as gRPC status errors.
)

// filerNotFoundMessage is the error filer_pb.ErrNotFound serializes to on the
// wire; LookupDirectoryEntry (and DeleteEntry of an absent file) report it.
const filerNotFoundMessage = "no entry is found in filer store"

// invoke performs one raw-codec call against the filer gRPC endpoint.
func (f *filerEtcClient) invoke(ctx context.Context, method string, req []byte) ([]byte, error) {
	conn, err := grpc.NewClient(f.target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial filer gRPC %s: %w", f.target, err)
	}
	defer func() { _ = conn.Close() }()
	var resp []byte
	if err := conn.Invoke(ctx, method, req, &resp, grpc.ForceCodec(rawCodec{})); err != nil {
		return nil, err
	}
	return resp, nil
}

// lookupIdentityEntry fetches the raw Entry bytes of one identity file;
// found=false for an absent entry.
func (f *filerEtcClient) lookupIdentityEntry(ctx context.Context, name string) (entry []byte, found bool, err error) {
	req := appendStringField(nil, fieldLookupReqDirectory, iamIdentitiesDir)
	req = appendStringField(req, fieldLookupReqName, name+".json")
	resp, err := f.invoke(ctx, filerSvcLookupEntry, req)
	if err != nil {
		if strings.Contains(err.Error(), filerNotFoundMessage) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("filer LookupDirectoryEntry %s/%s.json: %w", iamIdentitiesDir, name, err)
	}
	entry = lastBytesField(resp, fieldLookupRespEntry)
	return entry, entry != nil, nil
}

// readIdentity fetches one identity from its file's inline content. Decoding is
// strict: a payload that is not an identity must surface as an error, never as an
// empty identity a caller might overwrite with fresh credentials.
func (f *filerEtcClient) readIdentity(ctx context.Context, name string) (*s3Identity, bool, error) {
	entry, found, err := f.lookupIdentityEntry(ctx, name)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}
	data := lastBytesField(entry, fieldEntryContent)
	if len(bytes.TrimSpace(data)) == 0 {
		// The entry exists but carries no inline content: nothing the gateway
		// (which reads only Entry.Content) would use. Report it as absent so the
		// caller re-issues, which rewrites the file into a usable state.
		return nil, false, nil
	}

	id := &s3Identity{}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(id); err != nil {
		return nil, false, fmt.Errorf("decode %s/%s.json (%d bytes): %w", iamIdentitiesDir, name, len(data), err)
	}
	return id, true, nil
}

// writeIdentity stores one identity as a freshly built entry whose payload lives
// in the inline content field, then reads it back: the filer stores entries like
// any other data, so an accepted write is not proof the credential is readable —
// and reporting success on a lost write would hand out a key the S3 gateway
// rejects.
func (f *filerEtcClient) writeIdentity(ctx context.Context, id *s3Identity) error {
	payload, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return err
	}

	now := uint64(time.Now().Unix())
	attrs := appendVarintField(nil, fieldAttrFileSize, uint64(len(payload)))
	attrs = appendVarintField(attrs, fieldAttrMtime, now)
	attrs = appendVarintField(attrs, fieldAttrFileMode, 0o644)
	attrs = appendVarintField(attrs, fieldAttrCrtime, now)

	entry := appendStringField(nil, fieldEntryName, id.Name+".json")
	entry = appendBytesField(entry, fieldEntryAttributes, attrs)
	entry = appendBytesField(entry, fieldEntryContent, payload)

	req := appendStringField(nil, fieldCreateReqDirectory, iamIdentitiesDir)
	req = appendBytesField(req, fieldCreateReqEntry, entry)

	_, found, err := f.lookupIdentityEntry(ctx, id.Name)
	if err != nil {
		return err
	}
	if !found {
		// CreateEntry auto-creates the parent directories.
		resp, err := f.invoke(ctx, filerSvcCreateEntry, req)
		if err != nil {
			return fmt.Errorf("filer CreateEntry %s/%s.json: %w", iamIdentitiesDir, id.Name, err)
		}
		if inband := lastBytesField(resp, fieldCreateRespError); len(inband) > 0 {
			return fmt.Errorf("filer CreateEntry %s/%s.json: %s", iamIdentitiesDir, id.Name, string(inband))
		}
	} else {
		// UpdateEntryRequest has the same directory/entry field numbers. The fresh
		// minimal entry replaces the stored one, which also heals files written by
		// older driver versions over HTTP (stale chunks, zero FileSize).
		if _, err := f.invoke(ctx, filerSvcUpdateEntry, req); err != nil {
			return fmt.Errorf("filer UpdateEntry %s/%s.json: %w", iamIdentitiesDir, id.Name, err)
		}
	}

	written, found, err := f.readIdentity(ctx, id.Name)
	if err != nil {
		return fmt.Errorf("verify %s/%s.json after write: %w", iamIdentitiesDir, id.Name, err)
	}
	if !found || !identityEqual(*written, *id) {
		return fmt.Errorf("identity %s did not persist as written (found=%t)", id.Name, found)
	}
	return nil
}

// deleteIdentity removes one identity file. Idempotent: an absent file is
// success, anything else that prevents the delete is an error — a revocation that
// cannot confirm the file is gone must not report the key revoked.
func (f *filerEtcClient) deleteIdentity(ctx context.Context, name string) error {
	req := appendStringField(nil, fieldDeleteReqDirectory, iamIdentitiesDir)
	req = appendStringField(req, fieldDeleteReqName, name+".json")
	resp, err := f.invoke(ctx, filerSvcDeleteEntry, req)
	if err != nil {
		if strings.Contains(err.Error(), filerNotFoundMessage) {
			return nil
		}
		return fmt.Errorf("filer DeleteEntry %s/%s.json: %w", iamIdentitiesDir, name, err)
	}
	if inband := lastBytesField(resp, fieldDeleteRespError); len(inband) > 0 {
		if strings.Contains(string(inband), filerNotFoundMessage) {
			return nil
		}
		return fmt.Errorf("filer DeleteEntry %s/%s.json: %s", iamIdentitiesDir, name, string(inband))
	}
	return nil
}
