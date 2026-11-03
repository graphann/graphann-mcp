package main

import (
	"context"
	"strings"
	"testing"

	graphann "github.com/graphann/graphann-client-go"
)

func readyMemory(t *testing.T, f *fakeAPI) *Memory {
	t.Helper()
	m := NewMemory(f, "u-claude", "proj")
	if err := m.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestEnsureRetriesAfterFailure(t *testing.T) {
	f := newFake()
	f.failTimes = 1
	m := NewMemory(f, "u-claude", "proj")
	if err := m.Ensure(context.Background()); err == nil {
		t.Fatal("first Ensure: want error")
	}
	if err := m.Ensure(context.Background()); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if m.IndexID != "i_proj" {
		t.Errorf("IndexID = %q, want i_proj", m.IndexID)
	}
}

func TestEnsureIndexCollision(t *testing.T) {
	f := newFake()
	f.tenants["t_other"] = true
	f.indexes["i_proj"] = "t_other" // another tenant owns the plain id

	m := NewMemory(f, "u-claude", "proj")
	if err := m.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.IndexID != "i_u-claude-proj" {
		t.Fatalf("IndexID = %q, want i_u-claude-proj", m.IndexID)
	}
	m2 := NewMemory(f, "u-claude", "proj")
	if err := m2.Ensure(context.Background()); err != nil || m2.IndexID != m.IndexID {
		t.Fatalf("re-resolve: id=%q err=%v, want %q", m2.IndexID, err, m.IndexID)
	}
}

func TestEnsureExistingPlainIndexKept(t *testing.T) {
	f := newFake()
	f.tenants["t_u-claude"] = true
	f.indexes["i_proj"] = "t_u-claude"
	m := NewMemory(f, "u-claude", "proj")
	if err := m.Ensure(context.Background()); err != nil || m.IndexID != "i_proj" {
		t.Fatalf("id=%q err=%v, want i_proj", m.IndexID, err)
	}
}

func TestSameStatement(t *testing.T) {
	cases := []struct {
		name, a, b string
		want       bool
	}{
		{"identical", "The user prefers tabs over spaces in Go code.", "The user prefers tabs over spaces in Go code.", true},
		{"punctuation and case", "The user prefers tabs over spaces in Go code.", "the user prefers TABS over spaces in go code", true},
		{"swapped terms", "The user prefers tabs over spaces in Go code.", "The user prefers spaces over tabs in Go code.", false},
		{"negation", "Deploys run through ArgoCD on the home cluster", "Deploys do not run through ArgoCD on the home cluster", false},
		{"unrelated", "tabs over spaces", "postgres for billing", false},
		{"empty", "", "anything", false},
		{"unicode", "Café résumé naïve", "café résumé NAÏVE!", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sameStatement(c.a, c.b); got != c.want {
				t.Errorf("sameStatement = %v, want %v", got, c.want)
			}
		})
	}
}

func TestStoreDedup(t *testing.T) {
	f := newFake()
	m := readyMemory(t, f)
	ctx := context.Background()
	const tabs = "The user prefers tabs over spaces in Go code."
	f.scores[tabs] = 0.97
	f.scores["The user prefers spaces over tabs in Go code."] = 0.95

	if r, err := m.Store(ctx, StoreArgs{Text: tabs}); err != nil || r.Duplicate != nil {
		t.Fatalf("first store: %+v %v", r, err)
	}
	r, err := m.Store(ctx, StoreArgs{Text: "the user prefers tabs over spaces in Go code", Kind: "preference"})
	if err != nil || r.Duplicate == nil {
		t.Fatalf("equivalent text must be skipped: %+v %v", r, err)
	}
	r, err = m.Store(ctx, StoreArgs{Text: "The user prefers spaces over tabs in Go code."})
	if err != nil || r.Duplicate != nil {
		t.Fatalf("contradicting text must be stored: %+v %v", r, err)
	}
	if len(f.docs) != 2 {
		t.Errorf("docs = %d, want 2", len(f.docs))
	}
}

