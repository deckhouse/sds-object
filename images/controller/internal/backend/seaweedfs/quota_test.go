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
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

// varintField decodes the last varint field with the given number (test helper).
func varintField(b []byte, want protowire.Number) (uint64, bool) {
	var (
		val   uint64
		found bool
	)
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return val, found
		}
		b = b[n:]
		if typ == protowire.VarintType {
			v, vn := protowire.ConsumeVarint(b)
			if vn < 0 {
				return val, found
			}
			if num == want {
				val, found = v, true
			}
			b = b[vn:]
			continue
		}
		vn := protowire.ConsumeFieldValue(num, typ, b)
		if vn < 0 {
			return val, found
		}
		b = b[vn:]
	}
	return val, found
}

func TestAppendAndScanFields(t *testing.T) {
	b := appendStringField(nil, 1, "/buckets")
	b = appendStringField(b, 2, "my-bucket")
	if got := string(lastBytesField(b, 1)); got != "/buckets" {
		t.Errorf("field 1 = %q, want /buckets", got)
	}
	if got := string(lastBytesField(b, 2)); got != "my-bucket" {
		t.Errorf("field 2 = %q, want my-bucket", got)
	}
	if lastBytesField(b, 3) != nil {
		t.Errorf("absent field 3 should be nil")
	}
}

// TestQuotaAppendPreservesEntry verifies the core read-modify-write trick: an
// Entry's existing fields survive, and appending the quota field (11) overrides
// any prior value on decode (proto scalar last-wins).
func TestQuotaAppendPreservesEntry(t *testing.T) {
	// A fake Entry: name(1)="my-bucket", is_directory(2)=true, quota(11)=100,
	// plus an unknown extended-ish bytes field(5) that must be preserved.
	entry := appendStringField(nil, 1, "my-bucket")
	entry = protowire.AppendTag(entry, 2, protowire.VarintType)
	entry = protowire.AppendVarint(entry, 1) // is_directory = true
	entry = appendVarintField(entry, 11, 100)
	entry = appendBytesField(entry, 5, []byte("preserve-me"))

	// Apply the same transformation setBucketQuota does: copy + append new quota.
	updated := append([]byte(nil), entry...)
	updated = appendVarintField(updated, fieldEntryQuota, uint64(10*1024*1024))

	// Quota now decodes to the new value (last occurrence wins).
	q, ok := varintField(updated, fieldEntryQuota)
	if !ok || q != 10*1024*1024 {
		t.Errorf("quota = %d (found=%v), want %d", q, ok, 10*1024*1024)
	}
	// Other fields preserved.
	if got := string(lastBytesField(updated, 1)); got != "my-bucket" {
		t.Errorf("name = %q, want my-bucket", got)
	}
	if got := string(lastBytesField(updated, 5)); got != "preserve-me" {
		t.Errorf("field 5 = %q, want preserve-me", got)
	}
	if d, ok := varintField(updated, 2); !ok || d != 1 {
		t.Errorf("is_directory = %d (found=%v), want 1", d, ok)
	}
}

func TestSeaweedFSRawCodec(t *testing.T) {
	c := rawCodec{}
	in := []byte{0x08, 0x96, 0x01}
	out, err := c.Marshal(in)
	if err != nil || string(out) != string(in) {
		t.Fatalf("Marshal round-trip failed: out=%v err=%v", out, err)
	}
	var dst []byte
	if err := c.Unmarshal(in, &dst); err != nil || string(dst) != string(in) {
		t.Fatalf("Unmarshal round-trip failed: dst=%v err=%v", dst, err)
	}
	if _, err := c.Marshal("not-bytes"); err == nil {
		t.Errorf("Marshal should reject non-[]byte")
	}
}
