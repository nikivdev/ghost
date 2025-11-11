package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	toml "github.com/pelletier/go-toml/v2"
)

const (
	defaultDebounce     = 150 * time.Millisecond
	defaultRestartDelay = 200 * time.Millisecond
	defaultKillTimeout  = 5 * time.Second
)

var allowedEvents = map[string]struct{}{
	"add":       {},
	"addDir":    {},
	"change":    {},
	"rename":    {},
	"renameDir": {},
	"unlink":    {},
	"unlinkDir": {},
}

type rawConfig struct {
	Defaults      rawDefaults      `toml:"defaults"`
	Watchers      []rawWatcher     `toml:"watchers"`
	Servers       []rawServer      `toml:"servers"`
	WindowTracker rawWindowTracker `toml:"window_tracker"`
}

type rawDefaults struct {
	DebounceMs     *int64   `toml:"debounce_ms"`
	RestartDelayMs *int64   `toml:"restart_delay_ms"`
	KillTimeoutMs  *int64   `toml:"kill_timeout_ms"`
	Events         []string `toml:"events"`
}

type rawWatcher struct {
	Name           string            `toml:"name"`
	Path           any               `toml:"path"`
	Directory      any               `toml:"directory"`
	Command        any               `toml:"command"`
	Args           any               `toml:"args"`
	Cwd            any               `toml:"cwd"`
	Env            map[string]any    `toml:"env"`
	Match          any               `toml:"match"`
	Matches        any               `toml:"matches"`
	Events         []string          `toml:"events"`
	Restart        *bool             `toml:"restart"`
	RunOnStart     *bool             `toml:"run_on_start"`
	DebounceMs     *int64            `toml:"debounce_ms"`
	RestartDelayMs *int64            `toml:"restart_delay_ms"`
	KillTimeoutMs  *int64            `toml:"kill_timeout_ms"`
	Shell          *bool             `toml:"shell"`
	EnvOverrides   map[string]string `toml:"-"`
}

type rawServer struct {
	Name           string         `toml:"name"`
	Command        any            `toml:"command"`
	Args           any            `toml:"args"`
	Cwd            any            `toml:"cwd"`
	Env            map[string]any `toml:"env"`
	Restart        *bool          `toml:"restart"`
	RestartDelayMs *int64         `toml:"restart_delay_ms"`
	KillTimeoutMs  *int64         `toml:"kill_timeout_ms"`
	Shell          *bool          `toml:"shell"`
	LogPath        any            `toml:"log_path"`
	Pty            *bool          `toml:"pty"`
}

type rawWindowTracker struct {
	Enabled        *bool  `toml:"enabled"`
	Applications   any    `toml:"applications"`
	PollIntervalMs *int64 `toml:"poll_interval_ms"`
	DBPath         string `toml:"db_path"`
}

type NormalizedConfig struct {
	Watchers      []NormalizedWatcher
	Servers       []NormalizedServer
	WindowTracker WindowTrackerConfig
}

type matcher struct {
	raw string
	re  *regexp.Regexp
}

func (m matcher) matches(value string) bool {
	return m.re.MatchString(value)
}

type NormalizedWatcher struct {
	ID             string
	Name           string
	WatchRoot      string
	WatchPattern   string
	Command        []string
	CommandDisplay string
	Env            map[string]string
	Cwd            string
	Matchers       []matcher
	Events         map[string]struct{}
	Restart        bool
	RunOnStart     bool
	Debounce       time.Duration
	RestartDelay   time.Duration
	KillTimeout    time.Duration
	UseShell       bool
	SingleFile     string
}

type NormalizedServer struct {
	ID             string
	Name           string
	Command        []string
	CommandDisplay string
	Env            map[string]string
	Cwd            string
	Restart        bool
	RestartDelay   time.Duration
	KillTimeout    time.Duration
	UseShell       bool
	UsePTY         bool
	LogPath        string
}

type WindowTrackerConfig struct {
	Enabled      bool
	Applications []string
	PollInterval time.Duration
	DBPath       string
	TrackAll     bool
}

type Trigger struct {
	Event string
	Path  string
}

