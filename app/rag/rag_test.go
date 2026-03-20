package rag

import (
	"database/sql"
	"math"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"next/app/types"
)

// openTestDB opens an in-memory SQLite database with FTS5 support.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", "file::memory:?cache=shared&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

// newTestRAG creates a RAG instance backed by an in-memory SQLite database.
func newTestRAG(t *testing.T) *RAG {
	t.Helper()
	db := openTestDB(t)
	r, err := NewRAG(db)
	if err != nil {
		t.Fatalf("NewRAG: %v", err)
	}
	return r
}

// ---------------------------------------------------------------------------
// CRUD tests
// ---------------------------------------------------------------------------

func TestNewRAG(t *testing.T) {
	db := openTestDB(t)
	r, err := NewRAG(db)
	if err != nil {
		t.Fatalf("NewRAG failed: %v", err)
	}
	if r == nil {
		t.Fatal("expected non-nil RAG")
	}
}

func TestAddAndSearch(t *testing.T) {
	r := newTestRAG(t)

	id, err := r.AddEntry("Golang basics", "Go is a statically typed language", "go,programming")
	if err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive ID, got %d", id)
	}

	results, err := r.Search("", "golang", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Title != "Golang basics" {
		t.Errorf("expected title 'Golang basics', got %q", results[0].Title)
	}
}

