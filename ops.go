package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode"

	graphann "github.com/graphann/graphann-client-go"
)

const (
	// dedupThreshold is the minimum dense score for a stored memory to count
	// as a duplicate candidate. It is low because the stored kind/tags
	// prefix dilutes the score of short exact duplicates (~0.85). The score
	// alone is not enough: a negated or swapped statement ("prefers spaces
	// over tabs") scores ~0.95 against its opposite, so candidates must also
	// pass the lexical check.
	dedupThreshold = 0.75
	// dedupSimilarity is the minimum word-bigram Jaccard overlap for a
	// candidate to be a real duplicate.
	dedupSimilarity = 0.8
	// dedupProbeK is how many neighbours the duplicate check inspects.
	dedupProbeK = 3
	// forgetConfidence is the minimum score for the top match to be deleted.
	forgetConfidence = 0.85
	// forgetMargin is the minimum score lead the top match needs over the
	// runner-up document, so a query never deletes one of several lookalikes.
	forgetMargin = 0.05
	// recallMinScore drops weak matches from memory_recall. On the live
	// index, relevant top hits score >= 0.37 and unrelated queries <= 0.32.
	recallMinScore = 0.35
	// oversample multiplies k when fetching so per-document collapsing still
	// fills k results.
	oversample = 4
	// maxFetchK caps a single search request.
	maxFetchK = 100
	// recentLookupFactor bounds the document-ID walk in ListRecent.
	recentLookupFactor = 3
	recentMinLookups   = 20
	// memoryIDPrefix marks the external id each new memory gets. The id lets the server
	// group a memory's chunks and start a related search from it.
	memoryIDPrefix = "mem_"
	memoryIDBytes  = 16
	// relatedDiversity spreads a two-hop walk over different neighbours. On a GraphANN
	// index with near-duplicate documents, walks returned no new documents below 0.5.
	relatedDiversity = 0.5
	// relatedWalkHops is the walk depth that adds novel_only and diversity.
	relatedWalkHops = 2
	// relatedFrontierK covers the pool a walk picks its first hops from: the server's
	// default fanout of 10 times its diverse pool factor of 4.
	relatedFrontierK = 40
)

// queryOpts selects retrieval behaviour. The zero value is the raw dense
// search.
type queryOpts struct {
	MinScore float64
	Hybrid   bool
	Collapse bool
}

// defaultQueryOpts is the retrieval configuration used by the tools.
var defaultQueryOpts = queryOpts{Collapse: true, MinScore: recallMinScore}

// Hit is one search result with its document coordinates decoded.
type Hit struct {
	ChunkID    string
	Text       string
	Score      float64
	DocID      int
	ChunkIndex int
	HasDoc     bool
}

func toHit(r graphann.SearchResult) Hit {
	h := Hit{ChunkID: r.ID, Text: r.Text, Score: float64(r.Score)}
	h.DocID, h.HasDoc = docIDFromMetadata(r.Metadata)
	if m, ok := r.Metadata.(map[string]any); ok {
		if ci, ok := m["chunk_index"].(float64); ok {
			h.ChunkIndex = int(ci)
		}
	}
	return h
}

// collapseByDoc keeps the best-scoring chunk of each document, preserving
// rank order. Hits without a document ID are kept as they are.
func collapseByDoc(hits []Hit) []Hit {
	seen := make(map[int]bool, len(hits))
	out := make([]Hit, 0, len(hits))
	for _, h := range hits {
		if h.HasDoc {
			if seen[h.DocID] {
				continue
			}
			seen[h.DocID] = true
		}
		out = append(out, h)
	}
	return out
}

// Query searches memory and returns at most k hits.
func (m *Memory) Query(ctx context.Context, q string, k int, o queryOpts) ([]Hit, error) {
	fetch := k
	if o.Collapse {
		fetch = min(k*oversample, maxFetchK)
	}
	req := graphann.SearchRequest{Query: q, K: fetch, Hybrid: o.Hybrid}
	if o.Collapse {
		// The server groups memories that carry an external id; collapseByDoc still
		// merges the chunks of memories stored before external ids.
		req.GroupBy, req.MaxPerDoc = graphann.SearchGroupByDocument, 1
	}
	resp, err := m.Client.Search(ctx, m.TenantID, m.IndexID, req)
	if err != nil {
		return nil, err
	}
	return selectHits(resp, k, o), nil
}

