package rag

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"html"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"

	"next/app/ai"
	"next/app/types"
	"next/internal/logger"
)

var (
	reStripScript = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	reStripStyle  = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	reStripTags   = regexp.MustCompile(`<[^>]*>`)
)

// RAG manages the knowledge base with FTS5 full-text search.
type RAG struct {
	db     *sql.DB
	logger *logger.Logger
}

// SetLogger sets the logger for RAG operations.
func (r *RAG) SetLogger(l *logger.Logger) {
	r.logger = l
}

// NewRAG creates the knowledge tables, FTS5 index, and sync triggers.
func NewRAG(db *sql.DB) (*RAG, error) {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS knowledge (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			title      TEXT    NOT NULL,
			content    TEXT    NOT NULL,
			tags       TEXT    NOT NULL DEFAULT '',
			compressed TEXT    NOT NULL DEFAULT '',
			embedding  BLOB,
			enabled    INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL DEFAULT (unixepoch()),
			updated_at INTEGER NOT NULL DEFAULT (unixepoch())
		)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS knowledge_fts USING fts5(
			title, content, tags,
			content=knowledge, content_rowid=id,
			tokenize='unicode61 remove_diacritics 2'
		)`,
		// Triggers to keep FTS in sync with the main table.
		`CREATE TRIGGER IF NOT EXISTS knowledge_ai AFTER INSERT ON knowledge BEGIN
			INSERT INTO knowledge_fts(rowid, title, content, tags)
			VALUES (new.id, new.title, new.content, new.tags);
		END`,
		`CREATE TRIGGER IF NOT EXISTS knowledge_ad AFTER DELETE ON knowledge BEGIN
			INSERT INTO knowledge_fts(knowledge_fts, rowid, title, content, tags)
			VALUES ('delete', old.id, old.title, old.content, old.tags);
		END`,
		`CREATE TRIGGER IF NOT EXISTS knowledge_au AFTER UPDATE ON knowledge BEGIN
			INSERT INTO knowledge_fts(knowledge_fts, rowid, title, content, tags)
			VALUES ('delete', old.id, old.title, old.content, old.tags);
			INSERT INTO knowledge_fts(rowid, title, content, tags)
			VALUES (new.id, new.title, new.content, new.tags);
		END`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return nil, fmt.Errorf("rag init: %w\nSQL: %s", err, s)
		}
	}

	return &RAG{db: db}, nil
}

// AddEntry inserts a new knowledge entry and returns its ID.
func (r *RAG) AddEntry(title, content, tags string) (int64, error) {
	res, err := r.db.Exec(
		"INSERT INTO knowledge (title, content, tags) VALUES (?, ?, ?)",
		title, content, tags,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateEntry updates an existing knowledge entry.
// Clears compressed version if content changed.
func (r *RAG) UpdateEntry(id int64, title, content, tags string, enabled bool) error {
	enabledInt := 0
	if enabled {
		enabledInt = 1
	}

	// Check if content changed — if so, invalidate compressed version
	var oldContent string
	r.db.QueryRow("SELECT content FROM knowledge WHERE id=?", id).Scan(&oldContent)
	clearCompressed := oldContent != content

	if clearCompressed {
		_, err := r.db.Exec(
			"UPDATE knowledge SET title=?, content=?, tags=?, enabled=?, compressed='', embedding=NULL, updated_at=unixepoch() WHERE id=?",
			title, content, tags, enabledInt, id,
		)
		return err
	}
	_, err := r.db.Exec(
		"UPDATE knowledge SET title=?, content=?, tags=?, enabled=?, updated_at=unixepoch() WHERE id=?",
		title, content, tags, enabledInt, id,
	)
	return err
}

// DeleteEntry removes a knowledge entry by ID.
func (r *RAG) DeleteEntry(id int64) error {
	_, err := r.db.Exec("DELETE FROM knowledge WHERE id=?", id)
	return err
}

// ListEntries returns all knowledge entries ordered by most recent first.
func (r *RAG) ListEntries() ([]types.KnowledgeEntry, error) {
	rows, err := r.db.Query(
		`SELECT id, title, content, tags, compressed, enabled,
		 CASE WHEN embedding IS NOT NULL THEN 1 ELSE 0 END,
		 created_at, updated_at FROM knowledge ORDER BY updated_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []types.KnowledgeEntry
	for rows.Next() {
		var e types.KnowledgeEntry
		if err := rows.Scan(&e.ID, &e.Title, &e.Content, &e.Tags, &e.Compressed, &e.Enabled, &e.HasEmbedding, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// ListEntriesPaginated returns a page of knowledge entries with total count.
func (r *RAG) ListEntriesPaginated(limit, offset int) ([]types.KnowledgeEntry, int64, error) {
	var total int64
	r.db.QueryRow("SELECT COUNT(*) FROM knowledge").Scan(&total)

	rows, err := r.db.Query(
		`SELECT id, title, content, tags, compressed, enabled,
		 CASE WHEN embedding IS NOT NULL THEN 1 ELSE 0 END,
		 created_at, updated_at FROM knowledge ORDER BY updated_at DESC LIMIT ? OFFSET ?`,
		limit, offset,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var entries []types.KnowledgeEntry
	for rows.Next() {
		var e types.KnowledgeEntry
		if err := rows.Scan(&e.ID, &e.Title, &e.Content, &e.Tags, &e.Compressed, &e.Enabled, &e.HasEmbedding, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, 0, err
		}
		entries = append(entries, e)
	}
	return entries, total, rows.Err()
}

// Search performs a FTS5 full-text search and returns the top matching enabled entries.
// If allowedTags is non-empty, only entries that have at least one matching tag are returned.
func (r *RAG) Search(chatID, query string, maxResults int, allowedTags ...string) ([]types.KnowledgeEntry, error) {
	ftsQuery := sanitizeFTS5Query(query)
	if ftsQuery == "" {
		return nil, nil
	}

	// Fetch more results if filtering by tags, to compensate for filtered-out entries
	fetchLimit := maxResults
	if len(allowedTags) > 0 {
		fetchLimit = maxResults * 5
	}

	rows, err := r.db.Query(
		`SELECT k.id, k.title, k.content, k.tags, k.compressed, k.enabled, k.created_at, k.updated_at
		 FROM knowledge_fts f
		 JOIN knowledge k ON k.id = f.rowid
		 WHERE knowledge_fts MATCH ? AND k.enabled = 1
		 ORDER BY rank
		 LIMIT ?`,
		ftsQuery, fetchLimit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []types.KnowledgeEntry
	for rows.Next() {
		var e types.KnowledgeEntry
		if err := rows.Scan(&e.ID, &e.Title, &e.Content, &e.Tags, &e.Compressed, &e.Enabled, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		if len(allowedTags) > 0 && !entryMatchesTags(e.Tags, allowedTags) {
			continue
		}
		entries = append(entries, e)
		if len(entries) >= maxResults {
			break
		}
	}
	if r.logger != nil {
		titles := make([]string, len(entries))
		for i, e := range entries {
			titles[i] = e.Title
		}
		r.logger.Log("rag_fts_search", chatID, map[string]any{
			"query": query, "fts_query": ftsQuery, "results": len(entries),
			"titles": titles, "allowed_tags": allowedTags,
		})
	}
	return entries, rows.Err()
}

// entryMatchesTags checks if the entry's CSV tags contain at least one of the allowed tags.
func entryMatchesTags(entryTags string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, et := range strings.Split(entryTags, ",") {
		et = strings.TrimSpace(strings.ToLower(et))
		if et == "" {
			continue
		}
		for _, at := range allowed {
			if et == at {
				return true
			}
		}
	}
	return false
}

// FormatContextFromEntries builds a context string from types.KnowledgeEntry slice.
// Uses compressed version when available and useCompressed is true.
func FormatContextFromEntries(entries []types.KnowledgeEntry, useCompressed bool) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Base de conhecimento relevante:\n\n")
	for i, e := range entries {
		if i > 0 {
			b.WriteString("\n---\n")
		}
		b.WriteString("### ")
		b.WriteString(e.Title)
		b.WriteString("\n")
		body := e.Content
		if useCompressed && e.Compressed != "" {
			body = e.Compressed
		}
		b.WriteString(body)
	}
	return b.String()
}

// FormatContextFromEntriesWithBudget builds a context string respecting a token budget.
// Adds entries one at a time, stopping before exceeding the budget.
// If the first entry exceeds the budget, it is truncated to fit.
func FormatContextFromEntriesWithBudget(entries []types.KnowledgeEntry, useCompressed bool, maxTokens int) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	header := "Base de conhecimento relevante:\n\n"
	b.WriteString(header)
	budgetChars := maxTokens * 4 // 1 token ~ 4 chars
	added := 0

	for _, e := range entries {
		var entry strings.Builder
		if added > 0 {
			entry.WriteString("\n---\n")
		}
		entry.WriteString("### ")
		entry.WriteString(e.Title)
		entry.WriteString("\n")
		body := e.Content
		if useCompressed && e.Compressed != "" {
			body = e.Compressed
		}
		entry.WriteString(body)

		entryStr := entry.String()
		if budgetChars > 0 && b.Len()+len(entryStr) > budgetChars {
			if added == 0 {
				// First entry doesn't fit — truncate it to fill the budget
				remaining := budgetChars - b.Len()
				if remaining > 50 {
					if len(entryStr) > remaining {
						cut := entryStr[:remaining]
						if idx := strings.LastIndex(cut, "\n"); idx > 0 {
							cut = cut[:idx]
						}
						entryStr = cut
					}
					b.WriteString(entryStr)
					added++
				}
			}
			break
		}
		b.WriteString(entryStr)
		added++
	}

	if added == 0 {
		return ""
	}
	return b.String()
}

// TruncateContext truncates text to fit within a token limit.
// Cuts at the last newline before the limit.
func TruncateContext(text string, maxTokens int) string {
	if maxTokens <= 0 || text == "" {
		return text
	}
	maxChars := maxTokens * 4 // rough estimate: 1 token ~ 4 chars
	if len(text) <= maxChars {
		return text
	}
	// Find last newline before limit
	cut := text[:maxChars]
	if idx := strings.LastIndex(cut, "\n"); idx > 0 {
		return cut[:idx]
	}
	return cut
}

// CompressEntry compresses a single knowledge entry using AI and saves to DB.
func (r *RAG) CompressEntry(a *ai.AI, model string, id int64) (string, error) {
	var content string
	err := r.db.QueryRow("SELECT content FROM knowledge WHERE id=?", id).Scan(&content)
	if err != nil {
		return "", fmt.Errorf("entry not found: %w", err)
	}

	compressed, err := a.CompressRAG(model, content)
	if err != nil {
		return "", err
	}

	_, err = r.db.Exec("UPDATE knowledge SET compressed=? WHERE id=?", compressed, id)
	if err != nil {
		return "", fmt.Errorf("save compressed: %w", err)
	}
	return compressed, nil
}

// CompressAllEntries compresses all entries that don't have a compressed version yet.
func (r *RAG) CompressAllEntries(a *ai.AI, model string) (int, error) {
	rows, err := r.db.Query("SELECT id, content FROM knowledge WHERE compressed = ''")
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type entry struct {
		id      int64
		content string
	}
	var pending []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.id, &e.content); err != nil {
			return 0, err
		}
		pending = append(pending, e)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	count := 0
	for _, e := range pending {
		compressed, err := a.CompressRAG(model, e.content)
		if err != nil {
			return count, fmt.Errorf("compress id %d: %w", e.id, err)
		}
		if _, err := r.db.Exec("UPDATE knowledge SET compressed=? WHERE id=?", compressed, e.id); err != nil {
			return count, fmt.Errorf("save compressed id %d: %w", e.id, err)
		}
		count++
	}
	return count, nil
}

// ProcessEntry compresses and generates embedding for a single entry in one pass.
// If compress fails, returns error without embedding. If embed fails, saves compressed and returns partial error.
func (r *RAG) ProcessEntry(a *ai.AI, model, embModel string, id int64) (string, error) {
	var content string
	err := r.db.QueryRow("SELECT content FROM knowledge WHERE id=?", id).Scan(&content)
	if err != nil {
		return "", fmt.Errorf("entry not found: %w", err)
	}

	compressed, err := a.CompressRAG(model, content)
	if err != nil {
		return "", fmt.Errorf("compress: %w", err)
	}

	vec, embErr := a.Embed(embModel, content)
	if embErr != nil {
		// Save compressed only
		if _, err := r.db.Exec("UPDATE knowledge SET compressed=? WHERE id=?", compressed, id); err != nil {
			return "", fmt.Errorf("save compressed: %w", err)
		}
		return compressed, fmt.Errorf("embed: %w", embErr)
	}

	_, err = r.db.Exec("UPDATE knowledge SET compressed=?, embedding=? WHERE id=?", compressed, encodeEmbedding(vec), id)
	if err != nil {
		return compressed, fmt.Errorf("save: %w", err)
	}
	return compressed, nil
}

// ProcessAllEntries compresses and generates embeddings for all entries that need either.
func (r *RAG) ProcessAllEntries(a *ai.AI, model, embModel string) (compressed, embedded int, err error) {
	rows, err := r.db.Query("SELECT id, content, compressed, CASE WHEN embedding IS NOT NULL THEN 1 ELSE 0 END FROM knowledge WHERE compressed = '' OR embedding IS NULL")
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()

	type entry struct {
		id           int64
		content      string
		compressed   string
		hasEmbedding int
	}
	var pending []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.id, &e.content, &e.compressed, &e.hasEmbedding); err != nil {
			return 0, 0, err
		}
		pending = append(pending, e)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}

	for _, e := range pending {
		newCompressed := e.compressed
		didCompress := false
		didEmbed := false

		if newCompressed == "" {
			c, err := a.CompressRAG(model, e.content)
			if err != nil {
				return compressed, embedded, fmt.Errorf("compress id %d: %w", e.id, err)
			}
			newCompressed = c
			didCompress = true
		}

		var newEmbedding []byte
		if e.hasEmbedding == 0 {
			vec, err := a.Embed(embModel, e.content)
			if err != nil {
				// Save compressed if we got it, then return error
				if didCompress {
					r.db.Exec("UPDATE knowledge SET compressed=? WHERE id=?", newCompressed, e.id)
					compressed++
				}
				return compressed, embedded, fmt.Errorf("embed id %d: %w", e.id, err)
			}
			newEmbedding = encodeEmbedding(vec)
			didEmbed = true
		}

		if didCompress && didEmbed {
			if _, err := r.db.Exec("UPDATE knowledge SET compressed=?, embedding=? WHERE id=?", newCompressed, newEmbedding, e.id); err != nil {
				return compressed, embedded, fmt.Errorf("save id %d: %w", e.id, err)
			}
		} else if didCompress {
			if _, err := r.db.Exec("UPDATE knowledge SET compressed=? WHERE id=?", newCompressed, e.id); err != nil {
				return compressed, embedded, fmt.Errorf("save compressed id %d: %w", e.id, err)
			}
		} else if didEmbed {
			if _, err := r.db.Exec("UPDATE knowledge SET embedding=? WHERE id=?", newEmbedding, e.id); err != nil {
				return compressed, embedded, fmt.Errorf("save embedding id %d: %w", e.id, err)
			}
		}

		if didCompress {
			compressed++
		}
		if didEmbed {
			embedded++
		}
	}
	return compressed, embedded, nil
}

// encodeEmbedding serializes a float32 slice to bytes for BLOB storage.
func encodeEmbedding(vec []float32) []byte {
	buf := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return buf
}

// decodeEmbedding deserializes bytes from BLOB into a float32 slice.
func decodeEmbedding(data []byte) []float32 {
	if len(data) == 0 || len(data)%4 != 0 {
		return nil
	}
	vec := make([]float32, len(data)/4)
	for i := range vec {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[i*4:]))
	}
	return vec
}

