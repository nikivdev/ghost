package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

type WatchManager struct {
	mu   sync.Mutex
	jobs []*watchJob
}

func (m *WatchManager) Apply(cfg NormalizedConfig) {
	oldJobs := m.swapJobs(nil)
	for _, job := range oldJobs {
		if job == nil {
			continue
		}
		if err := job.Close(); err != nil {
			logError("failed to stop watcher: %v", err)
		}
	}

	newJobs := make([]*watchJob, 0, len(cfg.Watchers))
	for _, watcher := range cfg.Watchers {
		job, err := newWatchJob(watcher)
		if err != nil {
			logError("failed to initialize watcher %q: %v", watcher.Name, err)
			continue
		}
		newJobs = append(newJobs, job)
	}

	m.swapJobs(newJobs)
	logInfo("loaded %d watcher(s)", len(newJobs))
}

func (m *WatchManager) StopAll() {
	jobs := m.swapJobs(nil)
	for _, job := range jobs {
		if job == nil {
			continue
		}
		if err := job.Close(); err != nil {
			logError("failed to stop watcher: %v", err)
		}
	}
}

func (m *WatchManager) swapJobs(jobs []*watchJob) []*watchJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.jobs
	m.jobs = jobs
	return old
}

type GhostDaemon struct {
	configPath    string
	manager       *WatchManager
	windowTracker *WindowTracker
	watcher       *fsnotify.Watcher
	watcherDone   chan struct{}
	reloadMu      sync.Mutex
	configFiles   map[string]struct{}
	configDirs    map[string]struct{}
	debounceTime  time.Duration
}

func NewGhostDaemon(configPath string) *GhostDaemon {
	return &GhostDaemon{
		configPath:    configPath,
		manager:       &WatchManager{},
		windowTracker: NewWindowTracker(),
		debounceTime:  150 * time.Millisecond,
	}
}

func (d *GhostDaemon) Start() error {
	if _, err := os.Stat(d.configPath); err != nil {
		return fmt.Errorf("config file not found at %s", d.configPath)
	}
	if err := d.reloadConfig(); err != nil {
		return err
	}
	return d.startConfigWatcher()
}

func (d *GhostDaemon) Stop() {
	if d.watcher != nil {
		_ = d.watcher.Close()
		if d.watcherDone != nil {
			<-d.watcherDone
			d.watcherDone = nil
		}
		d.watcher = nil
	}
	d.manager.StopAll()
	if d.windowTracker != nil {
		d.windowTracker.Stop()
	}
}

func (d *GhostDaemon) reloadConfig() error {
	d.reloadMu.Lock()
	defer d.reloadMu.Unlock()

	cfg, err := readConfig(d.configPath)
	if err != nil {
		return err
	}
	if d.windowTracker != nil {
		if err := d.windowTracker.Apply(cfg.WindowTracker); err != nil {
			return err
		}
	}
	d.manager.Apply(cfg)
	return nil
}

func (d *GhostDaemon) startConfigWatcher() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create config watcher: %w", err)
	}

	d.watcher = watcher
	d.watcherDone = make(chan struct{})
	d.configFiles = make(map[string]struct{})
	d.configDirs = make(map[string]struct{})

	paths := d.collectConfigPaths()
	for _, path := range paths {
		if path == "" {
			continue
		}
		if err := watcher.Add(path); err != nil {
			_ = watcher.Close()
			return fmt.Errorf("watch config path %s: %w", path, err)
		}
		info, err := os.Stat(path)
		if err == nil && info.IsDir() {
			d.configDirs[path] = struct{}{}
		} else {
			d.configFiles[path] = struct{}{}
		}
	}

	go d.runConfigWatcher()
	return nil
}

func (d *GhostDaemon) runConfigWatcher() {
	defer close(d.watcherDone)

	var (
		timer   *time.Timer
		timerCh <-chan time.Time
	)

	for {
		select {
		case event, ok := <-d.watcher.Events:
			if !ok {
				return
			}
			if !d.shouldReloadForEvent(event) {
				continue
			}
			if timer == nil {
				timer = time.NewTimer(d.debounceTime)
				timerCh = timer.C
			} else {
				if !timer.Stop() && timerCh != nil {
					<-timerCh
				}
				timer.Reset(d.debounceTime)
			}
		case err, ok := <-d.watcher.Errors:
			if !ok {
				return
			}
			logError("config watcher error: %v", err)
		case <-timerCh:
			if timer != nil {
				timer.Stop()
			}
			timer = nil
			timerCh = nil
			if err := d.reloadConfig(); err != nil {
				logError("failed to reload config: %v", err)
			} else {
				logInfo("reloaded config")
			}
		}
	}
}

func (d *GhostDaemon) shouldReloadForEvent(event fsnotify.Event) bool {
	if event.Name == "" {
		return false
	}
	if _, ok := d.configFiles[event.Name]; ok {
		return true
	}
	dir := filepath.Dir(event.Name)
	if _, ok := d.configDirs[dir]; ok {
		target := filepath.Join(dir, filepath.Base(d.configPath))
		if target == event.Name {
			return true
		}
	}
	if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		base := filepath.Base(event.Name)
		if base == filepath.Base(d.configPath) {
			return true
		}
	}
	return false
}

func (d *GhostDaemon) collectConfigPaths() []string {
	paths := make([]string, 0, 4)
	appendUniquePath(&paths, d.configPath)
	appendUniquePath(&paths, filepath.Dir(d.configPath))

	if resolved, err := filepath.EvalSymlinks(d.configPath); err == nil && resolved != "" && resolved != d.configPath {
		appendUniquePath(&paths, resolved)
		appendUniquePath(&paths, filepath.Dir(resolved))
	}

	return paths
}

func appendUniquePath(paths *[]string, candidate string) {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return
	}
	for _, existing := range *paths {
		if existing == candidate {
			return
		}
	}
	*paths = append(*paths, candidate)
}
