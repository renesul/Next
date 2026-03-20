# System Prompt

- Code, comments, and variable names in English
- UI text in Portuguese (user-facing)
- Layered architecture: `cmd/` (entry point), `app/` (business logic), `internal/` (infrastructure)
- No frameworks: standard library `net/http`, HTML templates with inline CSS/JS
- No npm, no build step, no bundler
- SQLite for all persistent state (config, messages, summaries, logs)
- `.env` has ONLY 2 vars: `PORT` and `DB_PATH` (bootstrap)
- All other config lives in SQLite, managed via web UI
- Extremely simple, easy for humans to understand
- Private messages + optional groups, text only (ignore audio, images)
- Debounce rapid messages before sending to AI
- Session isolation with auto-summary to prevent hallucination
- Token budget estimation to prevent overflow
- Debug mode toggle: logs to DB only when ON, JSON file always
- Auth always mandatory: default admin/admin123 seeded on first run
- Tests for all implementation
- Documentation: only update `.md` files in the project root (`CLAUDE.md`, `AGENTS.md`) — do not create other docs

# User Prompt

## Dependencies
- `go.mau.fi/whatsmeow` — WhatsApp (personal number, QR code)
- `github.com/sashabaranov/go-openai` — any OpenAI-compatible provider
- `github.com/mattn/go-sqlite3` — SQLite (CGO required)
- `github.com/joho/godotenv` — loads .env
- `github.com/mark3labs/mcp-go` — MCP client (SSE transport, tool discovery) + MCP server (exposes tools via SSE)
- `github.com/go-sql-driver/mysql` — MySQL driver (pure Go)
- `github.com/lib/pq` — PostgreSQL driver (pure Go)
- `golang.org/x/crypto` — bcrypt for password hashing

## Files
```
cmd/next/main.go                 — entry point, wiring, signal handling, DB migration

app/                             — BUSINESS LOGIC / DOMAIN
  types/types.go                 — shared types (Message, Agent, Contact, etc.)
  ai/ai.go                      — OpenAI-compatible client (reply, summarize, embed + cache)
  memory/memory.go               — sessions + history + summaries + token budget
  rag/rag.go                     — knowledge base with FTS5 search (CRUD, search, format)
  guardrails/guardrails.go       — pre/post-AI message filtering (whitelist, injection, PII)
  tools/tools.go                 — function calling registry + built-in/custom tools + schedules + ext DB + agents
  tools/mcp.go                   — MCP client (SSE connect, tool discovery) + MCP server (NextMCPServer)
  pipeline/pipeline.go           — message processing pipeline (guardrails → session → RAG → AI → tools → save)

internal/                        — INFRASTRUCTURE / ADAPTERS
  config/config.go               — Config struct + SQLite key-value persistence
  logger/logger.go               — JSON file (always) + SQLite (debug=on)
  auth/auth.go                   — multi-user auth (bcrypt, sessions, middleware, CRUD)
  whatsapp/whatsapp.go           — whatsmeow connection (QR, send, receive, filters, read receipts)
  debounce/debounce.go           — groups rapid messages per contact
  web/web.go                     — HTTP routes and API handlers

templates/                       — index.html, conversas.html, logs.html, conhecimento.html, relatorios.html, chat.html, login.html
```

## Data Directory
- Default: `~/.next/` (created automatically)
- Override with `DB_PATH` env var

## Auth
- Always mandatory — no open access mode
- Default admin (`admin/admin123`) created automatically on first run
- Roles: `admin` (full access) and `user` (view only, can change own password)
- Sessions persisted in SQLite `sessions` table + in-memory cache, httpOnly cookie, 24h TTL
- Sessions survive app restart (loaded from DB on startup)
- Middleware redirects unauthenticated requests to `/login`

## Agents
- Static agents with own personality/config (system_prompt, user_prompt, model, max_tokens)
- Optional per-agent base_url/api_key for custom AI providers
- Max 10 agents, managed via "Agentes" tab in web UI
- Agent routing: assign agent to contact via Conversas page select
- Agent chaining: `chain_to` (agent ID) + `chain_condition` (optional string match), max depth 3
- Tables: `agents` (CRUD) + `agent_routing` (phone → agent_id)
- Pipeline checks agent routing before using global config
- Default model: `gpt-4.1-mini`

## Tools
- 15 built-in tools + custom API tools + MCP tools
- Configurable timeout per tool (`tool_timeout_sec`, default 30s)
- Scheduled messages support recurrence: hourly, daily, weekly, monthly, cron (5-field)
- Tools: `list_scheduled` and `cancel_scheduled` (by ID or all)

## MCP Server
- Exposes all registered tools via MCP SSE protocol at `/mcp/sse` + `/mcp/message`
- Optional bearer token auth (`mcp_server_token` config)
- Enabled via `mcp_server_enabled` config (requires restart)
- Routes exempt from web UI auth middleware (have own token auth)

## WhatsApp
- Read receipts: messages are marked as read when processed (private + group)

## Databases (stored in ~/.next/)
- `next.db` — app data (config, messages, summaries, logs, knowledge, tasks, scheduled_messages, custom_tools, mcp_servers, external_databases, users, sessions, agents, agent_routing)
- `whatsapp.db` — whatsmeow internal session store

## Running
```
./build.sh && ./next
# or
./run.sh
```
Open http://localhost:8080, login with admin/admin123, configure, scan QR, done.

## Testing
```
./test.sh              # all tests
./test.sh Config       # filter by name
```
