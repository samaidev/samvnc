// SamVNC tray application entry point.
//
// This binary provides a system tray icon that manages the samvnc-server
// subprocess. The tray menu provides:
//
//   - Start / Stop (mutually exclusive)
//   - Enable / Disable auto-start (mutually exclusive)
//   - Settings... (edit aitun.cc agent token + subdomain)
//   - Copy Address (copies the public URL with auth token to clipboard)
//   - Exit
//
// The tray app loads settings from ~/.samvnc/settings.json, auto-generates
// an auth token on first run, and passes it to samvnc-server via -token.
// The public URL (with ?token=xxx appended) can be copied to clipboard
// and opened in any browser to access the remote desktop.
package main

import (
        "bufio"
        "fmt"
        "log"
        "os"
        "os/exec"
        "os/signal"
        "path/filepath"
        "regexp"
        "runtime"
        "strconv"
        "strings"
        "sync"
        "syscall"
        "time"

        "github.com/samaidev/samvnc/internal/autostart"
        "github.com/samaidev/samvnc/internal/settings"
        "github.com/samaidev/samvnc/internal/tray"
)

// killStaleProcesses kills any leftover samvnc-server subprocess and the
// stale aitun tunnel clients of previous runs, so the new tunnel connection
// is not rejected with "Tunnel occupied by another client".
//
// tunnelKey (settings.AgentToken) identifies OUR clients: every tray/server
// restart orphans the previous aitun client (the server is killed but the
// client keeps running), and the relay rejects the new client until the
// zombie disconnects.
//
// Matching is done on the command line (the tunnel key), NOT on the image
// name alone — the machine may run unrelated aitun clients at the same time
// (e.g. the SSH port-22 bridge used to manage this very machine, or the
// user's own tunnels on other ports). Those must never be killed.
func killStaleProcesses(tunnelKey string) {
        if runtime.GOOS != "windows" {
                return
        }
        // Kill stale samvnc-server instances (image name is exact).
        _ = exec.Command("taskkill", "/F", "/IM",
                "samvnc-server.exe").Run()

        if tunnelKey == "" {
                // Free proxy-address mode: our stale clients carry NO key on
                // the command line, so match the shape samvnc spawns instead:
                // an HTTP forward (-p <port>) with neither -k nor --tcp-ports.
                // Keyed tunnels (ours, the user's own, the SSH port-22 bridge)
                // always carry -k and/or --tcp-ports and are never touched.
                filter := "name='aitun-client-windows-amd64.exe' and " +
                                "commandline like '%-p %' and " +
                                "not commandline like '%-k %' and " +
                                "not commandline like '%--tcp-ports%'"
                _ = exec.Command("wmic", "process", "where", filter,
                                "call", "terminate").Run()
                return
        }
        filter := fmt.Sprintf(
                "name='aitun-client-windows-amd64.exe' and commandline like '%%%s%%'",
                tunnelKey,
        )
        _ = exec.Command("wmic", "process", "where", filter,
                "call", "terminate").Run()
}

// Tray state constants.
const (
        stateStopped  = "stopped"
        stateStarting = "starting"
        stateRunning  = "running"
)

// TrayApp manages the tray icon and the samvnc-server subprocess.
type TrayApp struct {
        mu       sync.Mutex
        settings *settings.Settings
        tray     tray.Tray
        state    string

        // settingsDialogOpen guards against re-entrant settings dialogs.
        // The dialog runs its own message pump that also dispatches tray
        // messages, so the user can open the tray menu WHILE the dialog is
        // up; without the guard a second "Settings..." click would create a
        // nested dialog whose global dialogResult/dialogDone clobber the
        // outer one (mysteriously restored/corrupted values).
        settingsDialogOpen bool

        // Subprocess management.
        cmd      *exec.Cmd
        publicURL string // e.g. https://samvnc-host-abc.t.aitun.cc
        cancel   chan struct{}
}

