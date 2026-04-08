package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// dedupThreshold controls write-time dedup. If the nearest neighbour to a
// new memory scores above this value, the store is skipped. The live
// instance returns hybrid scores in roughly [0, 1] with near-duplicates
// landing above ~0.9.
const dedupThreshold = 0.92

// ---- memory_store -------------------------------------------------------

// StoreArgs is the schema for the memory_store tool. Struct tags drive
// both JSON decoding and the auto-generated MCP input schema via
// github.com/google/jsonschema-go.
type StoreArgs struct {
	Text   string   `json:"text" jsonschema:"the fact, preference, decision, or note to remember. Keep it self-contained — future recall will retrieve this verbatim."`
	Kind   string   `json:"kind,omitempty" jsonschema:"optional category such as fact, preference, decision, event, todo, or reference. Stored as a searchable tag prefix."`
	Source string   `json:"source,omitempty" jsonschema:"optional source hint: file path, URL, or a short conversational marker."`
	Tags   []string `json:"tags,omitempty" jsonschema:"optional free-form tags for filtering and future retrieval. Short lowercase words are best."`
}

// registerStore wires the memory_store tool onto the MCP server.
func registerStore(server *mcp.Server, client *Client) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "memory_store",
		Description: "Persist a fact, preference, decision, or note into the current project's long-term memory. " +
			"Use when the user tells you something worth remembering across sessions, " +
			"when you make a notable decision, or when you discover context that future sessions will need. " +
			"Writes are deduplicated against the nearest existing memory — near-identical content is skipped.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args StoreArgs) (*mcp.CallToolResult, any, error) {
		text := strings.TrimSpace(args.Text)
		if text == "" {
			return errorResult("text is required and must be non-empty"), nil, nil
		}
		if err := client.Bootstrap(ctx); err != nil {
			return errorResult(fmt.Sprintf("bootstrap: %v", err)), nil, nil
		}

		// dedup against nearest neighbour
		if existing, err := client.Search(ctx, text, 1); err == nil && len(existing.Results) > 0 {
			top := existing.Results[0]
			if top.Score >= dedupThreshold {
				return textResult(fmt.Sprintf(
					"Skipped — near-duplicate of existing memory %s (similarity %.2f):\n  %s",
					top.ID, top.Score, truncate(stripTagPrefix(top.Text), 160),
				)), nil, nil
			}
		}

		encoded := encodeMemoryText(args.Kind, args.Tags, args.Source, text)
		resp, err := client.AddDocuments(ctx, []Document{{Text: encoded}})
		if err != nil {
			return errorResult(fmt.Sprintf("store: %v", err)), nil, nil
		}
		if resp.Added == 0 || len(resp.ChunkIDs) == 0 {
			return errorResult("store: GraphANN reported 0 documents added"), nil, nil
		}
		_, indexID := client.IDs()
		return textResult(fmt.Sprintf("Stored memory %s in index %s.", resp.ChunkIDs[0], indexID)), nil, nil
	})
}

// ---- memory_search ------------------------------------------------------

// SearchArgs is the schema for the memory_search tool.
type SearchArgs struct {
	Query string `json:"query" jsonschema:"natural-language query describing what to find in memory"`
	K     int    `json:"k,omitempty" jsonschema:"maximum number of results (default 10, max 100)"`
}

// registerSearch wires the memory_search tool. It returns ranked results
// verbatim with similarity scores — useful when the caller wants to reason
// about what's in memory, not just pull context for the current task.
func registerSearch(server *mcp.Server, client *Client) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "memory_search",
		Description: "Search long-term memory and return ranked results with similarity scores. " +
			"Use this when you want to inspect what's in memory, compare multiple candidates, or reason about " +
			"whether a fact exists. For pulling context into the current task, prefer memory_recall instead.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args SearchArgs) (*mcp.CallToolResult, any, error) {
		q := strings.TrimSpace(args.Query)
		if q == "" {
			return errorResult("query is required and must be non-empty"), nil, nil
		}
		if err := client.Bootstrap(ctx); err != nil {
			return errorResult(fmt.Sprintf("bootstrap: %v", err)), nil, nil
		}
		resp, err := client.Search(ctx, q, args.K)
		if err != nil {
			return errorResult(fmt.Sprintf("search: %v", err)), nil, nil
		}
		return textResult(formatSearchResults(q, resp)), nil, nil
	})
}

// ---- memory_recall ------------------------------------------------------