func readConfig(path string) (NormalizedConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return NormalizedConfig{}, fmt.Errorf("read config: %w", err)
	}

	var raw rawConfig
	if err := toml.Unmarshal(data, &raw); err != nil {
		return NormalizedConfig{}, fmt.Errorf("parse config: %w", err)
	}

	return normalizeConfig(raw)
}

func normalizeConfig(raw rawConfig) (NormalizedConfig, error) {
	defaults := raw.Defaults

	if len(raw.Watchers) == 0 {
		logInfo("config contains no watchers")
	}

	result := NormalizedConfig{
		Watchers: make([]NormalizedWatcher, 0, len(raw.Watchers)),
		Servers:  make([]NormalizedServer, 0, len(raw.Servers)),
	}

	for i, watcher := range raw.Watchers {
		normalized, err := normalizeWatcher(watcher, i, defaults)
		if err != nil {
			return NormalizedConfig{}, err
		}
		result.Watchers = append(result.Watchers, normalized)
	}

	for i, server := range raw.Servers {
		normalized, err := normalizeServer(server, i, defaults)
		if err != nil {
			return NormalizedConfig{}, err
		}
		result.Servers = append(result.Servers, normalized)
	}

	tracker, err := normalizeWindowTracker(raw.WindowTracker)
	if err != nil {
		return NormalizedConfig{}, err
	}
	result.WindowTracker = tracker

	return result, nil
}

func normalizeWatcher(raw rawWatcher, index int, defaults rawDefaults) (NormalizedWatcher, error) {
	name := strings.TrimSpace(raw.Name)
	if name == "" {
		name = fmt.Sprintf("watcher-%d", index+1)
	}

	pathValue, err := choosePath(raw)
	if err != nil {
		return NormalizedWatcher{}, fmt.Errorf("watchers[%d]: %w", index, err)
	}
	resolvedPath, err := resolvePath(pathValue)
	if err != nil {
		return NormalizedWatcher{}, fmt.Errorf("watchers[%d]: resolve path: %w", index, err)
	}

	info, err := os.Stat(resolvedPath)
	if err != nil {
		return NormalizedWatcher{}, fmt.Errorf("watchers[%d]: %w", index, err)
	}

	var (
		watchRoot   string
		singleFile  string
		targetIsDir = info.IsDir()
	)

	if targetIsDir {
		watchRoot = resolvedPath
	} else {
		watchRoot = filepath.Dir(resolvedPath)
		singleFile = filepath.Base(resolvedPath)
	}

	if watchRoot == "" {
		return NormalizedWatcher{}, fmt.Errorf("watchers[%d]: resolved root is empty", index)
	}

	rootInfo, err := os.Stat(watchRoot)
	if err != nil {
		return NormalizedWatcher{}, fmt.Errorf("watchers[%d]: %w", index, err)
	}
	if !rootInfo.IsDir() {
		return NormalizedWatcher{}, fmt.Errorf("watchers[%d]: watch root %s is not a directory", index, watchRoot)
	}

	commandParts, displayParts, err := parseCommandSpec(raw.Command, raw.Args)
	if err != nil {
		return NormalizedWatcher{}, fmt.Errorf("watchers[%d]: %w", index, err)
	}
	if len(commandParts) == 0 {
		return NormalizedWatcher{}, fmt.Errorf("watchers[%d]: command must not be empty", index)
	}

	env, err := normalizeEnv(raw.Env)
	if err != nil {
		return NormalizedWatcher{}, fmt.Errorf("watchers[%d]: invalid env: %w", index, err)
	}

	cwd := watchRoot
	if str, ok := valueToString(raw.Cwd); ok {
		resolved, err := resolvePath(str)
		if err != nil {
			return NormalizedWatcher{}, fmt.Errorf("watchers[%d]: resolve cwd: %w", index, err)
		}
		cwd = resolved
	}

	matchers, err := compileMatchers(raw, singleFile)
	if err != nil {
		return NormalizedWatcher{}, fmt.Errorf("watchers[%d]: %w", index, err)
	}

	restart := valueOrDefaultBool(raw.Restart, false)
	runOnStart := restart
	if raw.RunOnStart != nil {
		runOnStart = *raw.RunOnStart
	}

	debounce := chooseDuration(raw.DebounceMs, defaults.DebounceMs, defaultDebounce)
	restartDelay := chooseDuration(raw.RestartDelayMs, defaults.RestartDelayMs, defaultRestartDelay)
	killTimeout := chooseDuration(raw.KillTimeoutMs, defaults.KillTimeoutMs, defaultKillTimeout)

	events := normalizeEvents(raw.Events, defaults.Events, restart)

	useShell := valueOrDefaultBool(raw.Shell, false)
	commandDisplay := joinDisplayParts(displayParts)

	commandExec := make([]string, len(commandParts))
	copy(commandExec, commandParts)

	if useShell {
		commandDisplay = buildShellCommand(displayParts)
		commandExec = []string{defaultShell(), "-lc", commandDisplay}
	}

	return NormalizedWatcher{
		ID:             fmt.Sprintf("watchers[%d]", index),
		Name:           name,
		WatchRoot:      watchRoot,
		WatchPattern:   filepath.Join(watchRoot, "..."),
		Command:        commandExec,
		CommandDisplay: commandDisplay,
		Env:            env,
		Cwd:            cwd,
		Matchers:       matchers,
		Events:         events,
		Restart:        restart,
		RunOnStart:     runOnStart,
		Debounce:       debounce,
		RestartDelay:   restartDelay,
		KillTimeout:    killTimeout,
		UseShell:       useShell,
		SingleFile:     singleFile,
	}, nil
}

