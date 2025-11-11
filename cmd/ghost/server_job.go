package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

type serverJob struct {
	cfg NormalizedServer

	stopCh chan struct{}
	doneCh chan struct{}

	mu        sync.Mutex
	cmd       *exec.Cmd
	pty       *os.File
	closed    bool
	killTimer *time.Timer
}

func newServerJob(cfg NormalizedServer) (*serverJob, error) {
	job := &serverJob{
		cfg:    cfg,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	go job.run()
	return job, nil
}

func (j *serverJob) run() {
	defer close(j.doneCh)

	for {
		err := j.launchOnce()
		if err != nil && !j.isClosed() {
			logError("%s failed: %v", j.prefix(), err)
		}

		if j.isClosed() || !j.cfg.Restart {
			return
		}

		if !j.waitForRestart() {
			return
		}
	}
}

func (j *serverJob) launchOnce() error {
	if j.isClosed() {
		return nil
	}

	logFile, err := j.openLogFile()
	if err != nil {
		return err
	}
	defer logFile.Close()

	header := fmt.Sprintf("\n--- [%s] ghost server %s starting: %s ---\n",
		time.Now().Format(time.RFC3339), j.cfg.Name, j.cfg.CommandDisplay)
	if _, err := logFile.WriteString(header); err != nil {
		return fmt.Errorf("write log header: %w", err)
	}

	lockedLog := &lockedWriter{w: logFile}

	cmd := exec.Command(j.cfg.Command[0], j.cfg.Command[1:]...)
	cmd.Dir = j.cfg.Cwd
	cmd.Env = buildEnvList(j.cfg.Env)
	cmd.Stdin = nil

	logInfo("%s starting %s", j.prefix(), j.cfg.CommandDisplay)

	var (
		wg      sync.WaitGroup
		ptmx    *os.File
		waitErr error
	)

	if j.cfg.UsePTY {
		ptmx, err = pty.Start(cmd)
		if err != nil {
			return fmt.Errorf("start command: %w", err)
		}
		j.setProcess(cmd, ptmx)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := io.Copy(io.MultiWriter(lockedLog, os.Stdout), ptmx); err != nil && !errors.Is(err, os.ErrClosed) && !j.isClosed() {
				logError("%s stream error: %v", j.prefix(), err)
			}
		}()
		waitErr = cmd.Wait()
		_ = ptmx.Close()
		wg.Wait()
	} else {
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return fmt.Errorf("stdout pipe: %w", err)
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return fmt.Errorf("stderr pipe: %w", err)
		}
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("start command: %w", err)
		}
		j.setProcess(cmd, nil)

		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := io.Copy(io.MultiWriter(lockedLog, os.Stdout), stdout); err != nil && !errors.Is(err, os.ErrClosed) && !j.isClosed() {
				logError("%s stdout stream error: %v", j.prefix(), err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := io.Copy(io.MultiWriter(lockedLog, os.Stderr), stderr); err != nil && !errors.Is(err, os.ErrClosed) && !j.isClosed() {
				logError("%s stderr stream error: %v", j.prefix(), err)
			}
		}()

		waitErr = cmd.Wait()
		wg.Wait()
	}

	j.clearProcess()

	if waitErr != nil && !j.isClosed() {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			logError("%s exited with code %d", j.prefix(), exitErr.ExitCode())
		} else {
			logError("%s exited: %v", j.prefix(), waitErr)
		}
	} else if waitErr == nil {
		logInfo("%s exited cleanly", j.prefix())
	}

	return waitErr
}

func (j *serverJob) waitForRestart() bool {
	delay := j.cfg.RestartDelay
	if delay <= 0 {
		delay = defaultRestartDelay
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return !j.isClosed()
	case <-j.stopCh:
		return false
	}
}

func (j *serverJob) openLogFile() (*os.File, error) {
	if strings.TrimSpace(j.cfg.LogPath) == "" {
		return nil, errors.New("log path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(j.cfg.LogPath), 0o755); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	file, err := os.OpenFile(j.cfg.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	return file, nil
}

func (j *serverJob) setProcess(cmd *exec.Cmd, pty *os.File) {
	j.mu.Lock()
	j.cmd = cmd
	j.pty = pty
	j.mu.Unlock()
}

func (j *serverJob) clearProcess() {
	j.mu.Lock()
	if j.killTimer != nil {
		j.killTimer.Stop()
		j.killTimer = nil
	}
	j.cmd = nil
	j.pty = nil
	j.mu.Unlock()
}

func (j *serverJob) stopProcessLocked() {
	if j.cmd == nil || j.cmd.Process == nil {
		if j.pty != nil {
			_ = j.pty.Close()
			j.pty = nil
		}
		return
	}

	process := j.cmd.Process
	if j.pty != nil {
		_ = j.pty.Close()
		j.pty = nil
	}
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
	if j.killTimer != nil {
		j.killTimer.Stop()
	}
	j.killTimer = timer
}

func (j *serverJob) Close() error {
	j.mu.Lock()
	if j.closed {
		j.mu.Unlock()
		<-j.doneCh
		return nil
	}
	j.closed = true
	close(j.stopCh)
	j.stopProcessLocked()
	j.mu.Unlock()

	<-j.doneCh
	return nil
}

func (j *serverJob) isClosed() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.closed
}

func (j *serverJob) prefix() string {
	return "ghost:server:" + j.cfg.Name
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}
