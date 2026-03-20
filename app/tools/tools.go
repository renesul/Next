package tools

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	openai "github.com/sashabaranov/go-openai"

	"next/app/ai"
	apprag "next/app/rag"
	"next/app/types"
	"next/internal/config"
	"next/internal/logger"
	"next/internal/whatsapp"
)

// ToolHandler defines a single callable tool.
type ToolHandler struct {
	Definition openai.Tool
	Execute    func(chatID, args string) (string, error)
}

// ExtDBConn holds an open connection to an external database.
type ExtDBConn struct {
	id     int64
	name   string
	driver string
	conn   *sql.DB
}

// ToolRegistry manages built-in, custom, and MCP tools.
type ToolRegistry struct {
	mu           sync.RWMutex
	tools        map[string]ToolHandler
	builtinNames map[string]bool
	customNames  map[string]bool
	mcpNames     map[string]bool
	db           *sql.DB
	readOnlyDB   *sql.DB // read-only connection for query_database
	rag          *apprag.RAG
	ai           *ai.AI
	cfg          *config.Config
	wa           *whatsapp.WhatsApp
	logger       *logger.Logger
	stopCh       chan struct{}
	mcpClients   []*MCPClient
	extDBs       map[int64]*ExtDBConn
	// Agents
	agentsMu     sync.RWMutex
	agents       map[int64]*types.Agent // id → agent
	agentRouting map[string]int64       // chatID → agent_id
}

// NewToolRegistry creates tables and registers built-in tools.
func NewToolRegistry(db *sql.DB, dbPath string, rag *apprag.RAG, a *ai.AI, cfg *config.Config, wa *whatsapp.WhatsApp, l *logger.Logger) (*ToolRegistry, error) {
	tables := []string{
		`CREATE TABLE IF NOT EXISTS tasks (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			chat_id     TEXT NOT NULL,
			description TEXT NOT NULL,
			done        INTEGER NOT NULL DEFAULT 0,
			created_at  INTEGER NOT NULL DEFAULT (unixepoch()),
			done_at     INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_chat ON tasks(chat_id, done, created_at)`,
		`CREATE TABLE IF NOT EXISTS notes (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			chat_id    TEXT NOT NULL,
			key        TEXT NOT NULL,
			value      TEXT NOT NULL,
			created_at INTEGER NOT NULL DEFAULT (unixepoch()),
			UNIQUE(chat_id, key)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_notes_chat ON notes(chat_id)`,
		`CREATE TABLE IF NOT EXISTS scheduled_messages (
			id      INTEGER PRIMARY KEY AUTOINCREMENT,
			chat_id TEXT NOT NULL,
			message TEXT NOT NULL,
			send_at INTEGER NOT NULL,
			sent    INTEGER NOT NULL DEFAULT 0,
			recurrence     TEXT NOT NULL DEFAULT '',
			recurrence_end INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL DEFAULT (unixepoch())
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sched_pending ON scheduled_messages(sent, send_at)`,
		`CREATE TABLE IF NOT EXISTS custom_tools (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			name          TEXT NOT NULL UNIQUE,
			description   TEXT NOT NULL,
			method        TEXT NOT NULL DEFAULT 'GET',
			url_template  TEXT NOT NULL,
			headers       TEXT NOT NULL DEFAULT '{}',
			body_template TEXT NOT NULL DEFAULT '',
			parameters    TEXT NOT NULL DEFAULT '[]',
			response_path TEXT NOT NULL DEFAULT '',
			max_bytes     INTEGER NOT NULL DEFAULT 10000,
			enabled       INTEGER NOT NULL DEFAULT 1,
			created_at    INTEGER NOT NULL DEFAULT (unixepoch())
		)`,
		`CREATE TABLE IF NOT EXISTS mcp_servers (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			name       TEXT NOT NULL UNIQUE,
			url        TEXT NOT NULL,
			api_key    TEXT NOT NULL DEFAULT '',
			enabled    INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL DEFAULT (unixepoch())
		)`,
		`CREATE TABLE IF NOT EXISTS external_databases (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			name       TEXT NOT NULL UNIQUE,
			driver     TEXT NOT NULL DEFAULT 'postgres',
			host       TEXT NOT NULL DEFAULT 'localhost',
			port       INTEGER NOT NULL DEFAULT 5432,
			username   TEXT NOT NULL DEFAULT '',
			password   TEXT NOT NULL DEFAULT '',
			dbname     TEXT NOT NULL DEFAULT '',
			ssl_mode   TEXT NOT NULL DEFAULT 'disable',
			max_rows   INTEGER NOT NULL DEFAULT 100,
			enabled    INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL DEFAULT (unixepoch())
		)`,
		`CREATE TABLE IF NOT EXISTS agents (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			name          TEXT NOT NULL UNIQUE,
			description   TEXT NOT NULL DEFAULT '',
			system_prompt TEXT NOT NULL,
			user_prompt   TEXT NOT NULL DEFAULT '',
			model         TEXT NOT NULL DEFAULT 'gpt-4.1-mini',
			max_tokens    INTEGER NOT NULL DEFAULT 1024,
			base_url      TEXT NOT NULL DEFAULT '',
			api_key       TEXT NOT NULL DEFAULT '',
			enabled       INTEGER NOT NULL DEFAULT 1,
			chain_to           INTEGER NOT NULL DEFAULT 0,
			chain_condition    TEXT NOT NULL DEFAULT '',
			rag_tags           TEXT NOT NULL DEFAULT '',
			is_default         INTEGER NOT NULL DEFAULT 0,
			rag_enabled        INTEGER NOT NULL DEFAULT 0,
			rag_max_results    INTEGER NOT NULL DEFAULT 3,
			rag_compressed     INTEGER NOT NULL DEFAULT 0,
			rag_max_tokens     INTEGER NOT NULL DEFAULT 500,
			rag_embeddings     INTEGER NOT NULL DEFAULT 0,
			rag_embedding_model TEXT NOT NULL DEFAULT '',
			tools_enabled      INTEGER NOT NULL DEFAULT 0,
			tools_max_rounds   INTEGER NOT NULL DEFAULT 3,
			tool_timeout_sec   INTEGER NOT NULL DEFAULT 30,
			guard_max_input    INTEGER NOT NULL DEFAULT 0,
			guard_max_output   INTEGER NOT NULL DEFAULT 0,
			guard_blocked_input  TEXT NOT NULL DEFAULT '',
			guard_blocked_output TEXT NOT NULL DEFAULT '',
			guard_phone_list     TEXT NOT NULL DEFAULT '',
			guard_phone_mode     TEXT NOT NULL DEFAULT 'off',
			guard_block_injection INTEGER NOT NULL DEFAULT 0,
			guard_block_pii       INTEGER NOT NULL DEFAULT 0,
			guard_block_pii_phone INTEGER NOT NULL DEFAULT 0,
			guard_block_pii_email INTEGER NOT NULL DEFAULT 0,
			guard_block_pii_cpf   INTEGER NOT NULL DEFAULT 0,
			knowledge_extract     INTEGER NOT NULL DEFAULT 0,
			created_at    INTEGER NOT NULL DEFAULT (unixepoch())
		)`,
		`CREATE TABLE IF NOT EXISTS agent_routing (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			chat_id    TEXT NOT NULL UNIQUE,
			agent_id   INTEGER NOT NULL,
			created_at INTEGER NOT NULL DEFAULT (unixepoch()),
			FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE
		)`,
	}
	for _, stmt := range tables {
		if _, err := db.Exec(stmt); err != nil {
			return nil, fmt.Errorf("create table: %w", err)
		}
	}

	// Open a read-only connection for query_database tool (defense in depth)
	sep := "?"
	if strings.Contains(dbPath, "?") {
		sep = "&"
	}
	roDB, err := sql.Open("sqlite3", dbPath+sep+"mode=ro&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open read-only db: %w", err)
	}
	roDB.SetMaxOpenConns(2)
	roDB.SetMaxIdleConns(1)

	tr := &ToolRegistry{
		tools:        make(map[string]ToolHandler),
		builtinNames: make(map[string]bool),
		customNames:  make(map[string]bool),
		mcpNames:     make(map[string]bool),
		db:           db,
		readOnlyDB:   roDB,
		rag:          rag,
		ai:           a,
		cfg:          cfg,
		wa:           wa,
		logger:       l,
		stopCh:       make(chan struct{}),
		extDBs:       make(map[int64]*ExtDBConn),
		agents:       make(map[int64]*types.Agent),
		agentRouting: make(map[string]int64),
	}
	tr.registerBuiltins()
	tr.loadCustomTools()
	tr.loadMCPServers()
	tr.loadExtDBs()
	tr.seedDefaultAgent()
	tr.loadAgents()

	return tr, nil
}

// Close releases resources held by the ToolRegistry.
func (tr *ToolRegistry) Close() {
	tr.StopExtDBs()
	if tr.readOnlyDB != nil {
		tr.readOnlyDB.Close()
	}
}

func (tr *ToolRegistry) registerBuiltin(name string, h ToolHandler) {
	tr.tools[name] = h
	tr.builtinNames[name] = true
}

