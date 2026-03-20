package web

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"next/app/ai"
	"next/app/guardrails"
	"next/app/memory"
	"next/app/pipeline"
	"next/app/rag"
	"next/app/tools"
	"next/app/types"
	"next/internal/auth"
	"next/internal/backup"
	"next/internal/config"
	"next/internal/debounce"
	"next/internal/logger"
	"next/internal/whatsapp"
)

// Web serves the HTTP interface.
type Web struct {
	cfg       *config.Config
	memory    *memory.Memory
	rag       *rag.RAG
	ai        *ai.AI
	wa        *whatsapp.WhatsApp
	debouncer *debounce.Debouncer
	logger    *logger.Logger
	tools     *tools.ToolRegistry
	guard     *guardrails.Guardrails
	pipe      *pipeline.Pipeline
	auth      *auth.Auth
	mcpServer *tools.NextMCPServer
	db        *sql.DB
	backup    *backup.Backup
	saveCfg   func() error
}

// WebDeps holds dependencies for the web server.
type WebDeps struct {
	Config    *config.Config
	Memory    *memory.Memory
	RAG       *rag.RAG
	AI        *ai.AI
	WhatsApp  *whatsapp.WhatsApp
	Debouncer *debounce.Debouncer
	Logger    *logger.Logger
	Tools     *tools.ToolRegistry
	Guard     *guardrails.Guardrails
	Pipeline  *pipeline.Pipeline
	Auth      *auth.Auth
	MCPServer *tools.NextMCPServer
	DB        *sql.DB
	Backup    *backup.Backup
	SaveCfg   func() error
}

// serveTemplate parses and executes a template file.
func serveTemplate(rw http.ResponseWriter, filename string) {
	tmpl, err := template.ParseFiles(filename)
	if err != nil {
		http.Error(rw, "Template error: "+err.Error(), 500)
		return
	}
	tmpl.Execute(rw, nil)
}

// jsonResponse writes a JSON response with the correct Content-Type header.
func jsonResponse(rw http.ResponseWriter, data any) {
	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(data)
}

// jsonError writes a JSON error response with the given status code.
func jsonError(rw http.ResponseWriter, message string, statusCode int) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(statusCode)
	json.NewEncoder(rw).Encode(map[string]string{"error": message})
}

// jsonCreated writes a JSON response with 201 Created status.
func jsonCreated(rw http.ResponseWriter, data any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(201)
	json.NewEncoder(rw).Encode(data)
}

// requireJSON returns true if the request has a JSON Content-Type, otherwise sends 415.
func requireJSON(rw http.ResponseWriter, r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if ct != "" && !strings.HasPrefix(ct, "application/json") {
		jsonError(rw, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return false
	}
	return true
}

// parsePagination extracts page and limit from query params, returning (page, limit, offset).
func parsePagination(r *http.Request, defaultLimit int) (int, int, int) {
	page := 1
	limit := defaultLimit

	if p := r.URL.Query().Get("page"); p != "" {
		if v, err := strconv.Atoi(p); err == nil && v > 0 {
			page = v
		}
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil && v > 0 {
			limit = v
		}
	}
	if limit > 500 {
		limit = 500
	}
	offset := (page - 1) * limit
	return page, limit, offset
}

// paginatedResponse wraps data with pagination metadata.
func paginatedResponse(rw http.ResponseWriter, data any, total int64, page, limit int) {
	totalPages := int(total) / limit
	if int(total)%limit != 0 {
		totalPages++
	}
	jsonResponse(rw, map[string]any{
		"data":        data,
		"total":       total,
		"page":        page,
		"limit":       limit,
		"total_pages": totalPages,
	})
}

// requirePOST returns true if the request method is POST, otherwise sends 405.
func requireGET(rw http.ResponseWriter, r *http.Request) bool {
	if r.Method != "GET" {
		jsonError(rw, "Method not allowed", 405)
		return false
	}
	return true
}

func requirePOST(rw http.ResponseWriter, r *http.Request) bool {
	if r.Method != "POST" {
		jsonError(rw, "Method not allowed", 405)
		return false
	}
	return true
}

// SetupRoutes registers all HTTP routes.
func SetupRoutes(mux *http.ServeMux, deps WebDeps) {
	w := &Web{
		cfg:       deps.Config,
		memory:    deps.Memory,
		rag:       deps.RAG,
		ai:        deps.AI,
		wa:        deps.WhatsApp,
		debouncer: deps.Debouncer,
		logger:    deps.Logger,
		tools:     deps.Tools,
		guard:     deps.Guard,
		pipe:      deps.Pipeline,
		auth:      deps.Auth,
		mcpServer: deps.MCPServer,
		db:        deps.DB,
		backup:    deps.Backup,
		saveCfg:   deps.SaveCfg,
	}

	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	mux.HandleFunc("/health", w.handleHealth)
	mux.HandleFunc("/", w.handleRoot)
	mux.HandleFunc("/config", w.handleConfigPage)
	mux.HandleFunc("/conversas", w.handleConversas)
	mux.HandleFunc("/logs", w.handleLogs)
	mux.HandleFunc("/api/status", w.handleStatus)
	mux.HandleFunc("/api/config", w.handleConfig)
	mux.HandleFunc("/api/test-ai", w.handleTestAI)
	mux.HandleFunc("/api/contacts", w.handleContacts)
	mux.HandleFunc("/api/messages", w.handleMessages)
	mux.HandleFunc("/api/logs", w.handleAPILogs)
	mux.HandleFunc("/api/groups", w.handleGroups)
	mux.HandleFunc("/api/reconnect", w.handleReconnect)
	mux.HandleFunc("/api/disconnect", w.handleDisconnect)
	mux.HandleFunc("/conhecimento", w.handleConhecimento)
	mux.HandleFunc("/api/knowledge", w.handleKnowledge)
	mux.HandleFunc("/api/knowledge/", w.handleKnowledgeByID)
	mux.HandleFunc("/api/knowledge-tags", w.handleKnowledgeTags)
	mux.HandleFunc("/api/knowledge-compress", w.handleKnowledgeCompressAll)
	mux.HandleFunc("/api/knowledge-embed", w.handleKnowledgeEmbedAll)
	mux.HandleFunc("/api/knowledge-process", w.handleKnowledgeProcessAll)
	mux.HandleFunc("/api/knowledge-strip-html", w.handleKnowledgeStripHTML)
	mux.HandleFunc("/chat", w.handleChatPage)
	mux.HandleFunc("/api/chat", w.handleChat)
	mux.HandleFunc("/api/chat/clear", w.handleChatClear)
	mux.HandleFunc("/api/notes", w.handleNotes)
	mux.HandleFunc("/api/custom-tools", w.handleCustomTools)
	mux.HandleFunc("/api/custom-tools/", w.handleCustomToolByID)
	mux.HandleFunc("/api/mcp-servers", w.handleMCPServers)
	mux.HandleFunc("/api/mcp-servers/", w.handleMCPServerByID)
	mux.HandleFunc("/api/ext-databases", w.handleExtDatabases)
	mux.HandleFunc("/api/ext-databases/", w.handleExtDatabaseByID)
	mux.HandleFunc("/api/agents", w.handleAgents)
	mux.HandleFunc("/api/agents/", w.handleAgentByID)
	mux.HandleFunc("/api/agent-routing", w.handleAgentRouting)
	mux.HandleFunc("/agentes", w.handleAgentes)
	mux.HandleFunc("/ferramentas", w.handleFerramentas)
	mux.HandleFunc("/mcp-admin", w.handleMCPAdmin)
	mux.HandleFunc("/databases", w.handleDatabases)
	mux.HandleFunc("/relatorios", w.handleRelatorios)
	mux.HandleFunc("/api/report", w.handleReport)
	mux.HandleFunc("/api/report/clear", w.handleReportClear)
	mux.HandleFunc("/api/backup", w.handleBackup)
	mux.HandleFunc("/api/backups", w.handleBackupList)
	// MCP server routes (outside web UI auth — have their own token auth)
	if w.mcpServer != nil {
		mux.Handle("/mcp/sse", w.mcpServer.SSEHandler())
		mux.Handle("/mcp/message", w.mcpServer.MessageHandler())
	}
	// Auth routes
	mux.HandleFunc("/login", w.auth.HandleLogin)
	mux.HandleFunc("/api/login", w.auth.HandleLogin)
	mux.HandleFunc("/api/logout", w.auth.HandleLogout)
	mux.HandleFunc("/api/auth/status", w.auth.HandleAuthStatus)
	mux.HandleFunc("/api/auth/change-password", w.auth.HandleChangePassword)
	mux.HandleFunc("/api/users", w.auth.HandleUsers)
	mux.HandleFunc("/api/users/", w.auth.HandleUserByID)
}

func (w *Web) handleRoot(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(rw, r)
		return
	}
	http.Redirect(rw, r, "/conversas", http.StatusFound)
}

func (w *Web) handleHealth(rw http.ResponseWriter, r *http.Request) {
	status := "healthy"
	httpCode := 200

	// Check database
	dbStatus := "ok"
	if w.db != nil {
		if err := w.db.QueryRow("SELECT 1").Err(); err != nil {
			dbStatus = "error: " + err.Error()
			status = "unhealthy"
			httpCode = 503
		}
	}

	// Check WhatsApp
	waStatus := "disconnected"
	waConnected := false
	if w.wa != nil {
		waConnected = w.wa.IsConnected()
		if waConnected {
			waStatus = "connected"
		}
	}
	if !waConnected && status == "healthy" {
		status = "degraded"
	}

	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(httpCode)
	json.NewEncoder(rw).Encode(map[string]any{
		"status":    status,
		"database":  map[string]any{"status": dbStatus},
		"whatsapp":  map[string]any{"status": waStatus, "connected": waConnected},
		"timestamp": time.Now().Unix(),
	})
}

func (w *Web) handleBackup(rw http.ResponseWriter, r *http.Request) {
	if !requirePOST(rw, r) {
		return
	}
	if !auth.RequireAdmin(rw, r) {
		return
	}
	if w.backup == nil {
		jsonResponse(rw, map[string]any{"error": "backup not configured"})
		return
	}
	name, err := w.backup.Run()
	if err != nil {
		jsonError(rw, err.Error(), 500)
		return
	}
	jsonResponse(rw, map[string]any{"ok": true, "name": name})
}

