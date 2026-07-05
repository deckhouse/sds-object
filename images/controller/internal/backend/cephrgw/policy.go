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
	"encoding/json"
	"fmt"
	"strings"
)

// bucketPolicy is a minimal AWS/RGW S3 bucket policy document. Each access user
// gets one statement, keyed by a deterministic Sid derived from its uid, so
// statements can be upserted and removed independently.
type bucketPolicy struct {
	Version   string            `json:"Version"`
	Statement []policyStatement `json:"Statement"`
}

type policyStatement struct {
	Sid       string          `json:"Sid"`
	Effect    string          `json:"Effect"`
	Principal policyPrincipal `json:"Principal"`
	Action    []string        `json:"Action"`
	Resource  []string        `json:"Resource"`
}

type policyPrincipal struct {
	AWS []string `json:"AWS"`
}

// parsePolicy decodes an existing policy document; an empty or invalid document
// yields a fresh policy.
func parsePolicy(raw string) *bucketPolicy {
	p := &bucketPolicy{Version: "2012-10-17"}
	if strings.TrimSpace(raw) == "" {
		return p
	}
	if err := json.Unmarshal([]byte(raw), p); err != nil {
		return &bucketPolicy{Version: "2012-10-17"}
	}
	if p.Version == "" {
		p.Version = "2012-10-17"
	}
	return p
}

// upsert inserts or replaces the statement for the given uid. Returns true when
// the document changed.
func (p *bucketPolicy) upsert(uid, bucket string, actions []string) bool {
	stmt := policyStatement{
		Sid:       sid(uid),
		Effect:    "Allow",
		Principal: policyPrincipal{AWS: []string{principalARN(uid)}},
		Action:    actions,
		Resource: []string{
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
	if a.Sid != b.Sid || a.Effect != b.Effect || !slicesEqual(a.Action, b.Action) ||
		!slicesEqual(a.Resource, b.Resource) || !slicesEqual(a.Principal.AWS, b.Principal.AWS) {
		return false
	}
	return true
}

func slicesEqual(a, b []string) bool {
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

// sid is a stable, alphanumeric statement id derived from the uid (S3 policy
// Sids must be alphanumeric).
func sid(uid string) string {
	var b strings.Builder
	for _, r := range uid {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return "osba" + b.String()
}

// principalARN is the RGW/AWS principal ARN for a user id.
func principalARN(uid string) string {
	return "arn:aws:iam:::user/" + uid
}