func (tr *ToolRegistry) registerBuiltins() {
	tr.registerBuiltin("get_datetime", ToolHandler{
		Definition: openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "get_datetime",
				Description: "Returns the current date, time, and day of week in Brazilian Portuguese",
				Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			},
		},
		Execute: tr.execGetDatetime,
	})

	tr.registerBuiltin("create_task", ToolHandler{
		Definition: openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "create_task",
				Description: "Creates a new task/reminder for the contact",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"description":{"type":"string","description":"Task description"}},"required":["description"]}`),
			},
		},
		Execute: tr.execCreateTask,
	})

	tr.registerBuiltin("list_tasks", ToolHandler{
		Definition: openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "list_tasks",
				Description: "Lists the contact's tasks, optionally filtered by status",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"status":{"type":"string","enum":["pending","done","all"],"description":"Filter by status (default: pending)"}}}`),
			},
		},
		Execute: tr.execListTasks,
	})

	tr.registerBuiltin("complete_task", ToolHandler{
		Definition: openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "complete_task",
				Description: "Marks a task as completed",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"task_id":{"type":"integer","description":"Task ID to mark as done"}},"required":["task_id"]}`),
			},
		},
		Execute: tr.execCompleteTask,
	})

	tr.registerBuiltin("search_knowledge", ToolHandler{
		Definition: openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "search_knowledge",
				Description: "Searches the knowledge base for relevant information",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Search query"}},"required":["query"]}`),
			},
		},
		Execute: tr.execSearchKnowledge,
	})

	tr.registerBuiltin("calculate", ToolHandler{
		Definition: openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "calculate",
				Description: "Evaluates a math expression. Supports +, -, *, /, ^, %, parentheses, and functions: sqrt, abs, round, floor, ceil",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"expression":{"type":"string","description":"Math expression, e.g. (10 + 5) * 2 or sqrt(144)"}},"required":["expression"]}`),
			},
		},
		Execute: tr.execCalculate,
	})

	tr.registerBuiltin("search_web", ToolHandler{
		Definition: openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "search_web",
				Description: "Searches the web using DuckDuckGo and returns top results with titles, links, and snippets",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Search query"}},"required":["query"]}`),
			},
		},
		Execute: tr.execSearchWeb,
	})

	tr.registerBuiltin("fetch_url", ToolHandler{
		Definition: openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "fetch_url",
				Description: "Fetches a URL and returns its text content (HTML tags stripped). Max 4000 chars.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","description":"Full URL (https://...)"}},"required":["url"]}`),
			},
		},
		Execute: tr.execFetchURL,
	})

	tr.registerBuiltin("weather", ToolHandler{
		Definition: openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "weather",
				Description: "Returns current weather for a location (temperature, humidity, wind, condition)",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"location":{"type":"string","description":"City name, e.g. Sao Paulo, London, Tokyo"}},"required":["location"]}`),
			},
		},
		Execute: tr.execWeather,
	})

	tr.registerBuiltin("save_note", ToolHandler{
		Definition: openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "save_note",
				Description: "Saves a persistent note about the contact (e.g. birthday, preferences). Overwrites if key already exists.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"key":{"type":"string","description":"Short label, e.g. aniversario, nome_completo, preferencia"},"value":{"type":"string","description":"The information to save"}},"required":["key","value"]}`),
			},
		},
		Execute: tr.execSaveNote,
	})

	tr.registerBuiltin("get_notes", ToolHandler{
		Definition: openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "get_notes",
				Description: "Retrieves saved notes about the contact. Returns all notes if no key specified.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"key":{"type":"string","description":"Optional: specific note key to retrieve"}}}`),
			},
		},
		Execute: tr.execGetNotes,
	})

	tr.registerBuiltin("currency", ToolHandler{
		Definition: openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "currency",
				Description: "Converts an amount between currencies using live exchange rates. Common codes: BRL, USD, EUR, GBP, JPY.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"amount":{"type":"number","description":"Amount to convert"},"from":{"type":"string","description":"Source currency code, e.g. USD"},"to":{"type":"string","description":"Target currency code, e.g. BRL"}},"required":["amount","from","to"]}`),
			},
		},
		Execute: tr.execCurrency,
	})

	tr.registerBuiltin("schedule_message", ToolHandler{
		Definition: openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "schedule_message",
				Description: "Schedules a message to be sent to the contact at a future date/time. Use the configured timezone. Supports optional recurrence.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"datetime":{"type":"string","description":"When to send, e.g. 2025-03-15 14:30 or 15/03/2025 14:30"},"message":{"type":"string","description":"Message text to send"},"recurrence":{"type":"string","description":"Optional: hourly, daily, weekly:1 (1=Mon..7=Sun), monthly:15, cron:0 9 * * 1-5"},"recurrence_end":{"type":"string","description":"Optional: end date for recurrence, e.g. 2025-12-31"}},"required":["datetime","message"]}`),
			},
		},
		Execute: tr.execScheduleMessage,
	})

	tr.registerBuiltin("list_scheduled", ToolHandler{
		Definition: openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "list_scheduled",
				Description: "Lists all pending scheduled messages for the contact",
				Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			},
		},
		Execute: tr.execListScheduled,
	})

	tr.registerBuiltin("cancel_scheduled", ToolHandler{
		Definition: openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "cancel_scheduled",
				Description: "Cancels pending scheduled messages. Use id for a specific one or all:true for all.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"id":{"type":"integer","description":"Specific scheduled message ID to cancel"},"all":{"type":"boolean","description":"Set true to cancel all pending scheduled messages"}}}`),
			},
		},
		Execute: tr.execCancelScheduled,
	})

	tr.registerBuiltin("query_database", ToolHandler{
		Definition: openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "query_database",
				Description: "Runs a read-only SQL query against a database. For local SQLite: tables messages, summaries, knowledge, tasks, notes, scheduled_messages, logs (blocked: config, custom_tools, mcp_servers, external_databases). For external databases: use the database parameter with the database name.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"SELECT query to execute"},"limit":{"type":"integer","description":"Max rows to return (default 50, max 100)"},"database":{"type":"string","description":"Database name. Empty or 'local' for local SQLite, or name of an external database."}},"required":["query"]}`),
			},
		},
		Execute: tr.execQueryDatabase,
	})
}

// ListToolInfo returns name and description of all registered tools.
func (tr *ToolRegistry) ListToolInfo() []types.ToolInfo {
	tr.mu.RLock()
	defer tr.mu.RUnlock()
	info := make([]types.ToolInfo, 0, len(tr.tools))
	for _, h := range tr.tools {
		if h.Definition.Function != nil {
			info = append(info, types.ToolInfo{
				Name:        h.Definition.Function.Name,
				Description: h.Definition.Function.Description,
			})
		}
	}
	return info
}

// GetTools returns all tool definitions for the OpenAI API.
func (tr *ToolRegistry) GetTools() []openai.Tool {
	tr.mu.RLock()
	defer tr.mu.RUnlock()
	tools := make([]openai.Tool, 0, len(tr.tools))
	for _, h := range tr.tools {
		tools = append(tools, h.Definition)
	}
	return tools
}

// GetToolsMap returns the tools map (locked) for MCP server registration.
func (tr *ToolRegistry) GetToolsMap() map[string]ToolHandler {
	tr.mu.RLock()
	defer tr.mu.RUnlock()
	result := make(map[string]ToolHandler, len(tr.tools))
	for k, v := range tr.tools {
		result[k] = v
	}
	return result
}

// Execute runs a tool by name with the default timeout (30s).
func (tr *ToolRegistry) Execute(name, chatID, args string) (string, error) {
	return tr.ExecuteWithTimeout(name, chatID, args, config.DefaultToolTimeoutSec)
}

// ExecuteWithTimeout runs a tool by name with a specified timeout.
func (tr *ToolRegistry) ExecuteWithTimeout(name, chatID, args string, timeoutSec int) (string, error) {
	tr.mu.RLock()
	handler, ok := tr.tools[name]
	tr.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("unknown tool: %s", name)
	}

	if timeoutSec <= 0 {
		timeoutSec = config.DefaultToolTimeoutSec
	}

	tr.logger.Log("tools_tool_execute", chatID, map[string]any{
		"tool": name, "arguments": args, "timeout_sec": timeoutSec,
	})

	start := time.Now()
	type result struct {
		s string
		e error
	}
	ch := make(chan result, 1)
	go func() {
		s, e := handler.Execute(chatID, args)
		ch <- result{s, e}
	}()

	select {
	case res := <-ch:
		latency := time.Since(start).Milliseconds()
		tr.logger.Log("tools_tool_execute_result", chatID, map[string]any{
			"tool": name, "result": res.s, "error": res.e != nil,
			"latency_ms": latency,
		})
		return res.s, res.e
	case <-time.After(time.Duration(timeoutSec) * time.Second):
		latency := time.Since(start).Milliseconds()
		tr.logger.Log("tools_tool_execute_result", chatID, map[string]any{
			"tool": name, "result": "timeout", "error": true,
			"latency_ms": latency,
		})
		return fmt.Sprintf("Erro: timeout da ferramenta após %ds.", timeoutSec), nil
	}
}

// DeleteTasks removes all tasks for a chat.
func (tr *ToolRegistry) DeleteTasks(chatID string) {
	tr.db.Exec("DELETE FROM tasks WHERE chat_id = ?", chatID)
}

// DeleteNotes removes all notes for a chat.
func (tr *ToolRegistry) DeleteNotes(chatID string) {
	tr.db.Exec("DELETE FROM notes WHERE chat_id = ?", chatID)
}

// DeleteScheduledMessages removes all scheduled messages for a chat.
func (tr *ToolRegistry) DeleteScheduledMessages(chatID string) {
	tr.db.Exec("DELETE FROM scheduled_messages WHERE chat_id = ?", chatID)
}

// validateURLSafety resolves a URL's hostname and rejects private/loopback IPs (SSRF protection).
func validateURLSafety(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("URL invalida: %w", err)
	}
	host := parsed.Hostname()
	if host == "" {
		return fmt.Errorf("URL sem hostname")
	}
	if strings.EqualFold(host, "localhost") {
		return fmt.Errorf("acesso a localhost bloqueado")
	}

	ips, err := net.LookupHost(host)
	if err != nil {
		return fmt.Errorf("DNS lookup falhou: %w", err)
	}

	for _, ipStr := range ips {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			continue
		}
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return fmt.Errorf("IP privado/loopback bloqueado: %s", ipStr)
		}
	}
	return nil
}

// BlockedTables lists tables that contain sensitive data (API keys, etc).
var BlockedTables = []string{"config", "custom_tools", "mcp_servers", "external_databases", "sqlite_master", "sqlite_schema"}

// writeKeywords lists SQL keywords that modify data.
var writeKeywords = []string{
	"INSERT", "UPDATE", "DELETE", "DROP", "ALTER", "CREATE", "REPLACE",
	"ATTACH", "DETACH", "PRAGMA", "VACUUM", "BEGIN", "COMMIT", "ROLLBACK",
}

// stripSQLComments removes SQL line comments (--) and block comments (/* */) before validation.
func stripSQLComments(q string) string {
	// Remove block comments
	for {
		start := strings.Index(q, "/*")
		if start == -1 {
			break
		}
		end := strings.Index(q[start+2:], "*/")
		if end == -1 {
			q = q[:start]
			break
		}
		q = q[:start] + " " + q[start+2+end+2:]
	}
	// Remove line comments
	lines := strings.Split(q, "\n")
	for i, line := range lines {
		if idx := strings.Index(line, "--"); idx >= 0 {
			lines[i] = line[:idx]
		}
	}
	return strings.Join(lines, "\n")
}

// ValidateReadOnlySQL checks that a query is a safe read-only SELECT.
func ValidateReadOnlySQL(query string) error {
	q := strings.TrimSpace(stripSQLComments(query))
	if q == "" {
		return fmt.Errorf("query vazia")
	}

	inner := strings.TrimSuffix(strings.TrimSpace(q), ";")
	if strings.Contains(inner, ";") {
		return fmt.Errorf("multiplos comandos nao permitidos")
	}

	upper := strings.ToUpper(q)
	if !strings.HasPrefix(upper, "SELECT") && !strings.HasPrefix(upper, "WITH") {
		return fmt.Errorf("apenas SELECT permitido")
	}

	for _, kw := range writeKeywords {
		pat := `(?i)\b` + kw + `\b`
		if matched, _ := regexp.MatchString(pat, q); matched {
			return fmt.Errorf("palavra-chave bloqueada: %s", kw)
		}
	}

	lowerQ := strings.ToLower(q)
	for _, tbl := range BlockedTables {
		pat := `(?i)\b` + tbl + `\b`
		if matched, _ := regexp.MatchString(pat, lowerQ); matched {
			return fmt.Errorf("tabela bloqueada: %s", tbl)
		}
	}

	return nil
}

