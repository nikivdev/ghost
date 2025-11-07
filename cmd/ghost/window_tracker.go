package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var errWindowEnumerationUnavailable = errors.New("window enumeration unavailable on this platform")

type windowSnapshot struct {
	ownerName   string
	windowTitle string
	windowID    uint64
	layer       int
}

type WindowTracker struct {
	mu        sync.Mutex
	cfg       WindowTrackerConfig
	db        *sql.DB
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	sessions  map[uint64]*windowSession
	appLookup map[string]string
	trackAll  bool
}

type windowSession struct {
	rowID       int64
	windowID    uint64
	appName     string
	windowTitle string
	openTime    time.Time
}

func NewWindowTracker() *WindowTracker {
	return &WindowTracker{}
}

func (t *WindowTracker) Apply(cfg WindowTrackerConfig) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !cfg.active() {
		if t.cfg.active() {
			logInfo("window tracker disabled")
		}
		t.stopLocked()
		t.cfg = WindowTrackerConfig{}
		return nil
	}

	if t.cfg.active() && windowTrackerConfigsEqual(t.cfg, cfg) {
		return nil
	}

	t.stopLocked()
	if err := t.startLocked(cfg); err != nil {
		return err
	}
	t.cfg = cfg
	return nil
}

func (t *WindowTracker) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopLocked()
	t.cfg = WindowTrackerConfig{}
}

func (t *WindowTracker) startLocked(cfg WindowTrackerConfig) error {
	if err := ensureWindowEnumerationAvailable(); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o755); err != nil {
		return fmt.Errorf("create db directory: %w", err)
	}

	db, err := sql.Open("sqlite", cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open sqlite db: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := initWindowTrackerSchema(db); err != nil {
		_ = db.Close()
		return err
	}

	t.db = db
	t.sessions = make(map[uint64]*windowSession)
	t.trackAll = cfg.TrackAll
	if !cfg.TrackAll {
		t.appLookup = make(map[string]string, len(cfg.Applications))
		for _, app := range cfg.Applications {
			t.appLookup[strings.ToLower(app)] = app
		}
	} else {
		t.appLookup = nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	t.wg.Add(1)
	go t.run(ctx, cfg.PollInterval)

	target := fmt.Sprintf("%d application(s)", len(cfg.Applications))
	if cfg.TrackAll {
		target = "all applications"
	}
	logInfo("window tracker tracking %s → %s", target, cfg.DBPath)
	return nil
}

func (t *WindowTracker) stopLocked() {
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
	t.wg.Wait()
	if t.db != nil {
		_ = t.db.Close()
		t.db = nil
	}
	t.sessions = nil
	t.appLookup = nil
	t.trackAll = false
}

func (t *WindowTracker) run(ctx context.Context, pollInterval time.Duration) {
	defer t.wg.Done()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			t.closeAllSessions(time.Now())
			return
		case <-ticker.C:
			if err := t.pollOnce(time.Now()); err != nil {
				if errors.Is(err, errWindowEnumerationUnavailable) {
					logError("window tracker stopped: %v", err)
					t.closeAllSessions(time.Now())
					return
				}
				logError("window tracker poll failed: %v", err)
			}
		}
	}
}