func (w *Web) handleBackupList(rw http.ResponseWriter, r *http.Request) {
	if !auth.RequireAdmin(rw, r) {
		return
	}
	if w.backup == nil {
		jsonResponse(rw, map[string]any{"backups": []any{}})
		return
	}
	list, err := w.backup.List()
	if err != nil {
		jsonError(rw, err.Error(), 500)
		return
	}
	jsonResponse(rw, map[string]any{"backups": list})
}

func (w *Web) handleConfigPage(rw http.ResponseWriter, r *http.Request) {
	serveTemplate(rw, "templates/index.html")
}

func (w *Web) handleConversas(rw http.ResponseWriter, r *http.Request) {
	serveTemplate(rw, "templates/conversas.html")
}

func (w *Web) handleLogs(rw http.ResponseWriter, r *http.Request) {
	serveTemplate(rw, "templates/logs.html")
}

func (w *Web) handleAgentes(rw http.ResponseWriter, r *http.Request) {
	serveTemplate(rw, "templates/agentes.html")
}

func (w *Web) handleFerramentas(rw http.ResponseWriter, r *http.Request) {
	serveTemplate(rw, "templates/ferramentas.html")
}

func (w *Web) handleMCPAdmin(rw http.ResponseWriter, r *http.Request) {
	serveTemplate(rw, "templates/mcp.html")
}

func (w *Web) handleDatabases(rw http.ResponseWriter, r *http.Request) {
	serveTemplate(rw, "templates/databases.html")
}

func (w *Web) handleStatus(rw http.ResponseWriter, r *http.Request) {
	if !requireGET(rw, r) {
		return
	}
	qrText := w.wa.GetQRCode()
	var qrBase64 string
	if qrText != "" {
		png, err := qrcode.Encode(qrText, qrcode.Medium, 256)
		if err == nil {
			qrBase64 = "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
		}
	}

	jsonResponse(rw, map[string]any{
		"connected": w.wa.IsConnected(),
		"phone":     w.wa.GetPhoneNumber(),
		"qr_code":   qrBase64,
	})
}
func (w *Web) handleGroups(rw http.ResponseWriter, r *http.Request) {
	if !requireGET(rw, r) {
		return
	}
	groups, err := w.wa.GetGroups()
	if err != nil {
		jsonResponse(rw, map[string]any{"groups": []any{}, "error": err.Error()})
		return
	}
	jsonResponse(rw, map[string]any{"groups": groups})
}

func (w *Web) handleReconnect(rw http.ResponseWriter, r *http.Request) {
	if !requirePOST(rw, r) {
		return
	}
	if !auth.RequireAdmin(rw, r) {
		return
	}
	w.wa.Reconnect(w.logger)
	jsonResponse(rw, map[string]any{"ok": true})
}

func (w *Web) handleDisconnect(rw http.ResponseWriter, r *http.Request) {
	if !requirePOST(rw, r) {
		return
	}
	if !auth.RequireAdmin(rw, r) {
		return
	}
	err := w.wa.Logout()
	if err != nil {
		w.logger.Log("error", "", map[string]any{"source": "web", "message": "logout: " + err.Error()})
	}
	w.logger.Log("web_whatsapp_logout", "", map[string]any{"reason": "manual_logout"})
	jsonResponse(rw, map[string]any{"ok": true})
}

func (w *Web) handleConfig(rw http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		w.cfg.Mu.RLock()
		// Mask API key: admin sees partial, user sees nothing
		masked := w.cfg.APIKey
		s := auth.GetSessionFromCtx(r)
		if s != nil && s.Role != "admin" {
			masked = "***"
		} else if len(masked) > 8 {
			masked = masked[:4] + "..." + masked[len(masked)-4:]
		}
		resp := map[string]any{
			"base_url":            w.cfg.BaseURL,
			"api_key":             masked,
			"api_key_set":         w.cfg.APIKey != "",
			"max_history":         w.cfg.MaxHistory,
			"debounce_ms":         w.cfg.DebounceMs,
			"debounce_max_ms":     w.cfg.DebounceMaxMs,
			"session_timeout_min": w.cfg.SessionTimeoutMin,
			"context_budget":      w.cfg.ContextBudget,
			"debug":               w.cfg.Debug,
			// MCP Server
			"mcp_server_enabled": w.cfg.MCPServerEnabled,
			"mcp_server_token":   w.cfg.MCPServerToken,
			// General
			"timezone":          w.cfg.Timezone,
			"response_mode":     w.cfg.ResponseMode,
			"groups_enabled":    w.cfg.GroupsEnabled,
			"group_list":        w.cfg.GroupList,
			"whatsapp_agent_id": w.cfg.WhatsAppAgentID,
		}
		w.cfg.Mu.RUnlock()
		if w.tools != nil {
			resp["tools_list"] = w.tools.ListToolInfo()
		}
		jsonResponse(rw, resp)
		return
	}

	if r.Method != "POST" {
		jsonError(rw, "Method not allowed", 405)
		return
	}

	if !auth.RequireAdmin(rw, r) {
		return
	}

	var req map[string]any
	if !requireJSON(rw, r) {
		return
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(rw, "Invalid JSON", 400)
		return
	}

	// Update fields and collect changed values for logging in one pass
	w.cfg.Mu.Lock()
	changed := map[string]any{}
	if v, ok := req["base_url"].(string); ok && v != w.cfg.BaseURL {
		w.cfg.BaseURL = v
		changed["base_url"] = v
	}
	if v, ok := req["api_key"].(string); ok && v != "" && v != w.cfg.APIKey {
		w.cfg.APIKey = v
		masked := "***"
		if len(v) > 8 {
			masked = v[:4] + "..." + v[len(v)-4:]
		}
		changed["api_key"] = masked
	}
	if v, ok := req["max_history"].(float64); ok {
		w.cfg.MaxHistory = int(v)
		changed["max_history"] = int(v)
	}
	if v, ok := req["debounce_ms"].(float64); ok {
		w.cfg.DebounceMs = int(v)
		changed["debounce_ms"] = int(v)
	}
	if v, ok := req["debounce_max_ms"].(float64); ok {
		w.cfg.DebounceMaxMs = int(v)
		changed["debounce_max_ms"] = int(v)
	}
	if v, ok := req["session_timeout_min"].(float64); ok {
		w.cfg.SessionTimeoutMin = int(v)
		changed["session_timeout_min"] = int(v)
	}
	if v, ok := req["context_budget"].(float64); ok {
		w.cfg.ContextBudget = int(v)
		changed["context_budget"] = int(v)
	}
	if v, ok := req["debug"].(bool); ok {
		w.cfg.Debug = v
		changed["debug"] = v
	}
	// MCP Server
	if v, ok := req["mcp_server_enabled"].(bool); ok {
		w.cfg.MCPServerEnabled = v
		changed["mcp_server_enabled"] = v
	}
	if v, ok := req["mcp_server_token"].(string); ok {
		w.cfg.MCPServerToken = v
		changed["mcp_server_token"] = "***"
	}
	// General
	if v, ok := req["timezone"].(string); ok && v != "" {
		w.cfg.Timezone = v
		changed["timezone"] = v
	}
	// Response mode
	if v, ok := req["response_mode"].(string); ok && (v == "all" || v == "owner" || v == "contacts") {
		w.cfg.ResponseMode = v
		changed["response_mode"] = v
	}
	// Groups
	if v, ok := req["groups_enabled"].(bool); ok {
		w.cfg.GroupsEnabled = v
		changed["groups_enabled"] = v
	}
	if v, ok := req["group_list"].(string); ok {
		w.cfg.GroupList = v
		changed["group_list"] = v
	}
	if v, ok := req["whatsapp_agent_id"].(float64); ok {
		w.cfg.WhatsAppAgentID = int(v)
		changed["whatsapp_agent_id"] = int(v)
	}

	// Update AI client if credentials changed
	baseURL := w.cfg.BaseURL
	apiKey := w.cfg.APIKey
	debounceMs := w.cfg.DebounceMs
	debounceMaxMs := w.cfg.DebounceMaxMs
	w.cfg.Mu.Unlock()

	if _, ok := changed["base_url"]; ok {
		w.ai.UpdateClient(baseURL, apiKey)
	} else if _, ok := changed["api_key"]; ok {
		w.ai.UpdateClient(baseURL, apiKey)
	}

	// Update debouncer timings
	if _, ok := changed["debounce_ms"]; ok {
		w.debouncer.UpdateTimings(time.Duration(debounceMs)*time.Millisecond, time.Duration(debounceMaxMs)*time.Millisecond)
	} else if _, ok := changed["debounce_max_ms"]; ok {
		w.debouncer.UpdateTimings(time.Duration(debounceMs)*time.Millisecond, time.Duration(debounceMaxMs)*time.Millisecond)
	}

	if err := w.saveCfg(); err != nil {
		jsonError(rw, "Save error: "+err.Error(), 500)
		return
	}

	// Build log data — values already collected in changed map
	changedFields := make([]string, 0, len(changed))
	for k := range changed {
		changedFields = append(changedFields, k)
	}
	logData := map[string]any{"changed_fields": changedFields}
	for k, v := range changed {
		logData[k] = v
	}
	w.logger.Log("web_config_saved", "", logData)

	jsonResponse(rw, map[string]any{"ok": true, "changed": changedFields})
}

