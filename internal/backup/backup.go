package backup

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Backup manages SQLite backups with rotation.
type Backup struct {
	mu       sync.Mutex
	db       *sql.DB
	dir      string
	maxCount int
	interval time.Duration
	stopCh   chan struct{}
	stopped  chan struct{}
}

// BackupInfo describes a single backup file.
type BackupInfo struct {
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	CreatedAt string `json:"created_at"`
}

// New creates a Backup manager.
func New(db *sql.DB, dataDir string, maxCount int, intervalHours int) *Backup {
	dir := filepath.Join(dataDir, "backups")
	os.MkdirAll(dir, 0700)
	return &Backup{
		db:       db,
		dir:      dir,
		maxCount: maxCount,
		interval: time.Duration(intervalHours) * time.Hour,
		stopCh:   make(chan struct{}),
		stopped:  make(chan struct{}),
	}
}

// Start begins the periodic backup scheduler.
func (b *Backup) Start() {
	go func() {
		defer close(b.stopped)
		ticker := time.NewTicker(b.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				b.Run()
			case <-b.stopCh:
				return
			}
		}
	}()
}

// Stop halts the backup scheduler.
func (b *Backup) Stop() {
	close(b.stopCh)
	<-b.stopped
}

// Run performs a backup now and rotates old ones.
func (b *Backup) Run() (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	name := fmt.Sprintf("next-backup-%s.db", time.Now().Format("2006-01-02-150405"))
	dest := filepath.Join(b.dir, name)

	_, err := b.db.Exec(fmt.Sprintf("VACUUM INTO '%s'", strings.ReplaceAll(dest, "'", "''")))
	if err != nil {
		return "", fmt.Errorf("backup VACUUM INTO: %w", err)
	}

	// Rotate old backups
	b.rotate()

	return name, nil
}

// List returns all backup files sorted newest first.
func (b *Backup) List() ([]BackupInfo, error) {
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		return nil, err
	}

	var backups []BackupInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "next-backup-") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		backups = append(backups, BackupInfo{
			Name:      e.Name(),
			Size:      info.Size(),
			CreatedAt: info.ModTime().Format(time.RFC3339),
		})
	}

	sort.Slice(backups, func(i, j int) bool {
		return backups[i].Name > backups[j].Name
	})

	return backups, nil
}

func (b *Backup) rotate() {
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		return
	}

	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "next-backup-") {
			files = append(files, e.Name())
		}
	}

	sort.Strings(files)

	// Delete oldest if over maxCount
	for len(files) > b.maxCount {
		os.Remove(filepath.Join(b.dir, files[0]))
		files = files[1:]
	}
}