// cosineSimilarity computes cosine similarity between two vectors.
func cosineSimilarity(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		ai, bi := float64(a[i]), float64(b[i])
		dot += ai * bi
		normA += ai * ai
		normB += bi * bi
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return float32(dot / denom)
}

// EmbedEntry generates an embedding for a single entry and saves it.
func (r *RAG) EmbedEntry(a *ai.AI, model string, id int64) error {
	var content string
	err := r.db.QueryRow("SELECT content FROM knowledge WHERE id=?", id).Scan(&content)
	if err != nil {
		return fmt.Errorf("entry not found: %w", err)
	}

	vec, err := a.Embed(model, content)
	if err != nil {
		return err
	}

	_, err = r.db.Exec("UPDATE knowledge SET embedding=? WHERE id=?", encodeEmbedding(vec), id)
	if err != nil {
		return fmt.Errorf("save embedding: %w", err)
	}
	return nil
}

// EmbedAllEntries generates embeddings for all entries that don't have one yet.
func (r *RAG) EmbedAllEntries(a *ai.AI, model string) (int, error) {
	rows, err := r.db.Query("SELECT id, content FROM knowledge WHERE embedding IS NULL")
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type entry struct {
		id      int64
		content string
	}
	var pending []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.id, &e.content); err != nil {
			return 0, err
		}
		pending = append(pending, e)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	count := 0
	for _, e := range pending {
		vec, err := a.Embed(model, e.content)
		if err != nil {
			return count, fmt.Errorf("embed id %d: %w", e.id, err)
		}
		if _, err := r.db.Exec("UPDATE knowledge SET embedding=? WHERE id=?", encodeEmbedding(vec), e.id); err != nil {
			return count, fmt.Errorf("save embedding id %d: %w", e.id, err)
		}
		count++
	}
	return count, nil
}