func (t *WindowTracker) pollOnce(now time.Time) error {
	snapshots, err := captureWindowSnapshot()
	if err != nil {
		return err
	}

	seen := make(map[uint64]struct{}, len(snapshots))
	for _, snap := range snapshots {
		if snap.layer != 0 || snap.windowID == 0 {
			continue
		}
		var (
			appName string
			ok      bool
		)
		if t.trackAll {
			appName = snap.ownerName
			ok = true
		} else {
			appName, ok = t.appLookup[strings.ToLower(snap.ownerName)]
		}
		if !ok {
			continue
		}
		title := normalizeWindowTitle(snap.windowTitle)
		seen[snap.windowID] = struct{}{}

		if session, exists := t.sessions[snap.windowID]; exists {
			if session.windowTitle != title {
				if err := t.updateWindowTitle(session.rowID, title); err != nil {
					logError("window tracker failed to update title: %v", err)
				} else {
					session.windowTitle = title
				}
			}
			continue
		}

		rowID, err := t.insertSession(appName, title, snap.windowID, now)
		if err != nil {
			logError("window tracker failed to insert session: %v", err)
			continue
		}
		t.sessions[snap.windowID] = &windowSession{
			rowID:       rowID,
			windowID:    snap.windowID,
			appName:     appName,
			windowTitle: title,
			openTime:    now,
		}
	}

	for id, session := range t.sessions {
		if _, ok := seen[id]; ok {
			continue
		}
		if err := t.closeSession(session.rowID, now); err != nil {
			logError("window tracker failed to close session: %v", err)
		}
		delete(t.sessions, id)
	}

	return nil
}

func (t *WindowTracker) closeAllSessions(now time.Time) {
	for id, session := range t.sessions {
		if err := t.closeSession(session.rowID, now); err != nil {
			logError("window tracker failed to close session %d: %v", id, err)
		}
		delete(t.sessions, id)
	}
}

func (t *WindowTracker) insertSession(appName, title string, windowID uint64, openedAt time.Time) (int64, error) {
	result, err := t.db.Exec(
		`INSERT INTO window_sessions (app_name, window_title, window_id, opened_at) VALUES (?, ?, ?, ?)`,
		appName,
		title,
		windowID,
		openedAt.UTC(),
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (t *WindowTracker) updateWindowTitle(rowID int64, title string) error {
	_, err := t.db.Exec(`UPDATE window_sessions SET window_title = ? WHERE id = ?`, title, rowID)
	return err
}

func (t *WindowTracker) closeSession(rowID int64, closedAt time.Time) error {
	_, err := t.db.Exec(`UPDATE window_sessions SET closed_at = COALESCE(closed_at, ?) WHERE id = ?`, closedAt.UTC(), rowID)
	return err
}

func initWindowTrackerSchema(db *sql.DB) error {
	statements := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA synchronous = NORMAL;",
		"PRAGMA foreign_keys = ON;",
		"PRAGMA busy_timeout = 5000;",
	}
	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("initialize window tracker db (%s): %w", strings.TrimSpace(stmt), err)
		}
	}

	schema := []string{
		`CREATE TABLE IF NOT EXISTS window_sessions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			app_name TEXT NOT NULL,
			window_title TEXT,
			window_id INTEGER NOT NULL,
			opened_at TIMESTAMP NOT NULL,
			closed_at TIMESTAMP
		);`,
		`CREATE INDEX IF NOT EXISTS idx_window_sessions_app_opened ON window_sessions(app_name, opened_at);`,
		`CREATE INDEX IF NOT EXISTS idx_window_sessions_window_id ON window_sessions(window_id, opened_at);`,
	}

	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("initialize window tracker schema: %w", err)
		}
	}
	return nil
}

func ensureWindowEnumerationAvailable() error {
	_, err := captureWindowSnapshot()
	if err == nil {
		return nil
	}
	if errors.Is(err, errWindowEnumerationUnavailable) {
		return fmt.Errorf("window tracking unsupported: %w", err)
	}
	return err
}

func normalizeWindowTitle(title string) string {
	return strings.TrimSpace(title)
}

func (cfg WindowTrackerConfig) active() bool {
	return cfg.Enabled && cfg.DBPath != "" && cfg.PollInterval > 0 && (cfg.TrackAll || len(cfg.Applications) > 0)
}

func windowTrackerConfigsEqual(a, b WindowTrackerConfig) bool {
	if a.Enabled != b.Enabled || a.DBPath != b.DBPath || a.PollInterval != b.PollInterval || a.TrackAll != b.TrackAll {
		return false
	}
	if len(a.Applications) != len(b.Applications) {
		return false
	}
	for i := range a.Applications {
		if a.Applications[i] != b.Applications[i] {
			return false
		}
	}
	return true
}