// validateWithExplain uses EXPLAIN to detect tables accessed by a query and blocks sensitive ones.
func (tr *ToolRegistry) validateWithExplain(query string) error {
	rows, err := tr.readOnlyDB.Query("EXPLAIN " + query)
	if err != nil {
		return fmt.Errorf("EXPLAIN falhou: %s", err.Error())
	}
	defer rows.Close()

	blockedSet := make(map[string]bool, len(BlockedTables))
	for _, t := range BlockedTables {
		blockedSet[strings.ToLower(t)] = true
	}

	for rows.Next() {
		cols, _ := rows.Columns()
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			continue
		}
		// EXPLAIN output: col[1] is opcode, col[5] is p5/comment which may contain table names
		// Check all string columns for blocked table references
		for _, v := range vals {
			s := fmt.Sprintf("%v", v)
			if blockedSet[strings.ToLower(s)] {
				return fmt.Errorf("tabela bloqueada detectada via EXPLAIN: %s", s)
			}
		}
	}
	return nil
}

func (tr *ToolRegistry) queryReadOnly(query string, maxRows int) ([]string, [][]string, error) {
	if err := ValidateReadOnlySQL(query); err != nil {
		return nil, nil, err
	}

	if maxRows <= 0 {
		maxRows = 50
	}

	upperQ := strings.ToUpper(strings.TrimSpace(query))
	if !strings.Contains(upperQ, "LIMIT") {
		q := strings.TrimSuffix(strings.TrimSpace(query), ";")
		query = fmt.Sprintf("%s LIMIT %d", q, maxRows)
	}

	// Second layer: EXPLAIN-based table detection
	if err := tr.validateWithExplain(query); err != nil {
		return nil, nil, err
	}

	rows, err := tr.readOnlyDB.Query(query)
	if err != nil {
		return nil, nil, fmt.Errorf("SQL: %s", err.Error())
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}

	var data [][]string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		row := make([]string, len(cols))
		for i, v := range vals {
			row[i] = formatSQLValue(v)
		}
		data = append(data, row)
	}
	return cols, data, nil
}

// QueryReadOnly is the public wrapper for web handlers.
func (tr *ToolRegistry) QueryReadOnly(query string, maxRows int) ([]string, [][]string, error) {
	return tr.queryReadOnly(query, maxRows)
}

func (tr *ToolRegistry) execQueryDatabase(chatID, args string) (string, error) {
	var p struct {
		Query    string `json:"query"`
		Limit    int    `json:"limit"`
		Database string `json:"database"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}

	limit := p.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}

	var cols []string
	var rows [][]string
	var err error

	if p.Database != "" && p.Database != "local" {
		cols, rows, err = tr.queryExtDB(p.Database, p.Query, limit)
	} else {
		cols, rows, err = tr.queryReadOnly(p.Query, limit)
	}

	if err != nil {
		return fmt.Sprintf("Erro: %s", err.Error()), nil
	}
	if len(rows) == 0 {
		return "Nenhum resultado.", nil
	}

	return formatQueryResult(cols, rows), nil
}

func formatQueryResult(cols []string, rows [][]string) string {
	var sb strings.Builder
	sb.WriteString(strings.Join(cols, " | "))
	sb.WriteString("\n")
	sb.WriteString(strings.Repeat("-", len(sb.String())-1))
	sb.WriteString("\n")

	for _, row := range rows {
		sb.WriteString(strings.Join(row, " | "))
		sb.WriteString("\n")
		if sb.Len() > 4000 {
			sb.WriteString("... (resultado truncado)")
			break
		}
	}

	return strings.TrimSpace(sb.String())
}

// GetSchemaDescription returns a description of all queryable tables for AI context.
func (tr *ToolRegistry) GetSchemaDescription() string {
	desc := `Tabelas disponíveis no banco de dados local (SQLite):

messages (id INTEGER, chat_id TEXT, role TEXT, content TEXT, session_id INTEGER, created_at INTEGER[unix])
summaries (id INTEGER, chat_id TEXT, session_id INTEGER, content TEXT, created_at INTEGER[unix])
knowledge (id INTEGER, title TEXT, content TEXT, tags TEXT, enabled INTEGER, created_at INTEGER[unix], updated_at INTEGER[unix])
tasks (id INTEGER, chat_id TEXT, description TEXT, done INTEGER[0=pendente,1=feita], created_at INTEGER[unix], done_at INTEGER[unix])
notes (id INTEGER, chat_id TEXT, key TEXT, value TEXT, created_at INTEGER[unix])
scheduled_messages (id INTEGER, chat_id TEXT, message TEXT, send_at INTEGER[unix], sent INTEGER[0=pendente,1=enviada,2=falhou], created_at INTEGER[unix])
logs (id INTEGER, event TEXT, chat_id TEXT, data TEXT[json], created_at INTEGER[unix])

Tabelas bloqueadas (não acessíveis): config, custom_tools, mcp_servers, external_databases.
Timestamps são Unix epoch (segundos). Use datetime(created_at, 'unixepoch') para converter.`

	tr.mu.RLock()
	extDBs := make([]*ExtDBConn, 0, len(tr.extDBs))
	for _, edb := range tr.extDBs {
		extDBs = append(extDBs, edb)
	}
	tr.mu.RUnlock()

	for _, edb := range extDBs {
		schema, err := tr.getExtDBSchema(edb.name)
		if err == nil && schema != "" {
			desc += fmt.Sprintf("\n\n--- Banco externo: %s (%s) ---\nUse database=\"%s\" no query_database.\n%s", edb.name, edb.driver, edb.name, schema)
		}
	}

	return desc
}

func formatSQLValue(v any) string {
	if v == nil {
		return "NULL"
	}
	var s string
	switch val := v.(type) {
	case []byte:
		s = string(val)
	default:
		s = fmt.Sprintf("%v", val)
	}
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

func (tr *ToolRegistry) execGetDatetime(chatID, args string) (string, error) {
	tr.cfg.Mu.RLock()
	tz := tr.cfg.Timezone
	tr.cfg.Mu.RUnlock()

	now := time.Now()
	if loc, err := time.LoadLocation(tz); err == nil {
		now = now.In(loc)
	}

	days := [...]string{"domingo", "segunda-feira", "terca-feira", "quarta-feira", "quinta-feira", "sexta-feira", "sabado"}
	months := [...]string{"", "janeiro", "fevereiro", "marco", "abril", "maio", "junho", "julho", "agosto", "setembro", "outubro", "novembro", "dezembro"}

	return fmt.Sprintf("Data: %02d de %s de %d (%s)\nHora: %02d:%02d",
		now.Day(), months[now.Month()], now.Year(),
		days[now.Weekday()],
		now.Hour(), now.Minute(),
	), nil
}

func (tr *ToolRegistry) execCreateTask(chatID, args string) (string, error) {
	var p struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if p.Description == "" {
		return "Erro: descricao da tarefa nao pode ser vazia.", nil
	}

	res, err := tr.db.Exec("INSERT INTO tasks (chat_id, description) VALUES (?, ?)", chatID, p.Description)
	if err != nil {
		return "", fmt.Errorf("create task: %w", err)
	}
	id, _ := res.LastInsertId()
	return fmt.Sprintf("Tarefa #%d criada: %s", id, p.Description), nil
}

func (tr *ToolRegistry) execListTasks(chatID, args string) (string, error) {
	var p struct {
		Status string `json:"status"`
	}
	json.Unmarshal([]byte(args), &p)
	if p.Status == "" {
		p.Status = "pending"
	}

	var query string
	switch p.Status {
	case "done":
		query = "SELECT id, description, done, created_at, done_at FROM tasks WHERE chat_id=? AND done=1 ORDER BY done_at DESC LIMIT 20"
	case "all":
		query = "SELECT id, description, done, created_at, done_at FROM tasks WHERE chat_id=? ORDER BY created_at DESC LIMIT 20"
	default:
		query = "SELECT id, description, done, created_at, done_at FROM tasks WHERE chat_id=? AND done=0 ORDER BY created_at ASC LIMIT 20"
	}

	rows, err := tr.db.Query(query, chatID)
	if err != nil {
		return "", fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()

	var sb strings.Builder
	count := 0
	for rows.Next() {
		var id int64
		var desc string
		var done int
		var createdAt int64
		var doneAt sql.NullInt64
		if err := rows.Scan(&id, &desc, &done, &createdAt, &doneAt); err != nil {
			return "", err
		}
		count++
		status := "pendente"
		if done == 1 {
			status = "feita"
		}
		created := time.Unix(createdAt, 0).Format("02/01/2006")
		sb.WriteString(fmt.Sprintf("#%d [%s] %s (criada %s)\n", id, status, desc, created))
	}

	if count == 0 {
		return "Nenhuma tarefa encontrada.", nil
	}
	return strings.TrimSpace(sb.String()), nil
}

func (tr *ToolRegistry) execCompleteTask(chatID, args string) (string, error) {
	var p struct {
		TaskID int64 `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}

	var owner string
	err := tr.db.QueryRow("SELECT chat_id FROM tasks WHERE id=?", p.TaskID).Scan(&owner)
	if err != nil {
		return "Tarefa nao encontrada.", nil
	}
	if owner != chatID {
		return "Tarefa nao encontrada.", nil
	}

	_, err = tr.db.Exec("UPDATE tasks SET done=1, done_at=unixepoch() WHERE id=? AND chat_id=?", p.TaskID, chatID)
	if err != nil {
		return "", fmt.Errorf("complete task: %w", err)
	}
	return fmt.Sprintf("Tarefa #%d marcada como feita.", p.TaskID), nil
}

