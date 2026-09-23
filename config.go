package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// cfgMu serializes every load→mutate→save cycle on the shared config file (see
// updateConfig). Whole-file saves are not composable, so concurrent writers must
// funnel through it.
var cfgMu sync.Mutex

// updateConfig serializes a load→mutate→save cycle on the shared config file.
// Every writer must go through it: the doctor loop and the Telegram update
// handlers mutate the config concurrently (profiles, model), and racing
// whole-file saves silently drop each other's changes.
// mutate runs on a freshly-loaded copy; return true to persist it. Returns the
// fresh (possibly mutated) config, or nil if the config could not be loaded
// or saved. Prefer commitConfig so the in-memory swap happens under cfgMu.
func updateConfig(mutate func(*Config) bool) *Config {
	cfg, err := updateConfigErr(mutate)
	if err != nil {
		return nil
	}
	return cfg
}

func updateConfigErr(mutate func(*Config) bool) (*Config, error) {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return writeConfigLocked(mutate)
}

// commitConfig is updateConfig plus the in-memory swap, both under cfgMu.
// A save error is returned and the live config is left unchanged, so a full
// disk cannot report "Added" for an account that will vanish on restart.
func (in *instance) commitConfig(mutate func(*Config) bool) (*Config, error) {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	config, err := writeConfigLocked(mutate)
	if err != nil {
		return nil, err
	}
	in.setConfig(config)
	return config, nil
}

func writeConfigLocked(mutate func(*Config) bool) (*Config, error) {
	config, err := loadConfig()
	if err != nil {
		return nil, err
	}
	if config == nil {
		return nil, fmt.Errorf("config could not be loaded")
	}
	if mutate != nil && mutate(config) {
		if err := saveConfig(config); err != nil {
			return nil, err
		}
	}
	return config, nil
}

// configDir returns ~/.config/ccc (created if needed)
func configDir() string {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".config", "ccc")
	os.MkdirAll(dir, 0755)
	return dir
}

// cacheDir returns ~/Library/Caches/ccc (created if needed)
func cacheDir() string {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, "Library", "Caches", "ccc")
	os.MkdirAll(dir, 0755)
	return dir
}

func getConfigPath() string {
	if p := strings.TrimSpace(os.Getenv("CCC_CONFIG")); p != "" {
		return p
	}
	// Migrate from old path if needed
	home, _ := os.UserHomeDir()
	oldPath := filepath.Join(home, ".ccc.json")
	newPath := filepath.Join(configDir(), "config.json")
	if _, err := os.Stat(newPath); os.IsNotExist(err) {
		if _, err := os.Stat(oldPath); err == nil {
			data, _ := os.ReadFile(oldPath)
			os.WriteFile(newPath, data, 0600)
			os.Remove(oldPath)
		}
	}
	return newPath
}

// loadConfig reads <config_dir>/config.json. Keys ccc no longer knows (the v2
// `sessions` map above all) are simply ignored: v3 keeps no session state in
// this file, and the first `saveConfig` drops them for good.
func loadConfig() (*Config, error) {
	data, err := os.ReadFile(getConfigPath())
	if err != nil {
		return nil, err
	}
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	return &config, nil
}

// saveConfig writes the config atomically: it marshals to a temp file in the
// same directory and renames it over config.json, so hook processes (and other
// readers) never observe a torn/partial write.
func saveConfig(config *Config) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	path := getConfigPath()
	tmp, err := os.CreateTemp(filepath.Dir(path), "config.json.tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()        // safe-ignore: best-effort cleanup on an error path
		os.Remove(tmpName) // safe-ignore: best-effort cleanup on an error path
		return err
	}
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()        // safe-ignore: best-effort cleanup on an error path
		os.Remove(tmpName) // safe-ignore: best-effort cleanup on an error path
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName) // safe-ignore: best-effort cleanup on an error path
		return err
	}
	return os.Rename(tmpName, path)
}

// ---------------------------------------------------------------------------
// Tuning knobs
// ---------------------------------------------------------------------------

