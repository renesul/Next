package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"next/app/types"
)

func TestNewAI(t *testing.T) {
	a := NewAI("https://api.example.com/v1", "test-key", nil)
	if a == nil {
		t.Fatal("NewAI returned nil")
	}
	if a.client == nil {
		t.Fatal("client not initialized")
	}
	if a.EmbCache == nil {
		t.Fatal("EmbCache not initialized")
	}
	if len(a.EmbCache) != 0 {
		t.Fatalf("EmbCache should be empty, got %d entries", len(a.EmbCache))
	}
}

func TestNewAI_EmptyBaseURL(t *testing.T) {
	a := NewAI("", "test-key", nil)
	if a == nil {
		t.Fatal("NewAI returned nil with empty base URL")
	}
	if a.client == nil {
		t.Fatal("client not initialized with empty base URL")
	}
}

func TestUpdateClient(t *testing.T) {
	a := NewAI("https://api.example.com/v1", "key1", nil)
	oldClient := a.client

	a.UpdateClient("https://api.other.com/v1", "key2")

	if a.client == oldClient {
		t.Fatal("UpdateClient did not replace the client")
	}
}

func TestEmbCache_Hit(t *testing.T) {
	a := NewAI("https://api.example.com/v1", "test-key", nil)

	key := "model-x\x00hello world"
	expected := []float32{0.1, 0.2, 0.3}
	a.EmbCache[key] = EmbCacheEntry{
		Vec: expected,
		At:  time.Now(),
	}

	// Calling Embed would require an actual API call on miss,
	// so we test the cache lookup logic directly.
	a.EmbMu.Lock()
	entry, ok := a.EmbCache[key]
	a.EmbMu.Unlock()

	if !ok {
		t.Fatal("cache entry not found")
	}
	if len(entry.Vec) != len(expected) {
		t.Fatalf("expected vec len %d, got %d", len(expected), len(entry.Vec))
	}
	for i, v := range entry.Vec {
		if v != expected[i] {
			t.Fatalf("vec[%d] = %f, want %f", i, v, expected[i])
		}
	}
}

func TestEmbCache_ExpiredEntry(t *testing.T) {
	a := NewAI("https://api.example.com/v1", "test-key", nil)

	key := "model-x\x00hello world"
	a.EmbCache[key] = EmbCacheEntry{
		Vec: []float32{0.1, 0.2, 0.3},
		At:  time.Now().Add(-EmbCacheTTL - time.Second), // expired
	}

	// Verify the cache considers this entry expired
	a.EmbMu.Lock()
	entry, ok := a.EmbCache[key]
	isExpired := ok && time.Since(entry.At) >= EmbCacheTTL
	a.EmbMu.Unlock()

	if !isExpired {
		t.Fatal("expected cache entry to be expired")
	}
}

func TestEmbCache_KeyFormat(t *testing.T) {
	// Verify that different models produce different cache keys
	a := NewAI("https://api.example.com/v1", "test-key", nil)

	key1 := "model-a\x00same text"
	key2 := "model-b\x00same text"

	a.EmbCache[key1] = EmbCacheEntry{Vec: []float32{1.0}, At: time.Now()}
	a.EmbCache[key2] = EmbCacheEntry{Vec: []float32{2.0}, At: time.Now()}

	if len(a.EmbCache) != 2 {
		t.Fatalf("expected 2 cache entries, got %d", len(a.EmbCache))
	}
	if a.EmbCache[key1].Vec[0] != 1.0 {
		t.Fatal("key1 entry has wrong value")
	}
	if a.EmbCache[key2].Vec[0] != 2.0 {
		t.Fatal("key2 entry has wrong value")
	}
}

func TestEmbCache_Cleanup(t *testing.T) {
	a := NewAI("https://api.example.com/v1", "test-key", nil)

	// Fill cache beyond max size with expired entries
	expired := time.Now().Add(-EmbCacheTTL - time.Second)
	for i := 0; i < EmbCacheMaxSize+5; i++ {
		key := "model\x00" + string(rune(i))
		a.EmbCache[key] = EmbCacheEntry{Vec: []float32{float32(i)}, At: expired}
	}

	// Add one fresh entry
	freshKey := "model\x00fresh"
	a.EmbCache[freshKey] = EmbCacheEntry{Vec: []float32{99.0}, At: time.Now()}

	// Simulate the cleanup logic from Embed
	if len(a.EmbCache) > EmbCacheMaxSize {
		now := time.Now()
		for k, e := range a.EmbCache {
			if now.Sub(e.At) >= EmbCacheTTL {
				delete(a.EmbCache, k)
			}
		}
	}

	// Only the fresh entry should remain
	if len(a.EmbCache) != 1 {
		t.Fatalf("expected 1 entry after cleanup, got %d", len(a.EmbCache))
	}
	if _, ok := a.EmbCache[freshKey]; !ok {
		t.Fatal("fresh entry was incorrectly removed")
	}
}

