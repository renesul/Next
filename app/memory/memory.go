package memory

import (
	"database/sql"
	"fmt"
	"time"

	"next/app/types"
)

// Memory manages sessions, conversation history, and summaries.
type Memory struct {
	db *sql.DB
}

func createMemoryTables(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS messages (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			chat_id    TEXT    NOT NULL,
			role       TEXT    NOT NULL,
			content    TEXT    NOT NULL,
			session_id INTEGER NOT NULL,
			wa_msg_id  TEXT    NOT NULL DEFAULT '',
			read_at    INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL DEFAULT (unixepoch())
		);
		CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(chat_id, session_id, created_at);

		CREATE TABLE IF NOT EXISTS summaries (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			chat_id    TEXT    NOT NULL,
			session_id INTEGER NOT NULL,
			content    TEXT    NOT NULL,
			created_at INTEGER NOT NULL DEFAULT (unixepoch())
		);
		CREATE INDEX IF NOT EXISTS idx_summaries_chat ON summaries(chat_id, created_at DESC);
	`)
	return err
}

// NewMemory creates a new Memory manager.
func NewMemory(db *sql.DB) (*Memory, error) {
	if err := createMemoryTables(db); err != nil {
		return nil, fmt.Errorf("create memory tables: %w", err)
	}
	return &Memory{db: db}, nil
}

// GetOrCreateSession returns the current session ID for a chat ID.
// If the last message is older than timeoutMin, starts a new session.
// Returns (sessionID, isNew, oldSessionID).
func (m *Memory) GetOrCreateSession(chatID string, timeoutMin int) (int64, bool, int64) {
	var lastCreatedAt int64
	var lastSessionID int64
	err := m.db.QueryRow(
		"SELECT created_at, session_id FROM messages WHERE chat_id = ? ORDER BY created_at DESC LIMIT 1",
		chatID,
	).Scan(&lastCreatedAt, &lastSessionID)

	nowSec := time.Now().Unix()
	newSessionID := time.Now().UnixMilli() // millis for uniqueness

	if err == sql.ErrNoRows {
		// First message from this contact
		return newSessionID, true, 0
	}

	elapsed := nowSec - lastCreatedAt
	if elapsed > int64(timeoutMin)*60 {
		// Session timed out
		return newSessionID, true, lastSessionID
	}

	return lastSessionID, false, 0
}

// SaveMessage persists a message to the database.
func (m *Memory) SaveMessage(chatID, role, content string, sessionID int64) error {
	_, err := m.db.Exec(
		"INSERT INTO messages (chat_id, role, content, session_id) VALUES (?, ?, ?, ?)",
		chatID, role, content, sessionID,
	)
	return err
}

// GetSessionHistory returns messages from a specific session, limited to `limit` most recent.
func (m *Memory) GetSessionHistory(chatID string, sessionID int64, limit int) ([]types.Message, error) {
	rows, err := m.db.Query(
		`SELECT id, chat_id, role, content, session_id, created_at, wa_msg_id, read_at
		 FROM messages WHERE chat_id = ? AND session_id = ?
		 ORDER BY created_at DESC LIMIT ?`,
		chatID, sessionID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []types.Message
	for rows.Next() {
		var msg types.Message
		if err := rows.Scan(&msg.ID, &msg.ChatID, &msg.Role, &msg.Content, &msg.SessionID, &msg.CreatedAt, &msg.WAMsgID, &msg.ReadAt); err != nil {
			return nil, err
		}
		msgs = append(msgs, msg)
	}
	// Reverse to chronological order
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, rows.Err()
}

// GetOldSessionMessages returns all messages from a previous session (for summarization).
func (m *Memory) GetOldSessionMessages(chatID string, sessionID int64) ([]types.Message, error) {
	rows, err := m.db.Query(
		`SELECT id, chat_id, role, content, session_id, created_at, wa_msg_id, read_at
		 FROM messages WHERE chat_id = ? AND session_id = ?
		 ORDER BY created_at ASC`,
		chatID, sessionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []types.Message
	for rows.Next() {
		var msg types.Message
		if err := rows.Scan(&msg.ID, &msg.ChatID, &msg.Role, &msg.Content, &msg.SessionID, &msg.CreatedAt, &msg.WAMsgID, &msg.ReadAt); err != nil {
			return nil, err
		}
		msgs = append(msgs, msg)
	}
	return msgs, rows.Err()
}

// SaveSummary persists a session summary.
func (m *Memory) SaveSummary(chatID string, sessionID int64, content string) error {
	_, err := m.db.Exec(
		"INSERT INTO summaries (chat_id, session_id, content) VALUES (?, ?, ?)",
		chatID, sessionID, content,
	)
	return err
}

// GetLatestSummary returns the most recent summary for a chat ID.
func (m *Memory) GetLatestSummary(chatID string) string {
	var content string
	m.db.QueryRow(
		"SELECT content FROM summaries WHERE chat_id = ? ORDER BY created_at DESC LIMIT 1",
		chatID,
	).Scan(&content)
	return content
}

// EstimateTokens gives a rough token count for a set of messages.
// Uses factor of 3 (more conservative than /4) plus per-message overhead.
func EstimateTokens(messages []types.Message) int {
	total := 0
	for _, m := range messages {
		total += len(m.Content)/3 + 4 // +4 for role/format overhead per message
	}
	return total
}

// TrimToTokenBudget removes oldest messages until within budget.
func TrimToTokenBudget(messages []types.Message, budget int) []types.Message {
	for len(messages) > 1 && EstimateTokens(messages) > budget {
		messages = messages[1:]
	}
	return messages
}

// GetContacts returns a list of contacts with their last message.
func (m *Memory) GetContacts() ([]types.Contact, error) {
	rows, err := m.db.Query(`
		SELECT m.chat_id, m.content, m.created_at, m.session_id
		FROM messages m
		INNER JOIN (
			SELECT chat_id, MAX(id) as max_id
			FROM messages GROUP BY chat_id
		) latest ON m.id = latest.max_id
		ORDER BY m.created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var contacts []types.Contact
	for rows.Next() {
		var c types.Contact
		if err := rows.Scan(&c.ChatID, &c.LastMessage, &c.LastTime, &c.SessionID); err != nil {
			return nil, err
		}
		contacts = append(contacts, c)
	}
	return contacts, rows.Err()
}