// These are config.json keys (`ccc config set debounce_ms 0`), not a Telegram
// surface: ccc prefers a default that is right for everyone over a command
// nobody remembers.
const (
	defaultDebounceMS      = 2500
	defaultCompactionModel = "haiku"
	defaultMaintenanceHour = 4
	defaultIdleCompactS    = 3600  // 1h: Claude's prompt-cache TTL, so we rotate before a cold rewrite
	defaultWatchTTLS       = 14400 // 4h: same safety cap as background jobs
	// defaultWorkerTurnTimeoutS caps a hung worker (or General report) CLI.
	// Owner work on General stays on the 60s chief cap.
	defaultWorkerTurnTimeoutS = 1800
	// maxDebounceMS keeps a typo (debounce_ms = 250000) from parking every bot.
	maxDebounceMS         = 60000
	maxIdleCompactS       = 24 * 3600
	maxWatchTTLS          = 7 * 24 * 3600
	maxWorkerTurnTimeoutS = 24 * 3600
)

// debounceMS is how long an idle bot waits for more messages before it starts a
// turn. Chat arrives in bursts — a sentence, then the correction, then the link
// — and each one becoming its own `claude -p` run is the single most wasteful
// thing ccc can do with the owner's tokens.
func debounceMS(c *Config) int {
	if c == nil || c.DebounceMS == nil {
		return defaultDebounceMS
	}
	switch ms := *c.DebounceMS; {
	case ms < 0:
		return 0
	case ms > maxDebounceMS:
		return maxDebounceMS
	default:
		return ms
	}
}

// compactionModel is the cheap model the memory compaction turn runs on
// (DESIGN §7). "haiku" is an alias `claude -p --model` accepts; an unknown name
// falls back to the instance model.
func compactionModel(c *Config) string {
	if c == nil {
		return defaultCompactionModel
	}
	if m := strings.TrimSpace(c.CompactionModel); m != "" {
		return m
	}
	return defaultCompactionModel
}

// maintenanceHour is the local hour (0-23) the daily maintenance job runs at.
// Quiet by default: nobody is chatting at 04:00.
func maintenanceHour(c *Config) int {
	if c == nil || c.MaintenanceHour == nil {
		return defaultMaintenanceHour
	}
	if h := *c.MaintenanceHour; h >= 0 && h <= 23 {
		return h
	}
	return defaultMaintenanceHour
}

// idleCompact is how long a bot's conversation may sit unused before ccc
// rotates it (the same as /new: memories stay, the transcript does not).
// 0 disables. The default matches Claude's 1h prompt-cache TTL: resuming a
// cold fat session re-charges the whole history as cache_creation.
func idleCompact(c *Config) time.Duration {
	n := defaultIdleCompactS
	if c != nil && c.IdleCompactS != nil {
		n = *c.IdleCompactS
	}
	if n <= 0 {
		return 0
	}
	if n > maxIdleCompactS {
		n = maxIdleCompactS
	}
	return time.Duration(n) * time.Second
}

// watchTTL is how long a watch lives before ccc cancels it and wakes the bot
// that set it. 0 disables. Routines (named cron on the schedules table) are
// not watches and do not expire.
func watchTTL(c *Config) time.Duration {
	n := defaultWatchTTLS
	if c != nil && c.WatchTTLS != nil {
		n = *c.WatchTTLS
	}
	if n <= 0 {
		return 0
	}
	if n > maxWatchTTLS {
		n = maxWatchTTLS
	}
	return time.Duration(n) * time.Second
}

// workerTurnTimeout caps one worker CLI turn (and General turns that are
// not the owner's). 0 disables. The 60s General owner cap is separate.
func workerTurnTimeout(c *Config) time.Duration {
	n := defaultWorkerTurnTimeoutS
	if c != nil && c.WorkerTurnTimeoutS != nil {
		n = *c.WorkerTurnTimeoutS
	}
	if n <= 0 {
		return 0
	}
	if n > maxWorkerTurnTimeoutS {
		n = maxWorkerTurnTimeoutS
	}
	return time.Duration(n) * time.Second
}

// expandPath expands ~ to home directory
func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return path
}
