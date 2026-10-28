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
        "runtime"
        "strings"
        "sync"
        "time"

        "github.com/samaidev/samvnc/internal/tunnel/clientbin"
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
        // Home isolates the client's persisted state (auth_token used for the
        // stable free-tunnel URL) inside this directory. When set, the child
        // process runs with HOME/USERPROFILE pointing here. This matters on
        // machines where the user ALREADY runs other aitun clients (samcommand,
        // their own tunnels): those share ~/.aitun, and two clients with the
        // same persisted token REPLACE each other at the relay ("Connection
        // replaced by newer connection of this client") — a tug-of-war that
        // kills both tunnels. Isolating the home gives SamVNC its own stable
        // identity without touching the user's tunnels.
        Home string
        // Logf receives client stdout/stderr lines. nil discards.
        Logf func(format string, args ...any)
}

// Tunnel is a running aitun client instance.
type Tunnel struct {
        cmd   *exec.Cmd
        cfg   Config
        urlMu sync.RWMutex
        url   string
        done  chan struct{}
        logf  func(string, ...any)
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
        hideWindow(cmd) // no console window for the tunnel client (Windows)
        stdout, err := cmd.StdoutPipe()
        if err != nil {
                return nil, err
        }
        cmd.Stderr = io.Discard
        cmd.Env = append(os.Environ(), "AITUN_NON_INTERACTIVE=1")
        if cfg.Home != "" {
                // Python wrappers (pip installs) resolve their module search
                // path from HOME, so overriding it would break them. Unwrap
                // to the bundled Go client when possible; only isolate the
                // home when we ended up with a native binary.
                realBin := unwrapGoClient(bin)
                if realBin != "" {
                        bin = realBin
                        cmd.Path = realBin
                        args = buildArgs(cfg)
                        cmd.Args = append([]string{realBin}, args...)
                        if err := os.MkdirAll(cfg.Home, 0o755); err == nil {
                                // Isolate the client's persisted state. Go
                                // binaries resolve the home via HOME (unix)
                                // / USERPROFILE (windows), so overriding
                                // both is enough.
                                cmd.Env = append(cmd.Env,
                                        "HOME="+cfg.Home,
                                        "USERPROFILE="+cfg.Home)
                        }
                }
        }

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
                        lo := strings.ToLower(line)
                        if strings.Contains(lo, "proxy url") ||
                                strings.Contains(lo, "subdomain mode") ||
                                strings.Contains(line, "aitun.cc") {
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

// unwrapGoClient returns the native Go client binary behind a pip python
// wrapper. pip's `aitun` on PATH is a #! script whose module directory
// bundles the real per-platform binary (site-packages/aitun/bin/
// aitun-client-<os>-<arch>). Spawning that native binary lets us isolate
// HOME safely; when nothing is found (or the candidate is already a native
// binary) the input is returned unchanged for wrappers we could not unwrap
// the empty string signals "cannot unwrap".
//
// Return convention: native binaries → returned unchanged (isolation OK);
// python wrapper + found Go binary → Go binary path; python wrapper + no
// Go binary → "" (caller must skip HOME isolation).
func unwrapGoClient(bin string) string {
        b, err := os.ReadFile(bin)
        if err != nil || len(b) < 2 || b[0] != '#' || b[1] != '!' {
                // Not a script — assume a native binary.
                return bin
        }
        // Wrapper detected: look for the bundled Go client in the standard
        // pip locations (pip install --user is the documented flow).
        var roots []string
        if home, err := os.UserHomeDir(); err == nil {
                roots = append(roots,
                        filepath.Join(home, ".local", "lib"),
                        filepath.Join(home, ".local", "lib64"))
        }
        roots = append(roots,
                "/usr/local/lib", "/usr/lib", "/opt/homebrew/lib")
        for _, root := range roots {
                for _, name := range bundledClientNames() {
                        pattern := filepath.Join(root, "python3*", "site-packages", "aitun", "bin", name)
                        if matches, _ := filepath.Glob(pattern); len(matches) > 0 {
                                return matches[0]
                        }
                        pattern = filepath.Join(root, "python3*", "dist-packages", "aitun", "bin", name)
                        if matches, _ := filepath.Glob(pattern); len(matches) > 0 {
                                return matches[0]
                        }
                }
        }
        return ""
}

func buildArgs(cfg Config) []string {
        var args []string
        if cfg.Token != "" {
                // The ait_ token returned by the management API is bound to a
                // specific subdomain on the server side, so we just hand it to
                // aitun-client via -k. The subdomain name itself does NOT go
                // on the aitun-client command line — aitun-client's -s flag
                // is the relay server address (default aitun.cc:6639), not a
                // subdomain selector. Passing the subdomain name via -s would
                // make aitun-client try to DNS-resolve it as a host.
                args = append(args, "-k", cfg.Token)
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
        // Prefer a client binary bundled next to the server binary (the
        // release zip ships samvnc-server.exe + aitun-client-windows-amd64.exe
        // in one directory). Bundling pins the tunnel protocol version; a
        // stale client found on PATH can fail its handshake against the
        // current relay in an endless retry loop (observed as "bad handshake"
        // storms in anonymous/free mode).
        if self, err := os.Executable(); err == nil {
                dir := filepath.Dir(self)
                for _, name := range bundledClientNames() {
                        p := filepath.Join(dir, name)
                        if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
                                return p, nil
                        }
                }
        }
        // Pinned client embedded in THIS binary (internal/tunnel/clientbin):
        // extracted to ~/.samvnc/bin on first use. Preferred over PATH for
        // the same pinned-version reason as above — the embedded client is
        // known to speak the current relay protocol, while a stale
        // pip-installed one can get stuck in endless "bad handshake" retries.
        if p, err := clientbin.Extract(); err == nil && p != "" {
                return p, nil
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
        return "", fmt.Errorf("tunnel: aitun client not found (no embedded client for %s/%s; install with `pip install aitun` or see https://aitun.cc)",
                runtime.GOOS, runtime.GOARCH)
}

// bundledClientNames returns the expected file names of an aitun client
// binary for this platform, as shipped in the release archive.
func bundledClientNames() []string {
        name := fmt.Sprintf("aitun-client-%s-%s", runtime.GOOS, runtime.GOARCH)
        if runtime.GOOS == "windows" {
                return []string{name + ".exe", "aitun-client.exe", "aitun.exe"}
        }
        return []string{name}
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
