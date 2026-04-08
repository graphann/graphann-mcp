package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Client is a thin HTTP client for the GraphANN REST API. It is scoped to a
// single tenant/index pair; call Bootstrap once before issuing document
// operations. Bootstrap is idempotent and safe under concurrent tool calls.
type Client struct {
	http           *http.Client
	bootstrapErr   error
	baseURL        string
	tenantFriendly string // e.g. "lukasz-claude"
	indexFriendly  string // e.g. "memory-palace"
	tenantDesc     string
	indexDesc      string
	tenantID       string // resolved server-side ID, e.g. "t_lukasz-claude"
	indexID        string // resolved server-side ID, e.g. "i_memory-palace"
	bootstrapOnce  sync.Once
}

// NewClient builds a Client pointed at baseURL (no trailing slash) scoped to
// a specific friendly tenant/index pair. Actual creation of those entities
// on the server is deferred to the first Bootstrap call.
func NewClient(baseURL, tenantFriendly, indexFriendly string) *Client {
	return &Client{
		baseURL:        strings.TrimRight(baseURL, "/"),
		tenantFriendly: tenantFriendly,
		indexFriendly:  indexFriendly,
		tenantDesc:     "Claude Code memory tenant",
		indexDesc:      "per-project Claude Code memory index",
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// apiError wraps a non-2xx response. The GraphANN error envelope is
// {"error":{"code":"...","message":"..."}}.
type apiError struct {
	Code   string
	Msg    string
	Status int
}

func (e *apiError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("graphann: HTTP %d", e.Status)
	}
	return fmt.Sprintf("graphann: HTTP %d %s: %s", e.Status, e.Code, e.Msg)
}

// doJSON issues a JSON request and decodes the response body into out (if
// non-nil). A nil body sends no request body. Non-2xx responses are returned
// as *apiError so callers can inspect Status/Code.
func (c *Client) doJSON(ctx context.Context, method, path string, body, out any) error {
	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("graphann request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	rawBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		var envelope struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rawBody, &envelope)
		return &apiError{
			Status: resp.StatusCode,
			Code:   envelope.Error.Code,
			Msg:    envelope.Error.Message,
		}
	}
	if out != nil && len(rawBody) > 0 {
		if err := json.Unmarshal(rawBody, out); err != nil {
			return fmt.Errorf("decode response: %w (body: %s)", err, truncate(string(rawBody), 200))
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ---- Tenant bootstrap ---------------------------------------------------

// tenantResponse matches GraphANN's tenant JSON (snake_case lowercase).
type tenantResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

// indexResponse matches GraphANN's index JSON (PascalCase, as observed live).
type indexResponse struct {
	ID        string `json:"ID"`
	TenantID  string `json:"TenantID"`
	Name      string `json:"Name"`
	Status    string `json:"Status"`
	NumDocs   int    `json:"NumDocs"`
	NumChunks int    `json:"NumChunks"`
}

// Bootstrap ensures the configured tenant and index exist, caching their
// resolved server-side IDs. Safe to call many times; the work runs only
// once per Client instance.
func (c *Client) Bootstrap(ctx context.Context) error {
	c.bootstrapOnce.Do(func() {
		c.bootstrapErr = c.bootstrap(ctx)
	})
	return c.bootstrapErr
}

func (c *Client) bootstrap(ctx context.Context) error {
	// --- tenant ---
	expectedTenantID := "t_" + c.tenantFriendly
	var t tenantResponse
	err := c.doJSON(ctx, http.MethodGet, "/v1/tenants/"+expectedTenantID, nil, &t)
	switch {
	case err == nil && t.ID != "":
		c.tenantID = t.ID
	case isNotFound(err):
		body := map[string]string{
			"id":          c.tenantFriendly,
			"name":        c.tenantFriendly,
			"description": c.tenantDesc,
		}
		var created tenantResponse
		if cerr := c.doJSON(ctx, http.MethodPost, "/v1/tenants", body, &created); cerr != nil {
			return fmt.Errorf("create tenant %q: %w", c.tenantFriendly, cerr)
		}
		if created.ID == "" {
			return fmt.Errorf("create tenant %q: empty id in response", c.tenantFriendly)
		}
		c.tenantID = created.ID
	default:
		return fmt.Errorf("lookup tenant %q: %w", expectedTenantID, err)
	}

	// --- index ---
	expectedIndexID := "i_" + c.indexFriendly
	var idx indexResponse
	err = c.doJSON(ctx, http.MethodGet, "/v1/tenants/"+c.tenantID+"/indexes/"+expectedIndexID, nil, &idx)
	freshlyCreated := false
	switch {
	case err == nil && idx.ID != "":
		c.indexID = idx.ID
	case isNotFound(err):
		body := map[string]string{
			"id":          c.indexFriendly,
			"name":        c.indexFriendly,
			"description": c.indexDesc,
		}
		var created indexResponse
		if cerr := c.doJSON(ctx, http.MethodPost, "/v1/tenants/"+c.tenantID+"/indexes", body, &created); cerr != nil {
			return fmt.Errorf("create index %q: %w", c.indexFriendly, cerr)
		}
		if created.ID == "" {
			return fmt.Errorf("create index %q: empty id in response", c.indexFriendly)
		}
		c.indexID = created.ID
		idx = created
		freshlyCreated = true
	default:
		return fmt.Errorf("lookup index %q: %w", expectedIndexID, err)
	}

	// Newly-created indexes briefly report status=pending|building before
	// they can serve search queries. Poll until ready or timeout. Already-
	// existing ready indexes short-circuit on the first check.
	if freshlyCreated || !strings.EqualFold(idx.Status, "ready") {
		if err := c.waitIndexReady(ctx); err != nil {
			return err
		}
	}
	return nil
}

// waitIndexReady polls the index status endpoint until it reports "ready"
// or the poll timeout expires. Required because a just-created index
// returns HTTP 503 index_not_ready for its first few hundred milliseconds.
func (c *Client) waitIndexReady(ctx context.Context) error {
	const (
		pollInterval = 200 * time.Millisecond
		pollTimeout  = 15 * time.Second
	)
	deadline := time.Now().Add(pollTimeout)
	path := fmt.Sprintf("/v1/tenants/%s/indexes/%s/status", c.tenantID, c.indexID)
	var statusResp struct {
		IndexID string `json:"index_id"`
		Status  string `json:"status"`
		Error   string `json:"error"`
	}
	for {
		err := c.doJSON(ctx, http.MethodGet, path, nil, &statusResp)
		if err != nil {
			var e *apiError
			if !asAPIError(err, &e) || (e.Status != http.StatusNotFound && e.Status != http.StatusServiceUnavailable) {
				return fmt.Errorf("poll index status: %w", err)
			}
			// else: transient during creation, fall through and retry
		} else {
			switch strings.ToLower(statusResp.Status) {
			case "ready":
				return nil
			case "error":
				return fmt.Errorf("index %s entered error state: %s", c.indexID, statusResp.Error)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("index %s not ready after %s (last status: %q)",
				c.indexID, pollTimeout, statusResp.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// isNotFound reports whether err is an *apiError with 404 status or a
// not_found code. GraphANN currently returns 404 for missing resources.
func isNotFound(err error) bool {
	var e *apiError
	if !asAPIError(err, &e) {
		return false
	}
	return e.Status == http.StatusNotFound || e.Code == "not_found"
}

func asAPIError(err error, target **apiError) bool {
	for err != nil {
		if e, ok := err.(*apiError); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		if u, ok := err.(unwrapper); ok {
			err = u.Unwrap()
			continue
		}
		return false
	}
	return false
}

// ---- Documents ----------------------------------------------------------

// Document is the minimal payload for POST /documents. We only send text;
// GraphANN's typed metadata schema silently drops arbitrary keys so
// kind/tags/source are encoded into the text itself by encodeMemoryText.
type Document struct {
	Text string `json:"text"`
}

type addDocumentsRequest struct {
	Documents []Document `json:"documents"`
}

// AddDocumentsResponse is the decoded response from POST /documents.
type AddDocumentsResponse struct {
	IndexID  string   `json:"index_id"`
	ChunkIDs []string `json:"chunk_ids"`
	Added    int      `json:"added"`
}

// AddDocuments stores one or more text documents in the configured index.
// The caller must have invoked Bootstrap at least once beforehand.
func (c *Client) AddDocuments(ctx context.Context, docs []Document) (*AddDocumentsResponse, error) {
	var out AddDocumentsResponse
	path := fmt.Sprintf("/v1/tenants/%s/indexes/%s/documents", c.tenantID, c.indexID)
	if err := c.doJSON(ctx, http.MethodPost, path, addDocumentsRequest{Documents: docs}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---- Search -------------------------------------------------------------

type searchRequest struct {
	Query string `json:"query"`
	K     int    `json:"k"`
}

// SearchResult matches the per-result shape returned by POST /search.
type SearchResult struct {
	Metadata map[string]any `json:"metadata"`
	ID       string         `json:"id"`
	Text     string         `json:"text"`
	Score    float64        `json:"score"`
}

// SearchResponse is the envelope for POST /search.
type SearchResponse struct {
	Results []SearchResult `json:"results"`
	Total   int            `json:"total"`
}

// Search performs a hybrid (vector + text) search against the configured
// index. k is clamped to a sane [1, 100] range.
func (c *Client) Search(ctx context.Context, query string, k int) (*SearchResponse, error) {
	if k <= 0 {
		k = 10
	}
	if k > 100 {
		k = 100
	}
	var out SearchResponse
	path := fmt.Sprintf("/v1/tenants/%s/indexes/%s/search", c.tenantID, c.indexID)
	if err := c.doJSON(ctx, http.MethodPost, path, searchRequest{Query: query, K: k}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// IDs exposes the resolved server-side IDs for diagnostics.
func (c *Client) IDs() (tenant, index string) { return c.tenantID, c.indexID }

// ---- Delete / Get / Stats (v0.2 endpoints) ------------------------------

// LiveStatsResponse is the decoded response from GET /live-stats. The
// Documents field is the total count of documents ever added, including
// tombstoned ones — useful for walking the monotonic doc_id space.
type LiveStatsResponse struct {
	IndexID       string `json:"index_id"`
	BaseChunks    int    `json:"base_chunks"`
	DeltaChunks   int    `json:"delta_chunks"`
	LiveChunks    int    `json:"live_chunks"`
	TotalChunks   int    `json:"total_chunks"`
	DeletedChunks int    `json:"deleted_chunks"`
	Documents     int    `json:"documents"`
	Dimension     int    `json:"dimension"`
	IsDirty       bool   `json:"is_dirty"`
	IsLive        bool   `json:"is_live"`
}

// LiveStats fetches document / chunk counts for the configured index.
func (c *Client) LiveStats(ctx context.Context) (*LiveStatsResponse, error) {
	var out LiveStatsResponse
	path := fmt.Sprintf("/v1/tenants/%s/indexes/%s/live-stats", c.tenantID, c.indexID)
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DocumentChunk mirrors the per-chunk entries returned by GET /documents/{id}.
type DocumentChunk struct {
	UUID       string `json:"uuid"`
	Text       string `json:"text"`
	FilePath   string `json:"file_path"`
	CommitSHA  string `json:"commit_sha"`
	RepoID     string `json:"repo_id"`
	ChunkID    int    `json:"chunk_id"`
	ChunkIndex int    `json:"chunk_index"`
	Start      int    `json:"start"`
	End        int    `json:"end"`
}

// GetDocumentResponse is the decoded response from GET /documents/{id}.
type GetDocumentResponse struct {
	IndexID     string          `json:"index_id"`
	ExternalID  string          `json:"external_id"`
	Chunks      []DocumentChunk `json:"chunks"`
	DocumentID  int             `json:"document_id"`
	TotalChunks int             `json:"total_chunks"`
}

// GetDocument fetches a document and its chunks by integer document ID.
// Returns (nil, nil) if the document does not exist (HTTP 404), so callers
// walking a range can distinguish "missing" from "error".
func (c *Client) GetDocument(ctx context.Context, docID int) (*GetDocumentResponse, error) {
	var out GetDocumentResponse
	path := fmt.Sprintf("/v1/tenants/%s/indexes/%s/documents/%d", c.tenantID, c.indexID, docID)
	err := c.doJSON(ctx, http.MethodGet, path, nil, &out)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &out, nil
}

// DeleteDocumentResponse mirrors the shape returned by DELETE /documents/{id}.
type DeleteDocumentResponse struct {
	IndexID       string `json:"index_id"`
	DocumentID    int    `json:"document_id"`
	DeletedChunks int    `json:"deleted_chunks"`
}

// DeleteDocument removes a single document by integer ID. Tombstones the
// underlying chunks; live_chunks decreases, total_chunks is unchanged.
func (c *Client) DeleteDocument(ctx context.Context, docID int) (*DeleteDocumentResponse, error) {
	var out DeleteDocumentResponse
	path := fmt.Sprintf("/v1/tenants/%s/indexes/%s/documents/%d", c.tenantID, c.indexID, docID)
	if err := c.doJSON(ctx, http.MethodDelete, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