func (w *Web) handleTestAI(rw http.ResponseWriter, r *http.Request) {
	if !requirePOST(rw, r) {
		return
	}
	model := "gpt-4.1-mini"
	if da := w.tools.GetDefaultAgent(); da != nil {
		model = da.Model
	}

	resp, err := w.ai.Reply("", model, 50, "Respond with exactly: OK", "", "", "", nil, "test")
	if err != nil {
		jsonResponse(rw, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(rw, map[string]any{"ok": true, "response": resp, "model": model})
}

func (w *Web) handleContacts(rw http.ResponseWriter, r *http.Request) {
	if !requireGET(rw, r) {
		return
	}
	if r.URL.Query().Get("page") != "" {
		page, limit, offset := parsePagination(r, 50)
		contacts, total, err := w.memory.GetContactsPaginated(limit, offset)
		if err != nil {
			jsonError(rw, err.Error(), 500)
			return
		}
		if contacts == nil {
			contacts = []types.Contact{}
		}
		paginatedResponse(rw, contacts, total, page, limit)
		return
	}
	contacts, err := w.memory.GetContacts()
	if err != nil {
		jsonError(rw, err.Error(), 500)
		return
	}
	if contacts == nil {
		contacts = []types.Contact{}
	}
	jsonResponse(rw, contacts)
}

func (w *Web) handleMessages(rw http.ResponseWriter, r *http.Request) {
	if !requireGET(rw, r) {
		return
	}
	chatID := r.URL.Query().Get("chat_id")
	if chatID == "" {
		jsonError(rw, "chat_id required", 400)
		return
	}

	if r.URL.Query().Get("page") != "" {
		page, limit, offset := parsePagination(r, 100)
		messages, total, err := w.memory.GetAllMessagesPaginated(chatID, limit, offset)
		if err != nil {
			jsonError(rw, err.Error(), 500)
			return
		}
		if messages == nil {
			messages = []types.Message{}
		}
		summaries, _ := w.memory.GetSummaries(chatID)
		if summaries == nil {
			summaries = []types.Summary{}
		}
		totalPages := int(total) / limit
		if int(total)%limit != 0 {
			totalPages++
		}
		jsonResponse(rw, map[string]any{
			"data":        messages,
			"summaries":   summaries,
			"total":       total,
			"page":        page,
			"limit":       limit,
			"total_pages": totalPages,
		})
		return
	}

	messages, err := w.memory.GetAllMessages(chatID)
	if err != nil {
		jsonError(rw, err.Error(), 500)
		return
	}

	summaries, err := w.memory.GetSummaries(chatID)
	if err != nil {
		jsonError(rw, err.Error(), 500)
		return
	}

	if messages == nil {
		messages = []types.Message{}
	}
	if summaries == nil {
		summaries = []types.Summary{}
	}

	jsonResponse(rw, map[string]any{
		"messages":  messages,
		"summaries": summaries,
	})
}

func (w *Web) handleAPILogs(rw http.ResponseWriter, r *http.Request) {
	if r.Method == "DELETE" {
		if !auth.RequireAdmin(rw, r) {
			return
		}
		w.logger.DeleteAllLogs()
		jsonResponse(rw, map[string]any{"ok": true})
		return
	}
	if r.Method != "GET" {
		jsonError(rw, "Method not allowed", 405)
		return
	}

	filter := types.LogFilter{
		Event:  r.URL.Query().Get("event"),
		ChatID: r.URL.Query().Get("chat_id"),
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		filter.Limit, _ = strconv.Atoi(l)
	}
	if s := r.URL.Query().Get("since"); s != "" {
		filter.Since, _ = strconv.ParseInt(s, 10, 64)
	}

	logs, err := w.logger.GetLogs(filter)
	if err != nil {
		jsonError(rw, err.Error(), 500)
		return
	}
	if logs == nil {
		logs = []types.LogEntry{}
	}
	jsonResponse(rw, logs)
}

func (w *Web) handleConhecimento(rw http.ResponseWriter, r *http.Request) {
	serveTemplate(rw, "templates/conhecimento.html")
}

func (w *Web) handleKnowledgeTags(rw http.ResponseWriter, r *http.Request) {
	if !requireGET(rw, r) {
		return
	}
	entries, err := w.rag.ListEntries()
	if err != nil {
		jsonError(rw, err.Error(), 500)
		return
	}
	seen := map[string]bool{}
	var tags []string
	for _, e := range entries {
		for _, t := range strings.Split(e.Tags, ",") {
			t = strings.TrimSpace(t)
			if t != "" && !seen[t] {
				seen[t] = true
				tags = append(tags, t)
			}
		}
	}
	sort.Strings(tags)
	jsonResponse(rw, tags)
}

func (w *Web) handleKnowledge(rw http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		if r.URL.Query().Get("page") != "" {
			page, limit, offset := parsePagination(r, 50)
			entries, total, err := w.rag.ListEntriesPaginated(limit, offset)
			if err != nil {
				jsonError(rw, err.Error(), 500)
				return
			}
			if entries == nil {
				entries = []types.KnowledgeEntry{}
			}
			paginatedResponse(rw, entries, total, page, limit)
			return
		}
		entries, err := w.rag.ListEntries()
		if err != nil {
			jsonError(rw, err.Error(), 500)
			return
		}
		if entries == nil {
			entries = []types.KnowledgeEntry{}
		}
		jsonResponse(rw, entries)

	case "POST":
		var req struct {
			Title   string `json:"title"`
			Content string `json:"content"`
			Tags    string `json:"tags"`
		}
		if !requireJSON(rw, r) {
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(rw, "Invalid JSON", 400)
			return
		}
		if req.Title == "" || req.Content == "" {
			jsonError(rw, "title and content required", 400)
			return
		}
		id, err := w.rag.AddEntry(req.Title, req.Content, req.Tags)
		if err != nil {
			jsonError(rw, err.Error(), 500)
			return
		}
		w.logger.Log("web_knowledge_created", "", map[string]any{"id": id, "title": req.Title})
		jsonCreated(rw, map[string]any{"ok": true, "id": id})

	default:
		jsonError(rw, "Method not allowed", 405)
	}
}

func (w *Web) handleKnowledgeByID(rw http.ResponseWriter, r *http.Request) {
	// Extract path after /api/knowledge/
	rest := strings.TrimPrefix(r.URL.Path, "/api/knowledge/")

	// Check for /api/knowledge/{id}/embed
	if strings.HasSuffix(rest, "/embed") {
		idStr := strings.TrimSuffix(rest, "/embed")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id <= 0 {
			jsonError(rw, "invalid id", 400)
			return
		}
		if !requirePOST(rw, r) {
			return
		}
		w.cfg.Mu.RLock()
		apiKey := w.cfg.APIKey
		w.cfg.Mu.RUnlock()
		embModel := ""
		if da := w.tools.GetDefaultAgent(); da != nil {
			embModel = da.RAGEmbeddingModel
		}
		if apiKey == "" {
			jsonError(rw, "API key not configured", 400)
			return
		}
		if err := w.rag.EmbedEntry(w.ai, embModel, id); err != nil {
			jsonError(rw, err.Error(), 500)
			return
		}
		jsonResponse(rw, map[string]any{"ok": true})
		return
	}

	// Check for /api/knowledge/{id}/process
	if strings.HasSuffix(rest, "/process") {
		idStr := strings.TrimSuffix(rest, "/process")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id <= 0 {
			jsonError(rw, "invalid id", 400)
			return
		}
		if !requirePOST(rw, r) {
			return
		}
		w.cfg.Mu.RLock()
		apiKey := w.cfg.APIKey
		w.cfg.Mu.RUnlock()
		model := "gpt-4.1-mini"
		embModel := ""
		if da := w.tools.GetDefaultAgent(); da != nil {
			model = da.Model
			embModel = da.RAGEmbeddingModel
		}
		if apiKey == "" {
			jsonError(rw, "API key not configured", 400)
			return
		}
		compressed, err := w.rag.ProcessEntry(w.ai, model, embModel, id)
		if err != nil && compressed == "" {
			jsonError(rw, err.Error(), 500)
			return
		}
		w.logger.Log("web_knowledge_processed", "", map[string]any{"id": id, "compressed_tokens": len(compressed) / 4})
		resp := map[string]any{
			"ok":                true,
			"compressed":        compressed,
			"tokens_compressed": len(compressed) / 4,
		}
		if err != nil {
			resp["warning"] = err.Error()
		}
		jsonResponse(rw, resp)
		return
	}

	// Check for /api/knowledge/{id}/compress
	if strings.HasSuffix(rest, "/compress") {
		idStr := strings.TrimSuffix(rest, "/compress")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id <= 0 {
			jsonError(rw, "invalid id", 400)
			return
		}
		if !requirePOST(rw, r) {
			return
		}
		w.cfg.Mu.RLock()
		apiKey := w.cfg.APIKey
		w.cfg.Mu.RUnlock()
		model := "gpt-4.1-mini"
		if da := w.tools.GetDefaultAgent(); da != nil {
			model = da.Model
		}
		if apiKey == "" {
			jsonError(rw, "API key not configured", 400)
			return
		}
		compressed, err := w.rag.CompressEntry(w.ai, model, id)
		if err != nil {
			jsonError(rw, err.Error(), 500)
			return
		}
		jsonResponse(rw, map[string]any{
			"ok":                true,
			"compressed":        compressed,
			"tokens_original":   0, // placeholder
			"tokens_compressed": len(compressed) / 4,
		})
		return
	}

	idStr := rest
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		jsonError(rw, "invalid id", 400)
		return
	}

	switch r.Method {
	case "PUT":
		var req struct {
			Title   string `json:"title"`
			Content string `json:"content"`
			Tags    string `json:"tags"`
			Enabled bool   `json:"enabled"`
		}
		if !requireJSON(rw, r) {
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(rw, "Invalid JSON", 400)
			return
		}
		if err := w.rag.UpdateEntry(id, req.Title, req.Content, req.Tags, req.Enabled); err != nil {
			jsonError(rw, err.Error(), 500)
			return
		}
		w.logger.Log("web_knowledge_updated", "", map[string]any{"id": id, "title": req.Title})
		jsonResponse(rw, map[string]any{"ok": true})

	case "DELETE":
		if err := w.rag.DeleteEntry(id); err != nil {
			jsonError(rw, err.Error(), 500)
			return
		}
		w.logger.Log("web_knowledge_deleted", "", map[string]any{"id": id})
		jsonResponse(rw, map[string]any{"ok": true})

	default:
		jsonError(rw, "Method not allowed", 405)
	}
}