func main() {
        // Redirect log output to ~/.samvnc/tray.log as well.
        if home, err := os.UserHomeDir(); err == nil {
                logPath := filepath.Join(home, ".samvnc", "tray.log")
                _ = os.MkdirAll(filepath.Dir(logPath), 0755)
                if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
                        log.SetOutput(f)
                }
        }

        log.Printf("=== Tray log started ===")

        // Install a handler that swallows CTRL_CLOSE_EVENT so the tray
        // survives when the parent shell exits.
        ignoreConsoleClose()

        // Load settings.
        s, err := settings.Load()
        if err != nil {
                log.Printf("warning: could not load settings: %v", err)
                s = settings.Default()
        }

        // Auto-generate auth token on first run.
        if s.AuthToken == "" {
                s.AuthToken = settings.GenerateToken()
                if err := settings.Save(s); err != nil {
                        log.Printf("warning: could not save settings: %v", err)
                }
        }

        // Resolve the UI language (settings override, else system default).
        SetLanguage(ResolveLang(s.Language))

        // Kill stale processes from a previous run (prevents "Tunnel occupied").
        // Needs the tunnel key so only OUR stale aitun clients are targeted.
        killStaleProcesses(s.AgentToken)

        app := &TrayApp{
                settings: s,
                state:    stateStopped,
                cancel:   make(chan struct{}),
        }

        // Create tray.
        t := tray.New()
        t.SetCallback(app.onMenuItem)
        t.SetTooltip("SamVNC — " + tr("tip.stopped"))
        app.tray = t

        // Build initial menu.
        app.refreshMenu()

        // Auto-start the server on launch.
        go app.startServer()

        // Run the tray event loop (blocks until exit).
        if err := t.Run(); err != nil {
                // No GUI/tray environment (headless Linux over SSH, macOS
                // launchd without an Aqua session, ...). DO NOT exit here:
                // the server subprocess is already up and serving, and the
                // documented one-click start command is `samvnc-tray` — it
                // must keep working without a desktop. Fall back to a
                // server-only foreground mode until interrupted.
                log.Printf("tray unavailable (%v) — running in server-only mode", err)
                runServerOnly(app)
        }

        // Cleanup: stop the server if running.
        app.stopServer()
}

// runServerOnly keeps the process alive without any GUI when the platform
// tray backend is unavailable (headless server, SSH session, CI, ...).
// It prints the public URL (with the access token) to stdout as soon as
// the tunnel reports it, so the one-click command still tells the user
// where to connect, and blocks until SIGINT/SIGTERM.
func runServerOnly(app *TrayApp) {
        fmt.Println("SamVNC " + tr("msg.serverOnly"))

        // Watch for the public URL so headless users can connect.
        last := ""
        stop := make(chan struct{})
        go func() {
                ticker := time.NewTicker(2 * time.Second)
                defer ticker.Stop()
                for {
                        select {
                        case <-stop:
                                return
                        case <-ticker.C:
                                app.mu.Lock()
                                u := app.publicURL
                                tok := app.settings.AuthToken
                                app.mu.Unlock()
                                if u != "" && u != last {
                                        last = u
                                        full := u
                                        if tok != "" {
                                                full += "?token=" + tok
                                        }
                                        fmt.Println(tr("msg.serverOnlyURL") + " " + full)
                                }
                        }
                }
        }()

        sig := make(chan os.Signal, 1)
        signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
        <-sig
        close(stop)
        fmt.Println("SamVNC: shutting down (server-only mode).")
}

// onMenuItem is called when a tray menu item is clicked.
//
// NOTE: this runs on the tray window thread (from the WM_COMMAND handler).
// Anything that can block (taskkill, cmd.Wait) MUST run in a goroutine —
// blocking here freezes the menu and all further tray interaction.
func (a *TrayApp) onMenuItem(itemID string) {
        switch itemID {
        case "start":
                go a.startServer()
        case "stop":
                // stopServer shells out to taskkill and waits for the
                // process to die — never do that on the UI thread.
                go a.stopServer()
        case "autostart_on":
                a.toggleAutoStart(true)
        case "autostart_off":
                a.toggleAutoStart(false)
        case "settings":
                // INLINE, not in a goroutine: the dialog runs a nested message pump
                // on THIS thread (same pattern as TrackPopupMenu's modal loop). A
                // dedicated goroutine can get pinned to the tray's locked OS thread
                // by the scheduler and then starve forever behind GetMessage — that
                // produced the frozen zombie dialog and a settingsDialogOpen guard
                // stuck true, which silently ignored every later Settings click.
                a.openSettings()
        case "copy_address":
                a.copyAddress()
        case "screenshot":
                go a.takeScreenshot()
        case "exit":
                go func() {
                        a.stopServer()
                        a.tray.Stop()
                }()
        }
}

