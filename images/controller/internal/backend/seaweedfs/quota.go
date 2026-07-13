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
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protowire"
)

// SeaweedFS stores a per-bucket size quota on the bucket's directory Entry
// (field `quota`, bytes) under the filer's buckets folder, set via the filer
// gRPC SeaweedFiler service — there is no HTTP/S3 API for it. Since SeaweedFS
// 4.31 the S3 gateway auto-enforces the quota (a ~1-minute leader-locked loop
// flips the bucket read-only when it exceeds the quota and back when under), so
// setting a positive quota value is sufficient; no enforce cron is needed.
//
// To avoid pulling the large github.com/seaweedfs/seaweedfs module (or a protoc
// codegen step) just for three calls, this talks to the SeaweedFiler service
// with a raw-bytes gRPC codec and hand-encodes the handful of protobuf fields we
// need with protowire. The bucket Entry is preserved intact on the
// read-modify-write: we append the `quota` field to the raw Entry bytes returned
// by LookupDirectoryEntry (a later occurrence of a scalar field wins on decode),
// so every other Entry field (chunks, attributes, extended, hard links, …) is
// carried through untouched.
const (
	filerSvcGetConfiguration = "/filer_pb.SeaweedFiler/GetFilerConfiguration"
	filerSvcLookupEntry      = "/filer_pb.SeaweedFiler/LookupDirectoryEntry"
	filerSvcUpdateEntry      = "/filer_pb.SeaweedFiler/UpdateEntry"

	// defaultBucketsDir is the fallback buckets folder when GetFilerConfiguration
	// does not report one (SeaweedFS default is /buckets).
	defaultBucketsDir = "/buckets"

	// protobuf field numbers on the filer messages we touch.
	fieldGetConfigDirBuckets = 5  // GetFilerConfigurationResponse.dir_buckets
	fieldLookupReqDirectory  = 1  // LookupDirectoryEntryRequest.directory
	fieldLookupReqName       = 2  // LookupDirectoryEntryRequest.name
	fieldLookupRespEntry     = 1  // LookupDirectoryEntryResponse.entry
	fieldEntryQuota          = 11 // Entry.quota (int64, bytes)
	fieldUpdateReqDirectory  = 1  // UpdateEntryRequest.directory
	fieldUpdateReqEntry      = 2  // UpdateEntryRequest.entry
)

// setBucketQuota sets the SeaweedFS bucket's size quota (bytes) on its filer
// directory Entry. A positive value is auto-enforced by the S3 gateway; 0 clears
// the limit.
func setBucketQuota(ctx context.Context, target, bucketName string, quotaBytes int64) error {
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial filer gRPC %s: %w", target, err)
	}
	defer func() { _ = conn.Close() }()

	invoke := func(method string, req []byte) ([]byte, error) {
		var resp []byte
		if err := conn.Invoke(ctx, method, req, &resp, grpc.ForceCodec(rawCodec{})); err != nil {
			return nil, err
		}
		return resp, nil
	}

	// 1. Resolve the buckets folder (GetFilerConfiguration.dir_buckets).
	cfg, err := invoke(filerSvcGetConfiguration, []byte{})
	if err != nil {
		return fmt.Errorf("filer GetFilerConfiguration: %w", err)
	}
	dirBuckets := string(lastBytesField(cfg, fieldGetConfigDirBuckets))
	if dirBuckets == "" {
		dirBuckets = defaultBucketsDir
	}

	// 2. Look up the bucket's directory Entry.
	lookupReq := appendStringField(nil, fieldLookupReqDirectory, dirBuckets)
	lookupReq = appendStringField(lookupReq, fieldLookupReqName, bucketName)
	lookupResp, err := invoke(filerSvcLookupEntry, lookupReq)
	if err != nil {
		return fmt.Errorf("filer LookupDirectoryEntry %s/%s: %w", dirBuckets, bucketName, err)
	}
	entry := lastBytesField(lookupResp, fieldLookupRespEntry)
	if entry == nil {
		return fmt.Errorf("bucket entry %s/%s not found on filer", dirBuckets, bucketName)
	}

	// 3. Append the quota field (scalar: last occurrence wins), preserving all
	//    other Entry fields. Copy first so we don't alias the response buffer.
	entry = append([]byte(nil), entry...)
	entry = appendVarintField(entry, fieldEntryQuota, uint64(quotaBytes))

	// 4. Write the Entry back.
	updReq := appendStringField(nil, fieldUpdateReqDirectory, dirBuckets)
	updReq = appendBytesField(updReq, fieldUpdateReqEntry, entry)
	if _, err := invoke(filerSvcUpdateEntry, updReq); err != nil {
		return fmt.Errorf("filer UpdateEntry %s/%s: %w", dirBuckets, bucketName, err)
	}
	return nil
}

// rawCodec is a gRPC codec passing message bodies through as raw protobuf bytes,
// so we can call the SeaweedFiler service without its generated Go types.
type rawCodec struct{}

func (rawCodec) Marshal(v any) ([]byte, error) {
	b, ok := v.([]byte)
	if !ok {
		return nil, fmt.Errorf("rawCodec.Marshal: expected []byte, got %T", v)
	}
	return b, nil
}

func (rawCodec) Unmarshal(data []byte, v any) error {
	p, ok := v.(*[]byte)
	if !ok {
		return fmt.Errorf("rawCodec.Unmarshal: expected *[]byte, got %T", v)
	}
	*p = append([]byte(nil), data...)
	return nil
}

func (rawCodec) Name() string { return "sds-object-seaweedfs-raw" }

// --- minimal protobuf field encode/scan helpers ----------------------------

func appendStringField(b []byte, num protowire.Number, s string) []byte {
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendString(b, s)
}

func appendBytesField(b []byte, num protowire.Number, v []byte) []byte {
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendBytes(b, v)
}

func appendVarintField(b []byte, num protowire.Number, v uint64) []byte {
	b = protowire.AppendTag(b, num, protowire.VarintType)
	return protowire.AppendVarint(b, v)
}

// lastBytesField returns the value of the last length-delimited field with the
// given number in the message, or nil when absent.
func lastBytesField(b []byte, want protowire.Number) []byte {
	var result []byte
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return result
		}
		b = b[n:]
		if typ == protowire.BytesType {
			v, vn := protowire.ConsumeBytes(b)
			if vn < 0 {
				return result
			}
			if num == want {
				result = v
			}
			b = b[vn:]
			continue
		}
		vn := protowire.ConsumeFieldValue(num, typ, b)
		if vn < 0 {
			return result
		}
		b = b[vn:]
	}
	return result
}
