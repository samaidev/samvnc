// Package clientbin embeds the pinned aitun tunnel client binary for the
// running platform and extracts it on first use.
//
// Why: samvnc-tray must work out of the box — the tunnel used to depend on
// an external `aitun` client (pip install aitun), and machines without it
// never got a public URL (the tray's "Copy Address" item stayed disabled).
// Each per-platform build now carries exactly ONE embedded client
// (aitun-client-<goos>-<goarch>, ~10-16 MB) via build-tagged //go:embed
// files; the binary is unpacked to ~/.samvnc/bin/ before being spawned.
//
// Sources of the embedded binaries (pinned, must all be the same release):
//   - PyPI wheel `aitun` (linux + darwin clients bundled in the wheel)
//   - https://aitun.cc/downloads/aitun-client-<goos>-<goarch>(.exe) (windows)
package clientbin

import (
        "fmt"
        "os"
        "path/filepath"
        "runtime"
)

// ClientVersion is the aitun client release the embedded binaries were
// taken from. Bump together with the files in this directory.
const ClientVersion = "4.12.6"

// Extract unpacks the embedded client for the running platform to
// ~/.samvnc/bin/<name> and returns its path. Idempotent: an existing file
// with the expected size is reused (so Start/Stop cycles don't rewrite
// it); a size mismatch (e.g. client upgrade after an app update) triggers
// a rewrite. Returns an error when the build carries no embedded client.
func Extract() (string, error) {
        if name == "" || len(embedded) == 0 {
                return "", fmt.Errorf("clientbin: no embedded client for %s/%s",
                        runtime.GOOS, runtime.GOARCH)
        }
        home, err := os.UserHomeDir()
        if err != nil {
                return "", fmt.Errorf("clientbin: resolve home: %w", err)
        }
        dir := filepath.Join(home, ".samvnc", "bin")
        if err := os.MkdirAll(dir, 0o755); err != nil {
                return "", fmt.Errorf("clientbin: mkdir %s: %w", dir, err)
        }
        target := filepath.Join(dir, name)
        if fi, err := os.Stat(target); err == nil && fi.Mode().IsRegular() &&
                fi.Size() == int64(len(embedded)) {
                return target, nil
        }
        // Write via a temp file + rename so a crash mid-write never leaves a
        // truncated client behind.
        tmp := target + ".tmp"
        if err := os.WriteFile(tmp, embedded, 0o755); err != nil {
                return "", fmt.Errorf("clientbin: write %s: %w", tmp, err)
        }
        if runtime.GOOS != "windows" {
                if err := os.Chmod(tmp, 0o755); err != nil {
                        _ = os.Remove(tmp)
                        return "", fmt.Errorf("clientbin: chmod: %w", err)
                }
        }
        if err := os.Rename(tmp, target); err != nil {
                _ = os.Remove(tmp)
                return "", fmt.Errorf("clientbin: rename: %w", err)
        }
        return target, nil
}