// startServer starts the samvnc-server subprocess.
func (a *TrayApp) startServer() {
        a.mu.Lock()
        if a.state == stateRunning || a.state == stateStarting {
                a.mu.Unlock()
                return
        }
        a.state = stateStarting
        a.mu.Unlock()

        log.Printf("[tray] startServer called")
        a.refreshMenu()
        a.tray.SetTooltip("SamVNC — " + tr("tip.starting"))

        // Find the samvnc-server binary.
        serverPath := findServerBinary()
        log.Printf("[tray] serverPath=%q", serverPath)
        if serverPath == "" {
                a.mu.Lock()
                a.state = stateStopped
                a.mu.Unlock()
                a.refreshMenu()
                a.tray.SetTooltip("SamVNC — " + tr("tip.noServer"))
                return
        }

        // Build command arguments.
        args := []string{
                "-token", a.settings.AuthToken,
                "-verbose",
        }
        // The key can be either an agent token (aitagt_...) or a tunnel token (ait_...).
        // Agent tokens auto-register a subdomain; tunnel tokens connect directly.
        key := a.settings.AgentToken
        if key != "" {
                if strings.HasPrefix(key, "ait_") {
                        // Tunnel token — use directly.
                        args = append(args, "-aitun-token", key)
                } else {
                        // Agent token — for auto subdomain registration.
                        args = append(args, "-aitun-agent-token", key)
                }
        }
        if a.settings.Subdomain != "" {
                args = append(args, "-aitun-subdomain", a.settings.Subdomain)
        }

        cmd := exec.Command(serverPath, args...)
        cmd.Env = os.Environ()

        // Capture stdout to find the public URL.
        stdout, err := cmd.StdoutPipe()
        if err != nil {
                a.mu.Lock()
                a.state = stateStopped
                a.mu.Unlock()
                a.refreshMenu()
                return
        }
        // Capture stderr too — server logs go to stderr.
        stderr, err := cmd.StderrPipe()
        if err != nil {
                stderr = nil
        }

        log.Printf("[tray] starting server: %s %v", serverPath, args)
        if err := cmd.Start(); err != nil {
                log.Printf("[tray] cmd.Start error: %v", err)
                a.mu.Lock()
                a.state = stateStopped
                a.mu.Unlock()
                a.refreshMenu()
                a.tray.SetTooltip("SamVNC — " + tr("tip.noStart"))
                return
        }
        log.Printf("[tray] server started, PID=%d", cmd.Process.Pid)

        a.mu.Lock()
        a.cmd = cmd
        a.state = stateRunning
        a.publicURL = ""
        a.mu.Unlock()

        a.refreshMenu()
        a.tray.SetTooltip("SamVNC — " + tr("tip.waitingURL"))

        // Pump stdout to find the public URL.
        urlRE := regexp.MustCompile(`https?://[^\s]+`)
        scanLines := func(r interface{ Read([]byte) (int, error) }) {
                scanner := bufio.NewScanner(r)
                scanner.Buffer(make([]byte, 0, 4096), 1<<20)
                for scanner.Scan() {
                        line := scanner.Text()
                        log.Printf("[server] %s", line)
                        // Remember the LOCAL UI address (used by the tray
                        // screenshot operation — fastest endpoint, works
                        // even before the tunnel is up).
                        if m := localUIRE.FindStringSubmatch(line); m != nil {
                                localURL = m[1]
                        }
                        // Look for the public URL line (case-insensitive).
                        lower := strings.ToLower(line)
                        if strings.Contains(lower, "public url") ||
                                strings.Contains(lower, "tunnel: public url") ||
                                strings.Contains(line, "aitun.cc") {
                                if u := urlRE.FindString(line); u != "" {
                                        a.mu.Lock()
                                        a.publicURL = u
                                        a.mu.Unlock()
                                        a.tray.SetTooltip("SamVNC — " + tr("tip.running"))
                                        a.refreshMenu()
                                }
                        }
                }
        }
        go scanLines(stdout)
        if stderr != nil {
                go scanLines(stderr)
        }

        // Wait for the process to exit.
        go func() {
                _ = cmd.Wait()
                a.mu.Lock()
                a.state = stateStopped
                a.publicURL = ""
                a.cmd = nil
                a.mu.Unlock()
                a.refreshMenu()
                a.tray.SetTooltip("SamVNC — " + tr("tip.stopped"))
        }()
}

