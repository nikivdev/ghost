package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

const configEnvVar = "GHOST_CONFIG"

func main() {
	configPath, err := determineConfigPath()
	if err != nil {
		logError("failed to determine config path: %v", err)
		os.Exit(1)
	}

	daemon := NewGhostDaemon(configPath)
	if err := daemon.Start(); err != nil {
		logError("failed to start daemon: %v", err)
		os.Exit(1)
	}

	logInfo("ghost daemon watching %s", configPath)

	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)

	sig := <-signalCh
	logInfo("received %s, shutting down", sig)

	daemon.Stop()
}

func determineConfigPath() (string, error) {
	if override := strings.TrimSpace(os.Getenv(configEnvVar)); override != "" {
		resolved, err := resolvePath(override)
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", override, err)
		}
		return resolved, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}

	return filepath.Join(home, ".config", "ghost", "ghost.toml"), nil
}