// searchByEmbedding does brute-force cosine similarity search over all enabled entries with embeddings.
func (r *RAG) searchByEmbedding(chatID string, queryVec []float32, limit int, allowedTags ...string) ([]types.KnowledgeEntry, error) {
	rows, err := r.db.Query(
		"SELECT id, title, content, tags, compressed, enabled, embedding, created_at, updated_at FROM knowledge WHERE enabled=1 AND embedding IS NOT NULL",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type scored struct {
		entry types.KnowledgeEntry
		score float32
	}
	var results []scored

	for rows.Next() {
		var e types.KnowledgeEntry
		var embData []byte
		if err := rows.Scan(&e.ID, &e.Title, &e.Content, &e.Tags, &e.Compressed, &e.Enabled, &embData, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		e.HasEmbedding = true

		if len(allowedTags) > 0 && !entryMatchesTags(e.Tags, allowedTags) {
			continue
		}

		vec := decodeEmbedding(embData)
		if vec == nil {
			continue
		}
		sim := cosineSimilarity(queryVec, vec)
		results = append(results, scored{entry: e, score: sim})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})

	if len(results) > limit {
		results = results[:limit]
	}

	entries := make([]types.KnowledgeEntry, len(results))
	for i, s := range results {
		entries[i] = s.entry
	}
	if r.logger != nil {
		titles := make([]string, len(entries))
		scores := make([]float32, len(entries))
		for i, s := range results {
			titles[i] = s.entry.Title
			scores[i] = s.score
		}
		r.logger.Log("rag_embedding_search", chatID, map[string]any{
			"results": len(entries), "titles": titles, "scores": scores,
			"allowed_tags": allowedTags,
		})
	}
	return entries, nil
}

// reciprocalRankFusion merges FTS5 and embedding results using RRF (k=60).
func reciprocalRankFusion(ftsResults, embResults []types.KnowledgeEntry, limit int) []types.KnowledgeEntry {
	scores := map[int64]float64{}
	entryMap := map[int64]types.KnowledgeEntry{}

	for i, e := range ftsResults {
		scores[e.ID] += 1.0 / float64(60+i)
		entryMap[e.ID] = e
	}
	for i, e := range embResults {
		scores[e.ID] += 1.0 / float64(60+i)
		entryMap[e.ID] = e
	}

	type scored struct {
		id    int64
		score float64
	}
	var ranked []scored
	for id, s := range scores {
		ranked = append(ranked, scored{id: id, score: s})
	}
	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].score > ranked[j].score
	})

	if len(ranked) > limit {
		ranked = ranked[:limit]
	}

	entries := make([]types.KnowledgeEntry, len(ranked))
	for i, r := range ranked {
		entries[i] = entryMap[r.id]
	}
	return entries
}

