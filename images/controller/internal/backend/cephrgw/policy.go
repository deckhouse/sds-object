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

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// bucketPolicy is an AWS/RGW S3 bucket policy document. Each access user gets
// one statement, keyed by a deterministic Sid derived from its uid, so
// statements can be upserted and removed independently — without disturbing
// statements written by anyone else.
type bucketPolicy struct {
	Version   string            `json:"Version"`
	Statement []policyStatement `json:"Statement"`
}

type policyStatement struct {
	Sid       string          `json:"Sid"`
	Effect    string          `json:"Effect"`
	Principal policyPrincipal `json:"Principal"`
	Action    stringOrSlice   `json:"Action"`
	Resource  stringOrSlice   `json:"Resource"`
}

// stringOrSlice decodes a JSON value that may be either a single string or an
// array of strings — both are valid for Action/Resource/Principal.AWS in AWS
// and RGW policy documents. Without this, a valid external statement using the
// scalar form would fail to parse.
type stringOrSlice []string

func (s *stringOrSlice) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		*s = []string{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return err
	}
	*s = many
	return nil
}

// policyPrincipal accepts either the bare wildcard string "*" or an object
// {"AWS": <string|[]string>} (both valid in RGW/AWS policies), and re-marshals
// in the same shape it was read.
type policyPrincipal struct {
	AWS      stringOrSlice
	Wildcard bool
}

func (p *policyPrincipal) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		p.Wildcard = s == "*"
		if !p.Wildcard && s != "" {
			p.AWS = stringOrSlice{s}
		}
		return nil
	}
	var obj struct {
		AWS stringOrSlice `json:"AWS"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	p.AWS = obj.AWS
	return nil
}

func (p policyPrincipal) MarshalJSON() ([]byte, error) {
	if p.Wildcard {
		return []byte(`"*"`), nil
	}
	return json.Marshal(struct {
		AWS stringOrSlice `json:"AWS"`
	}{AWS: p.AWS})
}

// parsePolicy decodes an existing policy document. An empty document yields a
// fresh policy. A non-empty document that fails to parse returns an error so the
// caller can fail closed: it must never overwrite (and thereby erase) a policy
// it could not fully understand — statements written by other users must be
// preserved.
func parsePolicy(raw string) (*bucketPolicy, error) {
	if strings.TrimSpace(raw) == "" {
		return &bucketPolicy{Version: "2012-10-17"}, nil
	}
	p := &bucketPolicy{}
	if err := json.Unmarshal([]byte(raw), p); err != nil {
		return nil, fmt.Errorf("parse bucket policy: %w", err)
	}
	if p.Version == "" {
		p.Version = "2012-10-17"
	}
	return p, nil
}

// upsert inserts or replaces the statement for the given uid. Returns true when
// the document changed.
func (p *bucketPolicy) upsert(uid, bucket string, actions []string) bool {
	stmt := policyStatement{
		Sid:       sid(uid),
		Effect:    "Allow",
		Principal: policyPrincipal{AWS: stringOrSlice{principalARN(uid)}},
		Action:    stringOrSlice(actions),
		Resource: stringOrSlice{
			fmt.Sprintf("arn:aws:s3:::%s", bucket),
			fmt.Sprintf("arn:aws:s3:::%s/*", bucket),
		},
	}
	for i := range p.Statement {
		if p.Statement[i].Sid == stmt.Sid {
			if statementEqual(p.Statement[i], stmt) {
				return false
			}
			p.Statement[i] = stmt
			return true
		}
	}
	p.Statement = append(p.Statement, stmt)
	return true
}

// remove drops the statement for the given uid. Returns true when it changed.
func (p *bucketPolicy) remove(uid string) bool {
	target := sid(uid)
	out := p.Statement[:0]
	changed := false
	for _, s := range p.Statement {
		if s.Sid == target {
			changed = true
			continue
		}
		out = append(out, s)
	}
	p.Statement = out
	return changed
}

// marshal serialises the policy, or returns "" when it has no statements (which
// clears the bucket policy).
func (p *bucketPolicy) marshal() string {
	if len(p.Statement) == 0 {
		return ""
	}
	data, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return string(data)
}

func statementEqual(a, b policyStatement) bool {
	return a.Sid == b.Sid && a.Effect == b.Effect &&
		slicesEqual(a.Action, b.Action) && slicesEqual(a.Resource, b.Resource) &&
		slicesEqual(a.Principal.AWS, b.Principal.AWS) && a.Principal.Wildcard == b.Principal.Wildcard
}

func slicesEqual(a, b stringOrSlice) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sid is a stable, alphanumeric, collision-free statement id derived from the
// uid. It hashes the uid rather than stripping non-alphanumeric characters:
// stripping would collapse distinct uids (e.g. "ns.name" and "nsname") to the
// same Sid, causing one tenant's statement to overwrite another's. S3 policy
// Sids must be alphanumeric, and a hex digest is.
func sid(uid string) string {
	sum := sha256.Sum256([]byte(uid))
	return "osba" + hex.EncodeToString(sum[:16])
}

// principalARN is the RGW/AWS principal ARN for a user id.
func principalARN(uid string) string {
	return "arn:aws:iam:::user/" + uid
}