func (w *Web) handleKnowledgeCompressAll(rw http.ResponseWriter, r *http.Request) {
	if !requirePOST(rw, r) {
		return
	}
	w.cfg.Mu.RLock()
	apiKey := w.cfg.APIKey
	w.cfg.Mu.RUnlock()
	model := "gpt-4.1-mini"
	if da := w.tools.GetDefaultAgent(); da != nil {
		model = da.Model
	}
	if apiKey == "" {
		jsonError(rw, "API key not configured", 400)
		return
	}
	count, err := w.rag.CompressAllEntries(w.ai, model)
	if err != nil {
		jsonError(rw, fmt.Sprintf("compressed %d, error: %s", count, err.Error()), 500)
		return
	}
	w.logger.Log("web_knowledge_bulk_op", "", map[string]any{"op": "compress", "count": count})
	jsonResponse(rw, map[string]any{"ok": true, "compressed": count})
}

func (w *Web) handleKnowledgeEmbedAll(rw http.ResponseWriter, r *http.Request) {
	if !requirePOST(rw, r) {
		return
	}
	w.cfg.Mu.RLock()
	apiKey := w.cfg.APIKey
	w.cfg.Mu.RUnlock()
	embModel := ""
	if da := w.tools.GetDefaultAgent(); da != nil {
		embModel = da.RAGEmbeddingModel
	}
	if apiKey == "" {
		jsonError(rw, "API key not configured", 400)
		return
	}
	count, err := w.rag.EmbedAllEntries(w.ai, embModel)
	if err != nil {
		jsonError(rw, fmt.Sprintf("embedded %d, error: %s", count, err.Error()), 500)
		return
	}
	w.logger.Log("web_knowledge_bulk_op", "", map[string]any{"op": "embed", "count": count})
	jsonResponse(rw, map[string]any{"ok": true, "embedded": count})
}

func (w *Web) handleKnowledgeProcessAll(rw http.ResponseWriter, r *http.Request) {
	if !requirePOST(rw, r) {
		return
	}
	w.cfg.Mu.RLock()
	apiKey := w.cfg.APIKey
	w.cfg.Mu.RUnlock()
	model := "gpt-4.1-mini"
	embModel := ""
	if da := w.tools.GetDefaultAgent(); da != nil {
		model = da.Model
		embModel = da.RAGEmbeddingModel
	}
	if apiKey == "" {
		jsonError(rw, "API key not configured", 400)
		return
	}
	compressed, embedded, err := w.rag.ProcessAllEntries(w.ai, model, embModel)
	if err != nil {
		jsonError(rw, fmt.Sprintf("compressed %d, embedded %d, error: %s", compressed, embedded, err.Error()), 500)
		return
	}
	w.logger.Log("web_knowledge_bulk_op", "", map[string]any{"op": "process", "compressed": compressed, "embedded": embedded})
	jsonResponse(rw, map[string]any{"ok": true, "compressed": compressed, "embedded": embedded})
}

func (w *Web) handleKnowledgeStripHTML(rw http.ResponseWriter, r *http.Request) {
	if !requirePOST(rw, r) {
		return
	}
	count, err := w.rag.StripHTMLAll()
	if err != nil {
		jsonError(rw, fmt.Sprintf("cleaned %d, error: %s", count, err.Error()), 500)
		return
	}
	w.logger.Log("web_knowledge_bulk_op", "", map[string]any{"op": "strip_html", "count": count})
	jsonResponse(rw, map[string]any{"ok": true, "cleaned": count})
}

func (w *Web) handleChatPage(rw http.ResponseWriter, r *http.Request) {
	serveTemplate(rw, "templates/chat.html")
}

// handleChat processes a message through the central pipeline and returns JSON.
func (w *Web) handleChat(rw http.ResponseWriter, r *http.Request) {
	if !requirePOST(rw, r) {
		return
	}

	var req struct {
		Message string `json:"message"`
		AgentID int64  `json:"agent_id"`
	}
	if !requireJSON(rw, r) {
		return
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(rw, "Invalid JSON", 400)
		return
	}
	if req.Message == "" {
		jsonError(rw, "message required", 400)
		return
	}

	const chatID = "webchat"
	result := w.pipe.ProcessWithAgent(chatID, req.Message, "", req.AgentID)

	if result.Error != nil {
		jsonResponse(rw, map[string]any{
			"response": "Erro ao processar mensagem.", "chat_id": chatID, "blocked": false, "reason": "ai_error",
		})
		return
	}

	jsonResponse(rw, map[string]any{
		"response": result.Response, "chat_id": chatID, "blocked": result.Blocked, "reason": result.Reason,
	})
}

func (w *Web) handleChatClear(rw http.ResponseWriter, r *http.Request) {
	if !requirePOST(rw, r) {
		return
	}
	if err := w.memory.DeleteMessages("webchat"); err != nil {
		jsonError(rw, err.Error(), 500)
		return
	}
	w.logger.DeleteLogs("webchat")
	if w.tools != nil {
		w.tools.DeleteTasks("webchat")
		w.tools.DeleteNotes("webchat")
		w.tools.DeleteScheduledMessages("webchat")
	}
	w.logger.Log("web_chat_cleared", "", map[string]any{"chat_id": "webchat"})
	jsonResponse(rw, map[string]any{"ok": true})
}

func (w *Web) handleNotes(rw http.ResponseWriter, r *http.Request) {
	if !requireGET(rw, r) {
		return
	}
	chatID := r.URL.Query().Get("chat_id")
	if chatID == "" {
		jsonError(rw, "chat_id required", 400)
		return
	}
	if w.tools == nil {
		jsonResponse(rw, []any{})
		return
	}
	db := w.tools.GetDB()
	rows, err := db.Query("SELECT key, value, created_at FROM notes WHERE chat_id = ? ORDER BY key", chatID)
	if err != nil {
		jsonError(rw, err.Error(), 500)
		return
	}
	defer rows.Close()

	type note struct {
		Key       string `json:"key"`
		Value     string `json:"value"`
		CreatedAt int64  `json:"created_at"`
	}
	var notes []note
	for rows.Next() {
		var n note
		if err := rows.Scan(&n.Key, &n.Value, &n.CreatedAt); err != nil {
			jsonError(rw, err.Error(), 500)
			return
		}
		notes = append(notes, n)
	}
	if notes == nil {
		notes = []note{}
	}
	jsonResponse(rw, notes)
}

// --- Custom Tools API ---

func (w *Web) handleCustomTools(rw http.ResponseWriter, r *http.Request) {
	db := w.tools.GetDB()
	switch r.Method {
	case "GET":
		rows, err := db.Query("SELECT id, name, description, method, url_template, headers, body_template, parameters, response_path, max_bytes, enabled, created_at FROM custom_tools ORDER BY name")
		if err != nil {
			jsonError(rw, err.Error(), 500)
			return
		}
		defer rows.Close()

		var cts []types.CustomTool
		for rows.Next() {
			var ct types.CustomTool
			if err := rows.Scan(&ct.ID, &ct.Name, &ct.Description, &ct.Method, &ct.URLTemplate, &ct.Headers, &ct.BodyTemplate, &ct.Parameters, &ct.ResponsePath, &ct.MaxBytes, &ct.Enabled, &ct.CreatedAt); err != nil {
				jsonError(rw, err.Error(), 500)
				return
			}
			cts = append(cts, ct)
		}
		if cts == nil {
			cts = []types.CustomTool{}
		}
		jsonResponse(rw, cts)

	case "POST":
		if !auth.RequireAdmin(rw, r) {
			return
		}
		var ct types.CustomTool
		if !requireJSON(rw, r) {
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&ct); err != nil {
			jsonError(rw, "Invalid JSON", 400)
			return
		}

		if err := tools.ValidateCustomTool(ct, w.tools.GetBuiltinNames()); err != nil {
			jsonError(rw, err.Error(), 400)
			return
		}

		// Check max 20 custom tools
		var count int
		db.QueryRow("SELECT COUNT(*) FROM custom_tools").Scan(&count)
		if count >= 20 {
			jsonError(rw, "Limite de 20 APIs personalizadas atingido", 400)
			return
		}

		res, err := db.Exec(
			"INSERT INTO custom_tools (name, description, method, url_template, headers, body_template, parameters, response_path, max_bytes, enabled) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			ct.Name, ct.Description, ct.Method, ct.URLTemplate, ct.Headers, ct.BodyTemplate, ct.Parameters, ct.ResponsePath, ct.MaxBytes, 1)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				jsonError(rw, "Nome ja existe", 400)
				return
			}
			jsonError(rw, err.Error(), 500)
			return
		}
		id, _ := res.LastInsertId()
		w.tools.ReloadCustomTools()
		w.logger.Log("web_custom_tool_created", "", map[string]any{"id": id, "name": ct.Name})
		jsonCreated(rw, map[string]any{"ok": true, "id": id})

	default:
		jsonError(rw, "Method not allowed", 405)
	}
}