func (tr *ToolRegistry) execSearchKnowledge(chatID, args string) (string, error) {
	var p struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if p.Query == "" {
		return "Erro: query nao pode ser vazia.", nil
	}

	// Read RAG settings from the agent for this chat
	maxResults := config.DefaultRAGMaxResults
	compressed := false
	maxTokens := config.DefaultRAGMaxTokens
	useEmb := false
	embModel := ""
	var ragTags []string
	agent := tr.GetAgentForChatID(chatID)
	if agent == nil {
		agent = tr.GetDefaultAgent()
	}
	if agent != nil {
		maxResults = agent.RAGMaxResults
		compressed = agent.RAGCompressed
		maxTokens = agent.RAGMaxTokens
		useEmb = agent.RAGEmbeddings
		embModel = agent.RAGEmbeddingModel
		if agent.RAGTags != "" {
			for _, t := range strings.Split(agent.RAGTags, ",") {
				t = strings.TrimSpace(strings.ToLower(t))
				if t != "" {
					ragTags = append(ragTags, t)
				}
			}
		}
	}

	entries, err := tr.rag.HybridSearch(chatID, tr.ai, embModel, p.Query, maxResults, useEmb, ragTags...)
	if err != nil {
		return "", fmt.Errorf("search: %w", err)
	}
	if len(entries) == 0 {
		return "Nenhum resultado encontrado na base de conhecimento.", nil
	}

	ctx := apprag.FormatContextFromEntries(entries, compressed)
	ctx = apprag.TruncateContext(ctx, maxTokens)
	return ctx, nil
}

func (tr *ToolRegistry) execCalculate(chatID, args string) (string, error) {
	var p struct {
		Expression string `json:"expression"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if p.Expression == "" {
		return "Erro: expressao nao pode ser vazia.", nil
	}

	result, err := calcEval(p.Expression)
	if err != nil {
		return fmt.Sprintf("Erro no calculo: %s", err.Error()), nil
	}

	text := strconv.FormatFloat(result, 'f', -1, 64)
	return fmt.Sprintf("%s = %s", p.Expression, text), nil
}

// --- Simple recursive-descent math parser ---

type calcParser struct {
	input string
	pos   int
}

func calcEval(expr string) (float64, error) {
	p := &calcParser{input: expr}
	result := p.parseExpr()
	p.skipSpaces()
	if p.pos < len(p.input) {
		return 0, fmt.Errorf("caractere inesperado: '%c'", p.input[p.pos])
	}
	if math.IsInf(result, 0) || math.IsNaN(result) {
		return 0, fmt.Errorf("resultado invalido (divisao por zero?)")
	}
	return result, nil
}

func (p *calcParser) skipSpaces() {
	for p.pos < len(p.input) && p.input[p.pos] == ' ' {
		p.pos++
	}
}

func (p *calcParser) parseExpr() float64 {
	result := p.parseTerm()
	for {
		p.skipSpaces()
		if p.pos >= len(p.input) {
			return result
		}
		op := p.input[p.pos]
		if op != '+' && op != '-' {
			return result
		}
		p.pos++
		right := p.parseTerm()
		if op == '+' {
			result += right
		} else {
			result -= right
		}
	}
}

func (p *calcParser) parseTerm() float64 {
	result := p.parsePower()
	for {
		p.skipSpaces()
		if p.pos >= len(p.input) {
			return result
		}
		op := p.input[p.pos]
		if op != '*' && op != '/' && op != '%' {
			return result
		}
		p.pos++
		right := p.parsePower()
		switch op {
		case '*':
			result *= right
		case '/':
			result /= right
		case '%':
			result = math.Mod(result, right)
		}
	}
}

func (p *calcParser) parsePower() float64 {
	base := p.parseUnary()
	p.skipSpaces()
	if p.pos < len(p.input) && p.input[p.pos] == '^' {
		p.pos++
		exp := p.parsePower()
		return math.Pow(base, exp)
	}
	return base
}

func (p *calcParser) parseUnary() float64 {
	p.skipSpaces()
	if p.pos < len(p.input) {
		if p.input[p.pos] == '-' {
			p.pos++
			return -p.parseUnary()
		}
		if p.input[p.pos] == '+' {
			p.pos++
			return p.parseUnary()
		}
	}
	return p.parseAtom()
}

func (p *calcParser) parseAtom() float64 {
	p.skipSpaces()

	if p.pos < len(p.input) && p.input[p.pos] == '(' {
		p.pos++
		result := p.parseExpr()
		p.skipSpaces()
		if p.pos < len(p.input) && p.input[p.pos] == ')' {
			p.pos++
		}
		return result
	}

	if p.pos < len(p.input) && unicode.IsLetter(rune(p.input[p.pos])) {
		start := p.pos
		for p.pos < len(p.input) && unicode.IsLetter(rune(p.input[p.pos])) {
			p.pos++
		}
		name := strings.ToLower(p.input[start:p.pos])
		p.skipSpaces()

		switch name {
		case "pi":
			return math.Pi
		case "e":
			return math.E
		}

		if p.pos < len(p.input) && p.input[p.pos] == '(' {
			p.pos++
			arg := p.parseExpr()
			p.skipSpaces()
			if p.pos < len(p.input) && p.input[p.pos] == ')' {
				p.pos++
			}
			switch name {
			case "sqrt":
				return math.Sqrt(arg)
			case "abs":
				return math.Abs(arg)
			case "round":
				return math.Round(arg)
			case "floor":
				return math.Floor(arg)
			case "ceil":
				return math.Ceil(arg)
			}
		}
		return 0
	}

	start := p.pos
	for p.pos < len(p.input) && (p.input[p.pos] >= '0' && p.input[p.pos] <= '9' || p.input[p.pos] == '.') {
		p.pos++
	}
	if p.pos == start {
		return 0
	}
	val, _ := strconv.ParseFloat(p.input[start:p.pos], 64)
	return val
}

// --- HTTP helpers for web tools ---

var (
	reScript     = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	reStyle      = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	reHTMLTags   = regexp.MustCompile(`<[^>]*>`)
	reDDGTitle   = regexp.MustCompile(`(?s)class="result__a"[^>]*href="([^"]*)"[^>]*>(.*?)</a>`)
	reDDGSnippet = regexp.MustCompile(`(?s)class="result__snippet"[^>]*>(.*?)</a>`)
)

var toolHTTPClient = &http.Client{Timeout: 10 * time.Second}

func toolHTTPGet(rawURL string, maxBytes int64) (string, error) {
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Next/1.0)")
	resp, err := toolHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func stripHTML(s string) string {
	s = reScript.ReplaceAllString(s, " ")
	s = reStyle.ReplaceAllString(s, " ")
	s = reHTMLTags.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.Join(strings.Fields(s), " ")
}

func extractDDGLink(href string) string {
	if i := strings.Index(href, "uddg="); i >= 0 {
		s := href[i+5:]
		if j := strings.IndexByte(s, '&'); j >= 0 {
			s = s[:j]
		}
		if decoded, err := url.QueryUnescape(s); err == nil {
			return decoded
		}
	}
	if strings.HasPrefix(href, "//") {
		return "https:" + href
	}
	return href
}

func (tr *ToolRegistry) execSearchWeb(chatID, args string) (string, error) {
	var p struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if p.Query == "" {
		return "Erro: query nao pode ser vazia.", nil
	}

	searchURL := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(p.Query)
	body, err := toolHTTPGet(searchURL, 200_000)
	if err != nil {
		return fmt.Sprintf("Erro na busca: %s", err), nil
	}

	titles := reDDGTitle.FindAllStringSubmatch(body, 5)
	snippets := reDDGSnippet.FindAllStringSubmatch(body, 5)

	if len(titles) == 0 {
		return "Nenhum resultado encontrado.", nil
	}

	var sb strings.Builder
	for i, m := range titles {
		title := stripHTML(m[2])
		link := extractDDGLink(m[1])
		snippet := ""
		if i < len(snippets) {
			snippet = stripHTML(snippets[i][1])
		}
		sb.WriteString(fmt.Sprintf("%d. %s\n   %s\n   %s\n\n", i+1, title, link, snippet))
	}
	return strings.TrimSpace(sb.String()), nil
}

func (tr *ToolRegistry) execFetchURL(chatID, args string) (string, error) {
	var p struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if p.URL == "" {
		return "Erro: url nao pode ser vazia.", nil
	}
	if !strings.HasPrefix(p.URL, "http://") && !strings.HasPrefix(p.URL, "https://") {
		return "Erro: apenas URLs http/https sao permitidas.", nil
	}

	body, err := toolHTTPGet(p.URL, 200_000)
	if err != nil {
		return fmt.Sprintf("Erro ao acessar URL: %s", err), nil
	}

	text := body
	prefix := strings.ToLower(body[:min(500, len(body))])
	if strings.Contains(prefix, "<html") || strings.Contains(prefix, "<!doctype") || strings.Contains(prefix, "<head") {
		text = stripHTML(body)
	}

	if len(text) > 4000 {
		text = text[:4000] + "\n[...truncado]"
	}
	return strings.TrimSpace(text), nil
}