func normalizeServer(raw rawServer, index int, defaults rawDefaults) (NormalizedServer, error) {
	name := strings.TrimSpace(raw.Name)
	if name == "" {
		name = fmt.Sprintf("server-%d", index+1)
	}

	commandParts, displayParts, err := parseCommandSpec(raw.Command, raw.Args)
	if err != nil {
		return NormalizedServer{}, fmt.Errorf("servers[%d]: %w", index, err)
	}
	if len(commandParts) == 0 {
		return NormalizedServer{}, fmt.Errorf("servers[%d]: command must not be empty", index)
	}

	env, err := normalizeEnv(raw.Env)
	if err != nil {
		return NormalizedServer{}, fmt.Errorf("servers[%d]: invalid env: %w", index, err)
	}

	cwd := ""
	if str, ok := valueToString(raw.Cwd); ok && str != "" {
		resolved, err := resolvePath(str)
		if err != nil {
			return NormalizedServer{}, fmt.Errorf("servers[%d]: resolve cwd: %w", index, err)
		}
		cwd = resolved
	} else {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		} else {
			cwd = "."
		}
	}

	restart := valueOrDefaultBool(raw.Restart, true)

	restartDelay := chooseDuration(raw.RestartDelayMs, defaults.RestartDelayMs, defaultRestartDelay)
	killTimeout := chooseDuration(raw.KillTimeoutMs, defaults.KillTimeoutMs, defaultKillTimeout)

	useShell := valueOrDefaultBool(raw.Shell, false)
	usePTY := valueOrDefaultBool(raw.Pty, true)

	logPathInput := ""
	if str, ok := valueToString(raw.LogPath); ok {
		logPathInput = str
	}
	if logPathInput == "" {
		defaultPath, err := defaultServerLogPath(name)
		if err != nil {
			return NormalizedServer{}, fmt.Errorf("servers[%d]: %w", index, err)
		}
		logPathInput = defaultPath
	}
	logPath, err := resolvePath(logPathInput)
	if err != nil {
		return NormalizedServer{}, fmt.Errorf("servers[%d]: resolve log path: %w", index, err)
	}

	commandDisplay := joinDisplayParts(displayParts)
	commandExec := make([]string, len(commandParts))
	copy(commandExec, commandParts)

	if useShell {
		commandDisplay = buildShellCommand(displayParts)
		commandExec = []string{defaultShell(), "-lc", commandDisplay}
	}

	return NormalizedServer{
		ID:             fmt.Sprintf("servers[%d]", index),
		Name:           name,
		Command:        commandExec,
		CommandDisplay: commandDisplay,
		Env:            env,
		Cwd:            cwd,
		Restart:        restart,
		RestartDelay:   restartDelay,
		KillTimeout:    killTimeout,
		UseShell:       useShell,
		UsePTY:         usePTY,
		LogPath:        logPath,
	}, nil
}