// HybridSearch combines FTS5 and embedding search using RRF.
// FTS5 and embedding run concurrently. Falls back to FTS5 if embedding fails.
func (r *RAG) HybridSearch(chatID string, a *ai.AI, embModel, query string, maxResults int, useEmbeddings bool, allowedTags ...string) ([]types.KnowledgeEntry, error) {
	if !useEmbeddings {
		return r.Search(chatID, query, maxResults, allowedTags...)
	}

	var (
		wg         sync.WaitGroup
		ftsResults []types.KnowledgeEntry
		ftsErr     error
		embResults []types.KnowledgeEntry
		embOK      bool
	)

	// FTS5 search (goroutine)
	wg.Add(1)
	go func() {
		defer wg.Done()
		ftsResults, ftsErr = r.Search(chatID, query, maxResults*2, allowedTags...)
	}()

	// Embed query + vector search (goroutine)
	wg.Add(1)
	go func() {
		defer wg.Done()
		queryVec, err := a.Embed(embModel, query)
		if err != nil {
			return
		}
		results, err := r.searchByEmbedding(chatID, queryVec, maxResults*2, allowedTags...)
		if err != nil {
			return
		}
		embResults = results
		embOK = true
	}()

	wg.Wait()

	if ftsErr != nil {
		return nil, ftsErr
	}

	// If embedding failed, return FTS5 only
	if !embOK {
		if len(ftsResults) > maxResults {
			ftsResults = ftsResults[:maxResults]
		}
		return ftsResults, nil
	}

	merged := reciprocalRankFusion(ftsResults, embResults, maxResults)
	if r.logger != nil {
		titles := make([]string, len(merged))
		for i, e := range merged {
			titles[i] = e.Title
		}
		r.logger.Log("rag_hybrid_search", chatID, map[string]any{
			"query": query, "fts_count": len(ftsResults), "emb_count": len(embResults),
			"merged_count": len(merged), "titles": titles, "allowed_tags": allowedTags,
		})
	}
	return merged, nil
}