func (tr *ToolRegistry) execWeather(chatID, args string) (string, error) {
	var p struct {
		Location string `json:"location"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if p.Location == "" {
		return "Erro: localizacao nao pode ser vazia.", nil
	}

	wttrURL := fmt.Sprintf("https://wttr.in/%s?format=j1", url.PathEscape(p.Location))
	body, err := toolHTTPGet(wttrURL, 200_000)
	if err != nil {
		return fmt.Sprintf("Erro ao consultar clima: %s", err), nil
	}

	var data struct {
		CurrentCondition []struct {
			TempC         string `json:"temp_C"`
			FeelsLikeC    string `json:"FeelsLikeC"`
			Humidity      string `json:"humidity"`
			WindspeedKmph string `json:"windspeedKmph"`
			WeatherDesc   []struct {
				Value string `json:"value"`
			} `json:"weatherDesc"`
		} `json:"current_condition"`
		NearestArea []struct {
			AreaName []struct {
				Value string `json:"value"`
			} `json:"areaName"`
			Region []struct {
				Value string `json:"value"`
			} `json:"region"`
			Country []struct {
				Value string `json:"value"`
			} `json:"country"`
		} `json:"nearest_area"`
	}

	if err := json.Unmarshal([]byte(body), &data); err != nil {
		return fmt.Sprintf("Erro ao processar dados do clima: %s", err), nil
	}
	if len(data.CurrentCondition) == 0 {
		return "Dados de clima nao encontrados para essa localizacao.", nil
	}

	cc := data.CurrentCondition[0]
	desc := ""
	if len(cc.WeatherDesc) > 0 {
		desc = cc.WeatherDesc[0].Value
	}

	location := p.Location
	if len(data.NearestArea) > 0 {
		na := data.NearestArea[0]
		var parts []string
		if len(na.AreaName) > 0 && na.AreaName[0].Value != "" {
			parts = append(parts, na.AreaName[0].Value)
		}
		if len(na.Region) > 0 && na.Region[0].Value != "" {
			parts = append(parts, na.Region[0].Value)
		}
		if len(na.Country) > 0 && na.Country[0].Value != "" {
			parts = append(parts, na.Country[0].Value)
		}
		if len(parts) > 0 {
			location = strings.Join(parts, ", ")
		}
	}

	return fmt.Sprintf("Clima em %s:\nCondicao: %s\nTemperatura: %s°C (sensacao %s°C)\nUmidade: %s%%\nVento: %s km/h",
		location, desc, cc.TempC, cc.FeelsLikeC, cc.Humidity, cc.WindspeedKmph), nil
}

func (tr *ToolRegistry) execSaveNote(chatID, args string) (string, error) {
	var p struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if p.Key == "" || p.Value == "" {
		return "Erro: chave e valor sao obrigatorios.", nil
	}

	_, err := tr.db.Exec(`INSERT INTO notes (chat_id, key, value) VALUES (?, ?, ?)
		ON CONFLICT(chat_id, key) DO UPDATE SET value = excluded.value, created_at = unixepoch()`,
		chatID, p.Key, p.Value)
	if err != nil {
		return "", fmt.Errorf("save note: %w", err)
	}
	return fmt.Sprintf("Nota salva: %s = %s", p.Key, p.Value), nil
}

func (tr *ToolRegistry) execGetNotes(chatID, args string) (string, error) {
	var p struct {
		Key string `json:"key"`
	}
	json.Unmarshal([]byte(args), &p)

	var rows *sql.Rows
	var err error
	if p.Key != "" {
		rows, err = tr.db.Query("SELECT key, value FROM notes WHERE chat_id = ? AND key = ?", chatID, p.Key)
	} else {
		rows, err = tr.db.Query("SELECT key, value FROM notes WHERE chat_id = ? ORDER BY key", chatID)
	}
	if err != nil {
		return "", fmt.Errorf("get notes: %w", err)
	}
	defer rows.Close()

	var sb strings.Builder
	count := 0
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return "", err
		}
		count++
		sb.WriteString(fmt.Sprintf("- %s: %s\n", key, value))
	}
	if count == 0 {
		if p.Key != "" {
			return fmt.Sprintf("Nenhuma nota encontrada com chave '%s'.", p.Key), nil
		}
		return "Nenhuma nota salva para este contato.", nil
	}
	return strings.TrimSpace(sb.String()), nil
}

func (tr *ToolRegistry) execCurrency(chatID, args string) (string, error) {
	var p struct {
		Amount float64 `json:"amount"`
		From   string  `json:"from"`
		To     string  `json:"to"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if p.From == "" || p.To == "" {
		return "Erro: moedas de origem e destino sao obrigatorias.", nil
	}
	if p.Amount <= 0 {
		p.Amount = 1
	}

	from := strings.ToUpper(p.From)
	to := strings.ToUpper(p.To)

	apiURL := fmt.Sprintf("https://api.frankfurter.dev/v1/latest?amount=%.2f&from=%s&to=%s",
		p.Amount, url.QueryEscape(from), url.QueryEscape(to))
	body, err := toolHTTPGet(apiURL, 10_000)
	if err != nil {
		return fmt.Sprintf("Erro na conversao: %s", err), nil
	}

	var data struct {
		Amount float64            `json:"amount"`
		Base   string             `json:"base"`
		Rates  map[string]float64 `json:"rates"`
	}
	if err := json.Unmarshal([]byte(body), &data); err != nil {
		return fmt.Sprintf("Erro ao processar resposta: %s", err), nil
	}

	rate, ok := data.Rates[to]
	if !ok {
		return fmt.Sprintf("Moeda '%s' nao encontrada. Use codigos como USD, BRL, EUR, GBP, JPY.", to), nil
	}

	return fmt.Sprintf("%.2f %s = %.2f %s", p.Amount, from, rate, to), nil
}

var scheduleLayouts = []string{
	"2006-01-02 15:04",
	"2006-01-02T15:04",
	"02/01/2006 15:04",
	"2006-01-02 15:04:05",
	"02/01/2006 15:04:05",
}

func (tr *ToolRegistry) execScheduleMessage(chatID, args string) (string, error) {
	var p struct {
		Datetime      string `json:"datetime"`
		Message       string `json:"message"`
		Recurrence    string `json:"recurrence,omitempty"`
		RecurrenceEnd string `json:"recurrence_end,omitempty"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if p.Datetime == "" || p.Message == "" {
		return "Erro: datetime e message sao obrigatorios.", nil
	}

	tr.cfg.Mu.RLock()
	tz := tr.cfg.Timezone
	tr.cfg.Mu.RUnlock()

	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}

	var sendAt time.Time
	var parsed bool
	for _, layout := range scheduleLayouts {
		if t, err := time.ParseInLocation(layout, p.Datetime, loc); err == nil {
			sendAt = t
			parsed = true
			break
		}
	}
	if !parsed {
		return "Erro: formato de data invalido. Use AAAA-MM-DD HH:MM ou DD/MM/AAAA HH:MM.", nil
	}

	now := time.Now().In(loc)
	if sendAt.Before(now) {
		return "Erro: a data deve ser no futuro.", nil
	}

	var recEnd int64
	if p.RecurrenceEnd != "" {
		for _, layout := range []string{"2006-01-02", "02/01/2006"} {
			if t, err := time.ParseInLocation(layout, p.RecurrenceEnd, loc); err == nil {
				recEnd = t.Unix()
				break
			}
		}
	}

	_, err = tr.db.Exec("INSERT INTO scheduled_messages (chat_id, message, send_at, recurrence, recurrence_end) VALUES (?, ?, ?, ?, ?)",
		chatID, p.Message, sendAt.Unix(), p.Recurrence, recEnd)
	if err != nil {
		return "", fmt.Errorf("schedule message: %w", err)
	}

	days := [...]string{"domingo", "segunda", "terca", "quarta", "quinta", "sexta", "sabado"}
	result := fmt.Sprintf("Mensagem agendada para %s %02d/%02d/%d as %02d:%02d:\n\"%s\"",
		days[sendAt.Weekday()], sendAt.Day(), sendAt.Month(), sendAt.Year(),
		sendAt.Hour(), sendAt.Minute(), p.Message)
	if p.Recurrence != "" {
		result += fmt.Sprintf("\nRecorrencia: %s", p.Recurrence)
	}
	return result, nil
}

func (tr *ToolRegistry) execListScheduled(chatID, args string) (string, error) {
	rows, err := tr.db.Query(
		"SELECT id, message, send_at, recurrence FROM scheduled_messages WHERE chat_id = ? AND sent = 0 ORDER BY send_at",
		chatID)
	if err != nil {
		return "", fmt.Errorf("list scheduled: %w", err)
	}
	defer rows.Close()

	tr.cfg.Mu.RLock()
	tz := tr.cfg.Timezone
	tr.cfg.Mu.RUnlock()
	loc, _ := time.LoadLocation(tz)
	if loc == nil {
		loc = time.UTC
	}

	var sb strings.Builder
	count := 0
	for rows.Next() {
		var id int64
		var msg string
		var sendAt int64
		var recurrence string
		if err := rows.Scan(&id, &msg, &sendAt, &recurrence); err != nil {
			continue
		}
		count++
		t := time.Unix(sendAt, 0).In(loc)
		line := fmt.Sprintf("#%d [%02d/%02d/%d %02d:%02d] %s", id, t.Day(), t.Month(), t.Year(), t.Hour(), t.Minute(), msg)
		if recurrence != "" {
			line += fmt.Sprintf(" (recorrencia: %s)", recurrence)
		}
		sb.WriteString(line + "\n")
	}
	if count == 0 {
		return "Nenhuma mensagem agendada.", nil
	}
	return strings.TrimSpace(sb.String()), nil
}

func (tr *ToolRegistry) execCancelScheduled(chatID, args string) (string, error) {
	var p struct {
		ID  int64 `json:"id"`
		All bool  `json:"all"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}

	if p.All {
		res, err := tr.db.Exec("DELETE FROM scheduled_messages WHERE chat_id = ? AND sent = 0", chatID)
		if err != nil {
			return "", fmt.Errorf("cancel all: %w", err)
		}
		n, _ := res.RowsAffected()
		return fmt.Sprintf("%d mensagens agendadas canceladas.", n), nil
	}

	if p.ID <= 0 {
		return "Erro: informe id ou all:true.", nil
	}
	var owner string
	err := tr.db.QueryRow("SELECT chat_id FROM scheduled_messages WHERE id = ? AND sent = 0", p.ID).Scan(&owner)
	if err != nil {
		return "Agendamento nao encontrado.", nil
	}
	if owner != chatID {
		return "Agendamento nao encontrado.", nil
	}
	tr.db.Exec("DELETE FROM scheduled_messages WHERE id = ?", p.ID)
	return fmt.Sprintf("Agendamento #%d cancelado.", p.ID), nil
}

// --- Custom tools ---

var ReToolName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func (tr *ToolRegistry) loadCustomTools() {
	rows, err := tr.db.Query("SELECT id, name, description, method, url_template, headers, body_template, parameters, response_path, max_bytes FROM custom_tools WHERE enabled = 1")
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var ct types.CustomTool
		if err := rows.Scan(&ct.ID, &ct.Name, &ct.Description, &ct.Method, &ct.URLTemplate, &ct.Headers, &ct.BodyTemplate, &ct.Parameters, &ct.ResponsePath, &ct.MaxBytes); err != nil {
			continue
		}
		def := BuildToolDefinition(ct)
		tr.tools[ct.Name] = ToolHandler{
			Definition: def,
			Execute:    MakeCustomExecutor(ct),
		}
		tr.customNames[ct.Name] = true
	}
}

// ReloadCustomTools removes all custom tools and re-loads from DB.
func (tr *ToolRegistry) ReloadCustomTools() {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for name := range tr.customNames {
		delete(tr.tools, name)
	}
	tr.customNames = make(map[string]bool)
	tr.loadCustomTools()
}

// BuildToolDefinition converts a CustomTool into an OpenAI tool definition.
func BuildToolDefinition(ct types.CustomTool) openai.Tool {
	var params []types.CustomToolParam
	json.Unmarshal([]byte(ct.Parameters), &params)

	props := map[string]any{}
	var required []string
	for _, p := range params {
		typ := p.Type
		if typ == "" {
			typ = "string"
		}
		props[p.Name] = map[string]any{
			"type":        typ,
			"description": p.Description,
		}
		if p.Required {
			required = append(required, p.Name)
		}
	}

	schema := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	schemaJSON, _ := json.Marshal(schema)

	return openai.Tool{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        ct.Name,
			Description: ct.Description,
			Parameters:  json.RawMessage(schemaJSON),
		},
	}
}