func TestStoreDedupTaggedShortMemory(t *testing.T) {
	f := newFake()
	m := readyMemory(t, f)
	ctx := context.Background()
	const text = "Deploy uses ArgoCD"
	if _, err := m.Store(ctx, StoreArgs{Text: text, Kind: "fact"}); err != nil {
		t.Fatal(err)
	}
	stored := encodeMemoryText("fact", nil, "", text)
	f.scores[stored] = 0.86 // prefix dilution lowers the live score of exact duplicates
	r, err := m.Store(ctx, StoreArgs{Text: text})
	if err != nil || r.Duplicate == nil {
		t.Fatalf("tagged duplicate must be skipped: %+v %v", r, err)
	}
}

func TestStoreValidation(t *testing.T) {
	m := readyMemory(t, newFake())
	if _, err := m.Store(context.Background(), StoreArgs{Text: "   "}); err == nil {
		t.Error("blank text: want error")
	}
}

func TestQueryCollapsesChunksPerDocument(t *testing.T) {
	f := newFake()
	m := readyMemory(t, f)
	f.docs = []*fakeDoc{
		{chunks: []string{"alpha retry one", "alpha retry two", "alpha retry three"}},
		{chunks: []string{"beta unrelated alpha"}},
	}
	raw, _ := m.Query(context.Background(), "alpha retry", 2, queryOpts{})
	if len(raw) != 2 || raw[0].DocID != raw[1].DocID {
		t.Fatalf("raw search should be flooded by one doc: %+v", raw)
	}
	got, _ := m.Query(context.Background(), "alpha retry", 2, queryOpts{Collapse: true})
	if len(got) != 2 || got[0].DocID == got[1].DocID {
		t.Fatalf("collapsed search must return distinct docs: %+v", got)
	}
}

func TestQueryMinScoreAndHybridPassthrough(t *testing.T) {
	f := newFake()
	m := readyMemory(t, f)
	f.docs = []*fakeDoc{{chunks: []string{"alpha beta"}}, {chunks: []string{"zzz yyy"}}}
	got, err := m.Query(context.Background(), "alpha beta", 5, queryOpts{MinScore: 0.5, Hybrid: true})
	if err != nil || len(got) != 1 || got[0].DocID != 0 {
		t.Fatalf("got %+v err=%v", got, err)
	}
	if !f.searches[len(f.searches)-1].Hybrid {
		t.Error("Hybrid flag not forwarded")
	}
}

func TestForget(t *testing.T) {
	f := newFake()
	m := readyMemory(t, f)
	ctx := context.Background()
	f.docs = []*fakeDoc{
		{chunks: []string{"prefers tabs"}}, {chunks: []string{"prefers spaces"}}, {chunks: []string{"uses postgres"}},
	}
	f.scores["prefers tabs"], f.scores["prefers spaces"], f.scores["uses postgres"] = 0.90, 0.88, 0.2

	r, err := m.Forget(ctx, "indentation", nil)
	if err != nil || r.Removed || !strings.Contains(r.Refused, "ambiguous") || len(r.Candidates) < 2 {
		t.Fatalf("ambiguous query must be refused with candidates: %+v %v", r, err)
	}
	f.scores["prefers spaces"] = 0.5
	r, _ = m.Forget(ctx, "indentation", nil)
	if !r.Removed || r.Deleted.DocID != 0 || !f.docs[0].deleted {
		t.Fatalf("clear winner must be deleted: %+v", r)
	}
	f.scores["prefers spaces"] = 0.6
	if r, _ = m.Forget(ctx, "indentation", nil); r.Removed || !strings.Contains(r.Refused, "below") {
		t.Fatalf("low confidence must be refused: %+v", r)
	}
	id := 2
	if r, _ = m.Forget(ctx, "", &id); !r.Removed || !f.docs[2].deleted {
		t.Fatalf("delete by id: %+v", r)
	}
	if r, _ = m.Forget(ctx, "", &id); r.Removed || r.Refused == "" {
		t.Fatalf("second delete must be refused: %+v", r)
	}
}