// RecallArgs is the schema for the memory_recall tool.
type RecallArgs struct {
	Query string `json:"query" jsonschema:"what context to pull from memory for the current task"`
	K     int    `json:"k,omitempty" jsonschema:"maximum number of items to return (default 5, max 20)"`
}

// registerRecall wires the memory_recall tool. It produces a compact,
// human-readable context block ready to drop into the model's reasoning.
func registerRecall(server *mcp.Server, client *Client) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "memory_recall",
		Description: "Recall the most relevant memories for the current task as a compact context block. " +
			"Call this at the start of work on a project or when the user references something you don't have in " +
			"current context. Returns fewer, cleaner results than memory_search — optimised for direct use.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args RecallArgs) (*mcp.CallToolResult, any, error) {
		q := strings.TrimSpace(args.Query)
		if q == "" {
			return errorResult("query is required and must be non-empty"), nil, nil
		}
		k := args.K
		if k <= 0 {
			k = 5
		}
		if k > 20 {
			k = 20
		}
		if err := client.Bootstrap(ctx); err != nil {
			return errorResult(fmt.Sprintf("bootstrap: %v", err)), nil, nil
		}
		resp, err := client.Search(ctx, q, k)
		if err != nil {
			return errorResult(fmt.Sprintf("recall: %v", err)), nil, nil
		}
		return textResult(formatRecall(q, resp)), nil, nil
	})
}

// ---- memory_forget ------------------------------------------------------

// forgetConfidence is the minimum similarity score required to confidently
// forget the top search match. Below this threshold, memory_forget refuses
// and shows a preview so the caller can refine the query.
const forgetConfidence = 0.85

// ForgetArgs is the schema for the memory_forget tool.
type ForgetArgs struct {
	Query string `json:"query" jsonschema:"natural-language description of the memory to forget. Must confidently identify a single memory — refine the query if results are ambiguous."`
}

// registerForget wires the memory_forget tool. Looks up the top-ranked
// memory for the query, deletes it by its underlying document_id if the
// similarity score is confident, otherwise returns a preview and refuses.
func registerForget(server *mcp.Server, client *Client) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "memory_forget",
		Description: "Delete a memory from long-term storage by natural-language query. " +
			"The top-ranked match is removed only when the similarity score is high enough to be unambiguous. " +
			"Use when the user explicitly asks you to forget something, or when a stored memory is outdated and needs removal.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args ForgetArgs) (*mcp.CallToolResult, any, error) {
		q := strings.TrimSpace(args.Query)
		if q == "" {
			return errorResult("query is required and must be non-empty"), nil, nil
		}
		if err := client.Bootstrap(ctx); err != nil {
			return errorResult(fmt.Sprintf("bootstrap: %v", err)), nil, nil
		}
		resp, err := client.Search(ctx, q, 3)
		if err != nil {
			return errorResult(fmt.Sprintf("forget: search failed: %v", err)), nil, nil
		}
		if resp == nil || len(resp.Results) == 0 {
			return textResult(fmt.Sprintf("No memory matches %q — nothing to forget.", q)), nil, nil
		}
		top := resp.Results[0]
		if top.Score < forgetConfidence {
			return errorResult(fmt.Sprintf(
				"Refusing to forget — top match score %.2f is below confidence threshold %.2f.\nCandidate: %s\nRefine the query to match the exact memory you want to delete.",
				top.Score, forgetConfidence, truncate(stripTagPrefix(top.Text), 200),
			)), nil, nil
		}
		docID, ok := docIDFromMetadata(top.Metadata)
		if !ok {
			return errorResult(fmt.Sprintf("forget: top match %s has no document_id in metadata; cannot delete", top.ID)), nil, nil
		}
		del, err := client.DeleteDocument(ctx, docID)
		if err != nil {
			return errorResult(fmt.Sprintf("forget: delete failed: %v", err)), nil, nil
		}
		return textResult(fmt.Sprintf(
			"Forgot memory (document_id %d, %d chunk(s)) with similarity %.2f:\n  %s",
			del.DocumentID, del.DeletedChunks, top.Score, truncate(stripTagPrefix(top.Text), 200),
		)), nil, nil
	})
}

// ---- memory_list_recent -------------------------------------------------

// ListRecentArgs is the schema for the memory_list_recent tool.
type ListRecentArgs struct {
	K int `json:"k,omitempty" jsonschema:"maximum number of recent items to return (default 10, max 50)"`
}

