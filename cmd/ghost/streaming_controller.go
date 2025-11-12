package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/andreykaipov/goobs"
	"github.com/andreykaipov/goobs/api/requests/scenes"
	"github.com/andreykaipov/goobs/api/requests/stream"
)

type StreamingController struct {
	mu     sync.Mutex
	cfg    StreamingConfig
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewStreamingController() *StreamingController {
	return &StreamingController{}
}

func (c *StreamingController) Apply(cfg StreamingConfig) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !cfg.active() {
		if c.cfg.active() {
			c.stopLocked()
		}
		c.cfg = StreamingConfig{}
		return nil
	}

	if c.cfg.active() && streamingConfigsEqual(c.cfg, cfg) {
		return nil
	}

	c.stopLocked()
	if err := c.startLocked(cfg); err != nil {
		return err
	}
	c.cfg = cfg
	return nil
}

func (c *StreamingController) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopLocked()
	c.cfg = StreamingConfig{}
}

func (c *StreamingController) startLocked(cfg StreamingConfig) error {
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.wg.Add(1)
	go c.run(ctx, cfg)
	logInfo("streaming monitor enabled (%d excluded app(s))", len(cfg.ExcludedApplications))
	return nil
}

func (c *StreamingController) stopLocked() {
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	c.wg.Wait()
}

func (c *StreamingController) run(ctx context.Context, cfg StreamingConfig) {
	defer c.wg.Done()

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	var (
		client       *goobs.Client
		currentScene string
		privacyOn    bool
	)

	reconnectDelay := 2 * time.Second

	for {
		if client == nil {
			select {
			case <-ctx.Done():
				return
			default:
			}

			var err error
			client, err = c.connectOBS(cfg)
			if err != nil {
				logError("streaming: obs connect failed: %v", err)
				if !waitForContext(ctx, reconnectDelay) {
					return
				}
				continue
			}
			logInfo("streaming: connected to OBS at %s://%s", cfg.OBSScheme, cfg.OBSHost)
			currentScene = ""
			if cfg.AutoStart {
				if err := ensureStreamRunning(client); err != nil {
					logError("streaming: failed to start stream: %v", err)
				}
			}
		}

		select {
		case <-ctx.Done():
			disconnectOBS(client)
			return
		case <-ticker.C:
			privacyNeeded, offenders, err := evaluatePrivacy(cfg)
			if err != nil {
				logError("streaming: window snapshot failed: %v", err)
				continue
			}
			targetScene := cfg.LiveScene
			if privacyNeeded {
				targetScene = cfg.PrivacyScene
			}
			if currentScene != targetScene {
				if err := switchScene(client, targetScene); err != nil {
					logError("streaming: switch scene failed: %v", err)
					disconnectOBS(client)
					client = nil
					continue
				}
				currentScene = targetScene
				if privacyNeeded {
					logInfo("streaming: privacy scene (%s)", strings.Join(offenders, ", "))
				} else if privacyOn {
					logInfo("streaming: resumed %s", cfg.LiveScene)
				} else {
					logInfo("streaming: scene set to %s", cfg.LiveScene)
				}
			}
			privacyOn = privacyNeeded
		}
	}
}

func (c *StreamingController) connectOBS(cfg StreamingConfig) (*goobs.Client, error) {
	opts := []goobs.Option{goobs.WithScheme(cfg.OBSScheme)}
	if cfg.OBSPassword != "" {
		opts = append(opts, goobs.WithPassword(cfg.OBSPassword))
	}
	return goobs.New(cfg.OBSHost, opts...)
}

func ensureStreamRunning(client *goobs.Client) error {
	if client == nil {
		return errors.New("obs client is nil")
	}
	status, err := client.Stream.GetStreamStatus(&stream.GetStreamStatusParams{})
	if err != nil {
		return err
	}
	if status.OutputActive {
		return nil
	}
	_, err = client.Stream.StartStream(&stream.StartStreamParams{})
	return err
}

func switchScene(client *goobs.Client, scene string) error {
	if client == nil {
		return errors.New("obs client is nil")
	}
	_, err := client.Scenes.SetCurrentProgramScene(
		scenes.NewSetCurrentProgramSceneParams().WithSceneName(scene),
	)
	return err
}

func evaluatePrivacy(cfg StreamingConfig) (bool, []string, error) {
	snapshots, err := captureWindowSnapshot()
	if err != nil {
		return false, nil, err
	}
	if len(cfg.ExcludedApplications) == 0 {
		return false, nil, nil
	}

	seen := make(map[string]struct{})
	var offenders []string
	var frontmost string
	for _, snap := range snapshots {
		if snap.layer != 0 || !snap.onScreen {
			continue
		}
		if frontmost == "" {
			frontmost = snap.ownerName
			if cfg.PrivacyMode == "frontmost" {
				if cfg.excludesApp(frontmost) {
					return true, []string{frontmost}, nil
				}
				return false, nil, nil
			}
		}
		if cfg.excludesApp(snap.ownerName) {
			name := snap.ownerName
			if _, ok := seen[name]; !ok {
				seen[name] = struct{}{}
				offenders = append(offenders, name)
			}
		}
	}
	return len(offenders) > 0, offenders, nil
}

func disconnectOBS(client *goobs.Client) {
	if client == nil {
		return
	}
	if err := client.Disconnect(); err != nil {
		logError("streaming: failed to disconnect OBS: %v", err)
	}
}

func waitForContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