func (w *Web) handleCustomToolByID(rw http.ResponseWriter, r *http.Request) {
	db := w.tools.GetDB()
	rest := strings.TrimPrefix(r.URL.Path, "/api/custom-tools/")

	// Check for /api/custom-tools/{id}/test
	if strings.HasSuffix(rest, "/test") {
		idStr := strings.TrimSuffix(rest, "/test")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id <= 0 {
			jsonError(rw, "invalid id", 400)
			return
		}
		if !requirePOST(rw, r) {
			return
		}

		// Get tool from DB
		var ct types.CustomTool
		err = db.QueryRow("SELECT id, name, description, method, url_template, headers, body_template, parameters, response_path, max_bytes FROM custom_tools WHERE id = ?", id).
			Scan(&ct.ID, &ct.Name, &ct.Description, &ct.Method, &ct.URLTemplate, &ct.Headers, &ct.BodyTemplate, &ct.Parameters, &ct.ResponsePath, &ct.MaxBytes)
		if err != nil {
			jsonError(rw, "Tool nao encontrada", 404)
			return
		}

		// Read test args from body
		var testReq struct {
			Args string `json:"args"`
		}
		json.NewDecoder(r.Body).Decode(&testReq)
		if testReq.Args == "" {
			testReq.Args = "{}"
		}

		executor := tools.MakeCustomExecutor(ct)
		result, err := executor("test", testReq.Args)
		if err != nil {
			jsonResponse(rw, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		jsonResponse(rw, map[string]any{"ok": true, "result": result})
		return
	}

	idStr := rest
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		jsonError(rw, "invalid id", 400)
		return
	}

	switch r.Method {
	case "PUT":
		if !auth.RequireAdmin(rw, r) {
			return
		}
		var ct types.CustomTool
		if !requireJSON(rw, r) {
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&ct); err != nil {
			jsonError(rw, "Invalid JSON", 400)
			return
		}

		if err := tools.ValidateCustomTool(ct, w.tools.GetBuiltinNames()); err != nil {
			jsonError(rw, err.Error(), 400)
			return
		}

		enabled := 1
		if ct.Enabled == 0 {
			enabled = 0
		}

		_, err := db.Exec(
			"UPDATE custom_tools SET name=?, description=?, method=?, url_template=?, headers=?, body_template=?, parameters=?, response_path=?, max_bytes=?, enabled=? WHERE id=?",
			ct.Name, ct.Description, ct.Method, ct.URLTemplate, ct.Headers, ct.BodyTemplate, ct.Parameters, ct.ResponsePath, ct.MaxBytes, enabled, id)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				jsonError(rw, "Nome ja existe", 400)
				return
			}
			jsonError(rw, err.Error(), 500)
			return
		}
		w.tools.ReloadCustomTools()
		w.logger.Log("web_custom_tool_updated", "", map[string]any{"id": id, "name": ct.Name})
		jsonResponse(rw, map[string]any{"ok": true})

	case "DELETE":
		if !auth.RequireAdmin(rw, r) {
			return
		}
		// Get name before deletion for logging
		var deletedName string
		db.QueryRow("SELECT name FROM custom_tools WHERE id = ?", id).Scan(&deletedName)

		_, err := db.Exec("DELETE FROM custom_tools WHERE id = ?", id)
		if err != nil {
			jsonError(rw, err.Error(), 500)
			return
		}
		w.tools.ReloadCustomTools()
		w.logger.Log("web_custom_tool_deleted", "", map[string]any{"id": id, "name": deletedName})
		jsonResponse(rw, map[string]any{"ok": true})

	default:
		jsonError(rw, "Method not allowed", 405)
	}
}

// --- MCP Servers API ---

func (w *Web) handleMCPServers(rw http.ResponseWriter, r *http.Request) {
	db := w.tools.GetDB()
	switch r.Method {
	case "GET":
		rows, err := db.Query("SELECT id, name, url, api_key, enabled, created_at FROM mcp_servers ORDER BY name")
		if err != nil {
			jsonError(rw, err.Error(), 500)
			return
		}
		defer rows.Close()

		type serverInfo struct {
			ID        int64            `json:"id"`
			Name      string           `json:"name"`
			URL       string           `json:"url"`
			HasAPIKey bool             `json:"has_api_key"`
			Enabled   bool             `json:"enabled"`
			Connected bool             `json:"connected"`
			Tools     []types.ToolInfo `json:"tools"`
			CreatedAt int64            `json:"created_at"`
		}

		// Build a map of MCP client infos by ID for lookup
		mcpInfos := w.tools.GetMCPClientInfos()
		mcpMap := make(map[int64]tools.MCPClientInfo, len(mcpInfos))
		for _, info := range mcpInfos {
			mcpMap[info.ID] = info
		}

		var servers []serverInfo
		for rows.Next() {
			var id int64
			var name, mcpURL, apiKey string
			var enabled int
			var createdAt int64
			if err := rows.Scan(&id, &name, &mcpURL, &apiKey, &enabled, &createdAt); err != nil {
				jsonError(rw, err.Error(), 500)
				return
			}

			si := serverInfo{
				ID:        id,
				Name:      name,
				URL:       mcpURL,
				HasAPIKey: apiKey != "",
				Enabled:   enabled == 1,
				CreatedAt: createdAt,
			}

			// Find matching MCPClient to get status and tools
			if info, ok := mcpMap[id]; ok {
				si.Connected = info.Connected
				for _, t := range info.Tools {
					si.Tools = append(si.Tools, types.ToolInfo{
						Name:        t.FullName,
						Description: t.Definition.Function.Description,
					})
				}
			}

			if si.Tools == nil {
				si.Tools = []types.ToolInfo{}
			}
			servers = append(servers, si)
		}
		if servers == nil {
			servers = []serverInfo{}
		}
		jsonResponse(rw, servers)

	case "POST":
		if !auth.RequireAdmin(rw, r) {
			return
		}
		var req struct {
			Name   string `json:"name"`
			URL    string `json:"url"`
			APIKey string `json:"api_key"`
		}
		if !requireJSON(rw, r) {
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(rw, "Invalid JSON", 400)
			return
		}
		if req.Name == "" || req.URL == "" {
			jsonError(rw, "name e url obrigatorios", 400)
			return
		}
		if !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") {
			jsonError(rw, "URL deve comecar com http:// ou https://", 400)
			return
		}

		// Max 10 MCP servers
		var count int
		db.QueryRow("SELECT COUNT(*) FROM mcp_servers").Scan(&count)
		if count >= 10 {
			jsonError(rw, "Limite de 10 servidores MCP atingido", 400)
			return
		}

		res, err := db.Exec("INSERT INTO mcp_servers (name, url, api_key) VALUES (?, ?, ?)", req.Name, req.URL, req.APIKey)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				jsonError(rw, "Nome ja existe", 400)
				return
			}
			jsonError(rw, err.Error(), 500)
			return
		}
		id, _ := res.LastInsertId()

		// Auto-connect the new server
		connectErr := w.tools.ConnectMCPServer(id)
		connected := connectErr == nil
		errMsg := ""
		if connectErr != nil {
			errMsg = connectErr.Error()
		}

		w.logger.Log("web_mcp_server_created", "", map[string]any{"id": id, "name": req.Name, "url": req.URL})
		jsonCreated(rw, map[string]any{"ok": true, "id": id, "connected": connected, "error": errMsg})

	default:
		jsonError(rw, "Method not allowed", 405)
	}
}

func (w *Web) handleMCPServerByID(rw http.ResponseWriter, r *http.Request) {
	db := w.tools.GetDB()
	rest := strings.TrimPrefix(r.URL.Path, "/api/mcp-servers/")

	// Check for /api/mcp-servers/{id}/reconnect
	if strings.HasSuffix(rest, "/reconnect") {
		idStr := strings.TrimSuffix(rest, "/reconnect")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id <= 0 {
			jsonError(rw, "invalid id", 400)
			return
		}
		if !requirePOST(rw, r) {
			return
		}
		if !auth.RequireAdmin(rw, r) {
			return
		}

		w.tools.DisconnectMCPServer(id)
		connectErr := w.tools.ConnectMCPServer(id)
		if connectErr != nil {
			jsonResponse(rw, map[string]any{"ok": false, "error": connectErr.Error()})
			return
		}
		jsonResponse(rw, map[string]any{"ok": true})
		return
	}

	idStr := rest
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		jsonError(rw, "invalid id", 400)
		return
	}

	switch r.Method {
	case "PUT":
		if !auth.RequireAdmin(rw, r) {
			return
		}
		var req struct {
			Name   string `json:"name"`
			URL    string `json:"url"`
			APIKey string `json:"api_key"`
			Enable *bool  `json:"enabled"`
		}
		if !requireJSON(rw, r) {
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(rw, "Invalid JSON", 400)
			return
		}
		if req.Name == "" || req.URL == "" {
			jsonError(rw, "name e url obrigatorios", 400)
			return
		}

		enabled := 1
		if req.Enable != nil && !*req.Enable {
			enabled = 0
		}

		_, err := db.Exec("UPDATE mcp_servers SET name=?, url=?, api_key=?, enabled=? WHERE id=?",
			req.Name, req.URL, req.APIKey, enabled, id)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				jsonError(rw, "Nome ja existe", 400)
				return
			}
			jsonError(rw, err.Error(), 500)
			return
		}

		// Reconnect if enabled, disconnect if not
		w.tools.DisconnectMCPServer(id)
		if enabled == 1 {
			w.tools.ConnectMCPServer(id)
		}

		w.logger.Log("web_mcp_server_updated", "", map[string]any{"id": id, "name": req.Name})
		jsonResponse(rw, map[string]any{"ok": true})

	case "DELETE":
		if !auth.RequireAdmin(rw, r) {
			return
		}
		// Get name before deletion for logging
		var deletedName string
		db.QueryRow("SELECT name FROM mcp_servers WHERE id = ?", id).Scan(&deletedName)

		w.tools.DisconnectMCPServer(id)
		_, err := db.Exec("DELETE FROM mcp_servers WHERE id = ?", id)
		if err != nil {
			jsonError(rw, err.Error(), 500)
			return
		}
		w.logger.Log("web_mcp_server_deleted", "", map[string]any{"id": id, "name": deletedName})
		jsonResponse(rw, map[string]any{"ok": true})

	default:
		jsonError(rw, "Method not allowed", 405)
	}
}

// --- External Databases API ---