func TestListRecentSkipsDeleted(t *testing.T) {
	f := newFake()
	m := readyMemory(t, f)
	for _, s := range []string{"[kind:fact] one", "two", "three"} {
		f.docs = append(f.docs, &fakeDoc{chunks: []string{s}})
	}
	f.docs[2].deleted = true
	items, total, err := m.ListRecent(context.Background(), 5)
	if err != nil || total != 3 || len(items) != 2 || items[0].Text != "two" || items[1].Text != "one" {
		t.Fatalf("items=%+v total=%d err=%v", items, total, err)
	}
}

func TestTruncateRuneSafe(t *testing.T) {
	if got := truncate("héllo wörld", 5); got != "héllo..." {
		t.Errorf("got %q", got)
	}
	if got := truncate("short", 10); got != "short" {
		t.Errorf("got %q", got)
	}
}

func TestClampK(t *testing.T) {
	for _, c := range []struct{ k, def, max, want int }{{0, 5, 20, 5}, {-1, 5, 20, 5}, {7, 5, 20, 7}, {99, 5, 20, 20}} {
		if got := clampK(c.k, c.def, c.max); got != c.want {
			t.Errorf("clampK(%d)=%d want %d", c.k, got, c.want)
		}
	}
}

var _ graphAPI = (*graphann.Client)(nil)