// stopServer stops the samvnc-server subprocess.
//
// Idempotent: duplicate "stop" commands (the tray shell can deliver two
// WM_COMMANDs for one click) resolve to a harmless no-op instead of
// killing a freshly restarted server.
func (a *TrayApp) stopServer() {
        a.mu.Lock()
        cmd := a.cmd
        a.state = stateStopped
        a.publicURL = ""
        a.cmd = nil
        a.mu.Unlock()

        if cmd != nil && cmd.Process != nil {
                log.Printf("[tray] stopping server PID=%d", cmd.Process.Pid)
                // Kill the entire process tree (server + aitun-client child).
                killProcessTree(cmd.Process)
                _ = cmd.Wait()
                log.Printf("[tray] server stopped")
        }
        a.refreshMenu()
        a.tray.SetTooltip("SamVNC — " + tr("tip.stopped"))
}

// killProcessTree kills a process and all its children.
func killProcessTree(proc *os.Process) {
        if proc == nil {
                return
        }
        // On Windows, use taskkill /T /F to kill the process tree.
        if runtime.GOOS == "windows" {
                _ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(proc.Pid)).Run()
        } else {
                _ = proc.Kill()
        }
}

// toggleAutoStart enables or disables boot-time auto-start.
func (a *TrayApp) toggleAutoStart(enable bool) {
        if enable {
                execPath := findSelfPath()
                if err := autostart.Enable(execPath); err != nil {
                        log.Printf("autostart enable: %v", err)
                }
        } else {
                if err := autostart.Disable(); err != nil {
                        log.Printf("autostart disable: %v", err)
                }
        }

        a.settings.AutoStart = enable
        if err := settings.Save(a.settings); err != nil {
                log.Printf("save settings: %v", err)
        }
        a.refreshMenu()
}

// openSettings opens a native GUI settings dialog.
//
// THREAD MODEL: runs ON the tray window thread (inline from onMenuItem).
// The dialog blocks in a nested GetMessage pump here — the tray window
// thread is the one thread guaranteed to keep pumping, and the nested pump
// dispatches tray messages, so the tray/menu stay usable while the dialog
// is up. Heavy post-OK work (file save, wmic, taskkill) is handed to a
// goroutine so this thread returns to the pump quickly.
func (a *TrayApp) openSettings() {
        // Re-entrancy guard: while the dialog pump is nested here it also
        // dispatches tray messages, so "Settings..." can be clicked again while
        // the dialog is up. A second dialog would share/overwrite the global
        // dialogResult state.
        a.mu.Lock()
        if a.settingsDialogOpen {
                a.mu.Unlock()
                log.Printf("[tray] settings dialog already open — ignoring")
                return
        }
        a.settingsDialogOpen = true
        current := &settings.Settings{
                AgentToken: a.settings.AgentToken,
                Subdomain:  a.settings.Subdomain,
                AuthToken:  a.settings.AuthToken,
                AutoStart:  a.settings.AutoStart,
                Language:   a.settings.Language,
        }
        publicURL := a.publicURL
        authToken := a.settings.AuthToken
        oldKey := a.settings.AgentToken
        oldToken := a.settings.AuthToken
        wasRunning := a.state == stateRunning || a.state == stateStarting
        a.mu.Unlock()

        defer func() {
                a.mu.Lock()
                a.settingsDialogOpen = false
                a.mu.Unlock()
        }()

        // Open the dialog (blocks in the nested pump until the user closes it).
        newSettings, copyAddr, ok := openSettingsDialog(current, publicURL, authToken)
        if !ok {
                // Even if user cancelled, they may have clicked Copy Address.
                return
        }

        // The dialog has no subdomain control anymore — never clobber the
        // stored value with the dialog's always-empty field.
        newSettings.Subdomain = current.Subdomain

        // Clearing the access token means "give me a new one": generate a
        // fresh token so the old shareable URLs stop working. (Silently
        // keeping the old token made the field impossible to clear.)
        if newSettings.AuthToken == "" {
                newSettings.AuthToken = settings.GenerateToken()
                log.Printf("[tray] access token cleared by user — new token generated")
        }

        // Save and apply — OFF this thread. settings.Save does file I/O,
        // killStaleProcesses shells out to wmic (multi-second), and the restart
        // waits on taskkill; any of those would freeze the tray message pump.
        go a.applySettings(newSettings, oldKey, oldToken, wasRunning)

        _ = copyAddr // copy already done inside dialog
}