// GetAllMessages returns all messages for a chat ID, ordered chronologically.
func (m *Memory) GetAllMessages(chatID string) ([]types.Message, error) {
	rows, err := m.db.Query(
		`SELECT id, chat_id, role, content, session_id, created_at, wa_msg_id, read_at
		 FROM messages WHERE chat_id = ? ORDER BY created_at ASC`,
		chatID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []types.Message
	for rows.Next() {
		var msg types.Message
		if err := rows.Scan(&msg.ID, &msg.ChatID, &msg.Role, &msg.Content, &msg.SessionID, &msg.CreatedAt, &msg.WAMsgID, &msg.ReadAt); err != nil {
			return nil, err
		}
		msgs = append(msgs, msg)
	}
	return msgs, rows.Err()
}

// GetSummaries returns all summaries for a chat ID.
func (m *Memory) GetSummaries(chatID string) ([]types.Summary, error) {
	rows, err := m.db.Query(
		`SELECT id, chat_id, session_id, content, created_at
		 FROM summaries WHERE chat_id = ? ORDER BY created_at ASC`,
		chatID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var summaries []types.Summary
	for rows.Next() {
		var s types.Summary
		if err := rows.Scan(&s.ID, &s.ChatID, &s.SessionID, &s.Content, &s.CreatedAt); err != nil {
			return nil, err
		}
		summaries = append(summaries, s)
	}
	return summaries, rows.Err()
}

// SetLastAssistantWAMsgID stores the WhatsApp message ID on the most recent
// assistant message for a chat (used right after SendText succeeds).
func (m *Memory) SetLastAssistantWAMsgID(chatID, waMsgID string) error {
	_, err := m.db.Exec(`UPDATE messages SET wa_msg_id = ? WHERE id = (
		SELECT id FROM messages WHERE chat_id = ? AND role = 'assistant' AND wa_msg_id = '' ORDER BY id DESC LIMIT 1
	)`, waMsgID, chatID)
	return err
}

// MarkReadByWAMsgID sets read_at on a message matched by its WhatsApp message ID.
func (m *Memory) MarkReadByWAMsgID(waMsgID string) error {
	_, err := m.db.Exec(`UPDATE messages SET read_at = unixepoch() WHERE wa_msg_id = ? AND read_at = 0`, waMsgID)
	return err
}

// GetContactsPaginated returns a page of contacts with total count.
func (m *Memory) GetContactsPaginated(limit, offset int) ([]types.Contact, int64, error) {
	var total int64
	m.db.QueryRow("SELECT COUNT(DISTINCT chat_id) FROM messages").Scan(&total)

	rows, err := m.db.Query(`
		SELECT m.chat_id, m.content, m.created_at, m.session_id
		FROM messages m
		INNER JOIN (
			SELECT chat_id, MAX(id) as max_id
			FROM messages GROUP BY chat_id
		) latest ON m.id = latest.max_id
		ORDER BY m.created_at DESC
		LIMIT ? OFFSET ?
	`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var contacts []types.Contact
	for rows.Next() {
		var c types.Contact
		if err := rows.Scan(&c.ChatID, &c.LastMessage, &c.LastTime, &c.SessionID); err != nil {
			return nil, 0, err
		}
		contacts = append(contacts, c)
	}
	return contacts, total, rows.Err()
}

// GetAllMessagesPaginated returns a page of messages for a chat ID with total count.
func (m *Memory) GetAllMessagesPaginated(chatID string, limit, offset int) ([]types.Message, int64, error) {
	var total int64
	m.db.QueryRow("SELECT COUNT(*) FROM messages WHERE chat_id = ?", chatID).Scan(&total)

	rows, err := m.db.Query(
		`SELECT id, chat_id, role, content, session_id, created_at, wa_msg_id, read_at
		 FROM messages WHERE chat_id = ? ORDER BY created_at ASC LIMIT ? OFFSET ?`,
		chatID, limit, offset,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var msgs []types.Message
	for rows.Next() {
		var msg types.Message
		if err := rows.Scan(&msg.ID, &msg.ChatID, &msg.Role, &msg.Content, &msg.SessionID, &msg.CreatedAt, &msg.WAMsgID, &msg.ReadAt); err != nil {
			return nil, 0, err
		}
		msgs = append(msgs, msg)
	}
	return msgs, total, rows.Err()
}

// DeleteMessages removes all messages and summaries for a chat ID, then vacuums.
func (m *Memory) DeleteMessages(chatID string) error {
	_, err := m.db.Exec("DELETE FROM messages WHERE chat_id = ?", chatID)
	if err != nil {
		return err
	}
	_, err = m.db.Exec("DELETE FROM summaries WHERE chat_id = ?", chatID)
	if err != nil {
		return err
	}
	_, err = m.db.Exec("VACUUM")
	return err
}
