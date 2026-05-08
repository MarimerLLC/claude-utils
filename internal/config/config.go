// Package config loads and saves the claude-memsync daemon configuration.
//
// Config lives at ~/.claudesync/config.json. We keep the format JSON to avoid
// a third-party dependency and keep the config trivially editable by hand.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Config controls the daemon's behavior. Defaults() returns a Config with
// reasonable per-platform defaults; only RemoteURL is required from the user.
type Config struct {
	// RemoteURL is the git remote to sync against (e.g.
	// git@github.com:MarimerLLC/claude-memories.git).
	RemoteURL string `json:"remoteUrl"`

	// SyncDir is the local sync work-tree (default: ~/.claudesync).
	SyncDir string `json:"syncDir"`

	// ClaudeProjectsDir is the source of truth for memories
	// (default: ~/.claude/projects).
	ClaudeProjectsDir string `json:"claudeProjectsDir"`

	// MergeDriverPath is the absolute path to the claude-memmerge binary.
	// Defaults to a sibling of the running daemon.
	MergeDriverPath string `json:"mergeDriverPath"`

	// DebounceMs is how long to wait after a file change before committing.
	DebounceMs int `json:"debounceMs"`

	// PullIntervalSec is how often to pull from the remote when idle.
	PullIntervalSec int `json:"pullIntervalSec"`
}

// Defaults returns a Config populated with platform-appropriate defaults.
// MergeDriverPath is left empty for the caller to fill via DiscoverMergeDriver().
func Defaults() Config {
	home, _ := os.UserHomeDir()
	return Config{
		SyncDir:           filepath.Join(home, ".claudesync"),
		ClaudeProjectsDir: filepath.Join(home, ".claude", "projects"),
		DebounceMs:        3000,
		PullIntervalSec:   30,
	}
}

// DiscoverMergeDriver returns the absolute path to claude-memmerge by looking
// for a sibling of the running executable.
func DiscoverMergeDriver() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate executable: %w", err)
	}
	dir := filepath.Dir(exe)
	name := "claude-memmerge"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	candidate := filepath.Join(dir, name)
	if _, err := os.Stat(candidate); err != nil {
		return "", fmt.Errorf("merge driver not found at %s: %w", candidate, err)
	}
	return candidate, nil
}

// Path returns the absolute path to the config file inside SyncDir.
func (c Config) Path() string {
	return filepath.Join(c.SyncDir, "config.json")
}

// Load reads the config from path. The path is typically Config.Path() but
// callers can supply their own (e.g. for tests).
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	c := Defaults()
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, nil
}

// Save writes the config as pretty-printed JSON. Caller is responsible for
// having created SyncDir.
func Save(c Config) error {
	if err := os.MkdirAll(c.SyncDir, 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(c.Path(), b, 0600)
}