func normalizeWindowTracker(raw rawWindowTracker) (WindowTrackerConfig, error) {
	const defaultDB = "~/.db/ghost/windows.sqlite"

	appsRaw, err := valueToStringSlice(raw.Applications)
	if err != nil {
		return WindowTrackerConfig{}, fmt.Errorf("window_tracker.applications: %w", err)
	}
	apps := normalizeAppList(appsRaw)
	trackAll := len(apps) == 0

	enabled := valueOrDefaultBool(raw.Enabled, len(apps) > 0)

	pollInterval := chooseDuration(raw.PollIntervalMs, nil, time.Second)
	if pollInterval <= 0 {
		pollInterval = time.Second
	}

	dbPathInput := strings.TrimSpace(raw.DBPath)
	if dbPathInput == "" {
		dbPathInput = defaultDB
	}
	dbPath, err := resolvePath(dbPathInput)
	if err != nil {
		return WindowTrackerConfig{}, fmt.Errorf("window_tracker.db_path: %w", err)
	}

	return WindowTrackerConfig{
		Enabled:      enabled && (trackAll || len(apps) > 0),
		Applications: apps,
		PollInterval: pollInterval,
		DBPath:       dbPath,
		TrackAll:     trackAll,
	}, nil
}

func choosePath(raw rawWatcher) (string, error) {
	if str, ok := valueToString(raw.Directory); ok && str != "" {
		return str, nil
	}
	if str, ok := valueToString(raw.Path); ok && str != "" {
		return str, nil
	}
	return "", errors.New(`"path" must be provided`)
}

func normalizeEnv(env map[string]any) (map[string]string, error) {
	if env == nil {
		return map[string]string{}, nil
	}
	result := make(map[string]string, len(env))
	for key, value := range env {
		if key == "" || value == nil {
			continue
		}
		str, ok := valueToString(value)
		if !ok {
			return nil, fmt.Errorf("environment value for %s must be string", key)
		}
		result[key] = str
	}
	return result, nil
}

func compileMatchers(raw rawWatcher, singleFile string) ([]matcher, error) {
	patterns, err := valueToStringSlice(raw.Match)
	if err != nil {
		return nil, fmt.Errorf("invalid match value: %w", err)
	}

	more, err := valueToStringSlice(raw.Matches)
	if err != nil {
		return nil, fmt.Errorf("invalid matches value: %w", err)
	}

	patterns = append(patterns, more...)

	if len(patterns) == 0 && singleFile != "" {
		patterns = append(patterns, singleFile)
	}

	matchers := make([]matcher, 0, len(patterns))
	for _, pattern := range continueIfEmpty(patterns) {
		re, err := globToRegexp(pattern)
		if err != nil {
			return nil, fmt.Errorf("compile match pattern %q: %w", pattern, err)
		}
		matchers = append(matchers, matcher{raw: pattern, re: re})
	}
	return matchers, nil
}

func continueIfEmpty(patterns []string) []string {
	result := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		result = append(result, pattern)
	}
	return result
}

func normalizeAppList(apps []string) []string {
	if len(apps) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(apps))
	result := make([]string, 0, len(apps))
	for _, app := range apps {
		app = strings.TrimSpace(app)
		if app == "" {
			continue
		}
		key := strings.ToLower(app)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, app)
	}
	return result
}

