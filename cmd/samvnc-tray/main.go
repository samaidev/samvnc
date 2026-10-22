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
	"strings"
	"sync"

	"github.com/samaidev/samvnc/internal/autostart"
	"github.com/samaidev/samvnc/internal/settings"
	"github.com/samaidev/samvnc/internal/tray"
)

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

	a.refreshMenu()
	a.tray.SetTooltip("SamVNC — Starting…")

	// Find the samvnc-server binary.
	serverPath := findServerBinary()
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
	if a.settings.AgentToken != "" {
		args = append(args, "-aitun-agent-token", a.settings.AgentToken)
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
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		a.mu.Lock()
		a.state = stateStopped
		a.mu.Unlock()
		a.refreshMenu()
		a.tray.SetTooltip("SamVNC — Error (could not start)")
		return
	}

	a.mu.Lock()
	a.cmd = cmd
	a.state = stateRunning
	a.publicURL = ""
	a.mu.Unlock()

	a.refreshMenu()
	a.tray.SetTooltip("SamVNC — Running (waiting for URL…)")

	// Pump stdout to find the public URL.
	urlRE := regexp.MustCompile(`https?://[^\s]+`)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	go func() {
		for scanner.Scan() {
			line := scanner.Text()
			log.Printf("[server] %s", line)
			// Look for the public URL line.
			if strings.Contains(strings.ToLower(line), "public url") ||
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
	}()

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
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	a.refreshMenu()
	a.tray.SetTooltip("SamVNC — Stopped")
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

// openSettings opens the settings file in the default text editor.
func (a *TrayApp) openSettings() {
	path := settings.DefaultPath()

	// Ensure the file exists.
	if _, err := os.Stat(path); err != nil {
		_ = settings.Save(a.settings)
	}

	// Open with the default editor.
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("notepad.exe", path)
	case "darwin":
		cmd = exec.Command("open", "-e", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	_ = cmd.Start()

	// Wait for the editor to close, then reload settings.
	go func() {
		_ = cmd.Wait()
		newSettings, err := settings.Load()
		if err != nil {
			log.Printf("reload settings: %v", err)
			return
		}
		// Preserve the auth token (don't overwrite it from the file if empty).
		if newSettings.AuthToken == "" {
			newSettings.AuthToken = a.settings.AuthToken
		}
		a.mu.Lock()
		a.settings = newSettings
		a.mu.Unlock()
		a.refreshMenu()

		// If server is running, restart it to apply new settings.
		if a.state == stateRunning {
			a.stopServer()
			go a.startServer()
		}
	}()
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
