# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

Next is a self-hosted WhatsApp AI secretary powered by any OpenAI-compatible LLM. It connects to WhatsApp via whatsmeow, processes messages through a pipeline (guardrails → session → RAG → LLM → tools → response), and is managed via a web UI. Written in Go with SQLite (FTS5) and no frontend build step.

## Build & Development

Requires Go 1.25+, CGO enabled, and gcc/build-essential for SQLite.

```bash
make build      # CGO_ENABLED=1 go build -tags "fts5" -o next
make test       # go test -tags fts5 -v -count=1 -timeout 60s ./...
make lint       # golangci-lint run --build-tags fts5 ./...
make fmt        # gofmt -w .
make vet        # go vet -tags fts5 ./...
make check      # deps + fmt + vet + test (full CI)
make run        # build + run
make install    # build + copy to ~/.next/
```

The `-tags "fts5"` build tag is required everywhere (build, test, vet, lint).

Environment: `PORT` (default 8080), `DB_PATH` (default `~/.next/`).

## Architecture

Two-layer design: `app/` (business logic) and `internal/` (infrastructure).

### Message Pipeline (`app/pipeline/pipeline.go`)
The core orchestration — processes each WhatsApp message through:
1. Agent resolution (routing table → default agent)
2. Guardrail pre-filter (whitelist, injection, PII, length)
3. Session management (resume/create, auto-summarize on timeout)
4. RAG search (FTS5 + embedding hybrid, per-agent toggle)
5. LLM call with tool execution loop (max rounds configurable)
6. Guardrail post-filter (length, PII redaction)

### Key Packages

| Package | Purpose |
|---|---|
| `app/ai` | OpenAI-compatible client, embedding cache (5min TTL), Reply/Summarize/Embed |
| `app/memory` | Session lifecycle, message history, token budgeting, auto-summaries |
| `app/rag` | Knowledge base: FTS5 text search + vector similarity (hybrid) |
| `app/guardrails` | Pre/post message filtering (whitelist, PII, injection, length) |
| `app/tools` | Function calling registry: 15 built-in tools + custom REST tools + MCP |
| `app/tools/mcp.go` | MCP client (SSE) + MCP server (`/mcp/sse`) |
| `app/types` | Shared types: Message, Agent, Contact, Summary, KnowledgeEntry, etc. |
| `internal/config` | SQLite key-value config store, sync.RWMutex |
| `internal/web` | HTTP routes + API handlers (net/http, html/template) |
| `internal/whatsapp` | WhatsApp connection via whatsmeow, message filtering, QR code |
| `internal/auth` | Multi-user auth (bcrypt cost=12, session cookies, role-based) |
| `internal/debounce` | Groups rapid messages per chat (3s wait, 15s max) |
| `internal/logger` | JSON file + optional SQLite logging |

### Data Storage

All data in `~/.next/`: `next.db` (app data) and `whatsapp.db` (whatsmeow session). SQLite pragmas: WAL mode, busy_timeout=5000, synchronous=NORMAL.

## Conventions

- Comments in English, UI text in Portuguese (Brazilian)
- No web framework — standard library `net/http` and `html/template`
- Raw SQL with `?` placeholders (no ORM)
- Concurrency: `sync.Mutex`/`sync.RWMutex` for shared state (config, auth sessions, tool registry, embedding cache)
- Error handling: explicit `if err != nil`, no panics
- Logging: `logger.Log(event, chatID, map[string]any{...})`
- Templates in `templates/` with inline CSS/JS
- Agents are configurable via web UI with independent system prompts, models, RAG/tool/guardrail settings