// GetSchemaDescription returns a text description of the next.db schema for the report tool.
func (r *RAG) GetSchemaDescription() string {
	return `messages (id, chat_id, role, content, session_id, created_at, wa_msg_id, read_at)
summaries (id, chat_id, session_id, content, created_at)
knowledge (id, title, content, tags, compressed, embedding, enabled, created_at, updated_at)
tasks (id, chat_id, description, done, created_at, done_at)
notes (id, chat_id, key, value, created_at)
scheduled_messages (id, chat_id, message, send_at, sent, recurrence, recurrence_end, created_at)
custom_tools (id, name, description, method, url_template, headers, body_template, parameters, response_path, max_bytes, enabled, created_at)
mcp_servers (id, name, url, api_key, enabled, created_at)
external_databases (id, name, driver, host, port, username, dbname, ssl_mode, max_rows, enabled, created_at)
agents (id, name, description, system_prompt, user_prompt, model, max_tokens, base_url, enabled, chain_to, chain_condition, created_at)
agent_routing (id, chat_id, agent_id, created_at)
users (id, username, role, created_at)
sessions (token, user_id, username, role, expiry, created_at)
logs (id, event, chat_id, data, created_at)
config (id, key, value)`
}

// StripHTML removes HTML tags, scripts, styles, and decodes entities from text.
func StripHTML(s string) string {
	s = reStripScript.ReplaceAllString(s, " ")
	s = reStripStyle.ReplaceAllString(s, " ")
	s = reStripTags.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.Join(strings.Fields(s), " ")
}

