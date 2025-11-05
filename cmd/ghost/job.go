package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rjeczalik/notify"
)

type watchJob struct {
	cfg NormalizedWatcher

	events chan notify.EventInfo
	stopCh chan struct{}
	doneCh chan struct{}

	mu             sync.Mutex
	closed         bool
	running        bool
	restartQueued  bool
	cmd            *exec.Cmd
	killTimer      *time.Timer
	pending        []Trigger
	pendingRestart []Trigger
}

func newWatchJob(cfg NormalizedWatcher) (*watchJob, error) {
	events := make(chan notify.EventInfo, 128)
	if err := notify.Watch(cfg.WatchPattern, events, notify.All); err != nil {
		return nil, fmt.Errorf("watch %s: %w", cfg.WatchPattern, err)
	}

	job := &watchJob{
		cfg:    cfg,
		events: events,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}

	go job.run()

	if cfg.RunOnStart {
		go job.scheduleTriggers([]Trigger{{Event: "startup"}})
	}

	return job, nil
}

func (j *watchJob) run() {
	defer func() {
		notify.Stop(j.events)
		close(j.doneCh)
	}()

	var (
		debounceTimer *time.Timer
		debounceChan  <-chan time.Time
		pending       []Trigger
	)

	for {
		select {
		case <-j.stopCh:
			if debounceTimer != nil {
				if !debounceTimer.Stop() && debounceChan != nil {
					<-debounceChan
				}
			}
			return
		case info, ok := <-j.events:
			if !ok {
				return
			}
			triggers := j.triggersForEvent(info)
			if len(triggers) == 0 {
				continue
			}
			pending = append(pending, triggers...)
			if debounceTimer == nil {
				debounceTimer = time.NewTimer(j.cfg.Debounce)
				debounceChan = debounceTimer.C
			} else {
				if !debounceTimer.Stop() && debounceChan != nil {
					<-debounceChan
				}
				debounceTimer.Reset(j.cfg.Debounce)
			}
		case <-debounceChan:
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = nil
			debounceChan = nil
			if len(pending) > 0 {
				j.handleTriggers(pending)
				pending = nil
			}
		}
	}
}

func (j *watchJob) handleTriggers(triggers []Trigger) {
	collapsed := dedupeTriggers(triggers)
	if len(collapsed) == 0 {
		return
	}
	j.scheduleTriggers(collapsed)
}

func (j *watchJob) scheduleTriggers(triggers []Trigger) {
	if len(triggers) == 0 {
		triggers = []Trigger{{Event: "manual"}}
	}

	j.mu.Lock()
	defer j.mu.Unlock()

	if j.closed {
		return
	}

	if j.cfg.Restart {
		j.pendingRestart = append(j.pendingRestart, triggers...)
		if j.running {
			if !j.restartQueued {
				j.restartQueued = true
				logInfo("%s restart requested — %s", j.prefix(), formatTriggers(triggers))
				j.stopProcessLocked()
			} else {
				logInfo("%s coalesced restart — %s", j.prefix(), formatTriggers(triggers))
			}
			return
		}
		pending := j.pendingRestart
		j.pendingRestart = nil
		j.launchLocked(pending)
		return
	}

	if j.running {
		j.pending = append(j.pending, triggers...)
		logInfo("%s queued run — %s", j.prefix(), formatTriggers(triggers))
		return
	}

	j.launchLocked(triggers)
}

func (j *watchJob) launchLocked(triggers []Trigger) {
	if len(triggers) == 0 {
		triggers = []Trigger{{Event: "manual"}}
	}

	summary := formatTriggers(triggers)
	logInfo("%s starting %s — %s", j.prefix(), j.cfg.CommandDisplay, summary)

	cmd := exec.Command(j.cfg.Command[0], j.cfg.Command[1:]...)
	cmd.Dir = j.cfg.Cwd
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = nil
	cmd.Env = buildEnvList(j.cfg.Env)

	if err := cmd.Start(); err != nil {
		logError("%s failed to start command: %v", j.prefix(), err)
		return
	}

	j.running = true
	j.cmd = cmd

	go j.waitForExit(cmd)
}

