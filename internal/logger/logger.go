package logger

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"next/app/types"
	"next/internal/config"
)

type dbLogEntry struct {
	event   string
	chatID  string
	data    string
	created int64
}

// Logger writes structured JSON logs to file (always) and to SQLite (when debug=true).
// DB writes go through a buffered channel to preserve order without blocking.
type Logger struct {
	mu      sync.Mutex
	LogDir  string
	db      *sql.DB
	cfg     *config.Config
	file    *os.File
	today   string
	dbCh    chan dbLogEntry
	flushCh chan chan struct{}
}

func createLogsTable(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS logs (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			event      TEXT    NOT NULL,
			chat_id    TEXT,
			data       TEXT    NOT NULL,
			created_at INTEGER NOT NULL DEFAULT (unixepoch())
		);
		CREATE INDEX IF NOT EXISTS idx_logs_event ON logs(event, created_at);
		CREATE INDEX IF NOT EXISTS idx_logs_chat ON logs(chat_id, created_at);
	`)
	return err
}

// NewLogger creates a logger that writes to logDir and optionally to db.
func NewLogger(logDir string, db *sql.DB, cfg *config.Config) (*Logger, error) {
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	if err := createLogsTable(db); err != nil {
		return nil, fmt.Errorf("create logs table: %w", err)
	}
	l := &Logger{LogDir: logDir, db: db, cfg: cfg, dbCh: make(chan dbLogEntry, 1000), flushCh: make(chan chan struct{})}
	go l.dbWriter()
	return l, nil
}

// dbWriter processes DB inserts sequentially from the channel, preserving order.
func (l *Logger) dbWriter() {
	for {
		select {
		case e, ok := <-l.dbCh:
			if !ok {
				return
			}
			l.db.Exec("INSERT INTO logs (event, chat_id, data, created_at) VALUES (?, ?, ?, ?)",
				e.event, e.chatID, e.data, e.created)
		case done := <-l.flushCh:
			// Drain any remaining items in the channel
			for {
				select {
				case e := <-l.dbCh:
					l.db.Exec("INSERT INTO logs (event, chat_id, data, created_at) VALUES (?, ?, ?, ?)",
						e.event, e.chatID, e.data, e.created)
				default:
					close(done)
					goto flushed
				}
			}
		flushed:
		}
	}
}

func (l *Logger) getFile() (*os.File, error) {
	today := time.Now().Format("2006-01-02")
	if l.file != nil && l.today == today {
		return l.file, nil
	}
	if l.file != nil {
		l.file.Close()
	}
	path := filepath.Join(l.LogDir, fmt.Sprintf("next-%s.log", today))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	l.file = f
	l.today = today
	return f, nil
}

// Log writes a structured event. Always to file, to DB via async channel if debug=true.
func (l *Logger) Log(event, chatID string, data map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now().Unix()
	entry := map[string]any{
		"ts":    now,
		"event": event,
	}
	if chatID != "" {
		entry["chat_id"] = chatID
	}
	if data != nil {
		entry["data"] = data
	}

	// Always write to file
	if f, err := l.getFile(); err == nil {
		b, _ := json.Marshal(entry)
		f.Write(b)
		f.Write([]byte("\n"))
	}

	// Queue DB write (non-blocking) — preserves order via channel
	l.cfg.Mu.RLock()
	debug := l.cfg.Debug
	l.cfg.Mu.RUnlock()

	if debug {
		dataJSON, _ := json.Marshal(data)
		select {
		case l.dbCh <- dbLogEntry{event: event, chatID: chatID, data: string(dataJSON), created: now}:
		default:
			// Channel full — drop to avoid blocking
		}
	}
}

// Flush blocks until all queued DB writes have been processed.
func (l *Logger) Flush() {
	done := make(chan struct{})
	l.flushCh <- done
	<-done
}

// GetLogs queries log entries from DB.
func (l *Logger) GetLogs(f types.LogFilter) ([]types.LogEntry, error) {
	query := "SELECT id, event, COALESCE(chat_id,''), data, created_at FROM logs WHERE 1=1"
	args := []any{}

	if f.Event != "" {
		query += " AND event = ?"
		args = append(args, f.Event)
	}
	if f.ChatID != "" {
		query += " AND chat_id = ?"
		args = append(args, f.ChatID)
	}
	if f.Since > 0 {
		query += " AND created_at >= ?"
		args = append(args, f.Since)
	}
	query += " ORDER BY created_at DESC"
	if f.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, f.Limit)
	} else {
		query += " LIMIT 200"
	}
	if f.Offset > 0 {
		query += " OFFSET ?"
		args = append(args, f.Offset)
	}

	rows, err := l.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []types.LogEntry
	for rows.Next() {
		var e types.LogEntry
		var dataStr string
		if err := rows.Scan(&e.ID, &e.Event, &e.ChatID, &dataStr, &e.CreatedAt); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(dataStr), &e.Data)
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// DeleteLogs removes all log entries for a chat ID.
func (l *Logger) DeleteLogs(chatID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.db.Exec("DELETE FROM logs WHERE chat_id = ?", chatID)
}

// DeleteAllLogs removes all log entries.
func (l *Logger) DeleteAllLogs() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.db.Exec("DELETE FROM logs")
}