func (w *Web) handleExtDatabases(rw http.ResponseWriter, r *http.Request) {
	db := w.tools.GetDB()
	switch r.Method {
	case "GET":
		rows, err := db.Query("SELECT id, name, driver, host, port, username, password, dbname, ssl_mode, max_rows, enabled, created_at FROM external_databases ORDER BY name")
		if err != nil {
			jsonError(rw, err.Error(), 500)
			return
		}
		defer rows.Close()

		type dbInfo struct {
			ID          int64  `json:"id"`
			Name        string `json:"name"`
			Driver      string `json:"driver"`
			Host        string `json:"host"`
			Port        int    `json:"port"`
			Username    string `json:"username"`
			HasPassword bool   `json:"has_password"`
			DBName      string `json:"dbname"`
			SSLMode     string `json:"ssl_mode"`
			MaxRows     int    `json:"max_rows"`
			Enabled     bool   `json:"enabled"`
			Connected   bool   `json:"connected"`
			CreatedAt   int64  `json:"created_at"`
		}

		var dbs []dbInfo
		for rows.Next() {
			var edb types.ExternalDatabase
			if err := rows.Scan(&edb.ID, &edb.Name, &edb.Driver, &edb.Host, &edb.Port, &edb.Username, &edb.Password, &edb.DBName, &edb.SSLMode, &edb.MaxRows, &edb.Enabled, &edb.CreatedAt); err != nil {
				jsonError(rw, err.Error(), 500)
				return
			}

			di := dbInfo{
				ID:          edb.ID,
				Name:        edb.Name,
				Driver:      edb.Driver,
				Host:        edb.Host,
				Port:        edb.Port,
				Username:    edb.Username,
				HasPassword: edb.Password != "",
				DBName:      edb.DBName,
				SSLMode:     edb.SSLMode,
				MaxRows:     edb.MaxRows,
				Enabled:     edb.Enabled == 1,
				Connected:   w.tools.IsExtDBConnected(edb.ID),
				CreatedAt:   edb.CreatedAt,
			}

			dbs = append(dbs, di)
		}
		if dbs == nil {
			dbs = []dbInfo{}
		}
		jsonResponse(rw, dbs)

	case "POST":
		if !auth.RequireAdmin(rw, r) {
			return
		}
		var edb types.ExternalDatabase
		if !requireJSON(rw, r) {
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&edb); err != nil {
			jsonError(rw, "Invalid JSON", 400)
			return
		}

		if err := tools.ValidateExternalDatabase(edb); err != nil {
			jsonError(rw, err.Error(), 400)
			return
		}

		// Max 10 external databases
		var count int
		db.QueryRow("SELECT COUNT(*) FROM external_databases").Scan(&count)
		if count >= 10 {
			jsonError(rw, "Limite de 10 bancos externos atingido", 400)
			return
		}

		if edb.MaxRows <= 0 {
			edb.MaxRows = 100
		}

		res, err := db.Exec(
			"INSERT INTO external_databases (name, driver, host, port, username, password, dbname, ssl_mode, max_rows, enabled) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1)",
			edb.Name, edb.Driver, edb.Host, edb.Port, edb.Username, edb.Password, edb.DBName, edb.SSLMode, edb.MaxRows)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				jsonError(rw, "Nome ja existe", 400)
				return
			}
			jsonError(rw, err.Error(), 500)
			return
		}
		id, _ := res.LastInsertId()
		edb.ID = id

		// Auto-connect
		connectErr := w.tools.ConnectExtDB(edb)
		connected := connectErr == nil
		errMsg := ""
		if connectErr != nil {
			errMsg = connectErr.Error()
		}

		w.logger.Log("web_extdb_created", "", map[string]any{"id": id, "name": edb.Name, "driver": edb.Driver})
		jsonCreated(rw, map[string]any{"ok": true, "id": id, "connected": connected, "error": errMsg})

	default:
		jsonError(rw, "Method not allowed", 405)
	}
}

