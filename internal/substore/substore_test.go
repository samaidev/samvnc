package substore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoad(t *testing.T) {
	dir := t.TempDir()

	// First load: no file → returns nil, nil.
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load on empty dir should not error, got %v", err)
	}
	if got != nil {
		t.Fatalf("Load on empty dir should return nil, got %+v", got)
	}

	// Save a subdomain info.
	info := &Info{
		Name:       "samcmd-host-a1b2c3d4e5",
		ID:         "sub_abc123",
		DomainBase: "t.aitun.cc",
		DomainURL:  "samcmd-host-a1b2c3d4e5.t.aitun.cc",
		Token:      "ait_secret_token",
	}
	if err := Save(dir, info); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Verify the file exists with mode 0600.
	p := filepath.Join(dir, DefaultFilename)
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %o, want 0600", fi.Mode().Perm())
	}

	// Load it back.
	got, err = Load(dir)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got == nil {
		t.Fatalf("Load returned nil after Save")
	}
	if got.Name != info.Name {
		t.Errorf("Name = %q, want %q", got.Name, info.Name)
	}
	if got.Token != info.Token {
		t.Errorf("Token = %q, want %q", got.Token, info.Token)
	}
	if got.SchemaVersion != currentSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", got.SchemaVersion, currentSchemaVersion)
	}
	if got.RegisteredAt == "" {
		t.Errorf("RegisteredAt should be set by Save, got empty")
	}
}

func TestClear(t *testing.T) {
	dir := t.TempDir()
	info := &Info{Name: "test", Token: "t"}
	if err := Save(dir, info); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err != nil {
		t.Fatal(err)
	}
	if err := Clear(dir); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load after Clear should not error, got %v", err)
	}
	if got != nil {
		t.Fatalf("Load after Clear should return nil, got %+v", got)
	}
}

func TestLoadCorrupt(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, DefaultFilename)
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatalf("Load should fail on corrupt JSON")
	}
}

func TestSaveEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, &Info{Name: ""}); err == nil {
		t.Fatalf("Save with empty Name should error")
	}
	if err := Save(dir, nil); err == nil {
		t.Fatalf("Save nil should error")
	}
}

func TestPathFallback(t *testing.T) {
	// Empty dataDir should fall back to ~/.samcommand/subdomain.json
	p, err := Path("")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(p) != DefaultFilename {
		t.Errorf("Path basename = %q, want %q", filepath.Base(p), DefaultFilename)
	}
}
