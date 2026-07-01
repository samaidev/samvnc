// Package tunnel wraps the aitun.cc client binary, exposing a local TCP port
// (our WebSocket server) to the public internet over aitun's TCP+TLS tunnel.
//
// aitun.cc itself supports IPv6/IPv4 P2P direct connection *and* server relay,
// which is exactly the transport SamVNC rides on. This package spawns the
// `aitun` client as a subprocess, parses its "Proxy URL:" banner, and exposes
// the resulting public URL to the rest of the server.
//
// Reference: https://aitun.cc  (client install: `pip install aitun`)
package tunnel

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Config configures the aitun client subprocess.
type Config struct {
	// Binary path. If empty, PATH is searched for "aitun" then "aitun-client".
	Binary string
	// Token is the aitun.cc account token (empty = anonymous ephemeral tunnel).
	Token string
	// LocalPort is the local port forwarded through the tunnel.
	LocalPort int
	// TCPOnly requests a TCP-only tunnel (no HTTP pass-through). When true the
	// server must front its own TLS on top of the relayed port.
	TCPOnly bool
	// Subdomain requests a specific short subdomain (requires a token).
	Subdomain string
	// Logf receives client stdout/stderr lines. nil discards.
	Logf func(format string, args ...any)
}

// Tunnel is a running aitun client instance.
type Tunnel struct {
	cmd     *exec.Cmd
	cfg     Config
	urlMu   sync.RWMutex
	url     string
	done    chan struct{}
	logf    func(string, ...any)
}

var urlRE = regexp.MustCompile(`https?://[^\s]+`)

// Start launches the aitun client and waits (up to timeout) for the public URL.
func Start(cfg Config, timeout time.Duration) (*Tunnel, error) {
	if cfg.LocalPort == 0 {
		return nil, errors.New("tunnel: LocalPort is required")
	}
	bin, err := resolveBinary(cfg.Binary)
	if err != nil {
		return nil, err
	}

	args := buildArgs(cfg)
	t := &Tunnel{
		cfg:  cfg,
		done: make(chan struct{}),
		logf: cfg.Logf,
	}
	if t.logf == nil {
		t.logf = func(string, ...any) {}
	}

	cmd := exec.Command(bin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	cmd.Env = append(os.Environ(), "AITUN_NON_INTERACTIVE=1")

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("tunnel: start aitun: %w", err)
	}
	t.cmd = cmd

	// Pump stdout: capture the proxy URL and forward lines to logf.
	urlCh := make(chan string, 1)
	go t.pump(stdout, urlCh)
	go func() {
		_ = cmd.Wait()
		close(t.done)
	}()

	select {
	case u := <-urlCh:
		t.urlMu.Lock()
		t.url = u
		t.urlMu.Unlock()
		t.logf("tunnel: public url = %s", u)
		return t, nil
	case <-time.After(timeout):
		// The URL may still arrive later; return what we have.
		if u := t.URL(); u != "" {
			return t, nil
		}
		t.Stop()
		return nil, fmt.Errorf("tunnel: timed out after %s waiting for public url", timeout)
	case <-t.done:
		return nil, errors.New("tunnel: aitun client exited before publishing a url")
	}
}

func (t *Tunnel) pump(r io.Reader, urlCh chan<- string) {
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 0, 4096), 1<<20)
	urlSent := false
	for scan.Scan() {
		line := scan.Text()
		t.logf("aitun> %s", line)
		if !urlSent {
			if strings.Contains(strings.ToLower(line), "proxy url") ||
				strings.Contains(line, "aitun.cc/") {
				if m := urlRE.FindString(line); m != "" {
					select {
					case urlCh <- m:
					default:
					}
					urlSent = true
				}
			}
		}
	}
}

// URL returns the public aitun.cc URL, or "" if not yet known.
func (t *Tunnel) URL() string {
	t.urlMu.RLock()
	defer t.urlMu.RUnlock()
	return t.url
}

// SetURL overrides the public URL (used by callers that learn it out-of-band).
func (t *Tunnel) SetURL(u string) {
	t.urlMu.Lock()
	t.url = u
	t.urlMu.Unlock()
}

// Done returns a channel closed when the aitun client exits.
func (t *Tunnel) Done() <-chan struct{} { return t.done }

// Stop terminates the aitun client.
func (t *Tunnel) Stop() {
	if t.cmd != nil && t.cmd.Process != nil {
		_ = t.cmd.Process.Signal(os.Interrupt)
		select {
		case <-t.done:
		case <-time.After(2 * time.Second):
			_ = t.cmd.Process.Kill()
		}
	}
}

func buildArgs(cfg Config) []string {
	var args []string
	if cfg.Token != "" {
		args = append(args, "-k", cfg.Token)
	}
	if cfg.Subdomain != "" {
		args = append(args, "-s", cfg.Subdomain)
	}
	if cfg.TCPOnly {
		args = append(args, "--tcp-ports", fmt.Sprintf("%d", cfg.LocalPort))
	} else {
		args = append(args, "-p", fmt.Sprintf("%d", cfg.LocalPort))
	}
	return args
}

func resolveBinary(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return explicit, nil
		}
		// fall through to PATH lookup with the explicit name
		if p, err := exec.LookPath(explicit); err == nil {
			return p, nil
		}
	}
	for _, name := range []string{"aitun", "aitun-client"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	// Common install locations as a last resort.
	for _, p := range []string{
		"/usr/local/bin/aitun", "/usr/bin/aitun",
		"./aitun-client", "./aitun",
		filepath.Join(os.Getenv("HOME"), ".local", "bin", "aitun"),
	} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	return "", errors.New("tunnel: aitun client not found (install with `pip install aitun` or see https://aitun.cc)")
}

// LocalIPv4 returns the best-guess outbound IPv4 address, used by the relay
// picker to decide P2P feasibility.
func LocalIPv4() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return addr.IP.String()
	}
	return ""
}