func (w *Web) handleExtDatabaseByID(rw http.ResponseWriter, r *http.Request) {
	db := w.tools.GetDB()
	rest := strings.TrimPrefix(r.URL.Path, "/api/ext-databases/")

	// POST /api/ext-databases/{id}/test
	if strings.HasSuffix(rest, "/test") {
		idStr := strings.TrimSuffix(rest, "/test")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id <= 0 {
			jsonError(rw, "invalid id", 400)
			return
		}
		if !requirePOST(rw, r) {
			return
		}

		var edb types.ExternalDatabase
		err = db.QueryRow("SELECT id, name, driver, host, port, username, password, dbname, ssl_mode, max_rows FROM external_databases WHERE id = ?", id).
			Scan(&edb.ID, &edb.Name, &edb.Driver, &edb.Host, &edb.Port, &edb.Username, &edb.Password, &edb.DBName, &edb.SSLMode, &edb.MaxRows)
		if err != nil {
			jsonError(rw, "Banco nao encontrado", 404)
			return
		}

		dsn := tools.BuildDSN(edb)
		tmpDB, err := sql.Open(edb.Driver, dsn)
		if err != nil {
			jsonResponse(rw, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		defer tmpDB.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tmpDB.PingContext(ctx); err != nil {
			jsonResponse(rw, map[string]any{"ok": false, "error": "ping: " + err.Error()})
			return
		}

		// Count tables
		var countQuery string
		switch edb.Driver {
		case "mysql":
			countQuery = "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE()"
		case "postgres":
			countQuery = "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public'"
		}
		var tableCount int
		if err := tmpDB.QueryRowContext(ctx, countQuery).Scan(&tableCount); err != nil {
			tableCount = -1
		}

		jsonResponse(rw, map[string]any{"ok": true, "tables": tableCount, "message": fmt.Sprintf("Conexao OK! %d tabelas encontradas.", tableCount)})
		return
	}

	// POST /api/ext-databases/{id}/schema
	if strings.HasSuffix(rest, "/schema") {
		idStr := strings.TrimSuffix(rest, "/schema")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id <= 0 {
			jsonError(rw, "invalid id", 400)
			return
		}
		if !requirePOST(rw, r) {
			return
		}

		var name string
		err = db.QueryRow("SELECT name FROM external_databases WHERE id = ?", id).Scan(&name)
		if err != nil {
			jsonError(rw, "Banco nao encontrado", 404)
			return
		}

		schema, err := w.tools.GetExtDBSchema(name)
		if err != nil {
			jsonResponse(rw, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		jsonResponse(rw, map[string]any{"ok": true, "schema": schema})
		return
	}

	// POST /api/ext-databases/{id}/reconnect
	if strings.HasSuffix(rest, "/reconnect") {
		idStr := strings.TrimSuffix(rest, "/reconnect")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id <= 0 {
			jsonError(rw, "invalid id", 400)
			return
		}
		if !requirePOST(rw, r) {
			return
		}
		if !auth.RequireAdmin(rw, r) {
			return
		}

		w.tools.DisconnectExtDB(id)

		var edb types.ExternalDatabase
		err = db.QueryRow("SELECT id, name, driver, host, port, username, password, dbname, ssl_mode, max_rows FROM external_databases WHERE id = ?", id).
			Scan(&edb.ID, &edb.Name, &edb.Driver, &edb.Host, &edb.Port, &edb.Username, &edb.Password, &edb.DBName, &edb.SSLMode, &edb.MaxRows)
		if err != nil {
			jsonResponse(rw, map[string]any{"ok": false, "error": "Banco nao encontrado"})
			return
		}

		if connectErr := w.tools.ConnectExtDB(edb); connectErr != nil {
			jsonResponse(rw, map[string]any{"ok": false, "error": connectErr.Error()})
			return
		}
		jsonResponse(rw, map[string]any{"ok": true})
		return
	}

	idStr := rest
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		jsonError(rw, "invalid id", 400)
		return
	}

	switch r.Method {
	case "PUT":
		if !auth.RequireAdmin(rw, r) {
			return
		}
		var edb types.ExternalDatabase
		if !requireJSON(rw, r) {
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&edb); err != nil {
			jsonError(rw, "Invalid JSON", 400)
			return
		}

		if err := tools.ValidateExternalDatabase(edb); err != nil {
			jsonError(rw, err.Error(), 400)
			return
		}

		enabled := 1
		if edb.Enabled == 0 {
			enabled = 0
		}

		// If password is empty, keep existing
		if edb.Password == "" {
			var existingPwd string
			db.QueryRow("SELECT password FROM external_databases WHERE id = ?", id).Scan(&existingPwd)
			edb.Password = existingPwd
		}

		if edb.MaxRows <= 0 {
			edb.MaxRows = 100
		}

		_, err := db.Exec(
			"UPDATE external_databases SET name=?, driver=?, host=?, port=?, username=?, password=?, dbname=?, ssl_mode=?, max_rows=?, enabled=? WHERE id=?",
			edb.Name, edb.Driver, edb.Host, edb.Port, edb.Username, edb.Password, edb.DBName, edb.SSLMode, edb.MaxRows, enabled, id)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				jsonError(rw, "Nome ja existe", 400)
				return
			}
			jsonError(rw, err.Error(), 500)
			return
		}

		// Reconnect if enabled
		w.tools.DisconnectExtDB(id)
		if enabled == 1 {
			edb.ID = id
			w.tools.ConnectExtDB(edb)
		}

		w.logger.Log("web_extdb_updated", "", map[string]any{"id": id, "name": edb.Name})
		jsonResponse(rw, map[string]any{"ok": true})

	case "DELETE":
		if !auth.RequireAdmin(rw, r) {
			return
		}
		// Get name before deletion for logging
		var deletedName string
		db.QueryRow("SELECT name FROM external_databases WHERE id = ?", id).Scan(&deletedName)

		w.tools.DisconnectExtDB(id)
		_, err := db.Exec("DELETE FROM external_databases WHERE id = ?", id)
		if err != nil {
			jsonError(rw, err.Error(), 500)
			return
		}
		w.logger.Log("web_extdb_deleted", "", map[string]any{"id": id, "name": deletedName})
		jsonResponse(rw, map[string]any{"ok": true})

	default:
		jsonError(rw, "Method not allowed", 405)
	}
}

// --- Reports ---

func (w *Web) handleRelatorios(rw http.ResponseWriter, r *http.Request) {
	serveTemplate(rw, "templates/relatorios.html")
}

// reportResponse is the JSON response for /api/report.
type reportResponse struct {
	SQL         string         `json:"sql"`
	Columns     []string       `json:"columns"`
	Rows        [][]string     `json:"rows"`
	ChartType   string         `json:"chart_type"`
	ChartConfig map[string]any `json:"chart_config"`
	Insight     string         `json:"insight"`
	Error       string         `json:"error,omitempty"`
}

func (w *Web) handleReport(rw http.ResponseWriter, r *http.Request) {
	if !requirePOST(rw, r) {
		return
	}

	var req struct {
		Question string `json:"question"`
		Database string `json:"database"`
		AgentID  int64  `json:"agent_id"`
	}
	if !requireJSON(rw, r) {
		return
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(rw, "Invalid JSON", 400)
		return
	}
	if req.Question == "" {
		jsonError(rw, "question required", 400)
		return
	}

	const chatID = "reports"

	w.cfg.Mu.RLock()
	apiKey := w.cfg.APIKey
	timeoutMin := w.cfg.SessionTimeoutMin
	w.cfg.Mu.RUnlock()

	model := "gpt-4.1-mini"
	maxTokens := 1024
	if da := w.tools.GetDefaultAgent(); da != nil {
		model = da.Model
		maxTokens = da.MaxTokens
	}

	// Load agent config if specified
	var agentBaseURL, agentAPIKey, agentSystemPrompt string
	if req.AgentID > 0 && w.tools != nil {
		agent := w.tools.GetAgentByID(req.AgentID)
		if agent != nil && agent.Enabled != 0 {
			if agent.Model != "" {
				model = agent.Model
			}
			if agent.MaxTokens > 0 {
				maxTokens = agent.MaxTokens
			}
			if agent.BaseURL != "" {
				agentBaseURL = agent.BaseURL
			}
			if agent.APIKey != "" {
				agentAPIKey = agent.APIKey
			}
			if agent.SystemPrompt != "" {
				agentSystemPrompt = agent.SystemPrompt
			}
		}
	}

	if apiKey == "" && agentAPIKey == "" {
		jsonResponse(rw, reportResponse{Error: "API key não configurada"})
		return
	}

	// Session management
	if timeoutMin <= 0 {
		timeoutMin = 30
	}
	sessionID, _, _ := w.memory.GetOrCreateSession(chatID, timeoutMin)

	// Load recent history for follow-up context
	var historyContext string
	history, _ := w.memory.GetSessionHistory(chatID, sessionID, 10)
	if len(history) > 0 {
		var sb strings.Builder
		sb.WriteString("Perguntas anteriores:\n")
		for i := 0; i < len(history)-1; i += 2 {
			if history[i].Role == "user" {
				sb.WriteString("- Q: " + history[i].Content + "\n")
				if i+1 < len(history) && history[i+1].Role == "assistant" {
					// Extract insight from JSON
					var parsed struct {
						Insight string `json:"insight"`
					}
					if json.Unmarshal([]byte(history[i+1].Content), &parsed) == nil && parsed.Insight != "" {
						sb.WriteString("  A: " + parsed.Insight + "\n")
					}
				}
			}
		}
		historyContext = sb.String()
	}

	isExternal := req.Database != "" && req.Database != "local"

	// Build schema and SQL prompt based on target database
	var schema, dialect string
	if isExternal {
		extSchema, err := w.tools.GetExtDBSchema(req.Database)
		if err != nil {
			jsonResponse(rw, reportResponse{Error: "Erro ao obter schema: " + err.Error()})
			return
		}
		schema = extSchema
		driver := w.tools.GetExtDBDriver(req.Database)
		if driver == "mysql" {
			dialect = "MySQL"
		} else {
			dialect = "PostgreSQL"
		}
	} else {
		schema = w.tools.GetSchemaDescription()
		dialect = "SQLite"
	}

	// Stage 1: AI generates SQL
	sqlPrompt := fmt.Sprintf("Você é um assistente que gera consultas SQL %s. "+
		"Responda APENAS com a query SQL, sem explicação, sem markdown.\n\n"+
		"Schema:\n%s", dialect, schema)
	if agentSystemPrompt != "" {
		sqlPrompt = agentSystemPrompt + "\n\n" + sqlPrompt
	}

	sqlQuestion := req.Question
	if historyContext != "" {
		sqlQuestion = historyContext + "\nPergunta atual: " + req.Question
	}

	// Use agent-specific client if configured
	aiReply := func(sysPrompt, userMsg string) (string, error) {
		if agentBaseURL != "" || agentAPIKey != "" {
			bURL := agentBaseURL
			aKey := agentAPIKey
			if aKey == "" {
				aKey = apiKey
			}
			return w.ai.ReplyWithClient(chatID, bURL, aKey, model, maxTokens, sysPrompt, "", "", "", nil, userMsg)
		}
		return w.ai.Reply(chatID, model, maxTokens, sysPrompt, "", "", "", nil, userMsg)
	}

	sqlReply, err := aiReply(sqlPrompt, sqlQuestion)
	if err != nil {
		jsonResponse(rw, reportResponse{Error: "Erro ao gerar SQL: " + err.Error()})
		return
	}

	sqlQuery := cleanSQL(sqlReply)
	resp := reportResponse{SQL: sqlQuery}

	// Stage 2: Execute query
	var cols []string
	var rows [][]string
	if isExternal {
		cols, rows, err = w.tools.QueryExtDB(req.Database, sqlQuery, 500)
	} else {
		cols, rows, err = w.tools.QueryReadOnly(sqlQuery, 500)
	}
	if err != nil {
		resp.Error = "Erro na consulta: " + err.Error()
		jsonResponse(rw, resp)
		return
	}
	resp.Columns = cols
	resp.Rows = rows

	if len(rows) == 0 {
		resp.Insight = "Nenhum resultado encontrado."
		resp.ChartType = "none"
		w.saveReportMessages(chatID, sessionID, req.Question, &resp, req.Database)
		jsonResponse(rw, resp)
		return
	}

	// Stage 3: AI interprets results
	dataSummary := formatDataSummary(cols, rows)
	interpPrompt := `Você analisa resultados de consultas SQL e retorna um JSON com:
- "insight": texto curto em português explicando os dados (1-3 frases)
- "chart_type": um de "bar", "line", "pie", "doughnut", "none"
- "chart_config": objeto Chart.js válido com "labels" (array de strings) e "datasets" (array com objetos contendo "label", "data", "backgroundColor")

Responda APENAS com JSON válido, sem markdown.
Se os dados não forem adequados para gráfico, use chart_type "none" e chart_config {}.`
	if agentSystemPrompt != "" {
		interpPrompt = agentSystemPrompt + "\n\n" + interpPrompt
	}

	interpMsg := fmt.Sprintf("Pergunta: %s\n\nResultados:\n%s", req.Question, dataSummary)
	interpReply, err := aiReply(interpPrompt, interpMsg)
	if err != nil {
		resp.Insight = "Dados retornados com sucesso."
		resp.ChartType = "none"
		w.saveReportMessages(chatID, sessionID, req.Question, &resp, req.Database)
		jsonResponse(rw, resp)
		return
	}

	parseInterpretation(interpReply, &resp)
	w.saveReportMessages(chatID, sessionID, req.Question, &resp, req.Database)
	jsonResponse(rw, resp)
}

// saveReportMessages persists user question and assistant response for the reports channel.
func (w *Web) saveReportMessages(chatID string, sessionID int64, question string, resp *reportResponse, database string) {
	w.memory.SaveMessage(chatID, "user", question, sessionID)

	// Build compact JSON (no rows)
	compact := map[string]any{
		"type":       "report",
		"sql":        resp.SQL,
		"insight":    resp.Insight,
		"chart_type": resp.ChartType,
	}
	if resp.ChartConfig != nil {
		compact["chart_config"] = resp.ChartConfig
	}
	if database != "" {
		compact["database"] = database
	}
	if resp.Error != "" {
		compact["error"] = resp.Error
	}
	if b, err := json.Marshal(compact); err == nil {
		w.memory.SaveMessage(chatID, "assistant", string(b), sessionID)
	}
}

func (w *Web) handleReportClear(rw http.ResponseWriter, r *http.Request) {
	if !requirePOST(rw, r) {
		return
	}
	if err := w.memory.DeleteMessages("reports"); err != nil {
		jsonError(rw, err.Error(), 500)
		return
	}
	w.logger.DeleteLogs("reports")
	if w.tools != nil {
		w.tools.DeleteTasks("reports")
		w.tools.DeleteNotes("reports")
		w.tools.DeleteScheduledMessages("reports")
	}
	w.logger.Log("web_report_cleared", "", map[string]any{"chat_id": "reports"})
	jsonResponse(rw, map[string]any{"ok": true})
}

// cleanSQL strips markdown fences from AI-generated SQL.
func cleanSQL(s string) string {
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

// formatDataSummary creates a pipe-delimited text summary (max 50 rows) for the AI.
func formatDataSummary(cols []string, rows [][]string) string {
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

// parseInterpretation extracts JSON from the AI interpretation response.
func parseInterpretation(raw string, resp *reportResponse) {
	raw = strings.TrimSpace(raw)
	// Strip markdown fences if present
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

// --- Agents API ---

func (w *Web) handleAgents(rw http.ResponseWriter, r *http.Request) {
	db := w.tools.GetDB()
	switch r.Method {
	case "GET":
		rows, err := db.Query(`SELECT id, name, description, system_prompt, user_prompt, model, max_tokens, base_url, api_key, enabled, is_default, chain_to, chain_condition, rag_tags,
			rag_enabled, rag_max_results, rag_compressed, rag_max_tokens, rag_embeddings, rag_embedding_model,
			tools_enabled, tools_max_rounds, tool_timeout_sec,
			guard_max_input, guard_max_output, guard_blocked_input, guard_blocked_output,
			guard_phone_list, guard_phone_mode, guard_block_injection, guard_block_pii,
			guard_block_pii_phone, guard_block_pii_email, guard_block_pii_cpf,
			knowledge_extract,
			created_at FROM agents ORDER BY is_default DESC, name`)
		if err != nil {
			jsonError(rw, err.Error(), 500)
			return
		}
		defer rows.Close()

		var agents []types.Agent
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
				jsonError(rw, err.Error(), 500)
				return
			}
			// Mask api_key
			if len(a.APIKey) > 8 {
				a.APIKey = a.APIKey[:4] + "..." + a.APIKey[len(a.APIKey)-4:]
			} else if a.APIKey != "" {
				a.APIKey = "****"
			}
			agents = append(agents, a)
		}
		if agents == nil {
			agents = []types.Agent{}
		}
		jsonResponse(rw, agents)

	case "POST":
		if !auth.RequireAdmin(rw, r) {
			return
		}
		var a types.Agent
		if !requireJSON(rw, r) {
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
			jsonError(rw, "Invalid JSON", 400)
			return
		}
		a.IsDefault = 0 // cannot create new default agents
		if err := tools.ValidateAgent(a, db, 0); err != nil {
			jsonError(rw, err.Error(), 400)
			return
		}

		res, err := db.Exec(`INSERT INTO agents (name, description, system_prompt, user_prompt, model, max_tokens, base_url, api_key, enabled, chain_to, chain_condition, rag_tags,
			rag_enabled, rag_max_results, rag_compressed, rag_max_tokens, rag_embeddings, rag_embedding_model,
			tools_enabled, tools_max_rounds, tool_timeout_sec,
			guard_max_input, guard_max_output, guard_blocked_input, guard_blocked_output,
			guard_phone_list, guard_phone_mode, guard_block_injection, guard_block_pii,
			guard_block_pii_phone, guard_block_pii_email, guard_block_pii_cpf,
			knowledge_extract)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?,
			?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?,
			?)`,
			a.Name, a.Description, a.SystemPrompt, a.UserPrompt, a.Model, a.MaxTokens, a.BaseURL, a.APIKey, 1, a.ChainTo, a.ChainCondition, a.RAGTags,
			a.RAGEnabled, a.RAGMaxResults, a.RAGCompressed, a.RAGMaxTokens, a.RAGEmbeddings, a.RAGEmbeddingModel,
			a.ToolsEnabled, a.ToolsMaxRounds, a.ToolTimeoutSec,
			a.GuardMaxInput, a.GuardMaxOutput, a.GuardBlockedInput, a.GuardBlockedOutput,
			a.GuardPhoneList, a.GuardPhoneMode, a.GuardBlockInjection, a.GuardBlockPII,
			a.GuardBlockPIIPhone, a.GuardBlockPIIEmail, a.GuardBlockPIICPF,
			a.KnowledgeExtract)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				jsonError(rw, "Nome ja existe", 400)
				return
			}
			jsonError(rw, err.Error(), 500)
			return
		}
		id, _ := res.LastInsertId()
		w.tools.ReloadAgents()
		w.logger.Log("web_agent_created", "", map[string]any{"id": id, "name": a.Name})
		jsonCreated(rw, map[string]any{"ok": true, "id": id})

	default:
		jsonError(rw, "Method not allowed", 405)
	}
}