// MakeCustomExecutor returns an executor closure for a custom API tool.
func MakeCustomExecutor(ct types.CustomTool) func(chatID, args string) (string, error) {
	return func(chatID, args string) (string, error) {
		var argsMap map[string]any
		json.Unmarshal([]byte(args), &argsMap)

		finalURL := ct.URLTemplate
		for k, v := range argsMap {
			finalURL = strings.ReplaceAll(finalURL, "{{"+k+"}}", url.QueryEscape(fmt.Sprint(v)))
		}

		if !strings.HasPrefix(finalURL, "http://") && !strings.HasPrefix(finalURL, "https://") {
			return "Erro: URL invalida.", nil
		}

		// SSRF protection: validate resolved IPs
		if err := validateURLSafety(finalURL); err != nil {
			return fmt.Sprintf("Erro de seguranca: %s", err), nil
		}

		var bodyReader io.Reader
		if ct.Method == "POST" {
			if ct.BodyTemplate != "" {
				body := ct.BodyTemplate
				for k, v := range argsMap {
					body = strings.ReplaceAll(body, "{{"+k+"}}", fmt.Sprint(v))
				}
				bodyReader = strings.NewReader(body)
			} else {
				bodyReader = bytes.NewReader([]byte(args))
			}
		}

		req, err := http.NewRequest(ct.Method, finalURL, bodyReader)
		if err != nil {
			return fmt.Sprintf("Erro ao criar request: %s", err), nil
		}
		req.Header.Set("User-Agent", "Next/1.0")

		var headers map[string]string
		if json.Unmarshal([]byte(ct.Headers), &headers) == nil {
			for k, v := range headers {
				req.Header.Set(k, v)
			}
		}
		if ct.Method == "POST" && req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/json")
		}

		maxBytes := ct.MaxBytes
		if maxBytes <= 0 {
			maxBytes = 10000
		}

		resp, err := toolHTTPClient.Do(req)
		if err != nil {
			return fmt.Sprintf("Erro HTTP: %s", err), nil
		}
		defer resp.Body.Close()

		bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)))
		if err != nil {
			return fmt.Sprintf("Erro ao ler resposta: %s", err), nil
		}

		if resp.StatusCode >= 400 {
			return fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(bodyBytes)), nil
		}

		result := string(bodyBytes)

		if ct.ResponsePath != "" {
			result = ExtractJSONPath(result, ct.ResponsePath)
		}

		if len(result) > 4000 {
			result = result[:4000] + "\n[...truncado]"
		}

		return strings.TrimSpace(result), nil
	}
}

// ExtractJSONPath navigates a JSON string using dot-notation and returns the value.
func ExtractJSONPath(jsonStr, path string) string {
	if path == "" {
		return jsonStr
	}
	parts := strings.Split(path, ".")
	var current any
	if err := json.Unmarshal([]byte(jsonStr), &current); err != nil {
		return jsonStr
	}
	for _, key := range parts {
		switch v := current.(type) {
		case map[string]any:
			val, ok := v[key]
			if !ok {
				return jsonStr
			}
			current = val
		case []any:
			idx, err := strconv.Atoi(key)
			if err != nil || idx < 0 || idx >= len(v) {
				return jsonStr
			}
			current = v[idx]
		default:
			return jsonStr
		}
	}
	switch v := current.(type) {
	case string:
		return v
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// --- MCP integration helpers ---

func (tr *ToolRegistry) loadMCPServers() {
	rows, err := tr.db.Query("SELECT id, name, url, api_key, enabled FROM mcp_servers WHERE enabled = 1")
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var name, mcpURL, apiKey string
		var enabled int
		if err := rows.Scan(&id, &name, &mcpURL, &apiKey, &enabled); err != nil {
			continue
		}
		mc := NewMCPClient(id, name, mcpURL, apiKey, tr)
		if err := mc.Connect(); err != nil {
			if tr.logger != nil {
				tr.logger.Log("tools_mcp_connect_error", "", map[string]any{"server": name, "error": err.Error()})
			}
			continue
		}
		tr.mcpClients = append(tr.mcpClients, mc)
	}
}

// ReloadMCPTools disconnects all MCP clients and reconnects.
func (tr *ToolRegistry) ReloadMCPTools() {
	tr.mu.Lock()
	for name := range tr.mcpNames {
		delete(tr.tools, name)
	}
	tr.mcpNames = make(map[string]bool)
	clients := tr.mcpClients
	tr.mcpClients = nil
	tr.mu.Unlock()

	for _, mc := range clients {
		mc.Disconnect()
	}

	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.loadMCPServers()
}

// ConnectMCPServer connects a single MCP server by DB id.
func (tr *ToolRegistry) ConnectMCPServer(id int64) error {
	var name, mcpURL, apiKey string
	err := tr.db.QueryRow("SELECT name, url, api_key FROM mcp_servers WHERE id = ?", id).Scan(&name, &mcpURL, &apiKey)
	if err != nil {
		return fmt.Errorf("server not found: %w", err)
	}

	mc := NewMCPClient(id, name, mcpURL, apiKey, tr)
	if err := mc.Connect(); err != nil {
		return err
	}

	tr.mu.Lock()
	tr.mcpClients = append(tr.mcpClients, mc)
	tr.mu.Unlock()
	return nil
}

// DisconnectMCPServer disconnects a single MCP server by DB id.
func (tr *ToolRegistry) DisconnectMCPServer(id int64) {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	var remaining []*MCPClient
	for _, mc := range tr.mcpClients {
		if mc.id == id {
			for _, t := range mc.tools {
				delete(tr.tools, t.FullName)
				delete(tr.mcpNames, t.FullName)
			}
			mc.Disconnect()
		} else {
			remaining = append(remaining, mc)
		}
	}
	tr.mcpClients = remaining
}

// RegisterMCPTool adds a single MCP tool to the registry (called from MCPClient).
func (tr *ToolRegistry) RegisterMCPTool(fullName string, handler ToolHandler) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.tools[fullName] = handler
	tr.mcpNames[fullName] = true
}

// StopMCPClients disconnects all MCP clients (called on shutdown).
func (tr *ToolRegistry) StopMCPClients() {
	tr.mu.Lock()
	clients := tr.mcpClients
	tr.mcpClients = nil
	tr.mu.Unlock()
	for _, mc := range clients {
		mc.Disconnect()
	}
}

// --- External databases ---

// BuildDSN builds a connection string for MySQL or PostgreSQL.
func BuildDSN(edb types.ExternalDatabase) string {
	switch edb.Driver {
	case "mysql":
		tls := "false"
		if edb.SSLMode == "require" || edb.SSLMode == "true" {
			tls = "true"
		} else if edb.SSLMode == "preferred" {
			tls = "preferred"
		}
		return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?tls=%s&timeout=5s&readTimeout=10s&parseTime=true",
			edb.Username, edb.Password, edb.Host, edb.Port, edb.DBName, tls)
	case "postgres":
		return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s connect_timeout=5",
			edb.Host, edb.Port, edb.Username, edb.Password, edb.DBName, edb.SSLMode)
	default:
		return ""
	}
}

// ValidateExternalDatabase validates fields of an ExternalDatabase.
func ValidateExternalDatabase(edb types.ExternalDatabase) error {
	if edb.Name == "" {
		return fmt.Errorf("nome obrigatorio")
	}
	if !ReToolName.MatchString(edb.Name) {
		return fmt.Errorf("nome invalido: use apenas letras minusculas, numeros e _ (comece com letra, max 64 chars)")
	}
	if edb.Name == "local" {
		return fmt.Errorf("nome 'local' e reservado")
	}
	if edb.Driver != "mysql" && edb.Driver != "postgres" {
		return fmt.Errorf("driver deve ser 'mysql' ou 'postgres'")
	}
	if edb.Host == "" {
		return fmt.Errorf("host obrigatorio")
	}
	if edb.Port < 1 || edb.Port > 65535 {
		return fmt.Errorf("porta deve ser entre 1 e 65535")
	}
	if edb.DBName == "" {
		return fmt.Errorf("nome do banco obrigatorio")
	}
	return nil
}

func (tr *ToolRegistry) connectExtDB(edb types.ExternalDatabase) error {
	dsn := BuildDSN(edb)
	if dsn == "" {
		return fmt.Errorf("driver desconhecido: %s", edb.Driver)
	}

	db, err := sql.Open(edb.Driver, dsn)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}

	db.SetMaxOpenConns(3)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return fmt.Errorf("ping: %w", err)
	}

	tr.mu.Lock()
	tr.extDBs[edb.ID] = &ExtDBConn{
		id:     edb.ID,
		name:   edb.Name,
		driver: edb.Driver,
		conn:   db,
	}
	tr.mu.Unlock()
	return nil
}

func (tr *ToolRegistry) disconnectExtDB(id int64) {
	tr.mu.Lock()
	if edb, ok := tr.extDBs[id]; ok {
		edb.conn.Close()
		delete(tr.extDBs, id)
	}
	tr.mu.Unlock()
}

func (tr *ToolRegistry) loadExtDBs() {
	rows, err := tr.db.Query("SELECT id, name, driver, host, port, username, password, dbname, ssl_mode, max_rows, enabled FROM external_databases WHERE enabled = 1")
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var edb types.ExternalDatabase
		if err := rows.Scan(&edb.ID, &edb.Name, &edb.Driver, &edb.Host, &edb.Port, &edb.Username, &edb.Password, &edb.DBName, &edb.SSLMode, &edb.MaxRows, &edb.Enabled); err != nil {
			continue
		}
		if err := tr.connectExtDB(edb); err != nil {
			if tr.logger != nil {
				tr.logger.Log("tools_extdb_connect_error", "", map[string]any{"database": edb.Name, "error": err.Error()})
			}
		}
	}
}

// ReloadExtDBs disconnects all external databases and re-loads from DB.
func (tr *ToolRegistry) ReloadExtDBs() {
	tr.StopExtDBs()
	tr.loadExtDBs()
}

// StopExtDBs closes all external database connections.
func (tr *ToolRegistry) StopExtDBs() {
	tr.mu.Lock()
	for id, edb := range tr.extDBs {
		edb.conn.Close()
		delete(tr.extDBs, id)
	}
	tr.mu.Unlock()
}

// ValidateExtReadOnlySQL checks that a query is a safe read-only SELECT (no blockedTables check for external DBs).
func ValidateExtReadOnlySQL(query string) error {
	q := strings.TrimSpace(query)
	if q == "" {
		return fmt.Errorf("query vazia")
	}

	inner := strings.TrimSuffix(strings.TrimSpace(q), ";")
	if strings.Contains(inner, ";") {
		return fmt.Errorf("multiplos comandos nao permitidos")
	}

	upper := strings.ToUpper(q)
	if !strings.HasPrefix(upper, "SELECT") && !strings.HasPrefix(upper, "WITH") {
		return fmt.Errorf("apenas SELECT permitido")
	}

	for _, kw := range writeKeywords {
		pat := `(?i)\b` + kw + `\b`
		if matched, _ := regexp.MatchString(pat, q); matched {
			return fmt.Errorf("palavra-chave bloqueada: %s", kw)
		}
	}

	return nil
}

