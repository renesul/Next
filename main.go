package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"nex/app/ai"
	"nex/app/guardrails"
	"nex/app/memory"
	"nex/app/pipeline"
	"nex/app/rag"
	"nex/app/tools"
	"nex/internal/auth"
	"nex/internal/config"
	"nex/internal/debounce"
	"nex/internal/logger"
	"nex/internal/web"
	"nex/internal/whatsapp"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Resolve data directory: DB_PATH overrides, otherwise ~/.nex/
	dataDir := ""
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			log.Fatal("resolve home dir:", err)
		}
		dataDir = filepath.Join(home, ".nex")
		if err := os.MkdirAll(dataDir, 0700); err != nil {
			log.Fatal("create data dir:", err)
		}
		dbPath = filepath.Join(dataDir, "nex.db")
	} else {
		dataDir = filepath.Dir(dbPath)
	}

	// 2. Open SQLite database with PRAGMAS for concurrency & performance
	db, err := sql.Open("sqlite3", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=cache_size(-2000)")
	if err != nil {
		log.Fatal("open database:", err)
	}
	defer db.Close()

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	// 3. Load config from SQLite
	cfg, err := config.LoadConfig(db)
	if err != nil {
		log.Fatal("load config:", err)
	}

	// 3b. Initialize logger
	l, err := logger.NewLogger(dataDir, db, cfg)
	if err != nil {
		log.Fatal("init logger:", err)
	}

	// 3c. Initialize auth
	a, err := auth.NewAuth(db, l)
	if err != nil {
		log.Fatal("init auth:", err)
	}

	// 5. Initialize memory
	mem, err := memory.NewMemory(db)
	if err != nil {
		log.Fatal("init memory:", err)
	}

	// 5b. Initialize RAG (knowledge base)
	r, err := rag.NewRAG(db)
	if err != nil {
		log.Fatal("init rag:", err)
	}
	r.SetLogger(l)

	// 5c. Initialize guardrails
	guard := guardrails.NewGuardrails(l)

	// 6. Initialize AI client
	aiClient := ai.NewAI(cfg.BaseURL, cfg.APIKey, l)

	// 7. Initialize WhatsApp (before tools, so scheduler can send)
	wa, err := whatsapp.NewWhatsApp(filepath.Join(dataDir, "whatsapp.db"), l, cfg)
	if err != nil {
		log.Fatal("init whatsapp:", err)
	}

	// 6b. Initialize tool registry
	tr, err := tools.NewToolRegistry(db, dbPath, r, aiClient, cfg, wa, l)
	if err != nil {
		log.Fatal("init tools:", err)
	}
	tr.StartScheduler()

	// 7b. Initialize MCP server (if enabled)
	var mcpServer *tools.NexMCPServer
	cfg.Mu.RLock()
	mcpEnabled := cfg.MCPServerEnabled
	cfg.Mu.RUnlock()
	if mcpEnabled {
		mcpServer = tools.NewNexMCPServer(tr, cfg)
		log.Println("MCP server enabled on /mcp/sse")
	}

	// 8. Create pipeline and debouncer
	pipe := pipeline.NewPipeline(cfg, mem, r, aiClient, l, guard, tr)

	cfg.Mu.RLock()
	debounceWait := time.Duration(cfg.DebounceMs) * time.Millisecond
	debounceMax := time.Duration(cfg.DebounceMaxMs) * time.Millisecond
	cfg.Mu.RUnlock()

	deb := debounce.NewDebouncer(debounceWait, debounceMax, func(chatID, combinedText, pushName string) {
		handleDebouncedMessage(pipe, wa, mem, l, cfg, chatID, combinedText, pushName)
	})

	// 9. Register WhatsApp message handler
	wa.OnMessage(func(chatID, text, pushName string) {
		l.Log("main_whatsapp_msg_received", chatID, map[string]any{"content": text, "push_name": pushName})
		deb.Add(chatID, text, pushName)
	})

	// 9b. Register read receipt handler
	wa.OnReceipt(func(waMsgID string) {
		if err := mem.MarkReadByWAMsgID(waMsgID); err != nil {
			l.Log("error", "", map[string]any{"source": "whatsapp", "message": "mark read: " + err.Error()})
		}
	})

	// 10. Start web server
	mux := http.NewServeMux()
	web.SetupRoutes(mux, web.WebDeps{
		Config:    cfg,
		Memory:    mem,
		RAG:       r,
		AI:        aiClient,
		WhatsApp:  wa,
		Debouncer: deb,
		Logger:    l,
		Tools:     tr,
		Guard:     guard,
		Pipeline:  pipe,
		Auth:      a,
		MCPServer: mcpServer,
		SaveCfg: func() error {
			return config.SaveConfig(db, cfg)
		},
	})

	server := &http.Server{Addr: ":" + port, Handler: a.Middleware(mux)}

	// 11. Connect WhatsApp if API key is set
	cfg.Mu.RLock()
	hasKey := cfg.APIKey != ""
	cfg.Mu.RUnlock()

	if hasKey {
		go connectWhatsApp(wa, l)
	}

	// 12. Print startup message
	fmt.Printf("Nex rodando em http://localhost:%s\n", port)

	// Start server in goroutine
	go func() {
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatal("server:", err)
		}
	}()

	// 13. Wait for SIGINT/SIGTERM
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	fmt.Println("\nDesligando...")
	tr.StopScheduler()
	tr.StopMCPClients()
	tr.StopExtDBs()
	tr.Close()
	wa.Disconnect()
	server.Close()
}

func connectWhatsApp(wa *whatsapp.WhatsApp, l *logger.Logger) {
	_, err := wa.Connect()
	if err != nil {
		l.Log("error", "", map[string]any{"source": "whatsapp", "message": "whatsapp connect: " + err.Error()})
		log.Println("WhatsApp connect error:", err)
	}
}

func handleDebouncedMessage(pipe *pipeline.Pipeline, wa *whatsapp.WhatsApp, mem *memory.Memory, l *logger.Logger, cfg *config.Config, chatID, combinedText, pushName string) {
	l.Log("main_debounce_fire", chatID, map[string]any{"combined_text": combinedText, "push_name": pushName})

	wa.SendComposing(chatID)

	cfg.Mu.RLock()
	waAgentID := int64(cfg.WhatsAppAgentID)
	cfg.Mu.RUnlock()
	result := pipe.ProcessWithAgent(chatID, combinedText, pushName, waAgentID)

	if result.Error != nil {
		wa.SendPaused(chatID)
		log.Println("AI error for", chatID, ":", result.Error)
		return
	}
	if result.Blocked && result.Response == "" {
		wa.SendPaused(chatID)
		return
	}

	// Simulate typing delay proportional to response length
	// ~30 chars/sec typing speed, min 1s, max 8s
	typingDelay := time.Duration(len(result.Response)/30) * time.Second
	if typingDelay < 1*time.Second {
		typingDelay = 1 * time.Second
	}
	if typingDelay > 8*time.Second {
		typingDelay = 8 * time.Second
	}
	time.Sleep(typingDelay)
	wa.SendPaused(chatID)

	waMsgID, err := wa.SendText(chatID, result.Response)
	if err != nil {
		l.Log("error", chatID, map[string]any{"source": "send", "message": "send: " + err.Error()})
		log.Println("Send error for", chatID, ":", err)
		return
	}
	// Store the WA message ID on the assistant message for read receipt tracking
	if waMsgID != "" {
		mem.SetLastAssistantWAMsgID(chatID, waMsgID)
	}
}