func (w *Web) handleAgentByID(rw http.ResponseWriter, r *http.Request) {
	db := w.tools.GetDB()
	idStr := strings.TrimPrefix(r.URL.Path, "/api/agents/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		jsonError(rw, "invalid id", 400)
		return
	}

	switch r.Method {
	case "PUT":
		if !auth.RequireAdmin(rw, r) {
			return
		}
		var a types.Agent
		if !requireJSON(rw, r) {
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
			jsonError(rw, "Invalid JSON", 400)
			return
		}
		if err := tools.ValidateAgent(a, db, id); err != nil {
			jsonError(rw, err.Error(), 400)
			return
		}

		enabled := 1
		if a.Enabled == 0 {
			enabled = 0
		}

		// Default agent cannot be disabled
		var isDefault int
		db.QueryRow("SELECT is_default FROM agents WHERE id = ?", id).Scan(&isDefault)
		if isDefault == 1 {
			enabled = 1
		}

		// If api_key is masked or empty, keep existing
		agentUpdateSuffix := `,
			rag_enabled=?, rag_max_results=?, rag_compressed=?, rag_max_tokens=?, rag_embeddings=?, rag_embedding_model=?,
			tools_enabled=?, tools_max_rounds=?, tool_timeout_sec=?,
			guard_max_input=?, guard_max_output=?, guard_blocked_input=?, guard_blocked_output=?,
			guard_phone_list=?, guard_phone_mode=?, guard_block_injection=?, guard_block_pii=?,
			guard_block_pii_phone=?, guard_block_pii_email=?, guard_block_pii_cpf=?,
			knowledge_extract=?
			WHERE id=?`
		agentExtraArgs := []any{
			a.RAGEnabled, a.RAGMaxResults, a.RAGCompressed, a.RAGMaxTokens, a.RAGEmbeddings, a.RAGEmbeddingModel,
			a.ToolsEnabled, a.ToolsMaxRounds, a.ToolTimeoutSec,
			a.GuardMaxInput, a.GuardMaxOutput, a.GuardBlockedInput, a.GuardBlockedOutput,
			a.GuardPhoneList, a.GuardPhoneMode, a.GuardBlockInjection, a.GuardBlockPII,
			a.GuardBlockPIIPhone, a.GuardBlockPIIEmail, a.GuardBlockPIICPF,
			a.KnowledgeExtract,
			id,
		}
		if a.APIKey == "" || strings.Contains(a.APIKey, "...") || a.APIKey == "****" {
			args := append([]any{a.Name, a.Description, a.SystemPrompt, a.UserPrompt, a.Model, a.MaxTokens, a.BaseURL, enabled, a.ChainTo, a.ChainCondition, a.RAGTags}, agentExtraArgs...)
			_, err = db.Exec(
				"UPDATE agents SET name=?, description=?, system_prompt=?, user_prompt=?, model=?, max_tokens=?, base_url=?, enabled=?, chain_to=?, chain_condition=?, rag_tags=?"+agentUpdateSuffix,
				args...)
		} else {
			args := append([]any{a.Name, a.Description, a.SystemPrompt, a.UserPrompt, a.Model, a.MaxTokens, a.BaseURL, a.APIKey, enabled, a.ChainTo, a.ChainCondition, a.RAGTags}, agentExtraArgs...)
			_, err = db.Exec(
				"UPDATE agents SET name=?, description=?, system_prompt=?, user_prompt=?, model=?, max_tokens=?, base_url=?, api_key=?, enabled=?, chain_to=?, chain_condition=?, rag_tags=?"+agentUpdateSuffix,
				args...)
		}
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				jsonError(rw, "Nome ja existe", 400)
				return
			}
			jsonError(rw, err.Error(), 500)
			return
		}
		w.tools.ReloadAgents()
		w.logger.Log("web_agent_updated", "", map[string]any{"id": id, "name": a.Name})
		jsonResponse(rw, map[string]any{"ok": true})

	case "DELETE":
		if !auth.RequireAdmin(rw, r) {
			return
		}
		var deletedName string
		var isDefaultDel int
		db.QueryRow("SELECT name, is_default FROM agents WHERE id = ?", id).Scan(&deletedName, &isDefaultDel)
		if isDefaultDel == 1 {
			jsonError(rw, "Agente padrão não pode ser excluído", 400)
			return
		}

		_, err := db.Exec("DELETE FROM agents WHERE id = ?", id)
		if err != nil {
			jsonError(rw, err.Error(), 500)
			return
		}
		// Cascade: remove routings for this agent
		db.Exec("DELETE FROM agent_routing WHERE agent_id = ?", id)
		w.tools.ReloadAgents()
		w.logger.Log("web_agent_deleted", "", map[string]any{"id": id, "name": deletedName})
		jsonResponse(rw, map[string]any{"ok": true})

	default:
		jsonError(rw, "Method not allowed", 405)
	}
}

func (w *Web) handleAgentRouting(rw http.ResponseWriter, r *http.Request) {
	db := w.tools.GetDB()
	switch r.Method {
	case "GET":
		chatID := r.URL.Query().Get("chat_id")
		if chatID != "" {
			// Get routing for specific chat ID
			var agentID int64
			var agentName string
			err := db.QueryRow("SELECT ar.agent_id, a.name FROM agent_routing ar JOIN agents a ON a.id = ar.agent_id WHERE ar.chat_id = ?", chatID).Scan(&agentID, &agentName)
			if err != nil {
				jsonResponse(rw, map[string]any{"agent_id": 0, "agent_name": ""})
				return
			}
			jsonResponse(rw, map[string]any{"agent_id": agentID, "agent_name": agentName})
			return
		}
		// List all routings
		rows, err := db.Query("SELECT ar.chat_id, ar.agent_id, a.name FROM agent_routing ar JOIN agents a ON a.id = ar.agent_id ORDER BY ar.chat_id")
		if err != nil {
			jsonError(rw, err.Error(), 500)
			return
		}
		defer rows.Close()
		type routing struct {
			ChatID    string `json:"chat_id"`
			AgentID   int64  `json:"agent_id"`
			AgentName string `json:"agent_name"`
		}
		var list []routing
		for rows.Next() {
			var r routing
			if err := rows.Scan(&r.ChatID, &r.AgentID, &r.AgentName); err != nil {
				continue
			}
			list = append(list, r)
		}
		if list == nil {
			list = []routing{}
		}
		jsonResponse(rw, list)

	case "POST":
		if !auth.RequireAdmin(rw, r) {
			return
		}
		var req struct {
			ChatID  string `json:"chat_id"`
			AgentID int64  `json:"agent_id"`
		}
		if !requireJSON(rw, r) {
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(rw, "Invalid JSON", 400)
			return
		}
		if req.ChatID == "" {
			jsonError(rw, "chat_id obrigatorio", 400)
			return
		}

		if req.AgentID == 0 {
			// Remove routing
			db.Exec("DELETE FROM agent_routing WHERE chat_id = ?", req.ChatID)
		} else {
			// Verify agent exists
			var exists int
			db.QueryRow("SELECT COUNT(*) FROM agents WHERE id = ?", req.AgentID).Scan(&exists)
			if exists == 0 {
				jsonError(rw, "Agente nao encontrado", 400)
				return
			}
			// Upsert routing
			_, err := db.Exec(
				"INSERT INTO agent_routing (chat_id, agent_id) VALUES (?, ?) ON CONFLICT(chat_id) DO UPDATE SET agent_id = excluded.agent_id",
				req.ChatID, req.AgentID)
			if err != nil {
				jsonError(rw, err.Error(), 500)
				return
			}
		}
		w.tools.ReloadAgents()
		w.logger.Log("web_agent_routing_changed", req.ChatID, map[string]any{"agent_id": req.AgentID})
		jsonResponse(rw, map[string]any{"ok": true})

	default:
		jsonError(rw, "Method not allowed", 405)
	}
}
