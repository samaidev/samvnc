// Package settings persists user-configurable settings for the SamVNC tray app.
//
// Settings are stored as JSON at ~/.samvnc/settings.json (mode 0600) so the
// embedded auth token is not world-readable.
package settings

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Settings holds the user-configurable options for the tray app.
type Settings struct {
	// AgentToken is the aitun.cc agent token (aitagt_...) used for subdomain
	// registration. If empty, the built-in default agent token is used.
	AgentToken string `json:"agent_token,omitempty"`

	// Subdomain is the requested subdomain name (e.g. "my-pc"). If empty,
	// samvnc-server auto-generates one.
	Subdomain string `json:"subdomain,omitempty"`

	// AuthToken is the client auth token required to access the remote desktop.
	// If empty on first run, the tray app auto-generates a random one.
	// This token is appended to the public URL as ?token=xxx.
	AuthToken string `json:"auth_token"`

	// AutoStart controls whether SamVNC starts on system boot.
	AutoStart bool `json:"auto_start"`
}

// Default returns sensible default settings.
func Default() *Settings {
	return &Settings{}
}

// DefaultPath returns the path to the settings file (~/.samvnc/settings.json).
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".samvnc", "settings.json")
}

// Load reads settings from the default path. Returns Default() if the file
// does not exist (first run). Any other error is returned as-is.
func Load() (*Settings, error) {
	p := DefaultPath()
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return Default(), nil
		}
		return nil, fmt.Errorf("read settings %s: %w", p, err)
	}
	var s Settings
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse settings %s: %w", p, err)
	}
	return &s, nil
}

// Save writes settings to the default path with mode 0600.
func Save(s *Settings) error {
	if s == nil {
		return fmt.Errorf("refusing to save nil settings")
	}
	p := DefaultPath()
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create settings dir %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write settings %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, p); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, p, err)
	}
	return nil
}

// GenerateToken returns a random 16-byte hex token (32 chars).
func GenerateToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "samvnc-default-token"
	}
	return hex.EncodeToString(b)
}