func (j *watchJob) waitForExit(cmd *exec.Cmd) {
	err := cmd.Wait()

	j.mu.Lock()
	if j.killTimer != nil {
		j.killTimer.Stop()
		j.killTimer = nil
	}
	if j.cmd == cmd {
		j.cmd = nil
	}
	j.running = false
	closed := j.closed
	restart := j.cfg.Restart
	restartQueued := j.restartQueued
	pending := j.pending
	j.pending = nil
	pendingRestart := j.pendingRestart
	j.pendingRestart = nil
	j.restartQueued = false
	j.mu.Unlock()

	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			logError("%s process exited with code %d", j.prefix(), exitErr.ExitCode())
		} else {
			logError("%s process exited: %v", j.prefix(), err)
		}
	}

	if closed {
		return
	}

	if restart {
		var triggers []Trigger
		if len(pendingRestart) > 0 {
			triggers = pendingRestart
		} else if restartQueued || j.cfg.RunOnStart {
			triggers = []Trigger{{Event: "restart"}}
		}
		if len(triggers) > 0 {
			j.scheduleTriggers(triggers)
		}
		return
	}

	if len(pending) > 0 {
		j.scheduleTriggers(pending)
	}
}

func (j *watchJob) stopProcessLocked() {
	if j.cmd == nil || j.cmd.Process == nil {
		return
	}

	process := j.cmd.Process
	if err := process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		logError("%s failed to send SIGTERM: %v", j.prefix(), err)
	}

	timer := time.AfterFunc(j.cfg.KillTimeout, func() {
		j.mu.Lock()
		defer j.mu.Unlock()
		if j.cmd == nil || j.cmd.Process != process {
			return
		}
		if err := process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			logError("%s failed to send SIGKILL: %v", j.prefix(), err)
		} else {
			logInfo("%s forcing process exit with SIGKILL", j.prefix())
		}
	})
	j.killTimer = timer
}

func (j *watchJob) triggersForEvent(info notify.EventInfo) []Trigger {
	events := mapNotifyEvents(info.Event())
	if len(events) == 0 {
		return nil
	}

	path := info.Path()
	if path == "" {
		return nil
	}

	rel, err := filepath.Rel(j.cfg.WatchRoot, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil
	}

	rel = posixPath(rel)

	if !j.cfg.matches(rel) {
		return nil
	}

	var triggers []Trigger
	for _, event := range events {
		if j.cfg.allowsEvent(event) {
			triggers = append(triggers, Trigger{Event: event, Path: rel})
		}
	}

	return triggers
}

func (j *watchJob) Close() error {
	j.mu.Lock()
	if j.closed {
		j.mu.Unlock()
		<-j.doneCh
		return nil
	}
	j.closed = true
	j.pending = nil
	j.pendingRestart = nil
	j.restartQueued = false
	close(j.stopCh)
	j.stopProcessLocked()
	j.mu.Unlock()

	<-j.doneCh
	return nil
}

func (j *watchJob) prefix() string {
	return "ghost:" + j.cfg.Name
}

func dedupeTriggers(triggers []Trigger) []Trigger {
	if len(triggers) <= 1 {
		return triggers
	}
	seen := make(map[string]struct{}, len(triggers))
	result := make([]Trigger, 0, len(triggers))
	for _, trigger := range triggers {
		key := trigger.Event + "|" + trigger.Path
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, trigger)
	}
	return result
}

func mapNotifyEvents(event notify.Event) []string {
	var result []string
	if event&notify.Create == notify.Create {
		result = append(result, "add", "addDir")
	}
	if event&notify.Write == notify.Write {
		result = append(result, "change")
	}
	if event&notify.Remove == notify.Remove {
		result = append(result, "unlink", "unlinkDir")
	}
	if event&notify.Rename == notify.Rename {
		result = append(result, "rename", "renameDir")
	}
	return result
}

func formatTriggers(triggers []Trigger) string {
	if len(triggers) == 0 {
		return "manual trigger"
	}
	seen := make(map[string]struct{}, len(triggers))
	parts := make([]string, 0, len(triggers))
	for _, trigger := range triggers {
		label := trigger.Event
		if trigger.Path != "" {
			label = fmt.Sprintf("%s:%s", trigger.Event, trigger.Path)
		}
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		parts = append(parts, label)
	}
	if len(parts) > 4 {
		head := strings.Join(parts[:4], ", ")
		return fmt.Sprintf("%s … (+%d more)", head, len(parts)-4)
	}
	return strings.Join(parts, ", ")
}