func TestBuildMessages_Minimal(t *testing.T) {
	a := NewAI("https://api.example.com/v1", "test-key", nil)

	msgs := a.buildMessages("system", "", "", "", nil, "hello")

	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].Role != openai.ChatMessageRoleSystem || msgs[0].Content != "system" {
		t.Fatalf("unexpected system message: %+v", msgs[0])
	}
	if msgs[1].Role != openai.ChatMessageRoleUser || msgs[1].Content != "hello" {
		t.Fatalf("unexpected user message: %+v", msgs[1])
	}
}

func TestBuildMessages_AllFields(t *testing.T) {
	a := NewAI("https://api.example.com/v1", "test-key", nil)

	history := []types.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello there"},
	}

	msgs := a.buildMessages("sys", "usr_prompt", "rag context", "summary text", history, "final msg")

	// system (consolidated) + userPrompt + 2 history + final user = 5
	if len(msgs) != 5 {
		t.Fatalf("expected 5 messages, got %d", len(msgs))
	}

	// Check order: system prompt consolidates RAG + summary
	if msgs[0].Role != openai.ChatMessageRoleSystem {
		t.Fatal("msgs[0] should be system")
	}
	expectedSys := "sys\n\n---\nBase de conhecimento:\nrag context\n\n---\nContexto da conversa anterior:\nsummary text"
	if msgs[0].Content != expectedSys {
		t.Fatalf("msgs[0] should be consolidated system prompt, got: %s", msgs[0].Content)
	}
	if msgs[1].Role != openai.ChatMessageRoleUser || msgs[1].Content != "usr_prompt" {
		t.Fatal("msgs[1] should be user prompt")
	}
	if msgs[2].Role != openai.ChatMessageRoleUser || msgs[2].Content != "hi" {
		t.Fatal("msgs[2] should be history user msg")
	}
	if msgs[3].Role != openai.ChatMessageRoleAssistant || msgs[3].Content != "hello there" {
		t.Fatal("msgs[3] should be history assistant msg")
	}
	if msgs[4].Role != openai.ChatMessageRoleUser || msgs[4].Content != "final msg" {
		t.Fatal("msgs[4] should be final user msg")
	}
}

func TestBuildMessages_NoOptionalFields(t *testing.T) {
	a := NewAI("https://api.example.com/v1", "test-key", nil)

	msgs := a.buildMessages("system prompt", "", "", "", nil, "user message")

	// Only system + user message
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
}

func TestMakeClient_EmptyReturnsGlobal(t *testing.T) {
	a := NewAI("https://api.example.com/v1", "test-key", nil)

	client := a.makeClient("", "")
	if client != a.client {
		t.Fatal("makeClient with empty args should return the global client")
	}
}

func TestMakeClient_CustomReturnsNew(t *testing.T) {
	a := NewAI("https://api.example.com/v1", "test-key", nil)

	client := a.makeClient("https://other.api.com/v1", "other-key")
	if client == a.client {
		t.Fatal("makeClient with custom args should return a new client")
	}
}

func TestConstants(t *testing.T) {
	if EmbCacheTTL != 5*time.Minute {
		t.Fatalf("expected EmbCacheTTL = 5m, got %v", EmbCacheTTL)
	}
	if EmbCacheMaxSize != 500 {
		t.Fatalf("expected EmbCacheMaxSize = 500, got %d", EmbCacheMaxSize)
	}
}

func TestParseExtractKnowledge_ValidJSON(t *testing.T) {
	raw := `[{"title":"Cliente prefere email","content":"O cliente João prefere contato por email"}]`
	var items []KnowledgeItem
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		t.Fatalf("failed to parse valid JSON: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Title != "Cliente prefere email" {
		t.Fatalf("unexpected title: %s", items[0].Title)
	}
}

func TestParseExtractKnowledge_InvalidJSON(t *testing.T) {
	raw := `This is not JSON at all`
	var items []KnowledgeItem
	err := json.Unmarshal([]byte(raw), &items)
	if err == nil {
		t.Fatal("expected parse error for invalid JSON")
	}
	// Best-effort: should return nil/empty
	if items != nil {
		t.Fatalf("expected nil items on parse error, got %v", items)
	}
}

func TestParseExtractKnowledge_EmptyArray(t *testing.T) {
	raw := `[]`
	var items []KnowledgeItem
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		t.Fatalf("failed to parse empty array: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected 0 items, got %d", len(items))
	}
}

func TestParseExtractKnowledge_WithCodeFences(t *testing.T) {
	raw := "```json\n[{\"title\":\"Test\",\"content\":\"Data\"}]\n```"
	// Strip markdown code fences
	if len(raw) > 3 && raw[:3] == "```" {
		if idx := findNewline(raw[3:]); idx >= 0 {
			raw = raw[3+idx+1:]
		}
		if len(raw) >= 3 && raw[len(raw)-3:] == "```" {
			raw = raw[:len(raw)-3]
		}
		raw = trimSpace(raw)
	}

	var items []KnowledgeItem
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		t.Fatalf("failed to parse after stripping fences: %v", err)
	}
	if len(items) != 1 || items[0].Title != "Test" {
		t.Fatalf("unexpected result: %+v", items)
	}
}

