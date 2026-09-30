package main

import (
	"context"
	"fmt"
	"strings"

	graphann "github.com/graphann/graphann-client-go"
)

// fakeAPI is an in-memory graphAPI. Search ranks stored chunks by word
// overlap with the query; fixed scores can be injected through scores.
type fakeAPI struct {
	tenants   map[string]bool
	indexes   map[string]string // index id -> owning tenant
	docs      []*fakeDoc
	scores    map[string]float32 // chunk text -> forced score
	failTimes int                // GetTenant failures before success
	searches  []graphann.SearchRequest
}

type fakeDoc struct {
	chunks  []string
	deleted bool
}

func newFake() *fakeAPI {
	return &fakeAPI{tenants: map[string]bool{}, indexes: map[string]string{}, scores: map[string]float32{}}
}

func (f *fakeAPI) GetTenant(_ context.Context, id string) (*graphann.Tenant, error) {
	if f.failTimes > 0 {
		f.failTimes--
		return nil, graphann.ErrNetwork
	}
	if f.tenants[id] {
		return &graphann.Tenant{ID: id}, nil
	}
	return nil, graphann.ErrNotFound
}

func (f *fakeAPI) CreateTenant(_ context.Context, r graphann.CreateTenantRequest) (*graphann.Tenant, error) {
	id := "t_" + r.ID
	f.tenants[id] = true
	return &graphann.Tenant{ID: id}, nil
}

func (f *fakeAPI) GetIndex(_ context.Context, tenantID, indexID string) (*graphann.Index, error) {
	if owner, ok := f.indexes[indexID]; ok && owner == tenantID {
		return &graphann.Index{ID: indexID}, nil
	}
	return nil, graphann.ErrNotFound
}

func (f *fakeAPI) CreateIndex(_ context.Context, tenantID string, r graphann.CreateIndexRequest) (*graphann.Index, error) {
	id := "i_" + r.ID
	if _, taken := f.indexes[id]; taken {
		return nil, graphann.ErrConflict
	}
	f.indexes[id] = tenantID
	return &graphann.Index{ID: id}, nil
}

func (f *fakeAPI) AddDocuments(_ context.Context, _, _ string, r graphann.AddDocumentsRequest) (*graphann.AddDocumentsResponse, error) {
	resp := &graphann.AddDocumentsResponse{}
	for _, d := range r.Documents {
		f.docs = append(f.docs, &fakeDoc{chunks: []string{d.Text}})
		resp.Added++
		resp.ChunkIDs = append(resp.ChunkIDs, fmt.Sprintf("chunk-%d", len(f.docs)-1))
	}
	return resp, nil
}

func (f *fakeAPI) Search(_ context.Context, _, _ string, r graphann.SearchRequest) (*graphann.SearchResponse, error) {
	f.searches = append(f.searches, r)
	var all []graphann.SearchResult
	for id, d := range f.docs {
		if d.deleted {
			continue
		}
		for ci, c := range d.chunks {
			sc, ok := f.scores[c]
			if !ok {
				sc = overlap(r.Query, c)
			}
			all = append(all, graphann.SearchResult{
				ID: fmt.Sprintf("c%d-%d", id, ci), Text: c, Score: sc,
				Metadata: map[string]any{"document_id": float64(id), "chunk_index": float64(ci)},
			})
		}
	}
	for i := range all {
		for j := i + 1; j < len(all); j++ {
			if all[j].Score > all[i].Score {
				all[i], all[j] = all[j], all[i]
			}
		}
	}
	if len(all) > r.K {
		all = all[:r.K]
	}
	return &graphann.SearchResponse{Results: all, Total: len(all)}, nil
}

func (f *fakeAPI) GetDocument(_ context.Context, _, _ string, id int) (*graphann.DocumentResponse, error) {
	if id < 0 || id >= len(f.docs) || f.docs[id].deleted {
		return nil, graphann.ErrNotFound
	}
	out := &graphann.DocumentResponse{DocumentID: id}
	for i, c := range f.docs[id].chunks {
		out.Chunks = append(out.Chunks, graphann.DocumentChunk{Text: c, ChunkIndex: i})
	}
	return out, nil
}

func (f *fakeAPI) DeleteDocument(_ context.Context, _, _ string, id int) (*graphann.DeleteDocumentResponse, error) {
	if id < 0 || id >= len(f.docs) || f.docs[id].deleted {
		return nil, graphann.ErrNotFound
	}
	f.docs[id].deleted = true
	return &graphann.DeleteDocumentResponse{DocumentID: id, DeletedChunks: len(f.docs[id].chunks)}, nil
}

func (f *fakeAPI) GetLiveStats(context.Context, string, string) (*graphann.LiveStatsResponse, error) {
	return &graphann.LiveStatsResponse{Documents: len(f.docs)}, nil
}

func overlap(q, c string) float32 {
	qs := map[string]bool{}
	for _, w := range words(q) {
		qs[w] = true
	}
	cw := words(strings.ToLower(c))
	if len(qs) == 0 || len(cw) == 0 {
		return 0
	}
	hit := 0
	for _, w := range cw {
		if qs[w] {
			hit++
		}
	}
	return float32(hit) / float32(len(cw))
}
