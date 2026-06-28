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
	"time"
)

// adminClient is a minimal client for the Garage admin API (v1, served on the
// admin port behind a Bearer token).
//
// NOTE: the request/response shapes below target the documented Garage v1
// admin API. They have not been validated against a live Garage yet and are
// the most likely place to need adjustment during the first in-cluster pass —
// they are deliberately localised here so a fix stays in one file.
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

// knownNode is one entry of GET /v1/status knownNodes.
type knownNode struct {
	ID       string `json:"id"`
	Addr     string `json:"addr"`
	IsUp     bool   `json:"isUp"`
	Hostname string `json:"hostname"`
}

// layoutRole is one assigned (or staged) role in the cluster layout.
type layoutRole struct {
	ID       string   `json:"id"`
	Zone     string   `json:"zone"`
	Capacity *int64   `json:"capacity"`
	Tags     []string `json:"tags"`
}

// clusterLayout mirrors the layout block of GET /v1/status and GET /v1/layout.
type clusterLayout struct {
	Version           int          `json:"version"`
	Roles             []layoutRole `json:"roles"`
	StagedRoleChanges []layoutRole `json:"stagedRoleChanges"`
}

// statusResponse is GET /v1/status.
type statusResponse struct {
	Node          string        `json:"node"`
	GarageVersion string        `json:"garageVersion"`
	KnownNodes    []knownNode   `json:"knownNodes"`
	Layout        clusterLayout `json:"layout"`
}

// healthResponse is GET /v1/health.
type healthResponse struct {
	Status           string `json:"status"` // healthy | degraded | unavailable
	KnownNodes       int    `json:"knownNodes"`
	ConnectedNodes   int    `json:"connectedNodes"`
	StorageNodes     int    `json:"storageNodes"`
	StorageNodesOk   int    `json:"storageNodesOk"`
	Partitions       int    `json:"partitions"`
	PartitionsQuorum int    `json:"partitionsQuorum"`
	PartitionsAllOk  int    `json:"partitionsAllOk"`
}

// roleChange is one entry of the POST /v1/layout request body. Garage expects
// a JSON array of these (each carrying the node id). For an assignment set
// Zone/Capacity/Tags; to drop a node set Remove=true.
type roleChange struct {
	ID       string   `json:"id"`
	Zone     string   `json:"zone,omitempty"`
	Capacity *int64   `json:"capacity,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Remove   bool     `json:"remove,omitempty"`
}

func (c *adminClient) status(ctx context.Context) (*statusResponse, error) {
	var out statusResponse
	if err := c.do(ctx, http.MethodGet, "/v1/status", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *adminClient) health(ctx context.Context) (*healthResponse, error) {
	var out healthResponse
	if err := c.do(ctx, http.MethodGet, "/v1/health", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// connect asks the node to connect to the given peers ("<id>@<host>:<port>").
func (c *adminClient) connect(ctx context.Context, peers []string) error {
	return c.do(ctx, http.MethodPost, "/v1/connect", peers, nil)
}

func (c *adminClient) layout(ctx context.Context) (*clusterLayout, error) {
	var out clusterLayout
	if err := c.do(ctx, http.MethodGet, "/v1/layout", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// stageLayout stages role changes. Garage expects a JSON array of role
// changes, each identified by its node id.
func (c *adminClient) stageLayout(ctx context.Context, changes []roleChange) error {
	return c.do(ctx, http.MethodPost, "/v1/layout", changes, nil)
}

// applyLayout applies the staged changes, producing layout version `version`.
func (c *adminClient) applyLayout(ctx context.Context, version int) error {
	return c.do(ctx, http.MethodPost, "/v1/layout/apply", map[string]int{"version": version}, nil)
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

// isNotFound reports whether err is an admin API 404.
func isNotFound(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.StatusCode == http.StatusNotFound
}

// --- Bucket / key management ------------------------------------------------
//
// NOTE: as with the cluster-management calls above, these target the documented
// Garage v1 admin API and are the most likely place to need adjustment on the
// first live-cluster pass; they are localised here on purpose.

// keyInfo is the response of POST /v1/key and GET /v1/key. secretAccessKey is
// only populated by the server at creation time.
type keyInfo struct {
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	Name            string `json:"name"`
}

// bucketInfo is the response of POST/GET /v1/bucket.
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
	if err := c.do(ctx, http.MethodPost, "/v1/key", map[string]string{"name": name}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// keyExists reports whether an access key with the given id exists.
func (c *adminClient) keyExists(ctx context.Context, accessKeyID string) (bool, error) {
	err := c.do(ctx, http.MethodGet, "/v1/key?id="+url.QueryEscape(accessKeyID), nil, nil)
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
	err := c.do(ctx, http.MethodDelete, "/v1/key?id="+url.QueryEscape(accessKeyID), nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// getBucketByAlias returns the bucket with the given global alias, or (nil,
// false, nil) when it does not exist.
func (c *adminClient) getBucketByAlias(ctx context.Context, alias string) (*bucketInfo, bool, error) {
	var out bucketInfo
	err := c.do(ctx, http.MethodGet, "/v1/bucket?globalAlias="+url.QueryEscape(alias), nil, &out)
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
	if err := c.do(ctx, http.MethodPost, "/v1/bucket", map[string]string{"globalAlias": alias}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// deleteBucket removes a bucket by id (idempotent).
func (c *adminClient) deleteBucket(ctx context.Context, bucketID string) error {
	err := c.do(ctx, http.MethodDelete, "/v1/bucket?id="+url.QueryEscape(bucketID), nil, nil)
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
	return c.do(ctx, http.MethodPost, "/v1/bucket/allow", body, nil)
}
