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
