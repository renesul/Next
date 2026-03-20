package logger

import (
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"next/app/types"
	"next/internal/config"
)

func setupTestLogger(t *testing.T, debug bool) (*Logger, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file::memory:?cache=shared&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	cfg := config.DefaultConfig()
	cfg.Debug = debug

	tmpDir := t.TempDir()
	logger, err := NewLogger(tmpDir, db, cfg)
	if err != nil {
		t.Fatal("NewLogger:", err)
	}
	return logger, db
}

func TestLogAndQuery(t *testing.T) {
	logger, db := setupTestLogger(t, true)
	defer db.Close()

	logger.Log("main_whatsapp_msg_received", "5511999", map[string]any{"content": "hello"})
	logger.Log("pipeline_ai_response", "5511999", map[string]any{"content": "hi there"})
	logger.Log("error", "", map[string]any{"message": "something broke"})
	logger.Flush()

	logs, err := logger.GetLogs(types.LogFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 3 {
		t.Fatalf("expected 3 logs, got %d", len(logs))
	}

	logs, err = logger.GetLogs(types.LogFilter{Event: "error"})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Errorf("expected 1 error log, got %d", len(logs))
	}
	if logs[0].Event != "error" {
		t.Errorf("event = %q, want error", logs[0].Event)
	}
}

func TestLogFilterPhone(t *testing.T) {
	logger, db := setupTestLogger(t, true)
	defer db.Close()

	logger.Log("main_whatsapp_msg_received", "5511111", map[string]any{"content": "msg1"})
	logger.Log("main_whatsapp_msg_received", "5511222", map[string]any{"content": "msg2"})
	logger.Log("main_whatsapp_msg_received", "5511111", map[string]any{"content": "msg3"})
	logger.Flush()

	logs, err := logger.GetLogs(types.LogFilter{ChatID: "5511111"})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 2 {
		t.Errorf("expected 2 logs for 5511111, got %d", len(logs))
	}
}

func TestDebugOff(t *testing.T) {
	logger, db := setupTestLogger(t, false)
	defer db.Close()

	logger.Log("test_event", "phone", map[string]any{"key": "val"})

	logs, err := logger.GetLogs(types.LogFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 0 {
		t.Errorf("debug=off should not write to DB, got %d logs", len(logs))
	}

	entries, err := os.ReadDir(logger.LogDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Error("log file should exist even with debug=off")
	}
}

func TestLogFilterSince(t *testing.T) {
	logger, db := setupTestLogger(t, true)
	defer db.Close()

	logger.Log("old_event", "phone", map[string]any{"key": "old"})
	mark := time.Now().Unix()
	time.Sleep(1100 * time.Millisecond)
	logger.Log("new_event", "phone", map[string]any{"key": "new"})
	logger.Flush()

	logs, err := logger.GetLogs(types.LogFilter{Since: mark + 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Errorf("expected 1 log since mark, got %d", len(logs))
	}
	if len(logs) > 0 && logs[0].Event != "new_event" {
		t.Errorf("event = %q, want new_event", logs[0].Event)
	}
}