func TestSearchMultipleEntries(t *testing.T) {
	r := newTestRAG(t)

	r.AddEntry("Python tutorial", "Python is a dynamic language", "python")
	r.AddEntry("Go tutorial", "Go is a compiled language", "go")
	r.AddEntry("Rust tutorial", "Rust is a systems language", "rust")

	results, err := r.Search("", "tutorial", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
}

func TestSearchLimit(t *testing.T) {
	r := newTestRAG(t)

	r.AddEntry("Entry one", "content about alpha", "tag")
	r.AddEntry("Entry two", "content about alpha", "tag")
	r.AddEntry("Entry three", "content about alpha", "tag")

	results, err := r.Search("", "alpha", 2)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
}

func TestSearchEmptyQuery(t *testing.T) {
	r := newTestRAG(t)

	r.AddEntry("Test", "content", "tag")

	results, err := r.Search("", "", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results for empty query, got %d", len(results))
	}
}

func TestDeleteEntry(t *testing.T) {
	r := newTestRAG(t)

	id, _ := r.AddEntry("To delete", "this will be deleted", "temp")

	err := r.DeleteEntry(id)
	if err != nil {
		t.Fatalf("DeleteEntry: %v", err)
	}

	// FTS5 should no longer find it
	results, err := r.Search("", "deleted", 10)
	if err != nil {
		t.Fatalf("Search after delete: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results after delete, got %d", len(results))
	}
}

func TestUpdateEntry(t *testing.T) {
	r := newTestRAG(t)

	id, _ := r.AddEntry("Original title", "original content", "tag1")

	err := r.UpdateEntry(id, "Updated title", "updated content", "tag2", true)
	if err != nil {
		t.Fatalf("UpdateEntry: %v", err)
	}

	// Old content should not be found
	results, err := r.Search("", "original", 10)
	if err != nil {
		t.Fatalf("Search old: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results for old content, got %d", len(results))
	}

	// New content should be found
	results, err = r.Search("", "updated", 10)
	if err != nil {
		t.Fatalf("Search new: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for new content, got %d", len(results))
	}
	if results[0].Title != "Updated title" {
		t.Errorf("expected title 'Updated title', got %q", results[0].Title)
	}
}

func TestUpdateEntryDisabled(t *testing.T) {
	r := newTestRAG(t)

	id, _ := r.AddEntry("Visible entry", "findable content here", "tag")

	// Disable the entry
	err := r.UpdateEntry(id, "Visible entry", "findable content here", "tag", false)
	if err != nil {
		t.Fatalf("UpdateEntry: %v", err)
	}

	// Search only returns enabled entries
	results, err := r.Search("", "findable", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results for disabled entry, got %d", len(results))
	}
}

func TestListEntries(t *testing.T) {
	r := newTestRAG(t)

	r.AddEntry("First", "content one", "a")
	r.AddEntry("Second", "content two", "b")

	entries, err := r.ListEntries()
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
}

// ---------------------------------------------------------------------------
// Embedding encode/decode roundtrip
// ---------------------------------------------------------------------------

func TestEncodeDecodeEmbedding(t *testing.T) {
	original := []float32{0.1, 0.2, -0.3, 1.0, 0.0, -1.0, 3.14}

	encoded := encodeEmbedding(original)
	decoded := decodeEmbedding(encoded)

	if len(decoded) != len(original) {
		t.Fatalf("length mismatch: got %d, want %d", len(decoded), len(original))
	}
	for i := range original {
		if decoded[i] != original[i] {
			t.Errorf("index %d: got %f, want %f", i, decoded[i], original[i])
		}
	}
}

func TestDecodeEmbeddingEmpty(t *testing.T) {
	result := decodeEmbedding(nil)
	if result != nil {
		t.Errorf("expected nil for nil input, got %v", result)
	}

	result = decodeEmbedding([]byte{})
	if result != nil {
		t.Errorf("expected nil for empty input, got %v", result)
	}
}

func TestDecodeEmbeddingInvalidLength(t *testing.T) {
	// Not a multiple of 4
	result := decodeEmbedding([]byte{1, 2, 3})
	if result != nil {
		t.Errorf("expected nil for invalid length, got %v", result)
	}
}

func TestEncodeDecodeSpecialValues(t *testing.T) {
	special := []float32{
		float32(math.Inf(1)),
		float32(math.Inf(-1)),
		float32(math.NaN()),
		0.0,
		-0.0,
		math.MaxFloat32,
		math.SmallestNonzeroFloat32,
	}

	encoded := encodeEmbedding(special)
	decoded := decodeEmbedding(encoded)

	if len(decoded) != len(special) {
		t.Fatalf("length mismatch: got %d, want %d", len(decoded), len(special))
	}

	// Check non-NaN values
	for i := range special {
		if i == 2 {
			// NaN != NaN, so check with IsNaN
			if !math.IsNaN(float64(decoded[i])) {
				t.Errorf("index %d: expected NaN, got %f", i, decoded[i])
			}
			continue
		}
		if decoded[i] != special[i] {
			t.Errorf("index %d: got %f, want %f", i, decoded[i], special[i])
		}
	}
}

// ---------------------------------------------------------------------------
// Cosine similarity
// ---------------------------------------------------------------------------

func TestCosineSimilarityIdentical(t *testing.T) {
	a := []float32{1.0, 2.0, 3.0}
	sim := cosineSimilarity(a, a)
	if math.Abs(float64(sim)-1.0) > 1e-6 {
		t.Errorf("expected ~1.0 for identical vectors, got %f", sim)
	}
}

func TestCosineSimilarityOrthogonal(t *testing.T) {
	a := []float32{1.0, 0.0}
	b := []float32{0.0, 1.0}
	sim := cosineSimilarity(a, b)
	if math.Abs(float64(sim)) > 1e-6 {
		t.Errorf("expected ~0.0 for orthogonal vectors, got %f", sim)
	}
}

func TestCosineSimilarityOpposite(t *testing.T) {
	a := []float32{1.0, 2.0, 3.0}
	b := []float32{-1.0, -2.0, -3.0}
	sim := cosineSimilarity(a, b)
	if math.Abs(float64(sim)+1.0) > 1e-6 {
		t.Errorf("expected ~-1.0 for opposite vectors, got %f", sim)
	}
}

func TestCosineSimilarityKnown(t *testing.T) {
	// cos([1,0,0], [1,1,0]) = 1/sqrt(2) ~ 0.7071
	a := []float32{1.0, 0.0, 0.0}
	b := []float32{1.0, 1.0, 0.0}
	sim := cosineSimilarity(a, b)
	expected := float32(1.0 / math.Sqrt(2.0))
	if math.Abs(float64(sim-expected)) > 1e-5 {
		t.Errorf("expected %f, got %f", expected, sim)
	}
}

func TestCosineSimilarityDifferentLengths(t *testing.T) {
	a := []float32{1.0, 2.0}
	b := []float32{1.0, 2.0, 3.0}
	sim := cosineSimilarity(a, b)
	if sim != 0 {
		t.Errorf("expected 0 for different-length vectors, got %f", sim)
	}
}

func TestCosineSimilarityEmpty(t *testing.T) {
	sim := cosineSimilarity([]float32{}, []float32{})
	if sim != 0 {
		t.Errorf("expected 0 for empty vectors, got %f", sim)
	}
}

func TestCosineSimilarityZeroVector(t *testing.T) {
	a := []float32{0.0, 0.0, 0.0}
	b := []float32{1.0, 2.0, 3.0}
	sim := cosineSimilarity(a, b)
	if sim != 0 {
		t.Errorf("expected 0 for zero vector, got %f", sim)
	}
}

// ---------------------------------------------------------------------------
// Reciprocal rank fusion
// ---------------------------------------------------------------------------

func TestReciprocalRankFusionDisjoint(t *testing.T) {
	fts := []types.KnowledgeEntry{
		{ID: 1, Title: "A"},
		{ID: 2, Title: "B"},
	}
	emb := []types.KnowledgeEntry{
		{ID: 3, Title: "C"},
		{ID: 4, Title: "D"},
	}

	result := reciprocalRankFusion(fts, emb, 10)
	if len(result) != 4 {
		t.Fatalf("expected 4 results, got %d", len(result))
	}
	// All entries from rank 0 have equal score (1/60), so just check all IDs are present
	ids := map[int64]bool{}
	for _, e := range result {
		ids[e.ID] = true
	}
	for _, id := range []int64{1, 2, 3, 4} {
		if !ids[id] {
			t.Errorf("missing entry ID %d", id)
		}
	}
}

func TestReciprocalRankFusionOverlap(t *testing.T) {
	// Entry 1 appears in both lists at rank 0 => highest score
	fts := []types.KnowledgeEntry{
		{ID: 1, Title: "Overlap"},
		{ID: 2, Title: "FTS only"},
	}
	emb := []types.KnowledgeEntry{
		{ID: 1, Title: "Overlap"},
		{ID: 3, Title: "Emb only"},
	}

	result := reciprocalRankFusion(fts, emb, 10)
	if len(result) != 3 {
		t.Fatalf("expected 3 results, got %d", len(result))
	}
	// Entry 1 should be first because it has score from both lists
	if result[0].ID != 1 {
		t.Errorf("expected ID 1 first (highest RRF score), got ID %d", result[0].ID)
	}
}

func TestReciprocalRankFusionLimit(t *testing.T) {
	fts := []types.KnowledgeEntry{
		{ID: 1, Title: "A"},
		{ID: 2, Title: "B"},
		{ID: 3, Title: "C"},
	}
	emb := []types.KnowledgeEntry{
		{ID: 4, Title: "D"},
		{ID: 5, Title: "E"},
	}

	result := reciprocalRankFusion(fts, emb, 3)
	if len(result) != 3 {
		t.Fatalf("expected 3 results (limited), got %d", len(result))
	}
}

func TestReciprocalRankFusionEmpty(t *testing.T) {
	result := reciprocalRankFusion(nil, nil, 10)
	if len(result) != 0 {
		t.Fatalf("expected 0 results for empty inputs, got %d", len(result))
	}
}

func TestReciprocalRankFusionOneEmpty(t *testing.T) {
	fts := []types.KnowledgeEntry{
		{ID: 1, Title: "A"},
		{ID: 2, Title: "B"},
	}

	result := reciprocalRankFusion(fts, nil, 10)
	if len(result) != 2 {
		t.Fatalf("expected 2 results, got %d", len(result))
	}
}

// ---------------------------------------------------------------------------
// FormatContextFromEntriesWithBudget
// ---------------------------------------------------------------------------

func TestFormatContextFromEntriesWithBudgetEmpty(t *testing.T) {
	result := FormatContextFromEntriesWithBudget(nil, false, 100)
	if result != "" {
		t.Errorf("expected empty for nil entries, got %q", result)
	}
}

func TestFormatContextFromEntriesWithBudgetFitsAll(t *testing.T) {
	entries := []types.KnowledgeEntry{
		{Title: "A", Content: "short content"},
		{Title: "B", Content: "another short"},
	}
	result := FormatContextFromEntriesWithBudget(entries, false, 1000)
	if !strings.Contains(result, "### A") || !strings.Contains(result, "### B") {
		t.Errorf("expected both entries in result, got %q", result)
	}
	if !strings.Contains(result, "Base de conhecimento relevante:") {
		t.Error("expected header in result")
	}
}

func TestFormatContextFromEntriesWithBudgetTruncatesFirst(t *testing.T) {
	// Create an entry that exceeds a small budget
	bigContent := strings.Repeat("word ", 500) // ~2500 chars
	entries := []types.KnowledgeEntry{
		{Title: "Big Entry", Content: bigContent},
	}
	// Budget: 100 tokens * 4 = 400 chars
	result := FormatContextFromEntriesWithBudget(entries, false, 100)
	if result == "" {
		t.Fatal("expected non-empty result when first entry is truncated")
	}
	if !strings.Contains(result, "### Big Entry") {
		t.Error("expected title in truncated result")
	}
	if len(result) > 400 {
		t.Errorf("expected result within budget (~400 chars), got %d chars", len(result))
	}
}

func TestFormatContextFromEntriesWithBudgetReturnsEmptyForTinyBudget(t *testing.T) {
	entries := []types.KnowledgeEntry{
		{Title: "Entry", Content: "some content"},
	}
	// Budget: 5 tokens * 4 = 20 chars — header alone is ~35 chars, remaining < 50
	result := FormatContextFromEntriesWithBudget(entries, false, 5)
	if result != "" {
		t.Errorf("expected empty for tiny budget, got %q", result)
	}
}

func TestFormatContextFromEntriesWithBudgetUsesCompressed(t *testing.T) {
	entries := []types.KnowledgeEntry{
		{Title: "Entry", Content: strings.Repeat("long ", 200), Compressed: "short compressed version"},
	}
	result := FormatContextFromEntriesWithBudget(entries, true, 1000)
	if !strings.Contains(result, "short compressed version") {
		t.Error("expected compressed content to be used")
	}
	if strings.Contains(result, "long long") {
		t.Error("expected full content NOT to be used when compressed is available")
	}
}

func TestFormatContextFromEntriesWithBudgetPartialEntries(t *testing.T) {
	entries := []types.KnowledgeEntry{
		{Title: "First", Content: "first content here"},
		{Title: "Second", Content: "second content here"},
		{Title: "Third", Content: strings.Repeat("big ", 500)},
	}
	// Budget: 50 tokens * 4 = 200 chars — fits first two entries but not third
	result := FormatContextFromEntriesWithBudget(entries, false, 50)
	if !strings.Contains(result, "### First") {
		t.Error("expected first entry")
	}
	// The third entry should not be fully included
	if strings.Contains(result, "### Third") && len(result) > 200 {
		t.Error("expected result to respect budget")
	}
}

// ---------------------------------------------------------------------------
// Search populates Compressed field
// ---------------------------------------------------------------------------

func TestSearchPopulatesCompressed(t *testing.T) {
	r := newTestRAG(t)

	id, err := r.AddEntry("Test Entry", "searchable content here", "tag")
	if err != nil {
		t.Fatalf("AddEntry: %v", err)
	}

	// Manually set compressed version
	_, err = r.db.Exec("UPDATE knowledge SET compressed='compressed version' WHERE id=?", id)
	if err != nil {
		t.Fatalf("update compressed: %v", err)
	}

	results, err := r.Search("", "searchable", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Compressed != "compressed version" {
		t.Errorf("expected Compressed='compressed version', got %q", results[0].Compressed)
	}
}

// ---------------------------------------------------------------------------
// StripHTML
// ---------------------------------------------------------------------------

func TestStripHTML(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain text", "hello world", "hello world"},
		{"simple tags", "<p>hello</p> <b>world</b>", "hello world"},
		{"script removal", "before<script>alert('xss')</script>after", "before after"},
		{"style removal", "text<style>.foo{color:red}</style>more", "text more"},
		{"entities", "&amp; &lt; &gt; &quot;", "& < > \""},
		{"nested tags", "<div><p>deep <em>nested</em></p></div>", "deep nested"},
		{"empty", "", ""},
		{"full page", "<html><head><title>T</title><style>body{}</style></head><body><h1>Hello</h1><script>x()</script><p>World</p></body></html>", "T Hello World"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := StripHTML(tc.input)
			if got != tc.want {
				t.Errorf("StripHTML(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestStripHTMLAll(t *testing.T) {
	r := newTestRAG(t)

	// Add entries: one with HTML, one without
	r.AddEntry("HTML Entry", "<p>Hello <b>World</b></p>", "tag")
	r.AddEntry("Plain Entry", "Just text", "tag")

	count, err := r.StripHTMLAll()
	if err != nil {
		t.Fatalf("StripHTMLAll: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 cleaned, got %d", count)
	}

	// Verify the HTML entry was cleaned
	entries, err := r.ListEntries()
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	for _, e := range entries {
		if e.Title == "HTML Entry" && strings.Contains(e.Content, "<") {
			t.Errorf("HTML entry still contains tags: %q", e.Content)
		}
		if e.Title == "Plain Entry" && e.Content != "Just text" {
			t.Errorf("plain entry was modified: %q", e.Content)
		}
	}
}

func TestStripHTMLAllClearsCompressed(t *testing.T) {
	r := newTestRAG(t)

	id, _ := r.AddEntry("Entry", "<div>content</div>", "tag")
	// Set compressed version
	r.db.Exec("UPDATE knowledge SET compressed='old compressed' WHERE id=?", id)

	r.StripHTMLAll()

	var compressed string
	r.db.QueryRow("SELECT compressed FROM knowledge WHERE id=?", id).Scan(&compressed)
	if compressed != "" {
		t.Errorf("expected compressed to be cleared, got %q", compressed)
	}
}

// ---------------------------------------------------------------------------
// sanitizeFTS5Query
// ---------------------------------------------------------------------------

func TestSanitizeFTS5Query(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello world", `"hello" OR "world"`},
		{"", ""},
		{"a b", ""},     // single-char words are dropped
		{"go*", `"go"`}, // special chars stripped
		{`"test" foo`, `"test" OR "foo"`},
	}
	for _, tc := range tests {
		got := sanitizeFTS5Query(tc.input)
		if got != tc.want {
			t.Errorf("sanitizeFTS5Query(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// TruncateContext
// ---------------------------------------------------------------------------

func TestTruncateContext(t *testing.T) {
	text := "line one\nline two\nline three\nline four"

	// maxTokens=0 returns original
	if got := TruncateContext(text, 0); got != text {
		t.Errorf("expected original for maxTokens=0, got %q", got)
	}

	// Empty text returns empty
	if got := TruncateContext("", 10); got != "" {
		t.Errorf("expected empty for empty text, got %q", got)
	}

	// Large enough limit returns original
	if got := TruncateContext(text, 1000); got != text {
		t.Errorf("expected original for large limit, got %q", got)
	}

	// Small limit truncates at newline
	result := TruncateContext(text, 3) // 3*4=12 chars
	if len(result) > 12 {
		t.Errorf("expected truncated result, got len=%d: %q", len(result), result)
	}
}

func TestListEntriesPaginated(t *testing.T) {
	r := newTestRAG(t)

	for i := 0; i < 5; i++ {
		_, err := r.AddEntry("Entry", "content", "tag")
		if err != nil {
			t.Fatalf("AddEntry %d: %v", i, err)
		}
	}

	entries, total, err := r.ListEntriesPaginated(2, 0)
	if err != nil {
		t.Fatalf("ListEntriesPaginated(2,0): %v", err)
	}
	if total != 5 {
		t.Errorf("total = %d, want 5", total)
	}
	if len(entries) != 2 {
		t.Errorf("len(entries) = %d, want 2", len(entries))
	}

	entries, total, err = r.ListEntriesPaginated(2, 4)
	if err != nil {
		t.Fatalf("ListEntriesPaginated(2,4): %v", err)
	}
	if total != 5 {
		t.Errorf("total = %d, want 5", total)
	}
	if len(entries) != 1 {
		t.Errorf("len(entries) = %d, want 1", len(entries))
	}
}