// New memories carry an external id, so the server can group their chunks and search from them.
func TestStoreGivesEachMemoryAnExternalID(t *testing.T) {
	f := newFake()
	m := readyMemory(t, f)
	for _, text := range []string{"The billing service uses Postgres.", "Deploys go through ArgoCD."} {
		if _, err := m.Store(context.Background(), StoreArgs{Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	a, b := f.docs[0].externalID, f.docs[1].externalID
	if !strings.HasPrefix(a, memoryIDPrefix) || !strings.HasPrefix(b, memoryIDPrefix) || a == b {
		t.Fatalf("external ids = %q, %q; want distinct %q ids", a, b, memoryIDPrefix)
	}
}

func TestQueryAsksTheServerToGroupByDocument(t *testing.T) {
	f := newFake()
	m := readyMemory(t, f)
	if _, err := m.Query(context.Background(), "x", 3, queryOpts{Collapse: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Query(context.Background(), "x", 3, queryOpts{}); err != nil {
		t.Fatal(err)
	}
	grouped, raw := f.searches[len(f.searches)-2], f.searches[len(f.searches)-1]
	if grouped.GroupBy != graphann.SearchGroupByDocument || grouped.MaxPerDoc != 1 {
		t.Fatalf("collapsing query = %+v, want group_by document, max_per_doc 1", grouped)
	}
	if raw.GroupBy != "" {
		t.Fatalf("raw query groups: %+v", raw)
	}
}

func storeAll(t *testing.T, m *Memory, texts ...string) {
	t.Helper()
	for _, text := range texts {
		if _, err := m.Store(context.Background(), StoreArgs{Text: text}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRelatedSearchesFromTheMemory(t *testing.T) {
	f := newFake()
	m := readyMemory(t, f)
	storeAll(t, m, "billing service postgres database", "billing service invoices postgres", "home cluster argocd deploys")
	res, err := m.Related(context.Background(), 0, 2, 1)
	if err != nil || res.Refused != "" || res.Legacy {
		t.Fatalf("related = %+v err = %v", res, err)
	}
	last := f.searches[len(f.searches)-1]
	if last.RelatedTo != f.docs[0].externalID || last.Query != "" || last.Hops != 0 || last.NovelOnly {
		t.Fatalf("request = %+v, want a one-hop related search from %q", last, f.docs[0].externalID)
	}
	if len(res.Hits) == 0 || res.Hits[0].DocID != 1 {
		t.Fatalf("hits = %+v, want #1 first", res.Hits)
	}
	for _, h := range res.Hits {
		if h.DocID == 0 {
			t.Fatal("the source memory must not be its own relative")
		}
	}
}

func TestRelatedWalkNamesTheMemoryItGoesThrough(t *testing.T) {
	f := newFake()
	m := readyMemory(t, f)
	storeAll(t, m, "billing service postgres database", "billing service invoices postgres", "postgres database backups nightly")
	res, err := m.Related(context.Background(), 0, 5, 2)
	if err != nil || res.Refused != "" {
		t.Fatalf("related = %+v err = %v", res, err)
	}
	walk := f.searches[len(f.searches)-2]
	if walk.Hops != 2 || !walk.NovelOnly || walk.Diversity != relatedDiversity {
		t.Fatalf("walk request = %+v", walk)
	}
	var walked *RelatedHit
	for i := range res.Hits {
		if len(res.Hits[i].Via) > 0 {
			walked = &res.Hits[i]
		}
	}
	if walked == nil || walked.Support != 2 {
		t.Fatalf("no walked hit with a path: %+v", res.Hits)
	}
	if walked.Via[0] != 1 {
		t.Fatalf("via = %v, want #1, the direct neighbour the walk went through", walked.Via)
	}
	if lookup := f.searches[len(f.searches)-1]; lookup.RelatedTo != f.docs[0].externalID || lookup.Hops != 0 || lookup.K != relatedFrontierK {
		t.Fatalf("hop lookup = %+v, want a one-hop related search of the frontier", lookup)
	}
}

// A memory stored before external ids has nothing to search from, so it is matched by its text.
func TestRelatedLegacyMemoryFallsBackToItsText(t *testing.T) {
	f := newFake()
	m := readyMemory(t, f)
	f.docs = append(f.docs, &fakeDoc{chunks: []string{"billing service postgres database"}})
	storeAll(t, m, "billing service invoices postgres")
	res, err := m.Related(context.Background(), 0, 3, 2)
	if err != nil || !res.Legacy {
		t.Fatalf("related = %+v err = %v, want the legacy path", res, err)
	}
	last := f.searches[len(f.searches)-1]
	if last.RelatedTo != "" || last.Query != "billing service postgres database" {
		t.Fatalf("legacy request = %+v, want a text query of the memory", last)
	}
	if len(res.Hits) != 1 || res.Hits[0].DocID != 1 {
		t.Fatalf("hits = %+v", res.Hits)
	}
}

func TestRelatedUnknownMemoryIsRefused(t *testing.T) {
	f := newFake()
	m := readyMemory(t, f)
	res, err := m.Related(context.Background(), 42, 3, 1)
	if err != nil || !strings.Contains(res.Refused, "42") {
		t.Fatalf("related = %+v err = %v", res, err)
	}
}

// In a small index every memory is a direct neighbour, so novel_only leaves nothing; the walk
// then runs once more without it.
func TestRelatedWalkRetriesWithoutNoveltyOnASmallIndex(t *testing.T) {
	f := newFake()
	f.novelEmpty = true
	m := readyMemory(t, f)
	storeAll(t, m, "billing service postgres database", "billing service invoices postgres", "postgres database backups nightly")
	before := len(f.searches)
	res, err := m.Related(context.Background(), 0, 5, 2)
	if err != nil || len(res.Hits) == 0 {
		t.Fatalf("related = %+v err = %v, want hits from the second walk", res, err)
	}
	walks := f.searches[before:]
	if len(walks) < 2 || !walks[0].NovelOnly || walks[1].NovelOnly || walks[1].Hops != relatedWalkHops {
		t.Fatalf("walks = %+v, want a novel_only walk then a plain walk", walks)
	}
}

func TestFormatRelated(t *testing.T) {
	hit := func(doc int, text string) Hit { return Hit{DocID: doc, HasDoc: true, Text: text, Score: 0.8} }
	walked := formatRelated(3, RelatedResult{Hits: []RelatedHit{{Hit: hit(7, "backups run nightly"), Via: []int{5}, Support: 2}}})
	for _, want := range []string{"related to #3", "(#7)", "backups run nightly", "via #5", "(2 paths)"} {
		if !strings.Contains(walked, want) {
			t.Fatalf("walk output %q lacks %q", walked, want)
		}
	}
	legacy := formatRelated(3, RelatedResult{Legacy: true})
	if !strings.Contains(legacy, "predates memory ids") || !strings.Contains(legacy, "nothing related to #3") {
		t.Fatalf("legacy output = %q", legacy)
	}
}
