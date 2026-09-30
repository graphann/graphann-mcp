package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	searchDefaultK = 10
	searchMaxK     = 100
	recallDefaultK = 5
	recallMaxK     = 20
	recentDefaultK = 10
	recentMaxK     = 50

	recallSnippetLen = 300
	searchSnippetLen = 400
	forgetSnippetLen = 200
)

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

func registerStore(server *mcp.Server, mem *Memory) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "memory_store",
		Description: "Persist a fact, preference, decision, or note into the current project's long-term memory. " +
			"Use when the user tells you something worth remembering across sessions, " +
			"when you make a notable decision, or when you discover context that future sessions will need. " +
			"Writes are deduplicated: a memory that states the same thing as an existing one is skipped. " +
			"A changed or contradicting statement is stored as a new memory — forget the outdated one.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args StoreArgs) (*mcp.CallToolResult, any, error) {
		res, err := mem.Store(ctx, args)
		if err != nil {
			return errorResult(err.Error()), nil, nil
		}
		if res.Duplicate != nil {
			return textResult(fmt.Sprintf(
				"Skipped — already stored as memory %s (similarity %.2f):\n  %s",
				hitLabel(*res.Duplicate), res.Duplicate.Score, truncate(stripTagPrefix(res.Duplicate.Text), forgetSnippetLen),
			)), nil, nil
		}
		return textResult(fmt.Sprintf("Stored memory %s in index %s.", res.ChunkID, mem.IndexID)), nil, nil
	})
}

// ---- memory_search ------------------------------------------------------

// SearchArgs is the schema for the memory_search tool.
type SearchArgs struct {
	Query string `json:"query" jsonschema:"natural-language query describing what to find in memory"`
	K     int    `json:"k,omitempty" jsonschema:"maximum number of results (default 10, max 100)"`
}

func registerSearch(server *mcp.Server, mem *Memory) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "memory_search",
		Description: "Search long-term memory and return ranked results with similarity scores and ids. " +
			"Use this when you want to inspect what's in memory, compare multiple candidates, or reason about " +
			"whether a fact exists. For pulling context into the current task, prefer memory_recall instead.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args SearchArgs) (*mcp.CallToolResult, any, error) {
		q, res := requireQuery(args.Query)
		if res != nil {
			return res, nil, nil
		}
		if err := mem.Ensure(ctx); err != nil {
			return errorResult(fmt.Sprintf("bootstrap: %v", err)), nil, nil
		}
		hits, err := mem.Query(ctx, q, clampK(args.K, searchDefaultK, searchMaxK), queryOpts{Collapse: true})
		if err != nil {
			return errorResult(fmt.Sprintf("search: %v", err)), nil, nil
		}
		return textResult(formatSearchResults(q, hits)), nil, nil
	})
}

// ---- memory_recall ------------------------------------------------------

// RecallArgs is the schema for the memory_recall tool.
type RecallArgs struct {
	Query string `json:"query" jsonschema:"what context to pull from memory for the current task"`
	K     int    `json:"k,omitempty" jsonschema:"maximum number of items to return (default 5, max 20)"`
}

func registerRecall(server *mcp.Server, mem *Memory) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "memory_recall",
		Description: "Recall the most relevant memories for the current task as a compact context block. " +
			"Call this at the start of work on a project or when the user references something you don't have in " +
			"current context. Returns fewer, cleaner results than memory_search — optimised for direct use.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args RecallArgs) (*mcp.CallToolResult, any, error) {
		q, res := requireQuery(args.Query)
		if res != nil {
			return res, nil, nil
		}
		if err := mem.Ensure(ctx); err != nil {
			return errorResult(fmt.Sprintf("bootstrap: %v", err)), nil, nil
		}
		hits, err := mem.Query(ctx, q, clampK(args.K, recallDefaultK, recallMaxK), defaultQueryOpts)
		if err != nil {
			return errorResult(fmt.Sprintf("recall: %v", err)), nil, nil
		}
		return textResult(formatRecall(q, hits)), nil, nil
	})
}

// ---- memory_forget ------------------------------------------------------

// ForgetArgs is the schema for the memory_forget tool.
type ForgetArgs struct {
	ID    *int   `json:"id,omitempty" jsonschema:"optional document id (the #N shown by memory_search, memory_recall and memory_list_recent). Deletes exactly that memory; takes precedence over query."`
	Query string `json:"query,omitempty" jsonschema:"natural-language description of the memory to forget. Must confidently identify a single memory — refine the query or pass id if results are ambiguous."`
}

