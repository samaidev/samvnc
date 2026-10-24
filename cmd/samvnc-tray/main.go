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
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/samaidev/samvnc/internal/autostart"
	"github.com/samaidev/samvnc/internal/settings"
	"github.com/samaidev/samvnc/internal/tray"
)

// killStaleProcesses kills any leftover samvnc-server / aitun-client
// processes from a previous run, so the new tunnel connection is not
// rejected with "Tunnel occupied by another client".
func killStaleProcesses() {
	if runtime.GOOS != "windows" {
		return
	}
	_ = exec.Command("taskkill", "/F", "/IM",
		"aitun-client-windows-amd6.exe").Run()
	_ = exec.Command("taskkill", "/F", "/IM",
		"samvnc-server.exe").Run()
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

	// Subprocess management.
	cmd      *exec.Cmd
	publicURL string // e.g. https://samvnc-host-abc.t.aitun.cc
	cancel   chan struct{}
}

func main() {
	// Kill stale processes from a previous run (prevents "Tunnel occupied").
	killStaleProcesses()

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

	app := &TrayApp{
		settings: s,
		state:    stateStopped,
		cancel:   make(chan struct{}),
	}

	// Create tray.
	t := tray.New()
	t.SetCallback(app.onMenuItem)
	t.SetTooltip("SamVNC — Stopped")
	app.tray = t

	// Build initial menu.
	app.refreshMenu()

	// Auto-start the server on launch.
	go app.startServer()

	// Run the tray event loop (blocks until exit).
	if err := t.Run(); err != nil {
		log.Printf("tray error: %v", err)
	}

	// Cleanup: stop the server if running.
	app.stopServer()
}

