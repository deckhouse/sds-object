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

package cephrgw

import "testing"

func TestSidNoCollision(t *testing.T) {
	// "ns.name" and "nsname" previously collapsed to the same Sid (non-alnum
	// stripping); they must now differ.
	a := sid("ns.name")
	b := sid("nsname")
	if a == b {
		t.Errorf("sid collision: sid(%q)==sid(%q)==%q", "ns.name", "nsname", a)
	}
	if a != sid("ns.name") {
		t.Errorf("sid must be deterministic")
	}
	for _, r := range a {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			t.Fatalf("sid %q is not alphanumeric (char %q)", a, r)
		}
	}
}

func TestParsePolicyLenient(t *testing.T) {
	t.Run("empty yields fresh policy", func(t *testing.T) {
		p, err := parsePolicy("")
		if err != nil || p == nil || p.Version != "2012-10-17" {
			t.Fatalf("empty: p=%v err=%v", p, err)
		}
	})

	t.Run("external policy with scalar Action/Resource and wildcard Principal parses", func(t *testing.T) {
		raw := `{"Version":"2012-10-17","Statement":[{"Sid":"External","Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`
		p, err := parsePolicy(raw)
		if err != nil {
			t.Fatalf("expected lenient parse, got err=%v", err)
		}
		if len(p.Statement) != 1 || !p.Statement[0].Principal.Wildcard {
			t.Fatalf("unexpected parse: %+v", p.Statement)
		}
		if len(p.Statement[0].Action) != 1 || p.Statement[0].Action[0] != "s3:GetObject" {
			t.Errorf("scalar Action not normalised: %v", p.Statement[0].Action)
		}
	})

	t.Run("object Principal with scalar AWS parses", func(t *testing.T) {
		raw := `{"Statement":[{"Sid":"X","Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/u"},"Action":["s3:*"],"Resource":["arn:aws:s3:::b"]}]}`
		p, err := parsePolicy(raw)
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if len(p.Statement[0].Principal.AWS) != 1 || p.Statement[0].Principal.AWS[0] != "arn:aws:iam:::user/u" {
			t.Errorf("scalar AWS not normalised: %v", p.Statement[0].Principal.AWS)
		}
	})

	t.Run("invalid JSON fails closed", func(t *testing.T) {
		if _, err := parsePolicy("{not json"); err == nil {
			t.Errorf("expected error for invalid JSON (fail-closed)")
		}
	})
}

// TestUpsertPreservesExternalStatements is the core C2 guarantee: adding our
// statement must not drop a statement written by someone else.
func TestUpsertPreservesExternalStatements(t *testing.T) {
	raw := `{"Version":"2012-10-17","Statement":[{"Sid":"External","Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`
	doc, err := parsePolicy(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !doc.upsert("app.reader", "b", []string{"s3:GetObject"}) {
		t.Fatalf("upsert should report a change")
	}
	// Re-marshal and re-parse to prove the external statement survives a round trip.
	out, err := parsePolicy(doc.marshal())
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	var haveExternal, haveOurs bool
	for _, s := range out.Statement {
		if s.Sid == "External" {
			haveExternal = true
		}
		if s.Sid == sid("app.reader") {
			haveOurs = true
		}
	}
	if !haveExternal {
		t.Errorf("external statement was erased")
	}
	if !haveOurs {
		t.Errorf("our statement was not added")
	}

	// Removing ours must keep the external one.
	doc.remove("app.reader")
	for _, s := range doc.Statement {
		if s.Sid == sid("app.reader") {
			t.Errorf("our statement not removed")
		}
	}
	if len(doc.Statement) != 1 || doc.Statement[0].Sid != "External" {
		t.Errorf("external statement not preserved after remove: %+v", doc.Statement)
	}
}