func registerForget(server *mcp.Server, mem *Memory) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "memory_forget",
		Description: "Delete a memory from long-term storage, by document id or by natural-language query. " +
			"A query deletes the top match only when it is confident and clearly ahead of the runner-up; otherwise candidates are listed so you can retry with an id. " +
			"Use when the user explicitly asks you to forget something, or when a stored memory is outdated and needs removal.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args ForgetArgs) (*mcp.CallToolResult, any, error) {
		q := strings.TrimSpace(args.Query)
		if q == "" && args.ID == nil {
			return errorResult("query or id is required"), nil, nil
		}
		res, err := mem.Forget(ctx, q, args.ID)
		if err != nil {
			return errorResult(err.Error()), nil, nil
		}
		if res.Refused != "" {
			return textResult(formatForgetRefusal(q, res)), nil, nil
		}
		return textResult(fmt.Sprintf("Forgot memory %s (%d chunk(s)):\n  %s",
			hitLabel(res.Deleted), res.Chunks, truncate(stripTagPrefix(res.Deleted.Text), forgetSnippetLen))), nil, nil
	})
}

// ---- memory_list_recent -------------------------------------------------

// ListRecentArgs is the schema for the memory_list_recent tool.
type ListRecentArgs struct {
	K int `json:"k,omitempty" jsonschema:"maximum number of recent items to return (default 10, max 50)"`
}

func registerListRecent(server *mcp.Server, mem *Memory) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "memory_list_recent",
		Description: "List the most recently stored memories in this project, newest first. " +
			"Use when the user asks 'what did I just remember' or when you want to see fresh context without a specific query. " +
			"Returns at most k items, skipping any that have been forgotten.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args ListRecentArgs) (*mcp.CallToolResult, any, error) {
		items, total, err := mem.ListRecent(ctx, clampK(args.K, recentDefaultK, recentMaxK))
		if err != nil {
			return errorResult(err.Error()), nil, nil
		}
		if total == 0 {
			return textResult("No memories stored in this project yet."), nil, nil
		}
		if len(items) == 0 {
			return textResult("No live memories found in the most recent document range."), nil, nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "[Memory: %d recent item(s) out of %d total documents]\n", len(items), total)
		for i, it := range items {
			fmt.Fprintf(&b, "%d. (#%d) %s\n", i+1, it.DocID, truncate(it.Text, recallSnippetLen))
		}
		return textResult(b.String()), nil, nil
	})
}

// ---- helpers ------------------------------------------------------------

// clampK normalises a caller-supplied k to [1, max], falling back to def
// when the input is non-positive.
func clampK(k, def, max int) int {
	if k <= 0 {
		return def
	}
	return min(k, max)
}

// requireQuery trims the query and returns an error result when it is empty.
func requireQuery(raw string) (string, *mcp.CallToolResult) {
	q := strings.TrimSpace(raw)
	if q == "" {
		return "", errorResult("query is required and must be non-empty")
	}
	return q, nil
}

// docIDFromMetadata extracts the integer document_id from a search
// result's metadata. The SDK types Metadata as `any`; under JSON decoding
// the live server returns map[string]any with document_id as float64.
func docIDFromMetadata(md any) (int, bool) {
	m, ok := md.(map[string]any)
	if !ok {
		return 0, false
	}
	switch v := m["document_id"].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	}
	return 0, false
}

// hitLabel renders the stable handle of a memory: its document id, falling
// back to the chunk id when the server sent no document metadata.
func hitLabel(h Hit) string {
	if h.HasDoc {
		return fmt.Sprintf("#%d", h.DocID)
	}
	return h.ChunkID
}

// textResult wraps a plain-text response into an MCP CallToolResult.
func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: s}},
	}
}

// errorResult wraps a plain-text error into an MCP CallToolResult with
// IsError set so the client can distinguish tool-level failures from
// transport errors. Handlers do not return err for tool-level failures:
// that surfaces as a protocol error instead of a readable message.
func errorResult(msg string) *mcp.CallToolResult {
	log.Printf("tool error: %s", msg)
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}

func formatSearchResults(query string, hits []Hit) string {
	if len(hits) == 0 {
		return fmt.Sprintf("No memories found for %q.", query)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Found %d memory item(s) for %q:\n", len(hits), query)
	for i, h := range hits {
		fmt.Fprintf(&b, "\n%d. [score %.3f] %s\n   id: %s\n",
			i+1, h.Score, truncate(stripTagPrefix(h.Text), searchSnippetLen), hitLabel(h))
	}
	return b.String()
}

func formatRecall(query string, hits []Hit) string {
	if len(hits) == 0 {
		return fmt.Sprintf("[Memory] no relevant items for %q.", query)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[Memory: %d relevant item(s)]\n", len(hits))
	for i, h := range hits {
		fmt.Fprintf(&b, "%d. (%s) %s\n", i+1, hitLabel(h), truncate(stripTagPrefix(h.Text), recallSnippetLen))
	}
	return b.String()
}

func formatForgetRefusal(query string, res ForgetResult) string {
	var b strings.Builder
	if len(res.Candidates) == 0 {
		fmt.Fprintf(&b, "Nothing forgotten — %s for %q.", res.Refused, query)
		return b.String()
	}
	fmt.Fprintf(&b, "Refusing to forget — %s.\nCandidates (retry with id):\n", res.Refused)
	for _, h := range res.Candidates {
		fmt.Fprintf(&b, "  %s [score %.2f] %s\n", hitLabel(h), h.Score, truncate(stripTagPrefix(h.Text), forgetSnippetLen))
	}
	return b.String()
}