// StripHTMLAll removes HTML from all knowledge entries' content.
// Returns the number of entries that were modified.
func (r *RAG) StripHTMLAll() (int, error) {
	rows, err := r.db.Query("SELECT id, content FROM knowledge")
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type entry struct {
		id      int64
		content string
	}
	var pending []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.id, &e.content); err != nil {
			return 0, err
		}
		// Only process entries that look like they contain HTML
		if strings.Contains(e.content, "<") {
			pending = append(pending, e)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	count := 0
	for _, e := range pending {
		cleaned := StripHTML(e.content)
		if cleaned == e.content {
			continue
		}
		// Clear compressed and embedding since content changed
		if _, err := r.db.Exec(
			"UPDATE knowledge SET content=?, compressed='', embedding=NULL, updated_at=unixepoch() WHERE id=?",
			cleaned, e.id,
		); err != nil {
			return count, fmt.Errorf("strip html id %d: %w", e.id, err)
		}
		count++
	}
	return count, nil
}

// sanitizeFTS5Query splits raw input into safe quoted OR terms for FTS5.
func sanitizeFTS5Query(raw string) string {
	words := strings.Fields(raw)
	var terms []string
	for _, w := range words {
		// Remove FTS5 special characters
		clean := strings.Map(func(r rune) rune {
			if r == '"' || r == '*' || r == '+' || r == '-' || r == '(' || r == ')' || r == '{' || r == '}' || r == '^' || r == ':' {
				return -1
			}
			return r
		}, w)
		if len(clean) < 2 {
			continue
		}
		terms = append(terms, `"`+clean+`"`)
	}
	return strings.Join(terms, " OR ")
}