// selectHits keeps the results at or above o.MinScore, collapses them per document when
// o.Collapse is set, and returns at most k.
func selectHits(resp *graphann.SearchResponse, k int, o queryOpts) []Hit {
	if resp == nil {
		return nil
	}
	hits := make([]Hit, 0, len(resp.Results))
	for _, r := range resp.Results {
		if h := toHit(r); h.Score >= o.MinScore {
			hits = append(hits, h)
		}
	}
	if o.Collapse {
		hits = collapseByDoc(hits)
	}
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits
}

// newMemoryID returns a random external id for a new memory.
func newMemoryID() (string, error) {
	b := make([]byte, memoryIDBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("memory id: %w", err)
	}
	return memoryIDPrefix + hex.EncodeToString(b), nil
}

// StoreResult reports the outcome of Store.
type StoreResult struct {
	Duplicate *Hit
	ChunkID   string
	Chunks    int
}

// Store persists a memory unless a lexically equivalent one already exists.
func (m *Memory) Store(ctx context.Context, args StoreArgs) (StoreResult, error) {
	text := strings.TrimSpace(args.Text)
	if text == "" {
		return StoreResult{}, errors.New("text is required and must be non-empty")
	}
	if err := m.Ensure(ctx); err != nil {
		return StoreResult{}, fmt.Errorf("bootstrap: %w", err)
	}
	if dup, ok := m.findDuplicate(ctx, text); ok {
		return StoreResult{Duplicate: &dup}, nil
	}
	id, err := newMemoryID()
	if err != nil {
		return StoreResult{}, fmt.Errorf("store: %w", err)
	}
	resp, err := m.Client.AddDocuments(ctx, m.TenantID, m.IndexID, graphann.AddDocumentsRequest{
		Documents: []graphann.Document{{ID: id, Text: encodeMemoryText(args.Kind, args.Tags, args.Source, text)}},
	})
	if err != nil {
		return StoreResult{}, fmt.Errorf("store: %w", err)
	}
	if resp.Added == 0 || len(resp.ChunkIDs) == 0 {
		return StoreResult{}, errors.New("store: GraphANN reported 0 documents added")
	}
	return StoreResult{ChunkID: resp.ChunkIDs[0], Chunks: len(resp.ChunkIDs)}, nil
}

// findDuplicate reports a stored memory equivalent to text. A failed probe
// is treated as "no duplicate": storing twice beats losing a memory.
func (m *Memory) findDuplicate(ctx context.Context, text string) (Hit, bool) {
	hits, err := m.Query(ctx, text, dedupProbeK, queryOpts{})
	if err != nil {
		return Hit{}, false
	}
	for _, h := range hits {
		if h.Score >= dedupThreshold && sameStatement(text, stripTagPrefix(h.Text)) {
			return h, true
		}
	}
	return Hit{}, false
}

// words lowercases s and splits it into letter/digit runs.
func words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

// sameStatement reports whether a and b state the same thing: equal word
// sequences, or word-bigram Jaccard overlap of at least dedupSimilarity.
// Bigrams keep word order, so swapped or negated statements differ.
func sameStatement(a, b string) bool {
	wa, wb := words(a), words(b)
	if len(wa) == 0 || len(wb) == 0 {
		return false
	}
	if strings.Join(wa, " ") == strings.Join(wb, " ") {
		return true
	}
	ba, bb := bigrams(wa), bigrams(wb)
	inter := 0
	for g := range ba {
		if bb[g] {
			inter++
		}
	}
	union := len(ba) + len(bb) - inter
	return union > 0 && float64(inter)/float64(union) >= dedupSimilarity
}

func bigrams(w []string) map[string]bool {
	out := make(map[string]bool, len(w))
	for i := 0; i+1 < len(w); i++ {
		out[w[i]+" "+w[i+1]] = true
	}
	return out
}

// ForgetResult reports the outcome of Forget. Refused is non-empty when
// nothing was deleted.
type ForgetResult struct {
	Refused    string
	Deleted    Hit
	Candidates []Hit
	Chunks     int
	Removed    bool
}

