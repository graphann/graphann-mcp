package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"unicode/utf8"

	graphann "github.com/graphann/graphann-client-go"
)

// graphAPI is the subset of the GraphANN client this server consumes.
// *graphann.Client satisfies it; tests substitute a fake.
type graphAPI interface {
	GetTenant(ctx context.Context, id string) (*graphann.Tenant, error)
	CreateTenant(ctx context.Context, req graphann.CreateTenantRequest) (*graphann.Tenant, error)
	GetIndex(ctx context.Context, tenantID, indexID string) (*graphann.Index, error)
	CreateIndex(ctx context.Context, tenantID string, req graphann.CreateIndexRequest) (*graphann.Index, error)
	Search(ctx context.Context, tenantID, indexID string, req graphann.SearchRequest) (*graphann.SearchResponse, error)
	AddDocuments(ctx context.Context, tenantID, indexID string, req graphann.AddDocumentsRequest) (*graphann.AddDocumentsResponse, error)
	GetDocument(ctx context.Context, tenantID, indexID string, docID int) (*graphann.DocumentResponse, error)
	DeleteDocument(ctx context.Context, tenantID, indexID string, docID int) (*graphann.DeleteDocumentResponse, error)
	GetLiveStats(ctx context.Context, tenantID, indexID string) (*graphann.LiveStatsResponse, error)
}

// Memory holds the API client plus the resolved server-side tenant/index
// IDs for the current process. Bootstrap runs lazily under a mutex and is
// retried on the next call after a failure.
type Memory struct {
	Client     graphAPI
	TenantSlug string
	IndexSlug  string
	TenantID   string
	IndexID    string
	indexDesc  string
	mu         sync.Mutex
	ready      bool
}

// NewMemory builds a Memory scoped to the given friendly slugs. IDs are
// resolved on the first Ensure call.
func NewMemory(api graphAPI, tenantSlug, indexSlug string) *Memory {
	return &Memory{
		Client:     api,
		TenantSlug: tenantSlug,
		IndexSlug:  indexSlug,
		indexDesc:  "per-project Claude Code memory index",
	}
}

// Ensure runs the tenant + index lookup-or-create dance until it succeeds
// once, then caches the result. Failures are not cached, so a transient
// outage at startup does not poison the process. Safe for concurrent use.
func (m *Memory) Ensure(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ready {
		return nil
	}
	tenantID, indexID, err := bootstrap(ctx, m.Client, m.TenantSlug, m.IndexSlug, m.indexDesc)
	if err != nil {
		return err
	}
	m.TenantID, m.IndexID, m.ready = tenantID, indexID, true
	return nil
}

// bootstrap ensures the friendly tenant + index exist and returns the
// server-side IDs. GraphANN identifies tenants/indexes by their friendly
// slug via prefixed IDs ("t_<slug>", "i_<slug>"); CreateTenant is
// idempotent on ID, CreateIndex is not, so we GET-then-POST for both.
//
// It does not wait for the index to become "ready": an empty index stays
// "pending" until the first ingest, and the SDK retry policy absorbs the
// later transition.
func bootstrap(ctx context.Context, api graphAPI, tenantSlug, indexSlug, indexDesc string) (string, string, error) {
	tenantID, err := ensureTenant(ctx, api, "t_"+tenantSlug, tenantSlug)
	if err != nil {
		return "", "", err
	}
	indexID, err := ensureIndex(ctx, api, tenantID, tenantSlug, indexSlug, indexDesc)
	if err != nil {
		return "", "", err
	}
	return tenantID, indexID, nil
}

func ensureTenant(ctx context.Context, api graphAPI, expectedID, slug string) (string, error) {
	t, err := api.GetTenant(ctx, expectedID)
	switch {
	case err == nil && t != nil && t.ID != "":
		return t.ID, nil
	case errors.Is(err, graphann.ErrNotFound):
		created, cerr := api.CreateTenant(ctx, graphann.CreateTenantRequest{ID: slug, Name: slug})
		if cerr != nil {
			return "", fmt.Errorf("create tenant %q: %w", slug, cerr)
		}
		if created == nil || created.ID == "" {
			return "", fmt.Errorf("create tenant %q: empty id in response", slug)
		}
		return created.ID, nil
	default:
		return "", fmt.Errorf("lookup tenant %q: %w", expectedID, err)
	}
}

// ensureIndex resolves the project index. Index IDs are global on the
// server, so the plain "<slug>" ID can already belong to another tenant.
// Lookup order keeps existing data reachable: plain ID first, then the
// tenant-qualified ID. Creation tries the plain ID and falls back to the
// qualified one on a conflict.
func ensureIndex(ctx context.Context, api graphAPI, tenantID, tenantSlug, slug, desc string) (string, error) {
	qualified := slugify(tenantSlug + "-" + slug)
	for _, cand := range []string{slug, qualified} {
		id, found, err := lookupIndex(ctx, api, tenantID, cand)
		if err != nil {
			return "", err
		}
		if found {
			return id, nil
		}
	}
	for _, cand := range []string{slug, qualified} {
		created, err := api.CreateIndex(ctx, tenantID, graphann.CreateIndexRequest{ID: cand, Name: slug, Description: desc})
		if errors.Is(err, graphann.ErrConflict) && cand != qualified {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("create index %q: %w", cand, err)
		}
		if created == nil || created.ID == "" {
			return "", fmt.Errorf("create index %q: empty id in response", cand)
		}
		return created.ID, nil
	}
	return "", fmt.Errorf("create index %q: no free id", slug)
}

func lookupIndex(ctx context.Context, api graphAPI, tenantID, slug string) (string, bool, error) {
	idx, err := api.GetIndex(ctx, tenantID, "i_"+slug)
	switch {
	case err == nil && idx != nil && idx.ID != "":
		return idx.ID, true, nil
	case err == nil || errors.Is(err, graphann.ErrNotFound):
		return "", false, nil
	default:
		return "", false, fmt.Errorf("lookup index %q: %w", "i_"+slug, err)
	}
}

// truncate trims s to at most n runes, appending "..." when shortened.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "..."
}