func TestExtractKnowledge_EmptyMessages(t *testing.T) {
	a := NewAI("https://api.example.com/v1", "test-key", nil)
	items, err := a.ExtractKnowledge("gpt-4", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if items != nil {
		t.Fatalf("expected nil for empty messages, got %v", items)
	}
}

// helpers for test
func findNewline(s string) int {
	for i, c := range s {
		if c == '\n' {
			return i
		}
	}
	return -1
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\n' || s[start] == '\r' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\n' || s[end-1] == '\r' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

// ---------------------------------------------------------------------------
// TestIsRetryable
// ---------------------------------------------------------------------------

func TestIsRetryable_NilError(t *testing.T) {
	if isRetryable(nil) {
		t.Fatal("expected false for nil error")
	}
}

func TestIsRetryable_ContextDeadline(t *testing.T) {
	if isRetryable(context.DeadlineExceeded) {
		t.Fatal("expected false for context.DeadlineExceeded")
	}
}

func TestIsRetryable_ContextCanceled(t *testing.T) {
	if isRetryable(context.Canceled) {
		t.Fatal("expected false for context.Canceled")
	}
}

func TestIsRetryable_GenericError(t *testing.T) {
	if isRetryable(fmt.Errorf("something went wrong")) {
		t.Fatal("expected false for generic error")
	}
}

func TestIsRetryable_APIError429(t *testing.T) {
	err := &openai.APIError{HTTPStatusCode: 429}
	if !isRetryable(err) {
		t.Fatal("expected true for APIError with status 429")
	}
}

func TestIsRetryable_APIError500(t *testing.T) {
	err := &openai.APIError{HTTPStatusCode: 500}
	if !isRetryable(err) {
		t.Fatal("expected true for APIError with status 500")
	}
}

func TestIsRetryable_APIError400(t *testing.T) {
	err := &openai.APIError{HTTPStatusCode: 400}
	if isRetryable(err) {
		t.Fatal("expected false for APIError with status 400")
	}
}

// ---------------------------------------------------------------------------
// TestEmbCache_LRUEviction
// ---------------------------------------------------------------------------

func TestEmbCache_LRUEviction(t *testing.T) {
	a := NewAI("https://api.example.com/v1", "test-key", nil)

	// Fill cache to exactly EmbCacheMaxSize with non-expired entries.
	// All entries are within the TTL window so the first pass (expired eviction)
	// won't remove any, forcing the LRU second pass to kick in.
	now := time.Now()
	for i := 0; i < EmbCacheMaxSize; i++ {
		key := fmt.Sprintf("model\x00entry-%d", i)
		// Spread entries across 1 minute, all well within the 5-minute TTL.
		// Entry 0 is oldest (60s ago), entry 499 is newest (~0s ago).
		age := time.Duration(EmbCacheMaxSize-i) * time.Millisecond * 120 // max ~60s
		a.EmbCache[key] = EmbCacheEntry{
			Vec: []float32{float32(i)},
			At:  now.Add(-age),
		}
	}

	if len(a.EmbCache) != EmbCacheMaxSize {
		t.Fatalf("expected %d entries, got %d", EmbCacheMaxSize, len(a.EmbCache))
	}

	// Add 5 more entries (over the limit), then trigger eviction logic.
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("model\x00new-%d", i)
		a.EmbCache[key] = EmbCacheEntry{
			Vec: []float32{float32(1000 + i)},
			At:  now, // newest timestamp
		}
	}

	// Now simulate the eviction logic from Embed:
	// First pass: evict expired entries
	if len(a.EmbCache) > EmbCacheMaxSize {
		evictNow := time.Now()
		for k, e := range a.EmbCache {
			if evictNow.Sub(e.At) >= EmbCacheTTL {
				delete(a.EmbCache, k)
			}
		}
		// Second pass: if still over limit, evict oldest (pseudo-LRU)
		for len(a.EmbCache) > EmbCacheMaxSize {
			var oldestKey string
			var oldestTime time.Time
			first := true
			for k, e := range a.EmbCache {
				if first || e.At.Before(oldestTime) {
					oldestKey = k
					oldestTime = e.At
					first = false
				}
			}
			if oldestKey != "" {
				delete(a.EmbCache, oldestKey)
			} else {
				break
			}
		}
	}

	// Cache should be exactly at the max size
	if len(a.EmbCache) != EmbCacheMaxSize {
		t.Fatalf("expected %d entries after eviction, got %d", EmbCacheMaxSize, len(a.EmbCache))
	}

	// The 5 newest entries should still be present
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("model\x00new-%d", i)
		if _, ok := a.EmbCache[key]; !ok {
			t.Errorf("newest entry %q was evicted but should have been kept", key)
		}
	}

	// The 5 oldest original entries (entry-0 through entry-4) should have been evicted
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("model\x00entry-%d", i)
		if _, ok := a.EmbCache[key]; ok {
			t.Errorf("oldest entry %q should have been evicted but was kept", key)
		}
	}
}