func (tr *ToolRegistry) queryExtDB(dbName, query string, maxRows int) ([]string, [][]string, error) {
	if err := ValidateExtReadOnlySQL(query); err != nil {
		return nil, nil, err
	}

	tr.mu.RLock()
	var edb *ExtDBConn
	for _, e := range tr.extDBs {
		if e.name == dbName {
			edb = e
			break
		}
	}
	tr.mu.RUnlock()

	if edb == nil {
		return nil, nil, fmt.Errorf("banco externo '%s' nao encontrado ou desconectado", dbName)
	}

	if maxRows <= 0 {
		maxRows = 50
	}

	upperQ := strings.ToUpper(strings.TrimSpace(query))
	if !strings.Contains(upperQ, "LIMIT") {
		q := strings.TrimSuffix(strings.TrimSpace(query), ";")
		query = fmt.Sprintf("%s LIMIT %d", q, maxRows)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rows, err := edb.conn.QueryContext(ctx, query)
	if err != nil {
		return nil, nil, fmt.Errorf("SQL: %s", err.Error())
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}

	var data [][]string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		row := make([]string, len(cols))
		for i, v := range vals {
			row[i] = formatSQLValue(v)
		}
		data = append(data, row)
	}
	return cols, data, nil
}

// QueryExtDB is the public wrapper for web handlers.
func (tr *ToolRegistry) QueryExtDB(dbName, query string, maxRows int) ([]string, [][]string, error) {
	return tr.queryExtDB(dbName, query, maxRows)
}

func (tr *ToolRegistry) getExtDBSchema(dbName string) (string, error) {
	tr.mu.RLock()
	var edb *ExtDBConn
	for _, e := range tr.extDBs {
		if e.name == dbName {
			edb = e
			break
		}
	}
	tr.mu.RUnlock()

	if edb == nil {
		return "", fmt.Errorf("banco externo '%s' nao encontrado", dbName)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var query string
	switch edb.driver {
	case "mysql":
		query = "SELECT table_name, column_name, data_type FROM information_schema.columns WHERE table_schema = DATABASE() ORDER BY table_name, ordinal_position"
	case "postgres":
		query = "SELECT table_name, column_name, data_type FROM information_schema.columns WHERE table_schema = 'public' ORDER BY table_name, ordinal_position"
	default:
		return "", fmt.Errorf("driver desconhecido: %s", edb.driver)
	}

	rows, err := edb.conn.QueryContext(ctx, query)
	if err != nil {
		return "", fmt.Errorf("schema query: %w", err)
	}
	defer rows.Close()

	type colInfo struct {
		name     string
		dataType string
	}
	tables := make(map[string][]colInfo)
	var tableOrder []string
	for rows.Next() {
		var tbl, col, dtype string
		if err := rows.Scan(&tbl, &col, &dtype); err != nil {
			continue
		}
		if _, seen := tables[tbl]; !seen {
			tableOrder = append(tableOrder, tbl)
		}
		tables[tbl] = append(tables[tbl], colInfo{col, dtype})
	}

	var sb strings.Builder
	for _, tbl := range tableOrder {
		cols := tables[tbl]
		parts := make([]string, len(cols))
		for i, c := range cols {
			parts[i] = c.name + " " + strings.ToUpper(c.dataType)
		}
		sb.WriteString(fmt.Sprintf("%s (%s)\n", tbl, strings.Join(parts, ", ")))
	}
	return strings.TrimSpace(sb.String()), nil
}

// GetExtDBNames returns names of all connected external databases.
func (tr *ToolRegistry) GetExtDBNames() []string {
	tr.mu.RLock()
	defer tr.mu.RUnlock()
	names := make([]string, 0, len(tr.extDBs))
	for _, edb := range tr.extDBs {
		names = append(names, edb.name)
	}
	return names
}

// GetExtDBDriver returns the driver type for a connected external database by name.
func (tr *ToolRegistry) GetExtDBDriver(name string) string {
	tr.mu.RLock()
	defer tr.mu.RUnlock()
	for _, edb := range tr.extDBs {
		if edb.name == name {
			return edb.driver
		}
	}
	return ""
}

// --- Scheduler goroutine ---

// StartScheduler starts the background goroutine that sends scheduled messages.
func (tr *ToolRegistry) StartScheduler() {
	go tr.schedulerLoop()
}

// StopScheduler stops the scheduler goroutine.
func (tr *ToolRegistry) StopScheduler() {
	close(tr.stopCh)
}

func (tr *ToolRegistry) schedulerLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-tr.stopCh:
			return
		case <-ticker.C:
			tr.processScheduledMessages()
		}
	}
}

func (tr *ToolRegistry) processScheduledMessages() {
	now := time.Now().Unix()
	rows, err := tr.db.Query(
		"SELECT id, chat_id, message, send_at, recurrence, recurrence_end FROM scheduled_messages WHERE sent = 0 AND send_at <= ?", now)
	if err != nil {
		return
	}
	defer rows.Close()

	type pending struct {
		id            int64
		chatID        string
		message       string
		sendAt        int64
		recurrence    string
		recurrenceEnd int64
	}
	var msgs []pending
	for rows.Next() {
		var m pending
		if err := rows.Scan(&m.id, &m.chatID, &m.message, &m.sendAt, &m.recurrence, &m.recurrenceEnd); err != nil {
			continue
		}
		msgs = append(msgs, m)
	}

	for _, m := range msgs {
		if tr.wa == nil {
			tr.db.Exec("UPDATE scheduled_messages SET sent = 2 WHERE id = ?", m.id)
			continue
		}
		_, err := tr.wa.SendText(m.chatID, m.message)
		if err != nil {
			if tr.logger != nil {
				tr.logger.Log("error", m.chatID, map[string]any{"source": "scheduler", "message": "scheduled send: " + err.Error()})
			}
			tr.db.Exec("UPDATE scheduled_messages SET sent = 2 WHERE id = ?", m.id)
			continue
		}
		tr.db.Exec("UPDATE scheduled_messages SET sent = 1 WHERE id = ?", m.id)
		if tr.logger != nil {
			tr.logger.Log("tools_scheduled_sent", m.chatID, map[string]any{"message": m.message})
		}

		if m.recurrence != "" {
			nextSendAt := nextOccurrence(m.sendAt, m.recurrence)
			if nextSendAt > 0 && (m.recurrenceEnd == 0 || nextSendAt <= m.recurrenceEnd) {
				tr.db.Exec(
					"INSERT INTO scheduled_messages (chat_id, message, send_at, recurrence, recurrence_end) VALUES (?, ?, ?, ?, ?)",
					m.chatID, m.message, nextSendAt, m.recurrence, m.recurrenceEnd)
			}
		}
	}
}

func nextOccurrence(sendAt int64, recurrence string) int64 {
	t := time.Unix(sendAt, 0)
	switch {
	case recurrence == "hourly":
		return t.Add(time.Hour).Unix()
	case recurrence == "daily":
		return t.AddDate(0, 0, 1).Unix()
	case strings.HasPrefix(recurrence, "weekly"):
		return t.AddDate(0, 0, 7).Unix()
	case strings.HasPrefix(recurrence, "monthly"):
		return t.AddDate(0, 1, 0).Unix()
	case strings.HasPrefix(recurrence, "cron:"):
		return nextCron(t, strings.TrimPrefix(recurrence, "cron:"))
	}
	return 0
}

func nextCron(after time.Time, expr string) int64 {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return 0
	}

	candidate := after.Truncate(time.Minute).Add(time.Minute)
	limit := after.Add(48 * time.Hour)
	for candidate.Before(limit) {
		if cronFieldMatch(fields[0], candidate.Minute()) &&
			cronFieldMatch(fields[1], candidate.Hour()) &&
			cronFieldMatch(fields[2], candidate.Day()) &&
			cronFieldMatch(fields[3], int(candidate.Month())) &&
			cronFieldMatch(fields[4], int(candidate.Weekday())) {
			return candidate.Unix()
		}
		candidate = candidate.Add(time.Minute)
	}
	return 0
}

func cronFieldMatch(field string, value int) bool {
	if field == "*" {
		return true
	}
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if strings.Contains(part, "-") {
			bounds := strings.SplitN(part, "-", 2)
			lo, _ := strconv.Atoi(bounds[0])
			hi, _ := strconv.Atoi(bounds[1])
			if value >= lo && value <= hi {
				return true
			}
		} else if strings.Contains(part, "/") {
			sub := strings.SplitN(part, "/", 2)
			step, _ := strconv.Atoi(sub[1])
			if step > 0 && value%step == 0 {
				return true
			}
		} else {
			n, _ := strconv.Atoi(part)
			if n == value {
				return true
			}
		}
	}
	return false
}

// --- Agents ---

// seedDefaultAgent creates the default agent if it doesn't exist.
func (tr *ToolRegistry) seedDefaultAgent() {
	var exists int
	tr.db.QueryRow("SELECT COUNT(*) FROM agents WHERE is_default = 1").Scan(&exists)
	if exists > 0 {
		return
	}

	tr.db.Exec(`INSERT INTO agents (name, description, system_prompt, user_prompt, model, max_tokens, enabled, is_default,
		rag_enabled, rag_max_results, rag_compressed, rag_max_tokens, rag_embeddings, rag_embedding_model,
		tools_enabled, tools_max_rounds, tool_timeout_sec,
		guard_max_input, guard_max_output, guard_blocked_input, guard_blocked_output,
		guard_phone_list, guard_phone_mode, guard_block_injection, guard_block_pii,
		guard_block_pii_phone, guard_block_pii_email, guard_block_pii_cpf)
		VALUES (?, ?, ?, ?, ?, ?, 1, 1,
		0, ?, 0, ?, 0, ?,
		0, ?, ?,
		?, ?, '', '',
		'', ?, 1, 0,
		0, 0, 0)`,
		"Next (Padrão)", "Agente padrão do sistema", config.DefaultSystemPrompt, config.DefaultUserPrompt, config.DefaultModel, config.DefaultMaxTokens,
		config.DefaultRAGMaxResults, config.DefaultRAGMaxTokens, config.DefaultRAGEmbeddingModel,
		config.DefaultToolsMaxRounds, config.DefaultToolTimeoutSec,
		config.DefaultGuardMaxInput, config.DefaultGuardMaxOutput, config.DefaultGuardPhoneMode,
	)
}

// GetDefaultAgent returns the default agent, or nil.
func (tr *ToolRegistry) GetDefaultAgent() *types.Agent {
	tr.agentsMu.RLock()
	defer tr.agentsMu.RUnlock()
	for _, a := range tr.agents {
		if a.IsDefault == 1 {
			return a
		}
	}
	return nil
}

var reAgentName = regexp.MustCompile(`^[\p{L}0-9][\p{L}0-9 _()-]{0,63}$`)

