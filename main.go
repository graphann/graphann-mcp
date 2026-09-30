// Command graphann-mcp is a Model Context Protocol stdio server that exposes
// a small set of long-term memory tools backed by a remote GraphANN instance.
//
// Intended shape:
//
//	Claude Code  <--stdio JSON-RPC-->  graphann-mcp  <--HTTP-->  GraphANN
//
// At launch the binary resolves a tenant (from $GRAPHANN_TENANT or
// $USER-claude) and a per-project index (from $GRAPHANN_INDEX, otherwise
// the basename of $CLAUDE_PROJECT_DIR or $PWD). Tenant and index are
// auto-created on first use if missing. No other external state is kept.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"strings"
	"time"

	graphann "github.com/graphann/graphann-client-go"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// defaultGraphANNURL is the fallback GraphANN base URL if $GRAPHANN_URL is
// not set. Matches the production instance on the internal network.
// version is the server build version, reported by -version and MCP initialize.
const serverVersion = "0.3.0"

const defaultGraphANNURL = "http://10.0.1.21:38888"

func main() {
	// Route all log output to stderr. stdio MCP uses stdout exclusively for
	// JSON-RPC framing — any stray stdout write corrupts the protocol.
	log.SetOutput(os.Stderr)
	log.SetPrefix("graphann-mcp ")
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)

	url := flag.String("url", envOr("GRAPHANN_URL", defaultGraphANNURL),
		"GraphANN base URL (or $GRAPHANN_URL)")
	tenant := flag.String("tenant", resolveTenant(),
		"tenant friendly name (or $GRAPHANN_TENANT; defaults to $USER-claude)")
	index := flag.String("index", resolveIndex(),
		"index friendly name (or $GRAPHANN_INDEX; defaults to basename of $CLAUDE_PROJECT_DIR / $PWD)")
	version := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *version {
		// stdout is safe here: we're exiting before the MCP loop starts.
		_, _ = os.Stdout.WriteString("graphann-mcp v" + serverVersion + "\n")
		return
	}

	log.Printf("start: url=%s tenant=%s index=%s", *url, *tenant, *index)

	// Extend the SDK's default retry budget so the post-create
	// "index_not_ready" window (live server typically settles within ~10s)
	// is absorbed transparently by Search/AddDocuments rather than surfaced
	// to the model. 8 attempts × exponential 100ms→5s ≈ ~16s headroom.
	retry := graphann.DefaultRetryPolicy()
	retry.MaxAttempts = 8
	retry.MaxBackoff = 5 * time.Second

	sdk, err := graphann.NewClient(
		graphann.WithBaseURL(*url),
		graphann.WithRetryPolicy(retry),
	)
	if err != nil {
		log.Fatalf("graphann client: %v", err)
	}
	defer func() { _ = sdk.Close() }()

	mem := NewMemory(sdk, *tenant, *index)

	// Best-effort eager bootstrap so we surface connectivity / auth issues at
	// startup rather than on the first tool call. Failures are logged but do
	// not abort — tool handlers will retry and return a readable error.
	if err := mem.Ensure(context.Background()); err != nil {
		log.Printf("warning: initial bootstrap failed (will retry on first tool call): %v", err)
	} else {
		log.Printf("bootstrap ok: tenant=%s index=%s", mem.TenantID, mem.IndexID)
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "graphann-memory",
		Version: serverVersion,
	}, nil)

	registerStore(server, mem)
	registerSearch(server, mem)
	registerRecall(server, mem)
	registerForget(server, mem)
	registerListRecent(server, mem)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("server exited: %v", err)
	}
}

// envOr returns the environment variable value or def if unset/empty after
// trimming whitespace.
func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