func valueOrDefaultBool(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func chooseDuration(value *int64, fallback *int64, defaultValue time.Duration) time.Duration {
	if value != nil {
		return millisecondsToDuration(*value)
	}
	if fallback != nil {
		return millisecondsToDuration(*fallback)
	}
	return defaultValue
}

func millisecondsToDuration(value int64) time.Duration {
	if value <= 0 {
		return 0
	}
	return time.Duration(value) * time.Millisecond
}

func normalizeEvents(
	events []string,
	defaults []string,
	restart bool,
) map[string]struct{} {
	source := events
	if len(source) == 0 && len(defaults) > 0 {
		source = defaults
	}

	if len(source) == 0 {
		if restart {
			source = []string{"add", "addDir", "change", "rename", "renameDir", "unlink", "unlinkDir"}
		} else {
			source = []string{"change"}
		}
	}

	result := make(map[string]struct{}, len(source))
	for _, event := range source {
		event = strings.TrimSpace(event)
		if event == "" {
			continue
		}
		if _, ok := allowedEvents[event]; !ok {
			logError("ignoring unsupported event %q", event)
			continue
		}
		result[event] = struct{}{}
	}
	return result
}

func parseCommandSpec(commandValue any, argsValue any) ([]string, []string, error) {
	base, err := valueToCommandParts(commandValue)
	if err != nil {
		return nil, nil, err
	}

	args, err := valueToArgs(argsValue)
	if err != nil {
		return nil, nil, err
	}

	parts := make([]string, len(base))
	copy(parts, base)
	display := make([]string, len(base))
	copy(display, base)

	if len(parts) == 0 && len(args) > 0 {
		parts = append(parts, args[0])
		display = append(display, args[0])
		args = args[1:]
	}

	parts = append(parts, args...)
	display = append(display, args...)

	return parts, display, nil
}

func valueToCommandParts(value any) ([]string, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case string:
		return splitCommandLine(v)
	case []any:
		result := make([]string, 0, len(v))
		for _, item := range v {
			str, ok := valueToString(item)
			if !ok {
				return nil, errors.New("command array must contain strings")
			}
			if str != "" {
				result = append(result, str)
			}
		}
		return result, nil
	default:
		return nil, errors.New("command must be string or array")
	}
}

func valueToArgs(value any) ([]string, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case string:
		return splitCommandLine(v)
	case []any:
		result := make([]string, 0, len(v))
		for _, item := range v {
			str, ok := valueToString(item)
			if !ok {
				return nil, errors.New("args array must contain strings")
			}
			if str != "" {
				result = append(result, str)
			}
		}
		return result, nil
	default:
		return nil, errors.New("args must be string or array")
	}
}

func splitCommandLine(input string) ([]string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, nil
	}

	var (
		result []string
		buf    strings.Builder
		quote  rune
		escape bool
	)

	for _, r := range input {
		switch {
		case escape:
			buf.WriteRune(r)
			escape = false
		case r == '\\':
			escape = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				buf.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote = r
		case isWhitespace(r):
			if buf.Len() > 0 {
				result = append(result, buf.String())
				buf.Reset()
			}
		default:
			buf.WriteRune(r)
		}
	}

	if escape {
		buf.WriteRune('\\')
	}

	if quote != 0 {
		return nil, errors.New("unterminated quoted string")
	}

	if buf.Len() > 0 {
		result = append(result, buf.String())
	}

	return result, nil
}

func isWhitespace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r'
}

func valueToString(value any) (string, bool) {
	switch v := value.(type) {
	case nil:
		return "", false
	case string:
		return strings.TrimSpace(v), true
	case fmt.Stringer:
		return strings.TrimSpace(v.String()), true
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", v)), true
	}
}

func valueToStringSlice(value any) ([]string, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, nil
		}
		return []string{strings.TrimSpace(v)}, nil
	case []any:
		result := make([]string, 0, len(v))
		for _, item := range v {
			str, ok := valueToString(item)
			if !ok {
				return nil, errors.New("array must contain strings")
			}
			if str != "" {
				result = append(result, str)
			}
		}
		return result, nil
	default:
		return nil, errors.New("value must be string or array")
	}
}

func resolvePath(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", errors.New("path must not be empty")
	}
	if input == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home: %w", err)
		}
		return filepath.Clean(home), nil
	}
	if strings.HasPrefix(input, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home: %w", err)
		}
		return filepath.Join(home, filepath.Clean(input[2:])), nil
	}

	if filepath.IsAbs(input) {
		return filepath.Clean(input), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home: %w", err)
	}

	return filepath.Join(home, filepath.Clean(input)), nil
}