// registerListRecent wires the memory_list_recent tool. GraphANN lacks a
// time-ordered browse endpoint, so we walk the monotonic document_id space
// from highest to lowest using GetDocument, skipping tombstoned entries.
// This is "newest first" under the assumption that document_ids are
// assigned in ingest order — which matches GraphANN's current behaviour.
func registerListRecent(server *mcp.Server, client *Client) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "memory_list_recent",
		Description: "List the most recently stored memories in this project, newest first. " +
			"Use when the user asks 'what did I just remember' or when you want to see fresh context without a specific query. " +
			"Returns at most k items, skipping any that have been forgotten.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args ListRecentArgs) (*mcp.CallToolResult, any, error) {
		k := args.K
		if k <= 0 {
			k = 10
		}
		if k > 50 {
			k = 50
		}
		if err := client.Bootstrap(ctx); err != nil {
			return errorResult(fmt.Sprintf("bootstrap: %v", err)), nil, nil
		}
		stats, err := client.LiveStats(ctx)
		if err != nil {
			return errorResult(fmt.Sprintf("list_recent: live-stats failed: %v", err)), nil, nil
		}
		if stats.Documents == 0 {
			return textResult("No memories stored in this project yet."), nil, nil
		}
		// Walk from highest doc_id down. Give up after 3*k lookups to avoid
		// pathological scans of heavily-tombstoned indexes.
		maxLookups := k * 3
		if maxLookups < 20 {
			maxLookups = 20
		}
		type item struct {
			docID int
			text  string
		}
		items := make([]item, 0, k)
		lookups := 0
		for id := stats.Documents - 1; id >= 0 && len(items) < k && lookups < maxLookups; id-- {
			lookups++
			doc, derr := client.GetDocument(ctx, id)
			if derr != nil {
				continue // transient — keep walking
			}
			if doc == nil || len(doc.Chunks) == 0 {
				continue // tombstoned
			}
			// Concatenate chunk text in chunk-index order (GraphANN already sorts).
			var txt strings.Builder
			for i, ch := range doc.Chunks {
				if i > 0 {
					txt.WriteString(" ")
				}
				txt.WriteString(ch.Text)
			}
			items = append(items, item{docID: id, text: stripTagPrefix(txt.String())})
		}
		if len(items) == 0 {
			return textResult("No live memories found in the most recent document range."), nil, nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "[Memory: %d recent item(s) out of %d total documents]\n",
			len(items), stats.Documents)
		for i, it := range items {
			fmt.Fprintf(&b, "%d. (doc %d) %s\n", i+1, it.docID, truncate(it.text, 300))
		}
		return textResult(b.String()), nil, nil
	})
}

// docIDFromMetadata extracts the integer document_id from a search result's
// metadata map. GraphANN returns it as a float64 under JSON decoding.
func docIDFromMetadata(md map[string]any) (int, bool) {
	if md == nil {
		return 0, false
	}
	raw, ok := md["document_id"]
	if !ok {
		return 0, false
	}
	switch v := raw.(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	}
	return 0, false
}

// ---- formatting helpers -------------------------------------------------

// textResult wraps a plain-text response into an MCP CallToolResult.
func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: s}},
	}
}

// errorResult wraps a plain-text error into an MCP CallToolResult with
// IsError set so the client can distinguish tool-level failures from
// transport errors. We deliberately do NOT return err from the handler for
// tool-level failures — that surfaces as a protocol error instead of a
// readable message to the model.
func errorResult(msg string) *mcp.CallToolResult {
	log.Printf("tool error: %s", msg)
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}

// formatSearchResults renders a ranked list with scores and chunk IDs for
// the memory_search tool. Falls through to a friendly empty-state string.
func formatSearchResults(query string, resp *SearchResponse) string {
	if resp == nil || len(resp.Results) == 0 {
		return fmt.Sprintf("No memories found for %q.", query)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Found %d memory item(s) for %q:\n", len(resp.Results), query)
	for i, r := range resp.Results {
		fmt.Fprintf(&b, "\n%d. [score %.3f] %s\n   id: %s\n",
			i+1, r.Score, truncate(stripTagPrefix(r.Text), 400), r.ID)
	}
	return b.String()
}

// formatRecall renders a compact context block for the memory_recall tool.
// Results are shown one per line with the tag prefix stripped.
func formatRecall(query string, resp *SearchResponse) string {
	if resp == nil || len(resp.Results) == 0 {
		return fmt.Sprintf("[Memory] no relevant items for %q.", query)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[Memory: %d relevant item(s)]\n", len(resp.Results))
	for i, r := range resp.Results {
		fmt.Fprintf(&b, "%d. %s\n", i+1, truncate(stripTagPrefix(r.Text), 300))
	}
	return b.String()
}
