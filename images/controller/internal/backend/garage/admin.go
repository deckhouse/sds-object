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

package garage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// adminClient is a minimal client for the Garage v2 admin API (served on the
// admin port behind a Bearer token). Garage v2 reworked the admin API: every
// call is `METHOD /v2/<CallName>` with the call name in the path, proper HTTP
// 404s for missing resources, and DeleteKey/DeleteBucket/UpdateBucket as POST
// with the id in a query parameter.
//
// NOTE: the request/response shapes below target the documented Garage v2.3.0
// admin API; the wire format is deliberately localised here so a fix stays in
// one file. The exported method set and the returned Go structs are unchanged
// from the v1 client, so the callers (mesh.go, rebalance.go, buckets.go,
// access.go) are not affected by the v2 migration.
type adminClient struct {
	http    *http.Client
	baseURL string // e.g. http://host:3903
	token   string
}

func newAdminClient(baseURL, token string) *adminClient {
	return &adminClient{
		http:    &http.Client{Timeout: 10 * time.Second},
		baseURL: baseURL,
		token:   token,
	}
}

// statusResponse carries the node identity of the queried Garage node (its own
// node id), used to seed the RPC mesh and layout.
type statusResponse struct {
	Node string
}

// layoutRole is one assigned role in the cluster layout.
type layoutRole struct {
	ID       string   `json:"id"`
	Zone     string   `json:"zone"`
	Capacity *int64   `json:"capacity"`
	Tags     []string `json:"tags"`
}

// clusterLayout mirrors GET /v2/GetClusterLayout.
type clusterLayout struct {
	Version int          `json:"version"`
	Roles   []layoutRole `json:"roles"`
}

// healthResponse is GET /v2/GetClusterHealth.
type healthResponse struct {
	Status         string `json:"status"` // healthy | degraded | unavailable
	KnownNodes     int    `json:"knownNodes"`
	ConnectedNodes int    `json:"connectedNodes"`
	StorageNodes   int    `json:"storageNodes"`
	// StorageNodesOk keeps the v1 Go field name used by callers; in v2 the JSON
	// field was renamed to storageNodesUp.
	StorageNodesOk   int `json:"storageNodesUp"`
	Partitions       int `json:"partitions"`
	PartitionsQuorum int `json:"partitionsQuorum"`
	PartitionsAllOk  int `json:"partitionsAllOk"`
}

// roleChange is one layout mutation the reconciler wants to make. It is the
// caller-facing intent type (mesh.go); stageLayout translates it to the v2
// UpdateClusterLayout wire shape. For an assignment set Zone/Capacity/Tags; to
// drop a node set Remove=true.
type roleChange struct {
	ID       string
	Zone     string
	Capacity *int64
	Tags     []string
	Remove   bool
}

// nodeInfoResponse is GET /v2/GetNodeInfo (a MultiResponse keyed by node id).
type nodeInfoResponse struct {
	Success map[string]struct {
		NodeID string `json:"nodeId"`
	} `json:"success"`
}

// status returns the queried node's own identity via GET /v2/GetNodeInfo?node=self.
func (c *adminClient) status(ctx context.Context) (*statusResponse, error) {
	var out nodeInfoResponse
	if err := c.do(ctx, http.MethodGet, "/v2/GetNodeInfo?node=self", nil, &out); err != nil {
		return nil, err
	}
	for _, v := range out.Success {
		if v.NodeID != "" {
			return &statusResponse{Node: v.NodeID}, nil
		}
	}
	return &statusResponse{}, nil
}

func (c *adminClient) health(ctx context.Context) (*healthResponse, error) {
	var out healthResponse
	if err := c.do(ctx, http.MethodGet, "/v2/GetClusterHealth", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// connect asks the node to connect to the given peers ("<id>@<host>:<port>").
func (c *adminClient) connect(ctx context.Context, peers []string) error {
	return c.do(ctx, http.MethodPost, "/v2/ConnectClusterNodes", peers, nil)
}

func (c *adminClient) layout(ctx context.Context) (*clusterLayout, error) {
	var out clusterLayout
	if err := c.do(ctx, http.MethodGet, "/v2/GetClusterLayout", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// stageLayout stages role changes. v2 UpdateClusterLayout takes an object
// {roles: [...]} where each entry is either an assignment (id+zone+capacity+tags)
// or a removal (id+remove:true) — the two variants must not be mixed on one entry.
func (c *adminClient) stageLayout(ctx context.Context, changes []roleChange) error {
	roles := make([]any, 0, len(changes))
	for _, ch := range changes {
		if ch.Remove {
			roles = append(roles, map[string]any{"id": ch.ID, "remove": true})
			continue
		}
		roles = append(roles, map[string]any{
			"id":       ch.ID,
			"zone":     ch.Zone,
			"capacity": ch.Capacity,
			"tags":     ch.Tags,
		})
	}
	return c.do(ctx, http.MethodPost, "/v2/UpdateClusterLayout", map[string]any{"roles": roles}, nil)
}

// applyLayout applies the staged changes, producing layout version `version`.
func (c *adminClient) applyLayout(ctx context.Context, version int) error {
	return c.do(ctx, http.MethodPost, "/v2/ApplyClusterLayout", map[string]int{"version": version}, nil)
}

// do performs an authenticated JSON request and optionally decodes the body.
func (c *adminClient) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &apiError{Method: method, Path: path, StatusCode: resp.StatusCode, Body: string(data)}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("garage admin %s %s: decode: %w", method, path, err)
		}
	}
	return nil
}

// apiError is returned for non-2xx admin API responses.
type apiError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("garage admin %s %s: status %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
}

// isNotFound reports whether err means "the resource does not exist". Garage v2
// returns a proper HTTP 404 for a missing key/bucket; the legacy 400-with-
// "not found"-body case is still tolerated defensively.
func isNotFound(err error) bool {
	var ae *apiError
	if !errors.As(err, &ae) {
		return false
	}
	if ae.StatusCode == http.StatusNotFound {
		return true
	}
	return ae.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(ae.Body), "not found")
}

