package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	graphann "github.com/graphann/graphann-client-go"
)

// Live retrieval eval against a real GraphANN. Runs only with
// GRAPHANN_EVAL=1; uses a throwaway tenant and index.

type evalMemory struct {
	key  string
	args StoreArgs
}

var evalCorpus = []evalMemory{
	{"indent", StoreArgs{Text: "The user prefers tabs over spaces for indentation in Go code.", Kind: "preference"}},
	{"db", StoreArgs{Text: "We chose Postgres over MySQL for the billing service because of JSONB and row-level security.", Kind: "decision", Tags: []string{"db", "billing"}}},
	{"deploy", StoreArgs{Text: "Deployments go through ArgoCD on the home cluster; never kubectl apply by hand.", Kind: "fact", Source: "CLAUDE.md"}},
	{"ssh", StoreArgs{Text: "SSH and git signing only work while the 1Password app is unlocked.", Kind: "fact"}},
	{"proxmox", StoreArgs{Text: "Containers and VMs live on the Proxmox host at 10.0.0.100 and are reached through it.", Kind: "reference"}},
	{"commit", StoreArgs{Text: "Commit messages use conventional commits and must never carry attribution trailers.", Kind: "preference"}},
	{"push", StoreArgs{Text: "Do not push until the change is fully verified locally, because CI pipelines build one commit at a time and take about twenty minutes.", Kind: "decision"}},
	{"redis", StoreArgs{Text: "Shared Redis and Dragonfly instances run in the shared-resources namespace and must be reused instead of deploying new caches.", Kind: "fact"}},
	{"ingress", StoreArgs{Text: "Ingress on the home cluster is handled by Traefik in the traefik namespace.", Kind: "fact"}},
	{"port", StoreArgs{Text: "The GraphANN server listens on port 38888 at 10.0.1.21.", Kind: "reference", Source: "graphann-mcp"}},
	{"tz", StoreArgs{Text: "Standup is at 09:30 London time every weekday and the retro is on Fridays.", Kind: "event"}},
	{"semver", StoreArgs{Text: "Major releases are opt-in and require a Semver-Major trailer in the commit message.", Kind: "decision"}},
	{"lang", StoreArgs{Text: "The user writes British English in prose and documentation.", Kind: "preference"}},
	{"test", StoreArgs{Text: "Go tests must be table-driven, run with the race detector, and stay deterministic.", Kind: "preference"}},
	{"sandbox", StoreArgs{Text: "The macOS shell has no GNU timeout command, so use the Bash tool timeout parameter.", Kind: "fact"}},
	{"vue", StoreArgs{Text: "Frontends use Vue 3 with Vite, Tailwind, FontAwesome icons and shadcn-vue.", Kind: "decision", Tags: []string{"frontend"}}},
	{"secrets", StoreArgs{Text: "Secrets never go in git; use environment variables, vault or sealed secrets.", Kind: "preference"}},
	{"encore", StoreArgs{Text: "The notifications service is built on the Encore framework with Pub/Sub topics for fan-out.", Kind: "fact"}},
	{"backup", StoreArgs{Text: "Nightly database backups run at 02:00 and are kept for fourteen days.", Kind: "fact"}},
	{"oncall", StoreArgs{Text: "Dana owns the payments migration and is the first escalation contact for it.", Kind: "fact"}},
	{"unicode", StoreArgs{Text: "Zażółć gęślą jaźń: the Polish test string used to verify UTF-8 handling end to end.", Kind: "reference"}},
	{"long", StoreArgs{Text: strings.Repeat("The retry policy for service alpha uses exponential backoff with jitter and a cap of five seconds. ", 30) + "FINAL: the alpha circuit breaker opens after seven consecutive failures."}},
	{"opposite", StoreArgs{Text: "The user prefers spaces over tabs for indentation in Python code.", Kind: "preference"}},
	{"cache", StoreArgs{Text: "Cache invalidation on the catalogue API uses a five minute TTL plus explicit purge on write.", Kind: "decision"}},
}

type evalQuery struct {
	q    string
	want string // corpus key; empty means no memory should match
	also string // second key that must also surface (multi-answer query)
}

