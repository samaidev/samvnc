package main

// screenshot.go — tray-menu screenshot operation.
//
// "Tray screenshot operation": one click on the tray menu grabs the remote
// desktop via the samvnc-server HTTP API (GET /api/screenshot, PNG) and
// saves it under ~/Pictures/SamVNC with a timestamped filename, then opens
// the image with the platform viewer. This replaces the old
// curl-based workflow (curl /api/screenshot -o screen.png) for humans:
// no terminal, no curl, no copy-paste.

import (
        "context"
        "fmt"
        "io"
        "log"
        "net/http"
        "os"
        "os/exec"
        "path/filepath"
        "regexp"
        "runtime"
        "strings"
        "time"
)

// localURL is the samvnc-server local HTTP address parsed from the server
// stdout line "local UI: http://127.0.0.1:8091" (set by scanLines in
// main.go). Used as the primary screenshot endpoint when available.
var localURL string

// localUIRE matches the "local UI:" line printed by samvnc-server at
// startup, e.g. "local UI:   http://127.0.0.1:8091".
var localUIRE = regexp.MustCompile(`local UI:\s*(https?://\S+)`)

// screenshotTargets returns the candidate base URLs to try, in order:
// the local UI address (fast, works even before the tunnel is up) and
// the public URL when known.
func (a *TrayApp) screenshotTargets() []string {
        a.mu.Lock()
        defer a.mu.Unlock()
        var out []string
        if localURL != "" {
                out = append(out, strings.TrimRight(localURL, "/"))
        }
        if a.publicURL != "" {
                out = append(out, strings.TrimRight(a.publicURL, "/"))
        }
        return out
}

// takeScreenshot grabs /api/screenshot from the first responding endpoint
// and saves it to ~/Pictures/SamVNC/samvnc-YYYYMMDD-HHMMSS.png, then opens
// the file with the platform image viewer. Feedback goes to the tray
// tooltip and tray.log.
func (a *TrayApp) takeScreenshot() {
        a.tray.SetTooltip("SamVNC — " + tr("tip.shotSaving"))

        data, err := fetchScreenshot(a.screenshotTargets())
        if err != nil {
                log.Printf("[tray] screenshot failed: %v", err)
                a.tray.SetTooltip("SamVNC — " + tr("tip.shotFail"))
                restoreTooltipLater(a)
                return
        }

        path, err := saveScreenshotPNG(data)
        if err != nil {
                log.Printf("[tray] screenshot save failed: %v", err)
                a.tray.SetTooltip("SamVNC — " + tr("tip.shotFail"))
                restoreTooltipLater(a)
                return
        }

        log.Printf("[tray] screenshot saved: %s (%d bytes)", path, len(data))
        a.tray.SetTooltip("SamVNC — " + fmt.Sprintf(tr("tip.shotSaved"), path))
        openImage(path)
        restoreTooltipLater(a)
}

// fetchScreenshot tries each base URL in order with a short timeout and
// returns the PNG bytes of the first success.
func fetchScreenshot(bases []string) ([]byte, error) {
        var lastErr error
        for _, base := range bases {
                url := base + "/api/screenshot"
                ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
                req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
                if err != nil {
                        cancel()
                        lastErr = err
                        continue
                }
                resp, err := http.DefaultClient.Do(req)
                if err != nil {
                        cancel()
                        lastErr = err
                        log.Printf("[tray] screenshot: %s: %v", url, err)
                        continue
                }
                if resp.StatusCode != http.StatusOK {
                        resp.Body.Close()
                        cancel()
                        lastErr = fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
                        log.Printf("[tray] screenshot: %v", lastErr)
                        continue
                }
                data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
                resp.Body.Close()
                cancel()
                if err != nil {
                        lastErr = err
                        continue
                }
                if len(data) == 0 {
                        lastErr = fmt.Errorf("empty response from %s", url)
                        continue
                }
                return data, nil
        }
        if lastErr == nil {
                lastErr = fmt.Errorf("no screenshot endpoint available (server not running?)")
        }
        return nil, lastErr
}

// saveScreenshotPNG writes the PNG under ~/Pictures/SamVNC (falling back to
// ~/SamVNC and then the temp dir) with a timestamped filename, returning
// the full path.
func saveScreenshotPNG(data []byte) (string, error) {
        base := ""
        if home, err := os.UserHomeDir(); err == nil {
                base = filepath.Join(home, "Pictures", "SamVNC")
                if err := os.MkdirAll(base, 0755); err != nil {
                        base = filepath.Join(home, "SamVNC")
                        if err := os.MkdirAll(base, 0755); err != nil {
                                base = home
                        }
                }
        } else {
                base = os.TempDir()
        }
        name := "samvnc-" + time.Now().Format("20060102-150405") + ".png"
        path := filepath.Join(base, name)
        if err := os.WriteFile(path, data, 0644); err != nil {
                return "", err
        }
        return path, nil
}

// openImage opens the PNG with the platform image viewer (non-fatal on
// failure — the tooltip already reports the saved path).
func openImage(path string) {
        var cmd *exec.Cmd
        switch runtime.GOOS {
        case "windows":
                // `explorer /select,<path>` highlights the file in Explorer;
                // `start` opens the default viewer. Use the viewer.
                cmd = exec.Command("cmd", "/c", "start", "", path)
        case "darwin":
                cmd = exec.Command("open", path)
        default:
                cmd = exec.Command("xdg-open", path)
        }
        if err := cmd.Start(); err != nil {
                log.Printf("[tray] open image: %v", err)
        }
}

// restoreTooltipLater puts the tooltip back to the state-appropriate text
// after a short pause so success/failure messages stay visible.
func restoreTooltipLater(a *TrayApp) {
        go func() {
                time.Sleep(4 * time.Second)
                a.mu.Lock()
                st := a.state
                a.mu.Unlock()
                if st == stateRunning {
                        a.tray.SetTooltip("SamVNC — " + tr("tip.running"))
                } else {
                        a.tray.SetTooltip("SamVNC — " + tr("tip.stopped"))
                }
        }()
}