// --- Bucket / key management ------------------------------------------------

// keyInfo is the response of POST /v2/CreateKey and GET /v2/GetKeyInfo.
// secretAccessKey is only populated by the server at creation time (or when
// GetKeyInfo is asked with showSecretKey=true).
type keyInfo struct {
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	Name            string `json:"name"`
}

// bucketInfo is the response of POST /v2/CreateBucket and GET /v2/GetBucketInfo.
type bucketInfo struct {
	ID            string   `json:"id"`
	GlobalAliases []string `json:"globalAliases"`
}

// permissions is the access granted to a key on a bucket.
type permissions struct {
	Read  bool `json:"read"`
	Write bool `json:"write"`
	Owner bool `json:"owner"`
}

// createKey creates a new access key with the given display name.
func (c *adminClient) createKey(ctx context.Context, name string) (*keyInfo, error) {
	var out keyInfo
	if err := c.do(ctx, http.MethodPost, "/v2/CreateKey", map[string]string{"name": name}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// keyExists reports whether an access key with the given id exists.
func (c *adminClient) keyExists(ctx context.Context, accessKeyID string) (bool, error) {
	err := c.do(ctx, http.MethodGet, "/v2/GetKeyInfo?id="+url.QueryEscape(accessKeyID), nil, nil)
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, err
}

// deleteKey removes an access key (idempotent: a missing key is not an error).
func (c *adminClient) deleteKey(ctx context.Context, accessKeyID string) error {
	err := c.do(ctx, http.MethodPost, "/v2/DeleteKey?id="+url.QueryEscape(accessKeyID), nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// getBucketByAlias returns the bucket with the given global alias, or (nil,
// false, nil) when it does not exist.
func (c *adminClient) getBucketByAlias(ctx context.Context, alias string) (*bucketInfo, bool, error) {
	var out bucketInfo
	err := c.do(ctx, http.MethodGet, "/v2/GetBucketInfo?globalAlias="+url.QueryEscape(alias), nil, &out)
	if err == nil {
		return &out, true, nil
	}
	if isNotFound(err) {
		return nil, false, nil
	}
	return nil, false, err
}

// createBucket creates a bucket with the given global alias.
func (c *adminClient) createBucket(ctx context.Context, alias string) (*bucketInfo, error) {
	var out bucketInfo
	if err := c.do(ctx, http.MethodPost, "/v2/CreateBucket", map[string]string{"globalAlias": alias}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// deleteBucket removes a bucket by id (idempotent).
func (c *adminClient) deleteBucket(ctx context.Context, bucketID string) error {
	err := c.do(ctx, http.MethodPost, "/v2/DeleteBucket?id="+url.QueryEscape(bucketID), nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// allow grants the given permissions to a key on a bucket (idempotent).
func (c *adminClient) allow(ctx context.Context, bucketID, accessKeyID string, perms permissions) error {
	body := map[string]any{
		"bucketId":    bucketID,
		"accessKeyId": accessKeyID,
		"permissions": perms,
	}
	return c.do(ctx, http.MethodPost, "/v2/AllowBucketKey", body, nil)
}

// bucketQuotas is the quota block of the UpdateBucket request. A nil field
// means "no limit"; Garage stores sizes in bytes.
type bucketQuotas struct {
	MaxSize    *int64 `json:"maxSize"`
	MaxObjects *int64 `json:"maxObjects"`
}

// updateBucket sets the bucket's quotas (idempotent). Passing a bucketQuotas
// with nil fields clears the limits, so it also reconciles quota removal.
func (c *adminClient) updateBucket(ctx context.Context, bucketID string, quotas bucketQuotas) error {
	body := map[string]any{"quotas": quotas}
	return c.do(ctx, http.MethodPost, "/v2/UpdateBucket?id="+url.QueryEscape(bucketID), body, nil)
}