var evalQueries = []evalQuery{
	{"what indentation style does the user like in Go", "indent", ""},
	{"which database did we pick for invoicing and why", "db", ""},
	{"how do I roll out a change to the cluster", "deploy", ""},
	{"git push fails with agent refused operation", "ssh", ""},
	{"where do the virtual machines run", "proxmox", ""},
	{"rules for writing a commit message", "commit", ""},
	{"should I push right after fixing", "push", ""},
	{"is there an existing cache I can use", "redis", ""},
	{"what routes external traffic into kubernetes", "ingress", ""},
	{"what is the address of the vector search server", "port", ""},
	{"when is the daily team meeting", "tz", ""},
	{"how do I trigger a major version bump", "semver", ""},
	{"which spelling variant to use in docs", "lang", ""},
	{"testing conventions for Go packages", "test", ""},
	{"timeout command not found on mac", "sandbox", ""},
	{"what UI stack do we use", "vue", ""},
	{"where to keep api keys", "secrets", ""},
	{"which framework does the notifications service use", "encore", ""},
	{"how long are database backups retained", "backup", ""},
	{"who do I contact about the payments migration", "oncall", ""},
	{"polish diacritics test", "unicode", ""},
	{"when does the alpha circuit breaker open", "long", ""},
	{"what indentation style does the user like in Python", "opposite", ""},
	{"how does the catalogue cache expire", "cache", ""},
	{"retry backoff jitter and cache TTL purge", "long", "cache"},
	{"tabs versus spaces indentation preference", "indent", "opposite"},
	{"postgres billing decision and shared redis cache", "db", "redis"},
	{"retry policy exponential backoff jitter service alpha", "long", ""},
	{"favourite pizza topping", "", ""},
	{"weather forecast for tomorrow in Lisbon", "", ""},
	{"quantum chromodynamics lecture notes", "", ""},
	{"how to bake sourdough bread", "", ""},
}

type evalConfig struct {
	name string
	opts queryOpts
}

func TestEvalRetrieval(t *testing.T) {
	if os.Getenv("GRAPHANN_EVAL") == "" {
		t.Skip("set GRAPHANN_EVAL=1 to run the live retrieval eval")
	}
	ctx := context.Background()
	sdk, err := graphann.NewClient(graphann.WithBaseURL(envOr("GRAPHANN_URL", defaultGraphANNURL)), graphann.WithRetryPolicy(evalRetry()))
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UnixNano()
	m := NewMemory(sdk, fmt.Sprintf("eval-%d", stamp), "corpus")
	if err := m.Ensure(ctx); err != nil {
		t.Fatal(err)
	}

	docKey := map[int]string{}
	for _, e := range evalCorpus {
		res, err := m.Store(ctx, e.args)
		if err != nil || res.Duplicate != nil {
			t.Fatalf("store %s: dup=%v err=%v", e.key, res.Duplicate, err)
		}
	}
	// Map doc ids to keys by store order: ids are issued sequentially.
	for i, e := range evalCorpus {
		docKey[i] = e.key
	}

	configs := []evalConfig{
		{"DEFAULT (tools)", defaultQueryOpts},
		{"baseline raw dense", queryOpts{}},
		{"collapse", queryOpts{Collapse: true}},
		{"collapse+hybrid", queryOpts{Collapse: true, Hybrid: true}},
		{"hybrid", queryOpts{Hybrid: true}},
	}
	for _, floor := range []float64{0.25, 0.3, 0.35, 0.4, 0.45} {
		configs = append(configs, evalConfig{fmt.Sprintf("collapse floor=%.2f", floor), queryOpts{Collapse: true, MinScore: floor}})
	}
	for _, c := range configs {
		mrr, r1, junk, multi, distinct, missed := scoreConfig(t, ctx, m, docKey, c.opts)
		fmt.Printf("EVAL %-24s MRR@5=%.4f R@1=%.3f junk=%.2f multi@5=%.3f distinct=%.3f missed=%v\n", c.name, mrr, r1, junk, multi, distinct, missed)
	}
}

func scoreConfig(t *testing.T, ctx context.Context, m *Memory, docKey map[int]string, o queryOpts) (mrr, r1, junk, multi, distinct float64, missed []string) {
	t.Helper()
	pos, neg, junkQ, multiN := 0, 0, 0, 0
	for _, q := range evalQueries {
		hits, err := m.Query(ctx, q.q, 5, o)
		if err != nil {
			t.Fatalf("query %q: %v", q.q, err)
		}
		if q.also != "" {
			multiN++
			got := map[string]bool{}
			for _, h := range hits {
				got[docKey[h.DocID]] = true
			}
			if got[q.want] {
				multi += 0.5
			}
			if got[q.also] {
				multi += 0.5
			}
			continue
		}
		if q.want == "" {
			neg++
			if len(hits) > 0 {
				junkQ++
			}
			continue
		}
		pos++
		seen := map[int]bool{}
		for _, h := range hits {
			seen[h.DocID] = true
		}
		if len(hits) > 0 {
			distinct += float64(len(seen)) / float64(len(hits))
		}
		found := false
		for rank, h := range hits {
			if h.HasDoc && docKey[h.DocID] == q.want {
				mrr += 1 / float64(rank+1)
				if rank == 0 {
					r1++
				}
				found = true
				break
			}
		}
		if !found {
			missed = append(missed, q.want)
		}
	}
	return mrr / float64(pos), r1 / float64(pos), float64(junkQ) / float64(neg), multi / float64(multiN), distinct / float64(pos), missed
}

func evalRetry() graphann.RetryPolicy {
	p := graphann.DefaultRetryPolicy()
	p.MaxAttempts = 8
	p.MaxBackoff = 5 * time.Second
	return p
}