// Forget deletes one memory, identified by document ID (id) or by a query
// that must single out one memory with confidence.
func (m *Memory) Forget(ctx context.Context, q string, id *int) (ForgetResult, error) {
	if err := m.Ensure(ctx); err != nil {
		return ForgetResult{}, fmt.Errorf("bootstrap: %w", err)
	}
	if id != nil {
		return m.forgetByID(ctx, *id)
	}
	hits, err := m.Query(ctx, q, 3, queryOpts{Collapse: true})
	if err != nil {
		return ForgetResult{}, fmt.Errorf("forget: search failed: %w", err)
	}
	if len(hits) == 0 {
		return ForgetResult{Refused: "no memory matches"}, nil
	}
	top := hits[0]
	switch {
	case top.Score < forgetConfidence:
		return ForgetResult{Refused: fmt.Sprintf("top match score %.2f is below confidence threshold %.2f", top.Score, forgetConfidence), Candidates: hits}, nil
	case len(hits) > 1 && top.Score-hits[1].Score < forgetMargin:
		return ForgetResult{Refused: fmt.Sprintf("ambiguous: top two matches score %.2f and %.2f", top.Score, hits[1].Score), Candidates: hits}, nil
	case !top.HasDoc:
		return ForgetResult{Refused: fmt.Sprintf("top match %s has no document_id in metadata; cannot delete", top.ChunkID), Candidates: hits}, nil
	}
	return m.deleteDoc(ctx, top)
}

func (m *Memory) forgetByID(ctx context.Context, id int) (ForgetResult, error) {
	doc, err := m.Client.GetDocument(ctx, m.TenantID, m.IndexID, id)
	if errors.Is(err, graphann.ErrNotFound) {
		return ForgetResult{Refused: fmt.Sprintf("no memory with document id %d", id)}, nil
	}
	if err != nil {
		return ForgetResult{}, fmt.Errorf("forget: lookup failed: %w", err)
	}
	return m.deleteDoc(ctx, Hit{DocID: id, HasDoc: true, Text: joinChunks(doc.Chunks)})
}

func (m *Memory) deleteDoc(ctx context.Context, h Hit) (ForgetResult, error) {
	del, err := m.Client.DeleteDocument(ctx, m.TenantID, m.IndexID, h.DocID)
	if err != nil {
		return ForgetResult{}, fmt.Errorf("forget: delete failed: %w", err)
	}
	return ForgetResult{Removed: true, Deleted: h, Chunks: del.DeletedChunks}, nil
}

// RecentItem is one memory returned by ListRecent.
type RecentItem struct {
	Text  string
	DocID int
}

// ListRecent returns up to k live memories, newest first. GraphANN has no
// time-ordered browse endpoint, so it walks the monotonic document-ID
// space downward, skipping deleted documents. total is the number of
// document IDs ever issued.
func (m *Memory) ListRecent(ctx context.Context, k int) (items []RecentItem, total int, err error) {
	if err := m.Ensure(ctx); err != nil {
		return nil, 0, fmt.Errorf("bootstrap: %w", err)
	}
	stats, err := m.Client.GetLiveStats(ctx, m.TenantID, m.IndexID)
	if err != nil {
		return nil, 0, fmt.Errorf("list_recent: live-stats failed: %w", err)
	}
	maxLookups := max(k*recentLookupFactor, recentMinLookups)
	lookups := 0
	for id := stats.Documents - 1; id >= 0 && len(items) < k && lookups < maxLookups; id-- {
		lookups++
		doc, derr := m.Client.GetDocument(ctx, m.TenantID, m.IndexID, id)
		if derr != nil || doc == nil || len(doc.Chunks) == 0 {
			continue // deleted or transient: keep walking
		}
		items = append(items, RecentItem{DocID: id, Text: stripTagPrefix(joinChunks(doc.Chunks))})
	}
	return items, stats.Documents, nil
}

func joinChunks(chunks []graphann.DocumentChunk) string {
	var b strings.Builder
	for i, ch := range chunks {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(ch.Text)
	}
	return b.String()
}

// RelatedHit is a memory related to the source. Via lists the document ids a walk passed
// through, and Support counts the walk paths that reached it.
type RelatedHit struct {
	Hit
	Via     []int
	Support int
}

// RelatedResult reports the outcome of Related. Refused is non-empty when the source was not
// found. Legacy means the source has no external id, so it was matched by its text, one hop only.
type RelatedResult struct {
	Refused string
	Source  Hit
	Hits    []RelatedHit
	Legacy  bool
}

