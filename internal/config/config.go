package config

import (
	"database/sql"
	"fmt"
	"strconv"
	"sync"
)

// Config holds all application settings.
// Stored in SQLite table "config" as key-value pairs.
type Config struct {
	Mu                sync.RWMutex
	BaseURL           string
	APIKey            string
	MaxHistory        int
	DebounceMs        int
	DebounceMaxMs     int
	SessionTimeoutMin int
	ContextBudget     int
	Debug             bool
	// MCP Server
	MCPServerEnabled bool
	MCPServerToken   string
	// General
	Timezone string
	// Response mode
	ResponseMode string // "all", "owner", "contacts"
	// Groups
	GroupsEnabled   bool
	GroupList       string // comma-separated group JIDs (ex: 120363XXX@g.us)
	WhatsAppAgentID int
}

// Default constants for agent-level settings (used by seed and migrations).
const (
	DefaultRAGMaxResults     = 3
	DefaultRAGMaxTokens      = 500
	DefaultRAGEmbeddingModel = "text-embedding-3-small"
	DefaultToolsMaxRounds    = 3
	DefaultToolTimeoutSec    = 30
	DefaultGuardMaxInput     = 2000
	DefaultGuardMaxOutput    = 3000
	DefaultGuardPhoneMode    = "off"
)

const DefaultModel = "gpt-4.1-mini"
const DefaultMaxTokens = 1024

const DefaultSystemPrompt = `Voce e o Next, um agente inteligente no WhatsApp.

Regras:
- Responda de forma curta, educada e util (maximo 3 linhas)
- Ajude com qualquer assunto
- Se nao souber algo, diga honestamente
- Use linguagem natural e amigavel
- Nunca invente informacoes

Grupos:
- Voce pode participar de conversas em grupos do WhatsApp
- Em grupos, responda de forma relevante ao contexto da conversa
- Seja breve e direto, evitando dominar a conversa do grupo

Correcoes:
- Se o usuario corrigir algo que voce disse, reconheca o erro imediatamente
- Peca desculpa de forma breve e natural, sem ser excessivo
- Ajuste sua resposta com a informacao correta
- Nunca insista em algo que o usuario ja corrigiu`

const DefaultUserPrompt = ``

// DefaultConfig returns a Config with all default values.
func DefaultConfig() *Config {
	return &Config{
		BaseURL:           "https://api.openai.com/v1",
		MaxHistory:        20,
		DebounceMs:        3000,
		DebounceMaxMs:     15000,
		SessionTimeoutMin: 240,
		ContextBudget:     4000,
		Debug:             false,
		// MCP Server
		MCPServerEnabled: false,
		MCPServerToken:   "",
		// General
		Timezone:      "America/Sao_Paulo",
		ResponseMode:  "all",
		GroupsEnabled: false,
		GroupList:     "",
	}
}

func createConfigTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS config (
		id    INTEGER PRIMARY KEY,
		key   TEXT UNIQUE NOT NULL,
		value TEXT NOT NULL
	)`)
	return err
}

// LoadConfig reads config from SQLite, filling defaults for missing keys.
func LoadConfig(db *sql.DB) (*Config, error) {
	if err := createConfigTable(db); err != nil {
		return nil, fmt.Errorf("create config table: %w", err)
	}

	cfg := DefaultConfig()

	rows, err := db.Query("SELECT key, value FROM config")
	if err != nil {
		return nil, fmt.Errorf("query config: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("scan config row: %w", err)
		}
		switch k {
		case "base_url":
			cfg.BaseURL = v
		case "api_key":
			cfg.APIKey = v
		case "max_history":
			cfg.MaxHistory, _ = strconv.Atoi(v)
		case "debounce_ms":
			cfg.DebounceMs, _ = strconv.Atoi(v)
		case "debounce_max_ms":
			cfg.DebounceMaxMs, _ = strconv.Atoi(v)
		case "session_timeout_min":
			cfg.SessionTimeoutMin, _ = strconv.Atoi(v)
		case "context_budget":
			cfg.ContextBudget, _ = strconv.Atoi(v)
		case "debug":
			cfg.Debug = v == "true"
		case "mcp_server_enabled":
			cfg.MCPServerEnabled = v == "true"
		case "mcp_server_token":
			cfg.MCPServerToken = v
		case "timezone":
			cfg.Timezone = v
		case "response_mode":
			cfg.ResponseMode = v
		case "groups_enabled":
			cfg.GroupsEnabled = v == "true"
		case "group_list":
			cfg.GroupList = v
		case "whatsapp_agent_id":
			cfg.WhatsAppAgentID, _ = strconv.Atoi(v)
		}
	}
	return cfg, rows.Err()
}

// SaveConfig upserts all fields to SQLite.
func SaveConfig(db *sql.DB, cfg *Config) error {
	cfg.Mu.RLock()
	pairs := map[string]string{
		"base_url":            cfg.BaseURL,
		"api_key":             cfg.APIKey,
		"max_history":         strconv.Itoa(cfg.MaxHistory),
		"debounce_ms":         strconv.Itoa(cfg.DebounceMs),
		"debounce_max_ms":     strconv.Itoa(cfg.DebounceMaxMs),
		"session_timeout_min": strconv.Itoa(cfg.SessionTimeoutMin),
		"context_budget":      strconv.Itoa(cfg.ContextBudget),
		"debug":               strconv.FormatBool(cfg.Debug),
		// MCP Server
		"mcp_server_enabled": strconv.FormatBool(cfg.MCPServerEnabled),
		"mcp_server_token":   cfg.MCPServerToken,
		// General
		"timezone":          cfg.Timezone,
		"response_mode":     cfg.ResponseMode,
		"groups_enabled":    strconv.FormatBool(cfg.GroupsEnabled),
		"group_list":        cfg.GroupList,
		"whatsapp_agent_id": strconv.Itoa(cfg.WhatsAppAgentID),
	}
	cfg.Mu.RUnlock()

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare("INSERT INTO config (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for k, v := range pairs {
		if k == "api_key" && v == "" {
			continue // não apaga chave já salva
		}
		if _, err := stmt.Exec(k, v); err != nil {
			return fmt.Errorf("upsert config %s: %w", k, err)
		}
	}
	return tx.Commit()
}