func globToRegexp(pattern string) (*regexp.Regexp, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return nil, errors.New("empty pattern")
	}

	var builder strings.Builder
	builder.WriteString("^")

	runes := []rune(pattern)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch r {
		case '*':
			if i+1 < len(runes) && runes[i+1] == '*' {
				builder.WriteString(".*")
				i++
			} else {
				builder.WriteString("[^/]*")
			}
		case '?':
			builder.WriteString("[^/]")
		case '.', '+', '(', ')', '|', '^', '$', '[', ']', '{', '}', '\\':
			builder.WriteRune('\\')
			builder.WriteRune(r)
		default:
			builder.WriteRune(r)
		}
	}

	builder.WriteString("$")
	return regexp.Compile(builder.String())
}

func joinDisplayParts(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	quoted := make([]string, len(parts))
	for i, part := range parts {
		if needsQuoting(part) {
			quoted[i] = fmt.Sprintf("%q", part)
		} else {
			quoted[i] = part
		}
	}
	return strings.Join(quoted, " ")
}

func buildShellCommand(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	escaped := make([]string, len(parts))
	for i, part := range parts {
		escaped[i] = shellQuote(part)
	}
	return strings.Join(escaped, " ")
}

func defaultShell() string {
	if shell := strings.TrimSpace(os.Getenv("SHELL")); shell != "" {
		return shell
	}
	return "/bin/sh"
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	if !strings.ContainsAny(value, " \t\n\"'`$&|;<>\\!") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func needsQuoting(value string) bool {
	if value == "" {
		return true
	}
	for _, r := range value {
		if !(r == '_' || r == '-' || r == '.' || r == '/' || r == ':' ||
			(r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9')) {
			return true
		}
	}
	return false
}

func buildEnvList(overrides map[string]string) []string {
	env := os.Environ()
	envMap := make(map[string]string, len(env)+len(overrides))

	for _, kv := range env {
		parts := strings.SplitN(kv, "=", 2)
		key := parts[0]
		value := ""
		if len(parts) == 2 {
			value = parts[1]
		}
		envMap[key] = value
	}

	for key, value := range overrides {
		envMap[key] = value
	}

	result := make([]string, 0, len(envMap))
	for key, value := range envMap {
		result = append(result, key+"="+value)
	}
	sort.Strings(result)
	return result
}

func (w NormalizedWatcher) allowsEvent(event string) bool {
	_, ok := w.Events[event]
	return ok
}

func (w NormalizedWatcher) matches(path string) bool {
	if len(w.Matchers) == 0 {
		return true
	}
	for _, matcher := range w.Matchers {
		if matcher.matches(path) {
			return true
		}
	}
	return false
}

func posixPath(input string) string {
	return strings.ReplaceAll(input, string(filepath.Separator), "/")
}

func defaultServerLogPath(name string) (string, error) {
	dir, err := defaultServersDir()
	if err != nil {
		return "", err
	}
	base := sanitizeFilename(name)
	if base == "" {
		base = "server"
	}
	return filepath.Join(dir, base+".log"), nil
}

func defaultServersDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home: %w", err)
	}
	return filepath.Join(home, ".local", "state", "ghost", "servers"), nil
}

func sanitizeFilename(input string) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return ""
	}

	var (
		builder  strings.Builder
		lastDash bool
	)

	for _, r := range input {
		lower := unicode.ToLower(r)
		switch {
		case (lower >= 'a' && lower <= 'z') || (lower >= '0' && lower <= '9'):
			builder.WriteRune(lower)
			lastDash = false
		case lower == '-' || lower == '_':
			builder.WriteRune(lower)
			lastDash = lower == '-'
		case unicode.IsSpace(lower) || lower == '/' || lower == '\\' || lower == '.':
			if !lastDash && builder.Len() > 0 {
				builder.WriteRune('-')
				lastDash = true
			}
		case unicode.IsLetter(lower) || unicode.IsDigit(lower):
			builder.WriteRune(lower)
			lastDash = false
		default:
			if !lastDash && builder.Len() > 0 {
				builder.WriteRune('-')
				lastDash = true
			}
		}
	}

	return strings.Trim(builder.String(), "-_")
}
