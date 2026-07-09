package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// cfgMu serializes every load→mutate→save cycle on the shared config file (see
// updateConfig). Whole-file saves are not composable, so concurrent writers must
// funnel through it.
var cfgMu sync.Mutex

// updateConfig serializes a load→mutate→save cycle on the shared config file.
// Every writer must go through it: the mirror poller and the Telegram update
// handlers mutate the config concurrently, and racing whole-file saves silently
// drop each other's changes (lost session entries → orphaned Telegram topics).
// mutate runs on a freshly-loaded copy; return true to persist it. Returns the
// fresh (possibly mutated) config, or nil if the config could not be loaded.
func updateConfig(mutate func(*Config) bool) *Config {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	config, err := loadConfig()
	if err != nil || config == nil {
		return nil
	}
	if mutate(config) {
		saveConfig(config) // safe-ignore: best-effort persist under the lock; caller keeps the fresh copy either way
	}
	return config
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

func loadConfig() (*Config, error) {
	data, err := os.ReadFile(getConfigPath())
	if err != nil {
		return nil, err
	}

	// First check if this is old format (sessions as map[string]int64)
	var rawConfig map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawConfig); err != nil {
		return nil, err
	}

	// Try to detect old sessions format
	var needsMigration bool
	var oldSessions map[string]int64
	if sessionsRaw, ok := rawConfig["sessions"]; ok {
		// Try to parse as old format (map of topic IDs)
		if json.Unmarshal(sessionsRaw, &oldSessions) == nil && len(oldSessions) > 0 {
			// Check if values are positive numbers (old format)
			for _, v := range oldSessions {
				if v > 0 {
					needsMigration = true
					break
				}
			}
		}
	}

	var config Config
	if needsMigration {
		// Parse everything except sessions first
		type ConfigWithoutSessions struct {
			BotToken    string `json:"bot_token"`
			ChatID      int64  `json:"chat_id"`
			GroupID     int64  `json:"group_id"`
			ProjectsDir string `json:"projects_dir"`
			Away        bool   `json:"away"`
		}
		var partial ConfigWithoutSessions
		json.Unmarshal(data, &partial)

		config.BotToken = partial.BotToken
		config.ChatID = partial.ChatID
		config.GroupID = partial.GroupID
		config.ProjectsDir = partial.ProjectsDir
		config.Away = partial.Away

		// Migrate sessions
		home, _ := os.UserHomeDir()
		config.Sessions = make(map[string]*SessionInfo)
		for name, topicID := range oldSessions {
			// For old sessions, try to figure out the path
			var sessionPath string
			if strings.HasPrefix(name, "/") {
				// Absolute path
				sessionPath = name
			} else if strings.HasPrefix(name, "~/") {
				// Home-relative path
				sessionPath = filepath.Join(home, name[2:])
			} else if config.ProjectsDir != "" {
				// Use projects_dir if set
				projectsDir := config.ProjectsDir
				if strings.HasPrefix(projectsDir, "~/") {
					projectsDir = filepath.Join(home, projectsDir[2:])
				}
				sessionPath = filepath.Join(projectsDir, name)
			} else {
				sessionPath = filepath.Join(home, name)
			}
			config.Sessions[name] = &SessionInfo{
				TopicID: topicID,
				Path:    sessionPath,
			}
		}
		// Save migrated config
		saveConfig(&config)
	} else {
		// Parse with new format
		if err := json.Unmarshal(data, &config); err != nil {
			return nil, err
		}
	}

	if config.Sessions == nil {
		config.Sessions = make(map[string]*SessionInfo)
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
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

// getProjectsDir returns the base directory for projects
func getProjectsDir(config *Config) string {
	if config.ProjectsDir != "" {
		// Expand ~ to home directory
		if strings.HasPrefix(config.ProjectsDir, "~/") {
			home, _ := os.UserHomeDir()
			return filepath.Join(home, config.ProjectsDir[2:])
		}
		return config.ProjectsDir
	}
	home, _ := os.UserHomeDir()
	return home
}

// resolveProjectPath resolves the full path for a project
// If name starts with / or ~/, it's treated as absolute/home-relative path
// Otherwise, it's relative to projects_dir
func resolveProjectPath(config *Config, name string) string {
	// Absolute path
	if strings.HasPrefix(name, "/") {
		return name
	}
	// Home-relative path (~/something or just ~)
	if strings.HasPrefix(name, "~/") || name == "~" {
		home, _ := os.UserHomeDir()
		if name == "~" {
			return home
		}
		return filepath.Join(home, name[2:])
	}
	// Relative to projects_dir
	return filepath.Join(getProjectsDir(config), name)
}

// expandPath expands ~ to home directory
func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return path
}
