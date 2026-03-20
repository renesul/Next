package backup

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func setupTestDB(t *testing.T, dir string) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(dir, "next.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	// Create a table so the backup has something to work with
	_, err = db.Exec("CREATE TABLE IF NOT EXISTS test_data (id INTEGER PRIMARY KEY, value TEXT)")
	if err != nil {
		t.Fatalf("failed to create test table: %v", err)
	}
	_, err = db.Exec("INSERT INTO test_data (value) VALUES ('hello')")
	if err != nil {
		t.Fatalf("failed to insert test data: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestRun_CreatesBackupFile(t *testing.T) {
	dataDir := t.TempDir()
	db := setupTestDB(t, dataDir)

	b := New(db, dataDir, 5, 24)

	name, err := b.Run()
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if name == "" {
		t.Fatal("Run() returned empty name")
	}

	fullPath := filepath.Join(dataDir, "backups", name)
	info, err := os.Stat(fullPath)
	if err != nil {
		t.Fatalf("backup file does not exist at %s: %v", fullPath, err)
	}
	if info.Size() == 0 {
		t.Fatal("backup file is empty")
	}
}

func TestRun_Rotation(t *testing.T) {
	dataDir := t.TempDir()
	db := setupTestDB(t, dataDir)

	maxCount := 2
	b := New(db, dataDir, maxCount, 24)

	// Run 4 backups with small delays so filenames differ (timestamp-based)
	for i := 0; i < 4; i++ {
		_, err := b.Run()
		if err != nil {
			t.Fatalf("Run() iteration %d error: %v", i, err)
		}
		time.Sleep(1100 * time.Millisecond) // timestamps are second-resolution
	}

	// Check that only maxCount files remain
	backupsDir := filepath.Join(dataDir, "backups")
	entries, err := os.ReadDir(backupsDir)
	if err != nil {
		t.Fatalf("failed to read backups dir: %v", err)
	}

	var backupFiles []os.DirEntry
	for _, e := range entries {
		if !e.IsDir() {
			backupFiles = append(backupFiles, e)
		}
	}

	if len(backupFiles) != maxCount {
		names := make([]string, len(backupFiles))
		for i, f := range backupFiles {
			names[i] = f.Name()
		}
		t.Fatalf("expected %d backup files after rotation, got %d: %v", maxCount, len(backupFiles), names)
	}
}

func TestList_ReturnsNewestFirst(t *testing.T) {
	dataDir := t.TempDir()
	db := setupTestDB(t, dataDir)

	b := New(db, dataDir, 10, 24)

	// Create multiple backups with delays for distinct timestamps
	for i := 0; i < 3; i++ {
		_, err := b.Run()
		if err != nil {
			t.Fatalf("Run() iteration %d error: %v", i, err)
		}
		time.Sleep(1100 * time.Millisecond)
	}

	list, err := b.List()
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}

	if len(list) != 3 {
		t.Fatalf("expected 3 backups, got %d", len(list))
	}

	// Verify sorted newest first (Name contains timestamp, so lexicographic order descending = newest first)
	for i := 0; i < len(list)-1; i++ {
		if list[i].Name < list[i+1].Name {
			t.Fatalf("backup at index %d (%s) is older than index %d (%s) — not sorted newest first",
				i, list[i].Name, i+1, list[i+1].Name)
		}
	}
}

func TestList_Empty(t *testing.T) {
	dataDir := t.TempDir()
	db := setupTestDB(t, dataDir)

	b := New(db, dataDir, 5, 24)

	list, err := b.List()
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}

	if len(list) != 0 {
		t.Fatalf("expected empty list, got %d items", len(list))
	}
}

func TestStartStop(t *testing.T) {
	dataDir := t.TempDir()
	db := setupTestDB(t, dataDir)

	b := New(db, dataDir, 5, 24)

	// Start should not block
	b.Start()

	// Give it a moment to initialize the goroutine
	time.Sleep(100 * time.Millisecond)

	// Stop should not panic or hang
	done := make(chan struct{})
	go func() {
		b.Stop()
		close(done)
	}()

	select {
	case <-done:
		// OK
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() hung for more than 5 seconds")
	}
}