// applySettings persists the dialog result and activates it: saves JSON,
// kills stale tunnel clients of the old key and restarts the server so the
// new settings take effect. Runs on its own goroutine (see openSettings).
func (a *TrayApp) applySettings(newSettings *settings.Settings, oldKey, oldToken string, wasRunning bool) {
        // Save and apply.
        a.mu.Lock()
        a.settings = newSettings
        a.mu.Unlock()

        if err := settings.Save(newSettings); err != nil {
                log.Printf("save settings: %v", err)
        } else {
                log.Printf("[tray] settings saved (key_len=%d token_len=%d lang=%s)",
                        len(newSettings.AgentToken), len(newSettings.AuthToken), newSettings.Language)
        }

        // Apply the UI language immediately: the menu rebuild below and the
        // next dialog open render in the new language. No restart involved.
        SetLanguage(ResolveLang(newSettings.Language))

        a.refreshMenu()

        // If the aitun key changed, kill stale tunnel clients that still
        // carry the OLD key. Without this, the previous client survives the
        // restart (startServer only kills clients matching the NEW key and
        // killStaleProcesses("") would miss it) and the old tunnel/subdomain
        // stays live — the change appears to have no effect.
        //
        // Mode switches need cleanup in BOTH directions: switching to no-key
        // leaves keyed zombies behind, and switching to a key leaves
        // anonymous free-mode zombies behind (whose random proxy URL would
        // keep forwarding to this machine for up to 24h). The anonymous
        // cleanup only targets keyless HTTP forwards, never keyed tunnels.
        if newSettings.AgentToken != oldKey {
                log.Printf("[tray] aitun key changed — killing stale clients of the old key")
                killStaleProcesses(oldKey)
                if oldKey == "" || newSettings.AgentToken == "" {
                        killStaleProcesses("")
                }
        }

        // Restart only when something the SERVER cares about changed (the
        // tunnel key or the auth token). Pure UI changes (language,
        // auto-start) must not bounce the tunnel — a restart briefly drops
        // the public URL and makes the aitun client reconnect for nothing.
        serverRelevant := newSettings.AgentToken != oldKey ||
                newSettings.AuthToken != oldToken
        if wasRunning && serverRelevant {
                a.stopServer()
                go a.startServer()
        }
}