// ValidateAgent validates agent fields.
func ValidateAgent(a types.Agent, db *sql.DB, editID int64) error {
	if !reAgentName.MatchString(a.Name) {
		return fmt.Errorf("Nome invalido: use letras, numeros, espacos, _ ou - (max 64 chars)")
	}
	if a.SystemPrompt == "" {
		return fmt.Errorf("System prompt obrigatorio")
	}
	if a.Model == "" {
		return fmt.Errorf("Modelo obrigatorio")
	}
	if a.MaxTokens < 50 || a.MaxTokens > 32000 {
		return fmt.Errorf("Max tokens deve ser entre 50 e 32000")
	}
	if a.BaseURL != "" && !strings.HasPrefix(a.BaseURL, "http://") && !strings.HasPrefix(a.BaseURL, "https://") {
		return fmt.Errorf("Base URL deve comecar com http:// ou https://")
	}
	var count int
	db.QueryRow("SELECT COUNT(*) FROM agents WHERE id != ?", editID).Scan(&count)
	if count >= 10 {
		return fmt.Errorf("Limite de 10 agentes atingido")
	}
	return nil
}

func (tr *ToolRegistry) loadAgents() {
	rows, err := tr.db.Query(`SELECT id, name, description, system_prompt, user_prompt, model, max_tokens, base_url, api_key, enabled, is_default, chain_to, chain_condition, rag_tags,
		rag_enabled, rag_max_results, rag_compressed, rag_max_tokens, rag_embeddings, rag_embedding_model,
		tools_enabled, tools_max_rounds, tool_timeout_sec,
		guard_max_input, guard_max_output, guard_blocked_input, guard_blocked_output,
		guard_phone_list, guard_phone_mode, guard_block_injection, guard_block_pii,
		guard_block_pii_phone, guard_block_pii_email, guard_block_pii_cpf,
		knowledge_extract,
		created_at FROM agents WHERE enabled = 1 OR is_default = 1`)
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var a types.Agent
		if err := rows.Scan(&a.ID, &a.Name, &a.Description, &a.SystemPrompt, &a.UserPrompt, &a.Model, &a.MaxTokens, &a.BaseURL, &a.APIKey, &a.Enabled, &a.IsDefault, &a.ChainTo, &a.ChainCondition, &a.RAGTags,
			&a.RAGEnabled, &a.RAGMaxResults, &a.RAGCompressed, &a.RAGMaxTokens, &a.RAGEmbeddings, &a.RAGEmbeddingModel,
			&a.ToolsEnabled, &a.ToolsMaxRounds, &a.ToolTimeoutSec,
			&a.GuardMaxInput, &a.GuardMaxOutput, &a.GuardBlockedInput, &a.GuardBlockedOutput,
			&a.GuardPhoneList, &a.GuardPhoneMode, &a.GuardBlockInjection, &a.GuardBlockPII,
			&a.GuardBlockPIIPhone, &a.GuardBlockPIIEmail, &a.GuardBlockPIICPF,
			&a.KnowledgeExtract,
			&a.CreatedAt); err != nil {
			continue
		}
		tr.agents[a.ID] = &a
	}

	rRows, err := tr.db.Query("SELECT chat_id, agent_id FROM agent_routing")
	if err != nil {
		return
	}
	defer rRows.Close()

	for rRows.Next() {
		var chatID string
		var agentID int64
		if err := rRows.Scan(&chatID, &agentID); err != nil {
			continue
		}
		if _, ok := tr.agents[agentID]; ok {
			tr.agentRouting[chatID] = agentID
		}
	}
}

// ReloadAgents clears and reloads agents + routing from DB.
func (tr *ToolRegistry) ReloadAgents() {
	tr.agentsMu.Lock()
	defer tr.agentsMu.Unlock()
	tr.agents = make(map[int64]*types.Agent)
	tr.agentRouting = make(map[string]int64)
	tr.loadAgents()
}

// GetAgentForChatID returns the agent assigned to a chat ID, or nil.
func (tr *ToolRegistry) GetAgentForChatID(chatID string) *types.Agent {
	tr.agentsMu.RLock()
	defer tr.agentsMu.RUnlock()
	agentID, ok := tr.agentRouting[chatID]
	if !ok {
		return nil
	}
	return tr.agents[agentID]
}

// GetAgentByID returns an agent by its ID, or nil.
func (tr *ToolRegistry) GetAgentByID(id int64) *types.Agent {
	tr.agentsMu.RLock()
	defer tr.agentsMu.RUnlock()
	return tr.agents[id]
}

// GetDB returns the database handle (used by web handlers for agents).
func (tr *ToolRegistry) GetDB() *sql.DB {
	return tr.db
}

// GetBuiltinNames returns a snapshot of the built-in tool name set.
func (tr *ToolRegistry) GetBuiltinNames() map[string]bool {
	tr.mu.RLock()
	defer tr.mu.RUnlock()
	out := make(map[string]bool, len(tr.builtinNames))
	for k, v := range tr.builtinNames {
		out[k] = v
	}
	return out
}

// MCPClientInfo holds display info about a connected MCP client.
type MCPClientInfo struct {
	ID        int64
	Connected bool
	Tools     []MCPTool
}

// GetMCPClientInfos returns status and tool info for all MCP clients.
func (tr *ToolRegistry) GetMCPClientInfos() []MCPClientInfo {
	tr.mu.RLock()
	defer tr.mu.RUnlock()
	out := make([]MCPClientInfo, 0, len(tr.mcpClients))
	for _, mc := range tr.mcpClients {
		out = append(out, MCPClientInfo{
			ID:        mc.id,
			Connected: mc.IsConnected(),
			Tools:     mc.GetTools(),
		})
	}
	return out
}

// IsExtDBConnected reports whether the external DB with the given ID is connected.
func (tr *ToolRegistry) IsExtDBConnected(id int64) bool {
	tr.mu.RLock()
	defer tr.mu.RUnlock()
	_, ok := tr.extDBs[id]
	return ok
}

// ConnectExtDB connects to an external database (exported wrapper).
func (tr *ToolRegistry) ConnectExtDB(edb types.ExternalDatabase) error {
	return tr.connectExtDB(edb)
}

// DisconnectExtDB disconnects an external database (exported wrapper).
func (tr *ToolRegistry) DisconnectExtDB(id int64) {
	tr.disconnectExtDB(id)
}

// GetExtDBSchema returns the schema description for an external database by name.
func (tr *ToolRegistry) GetExtDBSchema(dbName string) (string, error) {
	return tr.getExtDBSchema(dbName)
}

// ValidateCustomTool validates a custom tool against the built-in name set.
func ValidateCustomTool(ct types.CustomTool, builtins map[string]bool) error {
	if !ReToolName.MatchString(ct.Name) {
		return fmt.Errorf("Nome invalido: use apenas letras minusculas, numeros e _ (comece com letra, max 64 chars)")
	}
	if builtins[ct.Name] {
		return fmt.Errorf("Nome '%s' ja e uma ferramenta built-in", ct.Name)
	}
	if ct.Description == "" {
		return fmt.Errorf("Descricao obrigatoria")
	}
	if !strings.HasPrefix(ct.URLTemplate, "http://") && !strings.HasPrefix(ct.URLTemplate, "https://") {
		return fmt.Errorf("URL deve comecar com http:// ou https://")
	}
	if ct.Method != "GET" && ct.Method != "POST" {
		return fmt.Errorf("Metodo deve ser GET ou POST")
	}
	if ct.Parameters != "" && ct.Parameters != "[]" {
		var params []types.CustomToolParam
		if err := json.Unmarshal([]byte(ct.Parameters), &params); err != nil {
			return fmt.Errorf("Parametros JSON invalido: %s", err)
		}
	}
	if ct.Headers != "" && ct.Headers != "{}" {
		var h map[string]string
		if err := json.Unmarshal([]byte(ct.Headers), &h); err != nil {
			return fmt.Errorf("Headers JSON invalido: %s", err)
		}
	}
	// SSRF check at save time (if URL has no placeholders)
	if !strings.Contains(ct.URLTemplate, "{{") {
		if err := validateURLSafety(ct.URLTemplate); err != nil {
			return fmt.Errorf("URL bloqueada: %s", err)
		}
	}
	return nil
}

// min returns the minimum of two ints.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ReportResponse is the JSON response for the /api/report endpoint.
type ReportResponse struct {
	SQL         string         `json:"sql"`
	Columns     []string       `json:"columns"`
	Rows        [][]string     `json:"rows"`
	ChartType   string         `json:"chart_type"`
	ChartConfig map[string]any `json:"chart_config"`
	Insight     string         `json:"insight"`
	Error       string         `json:"error,omitempty"`
}

// CleanSQL strips markdown fences from AI-generated SQL.
func CleanSQL(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		lines := strings.Split(s, "\n")
		start, end := 0, len(lines)
		for i, l := range lines {
			if strings.HasPrefix(strings.TrimSpace(l), "```") {
				if i == 0 {
					start = 1
				} else {
					end = i
					break
				}
			}
		}
		s = strings.Join(lines[start:end], "\n")
	}
	return strings.TrimSpace(s)
}

// FormatDataSummary creates a pipe-delimited text summary (max 50 rows) for the AI.
func FormatDataSummary(cols []string, rows [][]string) string {
	var sb strings.Builder
	sb.WriteString(strings.Join(cols, " | "))
	sb.WriteString("\n")
	limit := len(rows)
	if limit > 50 {
		limit = 50
	}
	for i := 0; i < limit; i++ {
		sb.WriteString(strings.Join(rows[i], " | "))
		sb.WriteString("\n")
	}
	if len(rows) > 50 {
		sb.WriteString(fmt.Sprintf("... (%d linhas no total)\n", len(rows)))
	}
	return sb.String()
}

// ParseInterpretation extracts JSON from the AI interpretation response.
func ParseInterpretation(raw string, resp *ReportResponse) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		lines := strings.Split(raw, "\n")
		start, end := 0, len(lines)
		for i, l := range lines {
			if strings.HasPrefix(strings.TrimSpace(l), "```") {
				if i == 0 {
					start = 1
				} else {
					end = i
					break
				}
			}
		}
		raw = strings.Join(lines[start:end], "\n")
		raw = strings.TrimSpace(raw)
	}

	var parsed struct {
		Insight     string         `json:"insight"`
		ChartType   string         `json:"chart_type"`
		ChartConfig map[string]any `json:"chart_config"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		resp.Insight = raw
		resp.ChartType = "none"
		return
	}

	resp.Insight = parsed.Insight
	resp.ChartType = parsed.ChartType
	resp.ChartConfig = parsed.ChartConfig

	validTypes := map[string]bool{"bar": true, "line": true, "pie": true, "doughnut": true, "none": true}
	if !validTypes[resp.ChartType] {
		resp.ChartType = "none"
	}
}
