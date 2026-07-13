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
	"io"
	"mime/multipart"
	"net/http"
	"sync"
	"time"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

// SeaweedFS stores its S3 IAM config in the filer at this path. The S3 gateway
// subscribes to filer metadata events and reloads it automatically, so writing
// here is the supported way to manage identities dynamically.
const iamConfigPath = "/etc/iam/identity.json"

// SeaweedFS S3 actions.
const (
	actionAdmin   = "Admin"
	actionRead    = "Read"
	actionWrite   = "Write"
	actionList    = "List"
	actionTagging = "Tagging"
)

// s3Credential is one access key / secret key pair.
type s3Credential struct {
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
}

// s3Identity is one IAM identity (user) in identity.json.
type s3Identity struct {
	Name        string         `json:"name"`
	Credentials []s3Credential `json:"credentials,omitempty"`
	Actions     []string       `json:"actions"`
}

// identityConfig is the root of identity.json.
type identityConfig struct {
	Identities []s3Identity `json:"identities"`
}

// upsert inserts or replaces the identity with the same name. Returns true if
// the config changed.
func (c *identityConfig) upsert(id s3Identity) bool {
	for i := range c.Identities {
		if c.Identities[i].Name == id.Name {
			if identityEqual(c.Identities[i], id) {
				return false
			}
			c.Identities[i] = id
			return true
		}
	}
	c.Identities = append(c.Identities, id)
	return true
}

// remove drops the identity with the given name. Returns true if it changed.
func (c *identityConfig) remove(name string) bool {
	out := c.Identities[:0]
	changed := false
	for _, id := range c.Identities {
		if id.Name == name {
			changed = true
			continue
		}
		out = append(out, id)
	}
	c.Identities = out
	return changed
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

// identityLock returns the per-cluster mutex guarding identity.json.
func (d *Driver) identityLock(cluster *v1alpha1.ObjectStore) *sync.Mutex {
	v, _ := d.identityLocks.LoadOrStore(cluster.Name, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// mutateIdentities performs a serialized read-modify-write of the cluster's
// filer identity.json: read the current config, let mutate change it, and write
// it back only if it changed. The whole cycle holds a per-cluster in-process
// lock so concurrent reconciles (EnsureAccess / DeleteAccess / admin identity)
// cannot clobber each other's updates — a lost update here could revoke a
// just-issued key or the admin identity. Leader election guarantees a single
// active controller, and the filer has no compare-and-swap for this file, so an
// in-process lock is the right granularity. mutate reports whether it changed
// the config (and may return an error to abort without writing).
func (d *Driver) mutateIdentities(ctx context.Context, cluster *v1alpha1.ObjectStore, mutate func(*identityConfig) (bool, error)) error {
	lock := d.identityLock(cluster)
	lock.Lock()
	defer lock.Unlock()

	filer := newFilerClient(filerEndpoint(cluster, d.namespace, d.clusterDomain))
	cfg, err := filer.readIdentities(ctx)
	if err != nil {
		return fmt.Errorf("read IAM config: %w", err)
	}
	changed, err := mutate(cfg)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if err := filer.writeIdentities(ctx, cfg); err != nil {
		return fmt.Errorf("write IAM config: %w", err)
	}
	return nil
}

// filerClient reads and writes files via the SeaweedFS filer HTTP API.
//
// NOTE: the identity.json schema and the filer read/write semantics are taken
// from the SeaweedFS docs; they have not been validated against a live cluster
// yet and are localised here so a fix stays in one place.
type filerClient struct {
	http    *http.Client
	baseURL string // e.g. http://host:8888
}

func newFilerClient(baseURL string) *filerClient {
	return &filerClient{http: &http.Client{Timeout: 10 * time.Second}, baseURL: baseURL}
}

// readIdentities fetches identity.json. A missing file yields an empty config.
func (f *filerClient) readIdentities(ctx context.Context) (*identityConfig, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.baseURL+iamConfigPath, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return &identityConfig{}, nil
	}
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("filer GET %s: status %d: %s", iamConfigPath, resp.StatusCode, string(data))
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return &identityConfig{}, nil
	}
	cfg := &identityConfig{}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("decode identity.json: %w", err)
	}
	return cfg, nil
}

// writeIdentities uploads identity.json to the filer (multipart, field "file"),
// overwriting the existing file.
func (f *filerClient) writeIdentities(ctx context.Context, cfg *identityConfig) error {
	payload, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", "identity.json")
	if err != nil {
		return err
	}
	if _, err := part.Write(payload); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.baseURL+iamConfigPath, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := f.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("filer POST %s: status %d: %s", iamConfigPath, resp.StatusCode, string(data))
	}
	return nil
}
