# graphann-mcp

A Model Context Protocol (MCP) server that gives Claude Code long-term memory. It runs over stdio and stores memories in a [GraphANN](https://github.com/graphann/graphann) instance.

```
Claude Code  <--stdio JSON-RPC-->  graphann-mcp  <--HTTP-->  GraphANN
```

Each project gets its own index. Each user gets their own tenant. Both are created on first use, so you do not need to prepare the server.

## Contents

- [Requirements](#requirements)
- [Install](#install)
- [Register the server with Claude Code](#register-the-server-with-claude-code)
- [Configuration](#configuration)
- [How tenant and index are chosen](#how-tenant-and-index-are-chosen)
- [Tools](#tools)
- [How storage and retrieval behave](#how-storage-and-retrieval-behave)
- [Verify the setup](#verify-the-setup)
- [Develop and test](#develop-and-test)
- [Troubleshooting](#troubleshooting)

## Requirements

- A reachable GraphANN server. The default address is `http://10.0.1.21:38888`.
- Go 1.25 or later, to build from source.
- Claude Code, or any other MCP client that supports stdio servers.

The server does not send an API key. Use it with a GraphANN instance that accepts unauthenticated requests from your network.

## Install

Download a prebuilt binary from the [GitHub releases](https://github.com/graphann/graphann-mcp/releases) page. Each release has archives for Linux, macOS and Windows on amd64 and arm64, plus a checksum file.

Or clone the repository and build the binary:

```sh
git clone git@github.com:graphann/graphann-mcp.git
cd graphann-mcp
go build -o graphann-mcp .
./graphann-mcp -version
```

Put the binary in a stable location. The example below uses the Claude Code plugin directory:

```sh
mkdir -p ~/.claude/plugins/graphann-memory/bin
cp graphann-mcp ~/.claude/plugins/graphann-memory/bin/graphann-mcp
```

When you upgrade, keep a copy of the old binary first. Replace the file, then run `-version` to confirm the new build.

## Register the server with Claude Code

### With the CLI

```sh
claude mcp add graphann-memory \
  --scope user \
  -e GRAPHANN_URL=http://10.0.1.21:38888 \
  -- ~/.claude/plugins/graphann-memory/bin/graphann-mcp
```

Use an absolute path if your shell does not expand `~` inside the command.

### With the configuration file

Add this entry to the `mcpServers` object in `~/.claude.json` (user scope) or in `.mcp.json` at a project root (project scope):

```json
{
  "mcpServers": {
    "graphann-memory": {
      "type": "stdio",
      "command": "/Users/you/.claude/plugins/graphann-memory/bin/graphann-mcp",
      "env": {
        "GRAPHANN_URL": "http://10.0.1.21:38888"
      }
    }
  }
}
```

Restart Claude Code, or run `/mcp` and reconnect `graphann-memory`. The five `memory_*` tools then appear in the tool list.

### Give Claude standing instructions

Claude uses the tools when it decides they are useful. To make this reliable, add rules to your `CLAUDE.md`. For example:

```markdown
## Memory
- At the start of non-trivial work, call `memory_recall` with the task description.
- Call `memory_store` for decisions, preferences, and facts that a future session needs.
- Store the reason for a decision, not only the decision.
- When a stored memory is outdated, call `memory_forget`, then store the new one.
```

## Configuration

Every setting has a command-line flag and an environment variable. A flag overrides its environment variable.

| Flag | Environment variable | Default | Meaning |
|------|----------------------|---------|---------|
| `-url` | `GRAPHANN_URL` | `http://10.0.1.21:38888` | Base URL of the GraphANN server. |
| `-tenant` | `GRAPHANN_TENANT` | `<user>-claude` | Tenant name. See the next section. |
| `-index` | `GRAPHANN_INDEX` | Project directory name | Index name. See the next section. |
| `-version` | none | off | Print the version and exit. |

Log lines go to stderr. Stdout carries only the MCP protocol, so do not redirect stdout when you run the server by hand.

### Network behaviour

The client retries a failed request up to 8 times, with exponential backoff that grows from 100 ms to a maximum of 5 seconds. This absorbs the short `index_not_ready` window that follows the creation of a new index. The first `memory_store` in a brand-new project can take about 10 to 15 seconds for this reason. Later calls are fast.

## How tenant and index are chosen

Names are converted to a safe identifier ("slug"). The slug is lower case, uses only letters and digits, joins words with `-`, and has at most 64 characters. An empty name becomes `default`.

**Tenant** is the first of these that is set:

1. The `-tenant` flag or `GRAPHANN_TENANT`.
2. `$USER` followed by `-claude`, for example `nvm-claude`.
3. The current operating system user, followed by `-claude`.
4. `default-claude`.

**Index** is the first of these that is set:

1. The `-index` flag or `GRAPHANN_INDEX`.
2. The base name of `$CLAUDE_PROJECT_DIR`.
3. The base name of `$PWD`.
4. The base name of the working directory.
5. `default`.

On the server, the tenant ID is `t_<tenant>` and the index ID is `i_<index>`.

### Index ID conflicts

GraphANN index IDs are global, not per tenant. If another tenant already owns `i_<index>`, the server answers `409 conflict`. In that case this server uses `i_<tenant>-<index>` instead. Lookup order keeps existing data reachable:

1. Look for `i_<index>` in your tenant.
2. Look for `i_<tenant>-<index>` in your tenant.
3. Create `i_<index>`. If the server reports a conflict, create `i_<tenant>-<index>`.

The startup log line `bootstrap ok: tenant=... index=...` shows the IDs in use.

### Share one memory across projects

Set `GRAPHANN_INDEX` to the same value for each project:

```json
"env": { "GRAPHANN_INDEX": "shared-notes" }
```

### Separate memory per project

Do nothing. The default index follows the project directory name. Two directories with the same base name share one index, so set `GRAPHANN_INDEX` if that is not what you want.

## Tools

| Tool | Purpose |
|------|---------|
| `memory_store` | Save a memory. |
| `memory_recall` | Get the most relevant memories for the current task, as a compact block. |
| `memory_search` | Search with scores and ids, for inspection. |
| `memory_list_recent` | List the newest memories. |
| `memory_forget` | Delete a memory by id or by query. |

Each result shows a document id such as `#3`. Use this id with `memory_forget`.

### memory_store

| Argument | Type | Required | Meaning |
|----------|------|----------|---------|
| `text` | string | yes | The memory. Make it self-contained. |
| `kind` | string | no | A category, for example `fact`, `preference`, `decision`, `event`, `todo` or `reference`. |
| `source` | string | no | A file path, URL or short marker. |
| `tags` | string list | no | Short lowercase words. |

The server skips the write when an equivalent memory already exists, and it says which one. A changed or contradicting statement is stored as a new memory, so forget the outdated one.

`kind`, `tags` and `source` are stored as a prefix on the text, for example `[kind:decision tags:db source:chat] text`. GraphANN drops arbitrary metadata keys, so the prefix is the only way to keep them. Results strip the prefix before display.

### memory_recall

| Argument | Type | Required | Default | Limit |
|----------|------|----------|---------|-------|
| `query` | string | yes | none | none |
| `k` | integer | no | 5 | 20 |

Recall returns one result per memory, best match first. It hides weak matches (score below 0.35), so an unrelated query returns "no relevant items".

### memory_search

| Argument | Type | Required | Default | Limit |
|----------|------|----------|---------|-------|
| `query` | string | yes | none | none |
| `k` | integer | no | 10 | 100 |

Search returns one result per memory with its score and id. It has no score floor, so it also shows weak matches.

### memory_list_recent

| Argument | Type | Required | Default | Limit |
|----------|------|----------|---------|-------|
| `k` | integer | no | 10 | 50 |

GraphANN has no time-ordered listing. The server walks document ids from newest to oldest and skips deleted ones. It gives up after `max(3 × k, 20)` lookups on an index with many deletions.

### memory_forget

| Argument | Type | Required | Meaning |
|----------|------|----------|---------|
| `id` | integer | one of the two | The document id. Deletes exactly that memory. It takes precedence over `query`. |
| `query` | string | one of the two | A description of the memory. |

A query deletes the top match only when both conditions hold:

- The score is at least 0.85.
- The score is at least 0.05 higher than the second match.

Otherwise nothing is deleted. The tool lists the candidates with their ids, and you retry with `id`.

## How storage and retrieval behave

- **Duplicates.** A stored memory counts as a duplicate when its dense score is at least 0.75 and its words match in the same order (word-pair overlap of at least 0.8). Word order matters, so "prefers spaces over tabs" does not count as a duplicate of "prefers tabs over spaces".
- **Long memories.** GraphANN splits long text into chunks of roughly 500 characters. Search keeps the best chunk of each memory, so one long memory cannot fill the result list. Results show the matching chunk, not the whole memory.
- **Concurrent stores.** Two identical stores sent at the same moment can both succeed, because each checks before the other has written.
- **Scores.** Scores come from the GraphANN server and usually fall between 0 and 1. On the reference index, correct top hits scored 0.37 or higher and unrelated queries scored 0.32 or lower. The 0.35 floor sits between them. It comes from a small test set, so treat it as a starting point.

`experiments.md` records the measurements behind these thresholds.

## Verify the setup

Check the binary:

```sh
graphann-mcp -version
```

Check the connection and the bootstrap. Start the server by hand and read stderr:

```sh
GRAPHANN_TENANT=me-claude GRAPHANN_INDEX=scratch ./graphann-mcp < /dev/null
```

A healthy start logs `start: url=... tenant=... index=...` and then `bootstrap ok: ...`. A failed bootstrap logs a warning and retries on the first tool call.

Then, inside Claude Code, ask Claude to store a memory and recall it:

> Remember that we deploy through ArgoCD. Then recall how we deploy.

## Develop and test

```sh
go build ./...
go vet ./...
golangci-lint run
go test -race ./...
```

The unit tests use an in-memory fake of the GraphANN client, so they need no network.

The live retrieval evaluation runs against a real server. It creates a throwaway tenant and index, stores 24 memories, and runs 32 queries with several retrieval settings:

```sh
GRAPHANN_EVAL=1 go test -run TestEvalRetrieval -count=1 -v .
```

Set `GRAPHANN_URL` to test against a different server. The evaluation leaves its `eval-*` tenant on the server.

### Source layout

| File | Contents |
|------|----------|
| `main.go` | Flags, client setup, tool registration. |
| `bootstrap.go` | Tenant and index lookup and creation, retry after failure. |
| `ops.go` | Store, query, forget and list logic. Thresholds are constants here. |
| `tools.go` | MCP tool schemas and output formatting. |
| `slug.go` | Name resolution, slugs, and the tag prefix encoding. |
| `experiments.md` | Journal of tuning trials and their results. |

## Troubleshooting

| Symptom | Likely cause and fix |
|---------|----------------------|
| The tools do not appear in Claude Code. | Check the path in the configuration. Run the binary by hand. Run `/mcp` and reconnect. |
| `bootstrap: lookup tenant ...: network error` | The server is unreachable. Check `GRAPHANN_URL` and your network. The next tool call retries. |
| `create index ...: conflict` | Another tenant owns the plain ID, and the fallback ID is also taken. Set `GRAPHANN_INDEX` to a different name. |
| The first store is slow. | The new index is warming up. The client retries for up to about 16 seconds. This happens once per index. |
| `memory_recall` returns nothing for a memory you stored. | The match scored below 0.35. Use `memory_search` to see the score, and rephrase the query. |
| `memory_forget` refuses and lists candidates. | The query is ambiguous or weak. Retry with the `id` from the list. |
| `memory_store` reports "Skipped". | An equivalent memory exists. The message shows its id. |
| Memory is missing after you move a project. | The index follows the directory name. Set `GRAPHANN_INDEX` to the old name. |