// copyAddress copies the public URL with auth token to the clipboard.
func (a *TrayApp) copyAddress() {
        a.mu.Lock()
        url := a.publicURL
        token := a.settings.AuthToken
        a.mu.Unlock()

        if url == "" {
                a.tray.SetTooltip("SamVNC — " + tr("tip.noURL"))
                go func() {
                        // Restore tooltip after a moment.
                        a.mu.Lock()
                        st := a.state
                        a.mu.Unlock()
                        if st == stateRunning {
                                a.tray.SetTooltip("SamVNC — " + tr("tip.running"))
                        } else {
                                a.tray.SetTooltip("SamVNC — " + tr("tip.stopped"))
                        }
                }()
                return
        }

        // Append token as query parameter.
        fullURL := url
        if token != "" {
                sep := "?"
                if strings.Contains(url, "?") {
                        sep = "&"
                }
                fullURL = url + sep + "token=" + token
        }

        // Copy to clipboard. If no clipboard tool exists (stock GNOME
        // Wayland ships neither xclip nor wl-copy), fall back to a dialog
        // with the URL in a selectable text field so the user can copy it
        // manually — never fail silently.
        if err := copyToClipboard(fullURL); err != nil {
                log.Printf("clipboard: %v", err)
                showCopyFallback(fullURL)
                return
        }

        a.tray.SetTooltip("SamVNC — " + tr("tip.copied"))
        go func() {
                // Restore tooltip after a moment.
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

// refreshMenu rebuilds the tray context menu based on current state.
func (a *TrayApp) refreshMenu() {
        a.mu.Lock()
        state := a.state
        autoStart := a.settings.AutoStart
        hasURL := a.publicURL != ""
        a.mu.Unlock()

        items := []tray.MenuItem{}

        // Start / Stop (mutually exclusive).
        if state == stateRunning || state == stateStarting {
                items = append(items, tray.MenuItem{
                        ID:    "stop",
                        Label: tr("menu.stop"),
                })
        } else {
                items = append(items, tray.MenuItem{
                        ID:    "start",
                        Label: tr("menu.start"),
                })
        }

        items = append(items, tray.MenuItem{Separator: true})

        // Auto-start toggle (mutually exclusive). The native MFS_CHECKED
        // state draws the checkmark — do NOT also put "✓" in the label,
        // that produced the double-checkmark "✓✓" bug.
        if autoStart {
                items = append(items, tray.MenuItem{
                        ID:      "autostart_off",
                        Label:   tr("menu.autostart"),
                        Checked: true,
                })
        } else {
                items = append(items, tray.MenuItem{
                        ID:    "autostart_on",
                        Label: tr("menu.autostart"),
                })
        }

        items = append(items, tray.MenuItem{Separator: true})

        // Settings.
        items = append(items, tray.MenuItem{
                ID:    "settings",
                Label: tr("menu.settings"),
        })

        // Copy Address.
        items = append(items, tray.MenuItem{
                ID:       "copy_address",
                Label:    tr("menu.copy"),
                Disabled: !hasURL,
        })

        // Screenshot — one-click tray operation: grabs /api/screenshot from
        // the running server, saves a timestamped PNG under ~/Pictures/SamVNC
        // and opens it. Enabled whenever the server is running (works via the
        // local endpoint even before the public URL is known).
        items = append(items, tray.MenuItem{
                ID:       "screenshot",
                Label:    tr("menu.screenshot"),
                Disabled: state != stateRunning,
        })

        items = append(items, tray.MenuItem{Separator: true})

        // Exit.
        items = append(items, tray.MenuItem{
                ID:    "exit",
                Label: tr("menu.exit"),
        })

        a.tray.SetMenu(items)
}

// findServerBinary locates the samvnc-server binary.
func findServerBinary() string {
        // 1. Same directory as this binary.
        if self, err := os.Executable(); err == nil {
                dir := filepath.Dir(self)
                name := serverBinaryName()
                p := filepath.Join(dir, name)
                if _, err := os.Stat(p); err == nil {
                        return p
                }
        }

        // 2. Current working directory.
        p := filepath.Join(".", serverBinaryName())
        if _, err := os.Stat(p); err == nil {
                abs, _ := filepath.Abs(p)
                return abs
        }

        // 3. PATH lookup.
        if p, err := exec.LookPath(serverBinaryName()); err == nil {
                return p
        }

        return ""
}

func serverBinaryName() string {
        if runtime.GOOS == "windows" {
                return "samvnc-server.exe"
        }
        return "samvnc-server"
}

// findSelfPath returns the path to this binary (for auto-start registration).
func findSelfPath() string {
        if self, err := os.Executable(); err == nil {
                return self
        }
        return os.Args[0]
}

// copyToClipboard is implemented per-platform in clipboard_windows.go
// and clipboard_other.go (the windows-only helper cannot be referenced
// from shared code — it breaks cross-compilation).
