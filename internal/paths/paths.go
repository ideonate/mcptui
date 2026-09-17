// Package paths resolves XDG base directories for mcptui.
package paths

import (
	"os"
	"path/filepath"
)

const appName = "mcptui"

func xdg(envVar, fallback string) string {
	if v := os.Getenv(envVar); v != "" && filepath.IsAbs(v) {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return filepath.Join(home, fallback)
}

// ConfigDir is $XDG_CONFIG_HOME/mcptui (default ~/.config/mcptui).
func ConfigDir() string {
	if v := os.Getenv("MCPTUI_CONFIG_DIR"); v != "" {
		return v
	}
	return filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), appName)
}

// StateDir is $XDG_STATE_HOME/mcptui (default ~/.local/state/mcptui).
func StateDir() string {
	if v := os.Getenv("MCPTUI_STATE_DIR"); v != "" {
		return v
	}
	return filepath.Join(xdg("XDG_STATE_HOME", ".local/state"), appName)
}

// ConfigFile is the path of config.toml.
func ConfigFile() string { return filepath.Join(ConfigDir(), "config.toml") }

// CredentialsFile is the path of credentials.json.
func CredentialsFile() string { return filepath.Join(ConfigDir(), "credentials.json") }

// HistoryFile is the path of history.jsonl.
func HistoryFile() string { return filepath.Join(StateDir(), "history.jsonl") }

// EnsureDir creates dir with mode 0700 if needed.
func EnsureDir(dir string) error { return os.MkdirAll(dir, 0o700) }
