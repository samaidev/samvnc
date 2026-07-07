// Package substore persists the subdomain chosen on a previous successful
// run so the same public URL survives restarts.
//
// Why this exists:
//   Before this package, samvnc-server generated a fresh random subdomain name
//   on every start (e.g. `samcmd-host-a1b2c3`). Even though aitun.cc supports
//   reuse-by-name, the random suffix changed each run, so the public URL was
//   different every time. That broke bookmarks, monitoring URLs, and any
//   external system that recorded the URL.
//
// What this package does:
//   - Load(): read the previously saved subdomain info from disk (if any).
//   - Save(): persist subdomain info after a successful registration.
//   - The state file lives at <dataDir>/subdomain.json, mode 0600 so the
//     embedded ait_ tunnel token is not world-readable.
package substore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Info is the persisted subdomain state.
//
// All fields are populated from the aitun.cc management API response after a
// successful EnsureSubdomain call. On the next run we feed Name back into
// EnsureSubdomain(name, ..., reuse=true) so aitun.cc returns the same
// subdomain (and the same ait_ tunnel token).
type Info struct {
	// Name is the subdomain label (without the domain base). Example: "samcmd-host-a1b2c3".
	Name string `json:"name"`

	// ID is the aitun.cc internal subdomain ID. Saved for diagnostics /
	// manual RenewToken / Delete calls.
	ID string `json:"id"`

	// DomainBase is the aitun.cc domain base ("t.aitun.cc" by default,
	// "aitun.cc" for VIP 2-level domains).
	DomainBase string `json:"domain_base"`

	// DomainURL is the full public URL (Name + "." + DomainBase).
	DomainURL string `json:"domain_url"`

	// Token is the per-subdomain ait_ tunnel token that aitun-client
	// consumes via -k. Persisted so we can hand it back to aitun-client
	// without re-calling RenewToken on every start.
	Token string `json:"token"`

	// RegisteredAt is when the subdomain was first registered (RFC3339).
	RegisteredAt string `json:"registered_at"`

	// SchemaVersion lets us migrate the file format later without breaking
	// existing installs.
	SchemaVersion int `json:"schema_version"`
}

const currentSchemaVersion = 1

// DefaultFilename is the name of the state file inside DataDir.
const DefaultFilename = "subdomain.json"

// Path returns the absolute path to the state file for a given data dir.
// If dataDir is empty, it falls back to ~/.samvnc/.
func Path(dataDir string) (string, error) {
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		dataDir = filepath.Join(home, ".samvnc")
	}
	return filepath.Join(dataDir, DefaultFilename), nil
}

// Load reads the persisted subdomain info. Returns (nil, nil) if the file
// does not exist (first run). Any other error (corrupt JSON, permission
// issue) is returned as-is.
func Load(dataDir string) (*Info, error) {
	p, err := Path(dataDir)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read subdomain state %s: %w", p, err)
	}
	var info Info
	if err := json.Unmarshal(b, &info); err != nil {
		return nil, fmt.Errorf("parse subdomain state %s: %w", p, err)
	}
	if info.Name == "" {
		return nil, fmt.Errorf("subdomain state %s has empty name", p)
	}
	return &info, nil
}

// Save writes the subdomain info to disk with mode 0600 (the embedded
// ait_ tunnel token must not be world-readable). The data dir is created
// if missing.
func Save(dataDir string, info *Info) error {
	if info == nil || info.Name == "" {
		return fmt.Errorf("refusing to save empty subdomain info")
	}
	if info.SchemaVersion == 0 {
		info.SchemaVersion = currentSchemaVersion
	}
	if info.RegisteredAt == "" {
		info.RegisteredAt = time.Now().UTC().Format(time.RFC3339)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir %s: %w", dataDir, err)
	}
	p, err := Path(dataDir)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal subdomain state: %w", err)
	}
	// Write to a temp file + rename for atomicity.
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write subdomain state %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, p); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, p, err)
	}
	return nil
}

// Clear removes the persisted state file. Useful when the user explicitly
// changes the subdomain (so the old state no longer applies) or when the
// saved subdomain has been deleted from the aitun.cc account and we want
// the next start to generate a fresh one.
func Clear(dataDir string) error {
	p, err := Path(dataDir)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove subdomain state %s: %w", p, err)
	}
	return nil
}