// Related returns up to k memories related to the memory with document id id. With hops of
// 2 it walks through another memory and keeps only memories outside the direct neighbours,
// which surfaces connections a direct search does not show.
func (m *Memory) Related(ctx context.Context, id, k, hops int) (RelatedResult, error) {
	if err := m.Ensure(ctx); err != nil {
		return RelatedResult{}, fmt.Errorf("bootstrap: %w", err)
	}
	doc, err := m.Client.GetDocument(ctx, m.TenantID, m.IndexID, id)
	if errors.Is(err, graphann.ErrNotFound) {
		return RelatedResult{Refused: fmt.Sprintf("no memory with document id %d", id)}, nil
	}
	if err != nil {
		return RelatedResult{}, fmt.Errorf("related: lookup failed: %w", err)
	}
	src := Hit{DocID: id, HasDoc: true, Text: stripTagPrefix(joinChunks(doc.Chunks))}
	if doc.ExternalID == "" {
		hits, err := m.Query(ctx, src.Text, k+1, defaultQueryOpts)
		if err != nil {
			return RelatedResult{}, fmt.Errorf("related: %w", err)
		}
		return RelatedResult{Source: src, Legacy: true, Hits: withoutSource(hits, id, k)}, nil
	}
	req := graphann.SearchRequest{RelatedTo: doc.ExternalID, K: min(k*oversample, maxFetchK)}
	o := defaultQueryOpts
	if hops >= relatedWalkHops {
		// Walk scores multiply the hop scores, so the recall floor would drop every walked memory.
		req.Hops, req.NovelOnly, req.Diversity = relatedWalkHops, true, relatedDiversity
		o.MinScore = 0
	}
	resp, err := m.Client.Search(ctx, m.TenantID, m.IndexID, req)
	if err == nil && req.NovelOnly && resp != nil && len(resp.Results) == 0 {
		// Every memory is a direct neighbour of the source in a small index.
		req.NovelOnly = false
		resp, err = m.Client.Search(ctx, m.TenantID, m.IndexID, req)
	}
	if errors.Is(err, graphann.ErrNotFound) {
		return RelatedResult{Refused: fmt.Sprintf("memory #%d is not searchable yet", id)}, nil
	}
	if err != nil {
		return RelatedResult{}, fmt.Errorf("related: %w", err)
	}
	hits := withoutSource(selectHits(resp, k+1, o), id, k)
	if req.Hops >= relatedWalkHops {
		hits = withPaths(resp.Results, m.hopDocs(ctx, req.RelatedTo, resp.Results), hits)
	}
	return RelatedResult{Source: src, Hits: hits}, nil
}

// hopDocs maps the external ids on walk paths to document ids. A hop is usually a direct
// neighbour of the source, which novel_only leaves out of the walk results, so one one-hop
// search of the walk's frontier pool names it. A failed lookup only leaves hops unnamed.
func (m *Memory) hopDocs(ctx context.Context, source string, walk []graphann.SearchResult) map[string]int {
	docs := map[string]int{}
	missing := false
	for _, r := range walk {
		if h := toHit(r); h.HasDoc {
			docs[r.ID] = h.DocID
		}
	}
	for _, r := range walk {
		for _, hop := range r.Path {
			if _, ok := docs[hop.ID]; !ok {
				missing = true
			}
		}
	}
	if !missing {
		return docs
	}
	resp, err := m.Client.Search(ctx, m.TenantID, m.IndexID, graphann.SearchRequest{RelatedTo: source, K: relatedFrontierK})
	if err != nil || resp == nil {
		return docs
	}
	for _, r := range resp.Results {
		if h := toHit(r); h.HasDoc {
			docs[r.ID] = h.DocID
		}
	}
	return docs
}

// withoutSource drops the source memory and returns at most k hits.
func withoutSource(hits []Hit, id, k int) []RelatedHit {
	out := make([]RelatedHit, 0, k)
	for _, h := range hits {
		if h.HasDoc && h.DocID == id {
			continue
		}
		if len(out) == k {
			break
		}
		out = append(out, RelatedHit{Hit: h})
	}
	return out
}

// withPaths adds the walk path and support of each hit. A path names memories by external
// id; docOf maps each one to a document id, and an unknown one is left out.
func withPaths(walk []graphann.SearchResult, docOf map[string]int, hits []RelatedHit) []RelatedHit {
	paths := make(map[string]graphann.SearchResult, len(walk))
	for _, r := range walk {
		paths[r.ID] = r
	}
	for i := range hits {
		r, ok := paths[hits[i].ChunkID]
		if !ok || len(r.Path) < relatedWalkHops {
			continue
		}
		hits[i].Support = r.Support
		for _, hop := range r.Path[:len(r.Path)-1] {
			if d, ok := docOf[hop.ID]; ok {
				hits[i].Via = append(hits[i].Via, d)
			}
		}
	}
	return hits
}