// onMenuItem is called when a tray menu item is clicked.
func (a *TrayApp) onMenuItem(itemID string) {
	switch itemID {
	case "start":
		go a.startServer()
	case "stop":
		a.stopServer()
	case "autostart_on":
		a.toggleAutoStart(true)
	case "autostart_off":
		a.toggleAutoStart(false)
	case "settings":
		a.openSettings()
	case "copy_address":
		a.copyAddress()
	case "exit":
		a.stopServer()
		a.tray.Stop()
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
	a.tray.SetTooltip("SamVNC — Starting…")

	// Find the samvnc-server binary.
	serverPath := findServerBinary()
	log.Printf("[tray] serverPath=%q", serverPath)
	if serverPath == "" {
		a.mu.Lock()
		a.state = stateStopped
		a.mu.Unlock()
		a.refreshMenu()
		a.tray.SetTooltip("SamVNC — Error (server not found)")
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
		a.tray.SetTooltip("SamVNC — Error (could not start)")
		return
	}
	log.Printf("[tray] server started, PID=%d", cmd.Process.Pid)

	a.mu.Lock()
	a.cmd = cmd
	a.state = stateRunning
	a.publicURL = ""
	a.mu.Unlock()

	a.refreshMenu()
	a.tray.SetTooltip("SamVNC — Running (waiting for URL…)")

	// Pump stdout to find the public URL.
	urlRE := regexp.MustCompile(`https?://[^\s]+`)
	scanLines := func(r interface{ Read([]byte) (int, error) }) {
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 4096), 1<<20)
		for scanner.Scan() {
			line := scanner.Text()
			log.Printf("[server] %s", line)
			// Look for the public URL line (case-insensitive).
			lower := strings.ToLower(line)
			if strings.Contains(lower, "public url") ||
				strings.Contains(lower, "tunnel: public url") ||
				strings.Contains(line, "aitun.cc") {
				if u := urlRE.FindString(line); u != "" {
					a.mu.Lock()
					a.publicURL = u
					a.mu.Unlock()
					a.tray.SetTooltip("SamVNC — Running")
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
		a.tray.SetTooltip("SamVNC — Stopped")
	}()
}

// stopServer stops the samvnc-server subprocess.
func (a *TrayApp) stopServer() {
	a.mu.Lock()
	cmd := a.cmd
	a.state = stateStopped
	a.publicURL = ""
	a.cmd = nil
	a.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		// Kill the entire process tree (server + aitun-client child).
		killProcessTree(cmd.Process)
		_ = cmd.Wait()
	}
	a.refreshMenu()
	a.tray.SetTooltip("SamVNC — Stopped")
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
func (a *TrayApp) openSettings() {
	a.mu.Lock()
	current := &settings.Settings{
		AgentToken: a.settings.AgentToken,
		Subdomain:  a.settings.Subdomain,
		AuthToken:  a.settings.AuthToken,
		AutoStart:  a.settings.AutoStart,
	}
	publicURL := a.publicURL
	authToken := a.settings.AuthToken
	a.mu.Unlock()

	// Open the dialog (blocks until user closes it).
	newSettings, copyAddr, ok := openSettingsDialog(current, publicURL, authToken)
	if !ok {
		// Even if user cancelled, they may have clicked Copy Address.
		return
	}

	// Preserve auth token if user cleared it.
	if newSettings.AuthToken == "" {
		newSettings.AuthToken = current.AuthToken
	}

	// Save and apply.
	a.mu.Lock()
	a.settings = newSettings
	a.mu.Unlock()

	if err := settings.Save(newSettings); err != nil {
		log.Printf("save settings: %v", err)
	}

	a.refreshMenu()

	// If server is running, restart it to apply new settings.
	if a.state == stateRunning {
		a.stopServer()
		go a.startServer()
	}

	_ = copyAddr // copy already done inside dialog
}

// copyAddress copies the public URL with auth token to the clipboard.
func (a *TrayApp) copyAddress() {
	a.mu.Lock()
	url := a.publicURL
	token := a.settings.AuthToken
	a.mu.Unlock()

	if url == "" {
		a.tray.SetTooltip("SamVNC — No URL yet (server not ready)")
		go func() {
			// Restore tooltip after a moment.
			a.mu.Lock()
			st := a.state
			a.mu.Unlock()
			if st == stateRunning {
				a.tray.SetTooltip("SamVNC — Running")
			} else {
				a.tray.SetTooltip("SamVNC — Stopped")
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

	// Copy to clipboard.
	if err := copyToClipboard(fullURL); err != nil {
		log.Printf("clipboard: %v", err)
		return
	}

	a.tray.SetTooltip("SamVNC — Address copied!")
	go func() {
		// Restore tooltip after a moment.
		a.mu.Lock()
		st := a.state
		a.mu.Unlock()
		if st == stateRunning {
			a.tray.SetTooltip("SamVNC — Running")
		} else {
			a.tray.SetTooltip("SamVNC — Stopped")
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
			Label: "Stop",
		})
	} else {
		items = append(items, tray.MenuItem{
			ID:    "start",
			Label: "Start",
		})
	}

	items = append(items, tray.MenuItem{Separator: true})

	// Auto-start toggle (mutually exclusive).
	if autoStart {
		items = append(items, tray.MenuItem{
			ID:      "autostart_off",
			Label:   "✓ Auto-start on boot",
			Checked: true,
		})
	} else {
		items = append(items, tray.MenuItem{
			ID:    "autostart_on",
			Label: "Auto-start on boot",
		})
	}

	items = append(items, tray.MenuItem{Separator: true})

	// Settings.
	items = append(items, tray.MenuItem{
		ID:    "settings",
		Label: "Settings...",
	})

	// Copy Address.
	items = append(items, tray.MenuItem{
		ID:       "copy_address",
		Label:    "Copy Address",
		Disabled: !hasURL,
	})

	items = append(items, tray.MenuItem{Separator: true})

	// Exit.
	items = append(items, tray.MenuItem{
		ID:    "exit",
		Label: "Exit",
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

// copyToClipboard copies text to the system clipboard.
func copyToClipboard(text string) error {
	switch runtime.GOOS {
	case "windows":
		return copyToClipboardWindows(text)
	case "darwin":
		cmd := exec.Command("pbcopy")
		cmd.Stdin = strings.NewReader(text)
		return cmd.Run()
	default:
		// Try xclip, then xsel.
		for _, tool := range []string{"xclip", "xsel"} {
			if p, err := exec.LookPath(tool); err == nil {
				cmd := exec.Command(p)
				cmd.Stdin = strings.NewReader(text)
				if tool == "xclip" {
					cmd.Args = append(cmd.Args, "-selection", "clipboard")
				}
				return cmd.Run()
			}
		}
	}
	return fmt.Errorf("no clipboard tool available")
}
