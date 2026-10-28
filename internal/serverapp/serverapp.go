// Package serverapp runs the samvnc server stack (HTTP API + desktop
// capture + aitun tunnel) as an embeddable, stoppable application.
//
// It exists so the tray binary can host the server IN-PROCESS instead of
// spawning a separate samvnc-server subprocess. One binary — samvnc-tray —
// is now the complete product on every platform; the standalone
// cmd/samvnc-server remains only as a thin CLI wrapper for headless/CI
// setups, and is no longer shipped as a release asset.
//
// Typical usage:
//
//	app, err := serverapp.Start(serverapp.Options{
//	        Config:     cfg,
//	        AgentToken: agentToken,           // aitagt_… or "" for free mode
//	        OnURL:      func(u string) { … }, // public URL once known
//	})
//	…
//	app.Stop() // idempotent
package serverapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/samaidev/samvnc/internal/aitunmgr"
	"github.com/samaidev/samvnc/internal/config"
	"github.com/samaidev/samvnc/internal/server"
	"github.com/samaidev/samvnc/internal/settings"
	"github.com/samaidev/samvnc/internal/substore"
	"github.com/samaidev/samvnc/internal/tunnel"
)

// Options configures a serverapp run.
type Options struct {
	// Config is the resolved server configuration (flags/env applied by
	// the caller). An empty DataDir is defaulted to ~/.samvnc.
	Config config.Config

	// AgentToken is the aitun.cc agent token (aitagt_...) used for
	// auto-registering a subdomain. When the config already carries a
	// tunnel token (ait_...), the tunnel token wins and this is unused.
	// Empty means free proxy-address mode (random ~24h URL).
	AgentToken string

	// OnURL, when set, is called with the public base URL (no ?token=
	// query — the caller appends its own auth token) as soon as the
	// tunnel reports one.
	OnURL func(publicURL string)
}

// App is a running server stack. Create one with Start; stop it with Stop.
type App struct {
	mu      sync.Mutex
	cfg     config.Config
	srv     *server.Server
	httpSrv *http.Server
	tun     *tunnel.Tunnel
	url     string
	done    chan struct{}
	stopped bool
}

// Start brings up the full server stack and returns once it is serving.
// The returned App keeps running in the background until Stop is called.
func Start(opts Options) (*App, error) {
	cfg := opts.Config

	// Default DataDir to ~/.samvnc if not set.
	if cfg.DataDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			cfg.DataDir = home + "/.samvnc"
		} else {
			cfg.DataDir = ".samvnc"
		}
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config error: %w", err)
	}

	log.Printf("SamVNC starting (quality=%s mode=%s fps=%d clipboard=%v cdp=%s)",
		cfg.DefaultQuality, cfg.Mode, cfg.MaxFPS, cfg.Clipboard, cfg.CDPAddr)

	// Auto-generate the access token when none is provided. An empty
	// token used to DISABLE auth — with a public tunnel URL that meant
	// anyone who found the URL could control the desktop. Instead, a
	// random token is generated and persisted in <data-dir>/auth_token
	// so the printed URL keeps working across restarts. (The tray app
	// always passes its own token, so this only affects standalone runs.)
	if cfg.AuthToken == "" {
		tok, generated, err := loadOrGenAuthToken(cfg.DataDir)
		if err != nil {
			log.Printf("[samvnc] warning: could not persist auto-generated token: %v", err)
		}
		cfg.AuthToken = tok
		if generated {
			log.Printf("[samvnc] no token configured — auto-generated access token: %s", tok)
		} else {
			log.Printf("[samvnc] reusing saved access token: %s", tok)
		}
	}

	srv := server.New(cfg, "")

	// Bind the listening socket up front so we can retry on a busy port.
	// If the configured port (default 8091) is already in use, we try
	// port+1, port+2, ... up to 5 attempts before giving up. The actual
	// port that succeeded is propagated back into cfg.ListenAddr /
	// cfg.Tunnel.LocalPort so the aitun tunnel connects to the right
	// local port.
	const portRetryCount = 5
	ln, actualAddr, err := pickListener(cfg.ListenAddr, portRetryCount)
	if err != nil {
		return nil, fmt.Errorf("bind local WebSocket address (tried %s + %d attempts): %w",
			cfg.ListenAddr, portRetryCount, err)
	}
	if actualAddr != cfg.ListenAddr {
		log.Printf("[samvnc] address %s was busy, using %s instead", cfg.ListenAddr, actualAddr)
		cfg.ListenAddr = actualAddr
		// Also update the tunnel's local port so aitun forwards to the
		// right place.
		if cfg.Tunnel.LocalPort == 0 {
			cfg.Tunnel.LocalPort = portFromAddr(actualAddr)
		}
	}
	log.Printf("local UI:   http://%s", actualAddr)

	app := &App{
		cfg:     cfg,
		srv:     srv,
		httpSrv: &http.Server{Handler: srv.Handler()},
		done:    make(chan struct{}),
	}

	// Serve on the already-bound listener so the retry logic above can
	// pick a free port.
	go func() {
		if err := app.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[samvnc] http: %v", err)
		}
	}()

	port := portFromAddr(cfg.ListenAddr)
	if cfg.Tunnel.LocalPort != 0 {
		port = cfg.Tunnel.LocalPort
	}

	if cfg.Tunnel.Enabled {
		app.startTunnel(port, opts.AgentToken, opts.OnURL)
	} else {
		log.Printf("tunnel disabled; open http://%s in a browser on this host", cfg.ListenAddr)
	}

	return app, nil
}

// URL returns the public tunnel base URL, or "" when the tunnel has not
// reported one yet (or is disabled / failed).
func (a *App) URL() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.url
}

// Wait blocks until the app is stopped (via Stop). It is the in-process
// equivalent of waiting on the old samvnc-server child process.
func (a *App) Wait() {
	<-a.done
}

// Stop shuts the stack down: tunnel client first (so no new clients
// arrive), then the HTTP server with a short drain timeout. Idempotent
// and safe to call from multiple goroutines.
func (a *App) Stop() {
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return
	}
	a.stopped = true
	tun := a.tun
	a.mu.Unlock()

	log.Printf("[samvnc] shutting down…")

	if tun != nil {
		tun.Stop()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = a.httpSrv.Shutdown(ctx)

	// Unblock Wait()ers.
	select {
	case <-a.done:
	default:
		close(a.done)
	}
}

// startTunnel registers/reuses the aitun subdomain when only an agent
// token is available, spawns the tunnel client, and reports the public
// URL through both the App state and the OnURL callback.
//
// Failure to establish a tunnel is non-fatal: the local HTTP API keeps
// serving and the caller may retry later with a fresh Start.
func (a *App) startTunnel(port int, agentToken string, onURL func(string)) {
	cfg := a.cfg
	tunnelToken := cfg.Tunnel.Token

	// If no tunnel token (ait_...) is provided but we have an agent token
	// (aitagt_...) or the built-in default, auto-register a subdomain via
	// the aitun.cc management API. This gives samvnc a "just run" experience
	// similar to samcommand.
	//
	// When both tunnelToken and agentToken are empty, skip subdomain
	// registration entirely and start an anonymous ephemeral tunnel
	// (proxy address mode). The user gets a random https://xxx.t.aitun.cc URL.
	if tunnelToken == "" && agentToken != "" {
		// Resolve the subdomain name with the same precedence as samcommand:
		//   a. Explicit --aitun-subdomain / SAMVNC_AITUN_SUBDOMAIN
		//   b. Persisted subdomain from a previous successful run (stable URL)
		//   c. Fresh auto-generated name (first run)
		subName := cfg.Tunnel.Subdomain
		subdomainFromState := false
		if subName == "" {
			if saved, err := substore.Load(cfg.DataDir); err != nil {
				log.Printf("[samvnc] warning: could not load saved subdomain: %v", err)
			} else if saved != nil {
				subName = saved.Name
				subdomainFromState = true
				log.Printf("[samvnc] reusing saved subdomain: %s (registered %s)",
					saved.Name, saved.RegisteredAt)
			}
		}
		if subName == "" {
			subName = generateSubdomain()
			log.Printf("[samvnc] no subdomain set, generated: %s", subName)
		}

		client := aitunmgr.New(agentToken)

		// Register / reuse with a small retry loop. On conflict we generate
		// a fresh name (only when the original was auto-generated, not when
		// the user explicitly asked for a specific name).
		var sub *aitunmgr.Subdomain
		for attempt := 1; ; attempt++ {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			s, err := client.EnsureSubdomain(ctx, subName, "t.aitun.cc", true)
			cancel()
			if err == nil {
				sub = s
				break
			}
			if aitunmgr.IsConflict(err) && cfg.Tunnel.Subdomain == "" && !subdomainFromState && attempt < 4 {
				oldName := subName
				subName = generateSubdomain()
				log.Printf("[samvnc] subdomain %q taken, retrying with: %s", oldName, subName)
				continue
			}
			// Saved subdomain no longer exists on aitun.cc side: clear state
			// and retry once with a fresh name.
			if subdomainFromState && attempt < 2 {
				log.Printf("[samvnc] saved subdomain %q no longer usable (%v); generating a fresh one",
					subName, err)
				_ = substore.Clear(cfg.DataDir)
				subdomainFromState = false
				subName = generateSubdomain()
				continue
			}
			log.Printf("[samvnc] auto-register subdomain failed: %v", err)
			log.Printf("[samvnc] continuing without tunnel. Set --aitun-token or --aitun-agent-token manually.")
			return
		}

		// Persist the subdomain so the next run reuses the same public URL.
		// Skip when the user explicitly passed --aitun-subdomain (they manage
		// the name themselves).
		if cfg.Tunnel.Subdomain == "" {
			info := &substore.Info{
				Name:       sub.Subdomain,
				ID:         sub.ID,
				DomainBase: sub.DomainBase,
				DomainURL:  sub.DomainURL,
				Token:      sub.Token,
			}
			if err := substore.Save(cfg.DataDir, info); err != nil {
				log.Printf("[samvnc] warning: could not persist subdomain state: %v", err)
			} else if p, _ := substore.Path(cfg.DataDir); p != "" {
				log.Printf("[samvnc] subdomain state saved to %s", p)
			}
		}

		tunnelToken = sub.Token
		log.Printf("[samvnc] subdomain ready: %s (id=%s)", sub.DomainURL, sub.ID)
		log.Printf("[samvnc] public URL: https://%s", sub.DomainURL)
		a.srv.SetRelayURL("https://" + sub.DomainURL)
		if cfg.AuthToken != "" {
			log.Printf("[samvnc] access token: %s", cfg.AuthToken)
			log.Printf("[samvnc] open in browser: %s",
				urlWithToken("https://"+sub.DomainURL, cfg.AuthToken))
		}
	}

	if tunnelToken == "" && agentToken == "" {
		log.Printf("[samvnc] no aitun key configured — starting free " +
			"proxy-address mode (random https://aitun.cc/<code> URL, valid ~24h; " +
			"set a key in the tray settings for a permanent subdomain)")
	}

	tcfg := tunnel.Config{
		Binary:    cfg.Tunnel.Binary,
		Token:     tunnelToken,
		LocalPort: port,
		TCPOnly:   cfg.Tunnel.TCPOnly,
		Subdomain: cfg.Tunnel.Subdomain,
		// Isolate the client's persisted identity in our data dir:
		// sharing ~/.aitun with the user's other aitun clients makes
		// the relay treat them as ONE client and the tunnels kick
		// each other out ("Connection replaced by newer connection").
		Home: filepath.Join(cfg.DataDir, "aitun-home"),
		Logf: func(format string, args ...any) {
			log.Printf("[aitun] "+format, args...)
		},
	}
	t, err := tunnel.Start(tcfg, 90*time.Second)
	if err != nil {
		log.Printf("tunnel: %v (continuing without public URL; use --tunnel off or install aitun)", err)
		return
	}
	a.mu.Lock()
	a.tun = t
	a.mu.Unlock()

	if u := t.URL(); u != "" {
		a.setURL(u, onURL)
		a.srv.SetRelayURL(u)
		log.Printf("public URL: %s", u)
		if cfg.AuthToken != "" {
			log.Printf("access token: %s", cfg.AuthToken)
			log.Printf("open in browser: %s", urlWithToken(u, cfg.AuthToken))
		}
		log.Printf("open this URL in any browser to control the desktop remotely")
	}
	go func() {
		<-t.Done()
		log.Printf("tunnel exited")
	}()
}

// setURL records the public URL and fires the OnURL callback (never with
// a query string — callers append their own auth token).
func (a *App) setURL(u string, onURL func(string)) {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		u = u[:i]
	}
	a.mu.Lock()
	changed := a.url != u
	a.url = u
	a.mu.Unlock()
	if changed && onURL != nil && u != "" {
		onURL(u)
	}
}

// generateSubdomain returns a unique, DNS-safe name like "samvnc-<host>-<rand>".
//
// The random suffix is 5 bytes (10 hex chars, 40 bits of entropy). See the
// comment on the samcommand equivalent for collision-probability math.
func generateSubdomain() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "agent"
	}
	host = sanitize(host)
	if len(host) > 16 {
		host = host[:16]
	}
	return fmt.Sprintf("samvnc-%s-%s", host, randHex(5))
}

func sanitize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteRune('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "agent"
	}
	return out
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "000000"
	}
	return hex.EncodeToString(b)
}

// urlWithToken appends the auth token to a public URL as ?token=…
// (or &token=… when the URL already carries a query string).
func urlWithToken(u, token string) string {
	if token == "" {
		return u
	}
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + "token=" + token
}

// loadOrGenAuthToken returns the persisted access token from
// <dataDir>/auth_token, generating and saving a fresh one when missing.
// The file is written with mode 0600 (it IS the desktop access secret).
func loadOrGenAuthToken(dataDir string) (token string, generated bool, err error) {
	p := filepath.Join(dataDir, "auth_token")
	if b, rerr := os.ReadFile(p); rerr == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			return t, false, nil
		}
	}
	t := settings.GenerateToken()
	if merr := os.MkdirAll(dataDir, 0o755); merr != nil {
		return t, true, merr
	}
	if werr := os.WriteFile(p, []byte(t+"\n"), 0o600); werr != nil {
		return t, true, werr
	}
	return t, true, nil
}

func portFromAddr(addr string) int {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			n, _ := strconv.Atoi(addr[i+1:])
			return n
		}
	}
	return 8091
}

// pickListener tries to bind a TCP listener on baseAddr (e.g. "127.0.0.1:8091"),
// and on failure retries with port+1, port+2, ... up to maxRetries attempts.
//
// This avoids the "address already in use" hard failure when the default port
// (8091) is already taken by another process (e.g. another samvnc instance, or
// a leftover process from a previous run). The caller propagates the returned
// actual address back into cfg.ListenAddr so the aitun tunnel is started
// against the correct local port.
//
// On success the returned net.Listener is already bound; the caller must Serve
// on it. On exhaustion of retries the last bind error is returned.
//
// This mirrors the equivalent pickListener in samaidev/samcommand/main.go so
// samvnc and samcommand behave consistently on busy ports.
func pickListener(baseAddr string, maxRetries int) (net.Listener, string, error) {
	if maxRetries < 1 {
		maxRetries = 1
	}

	// Split baseAddr into "host:port" so we can increment the port.
	host, portStr, err := net.SplitHostPort(baseAddr)
	if err != nil {
		// baseAddr might be just ":port" or "port" — try to recover.
		// If we can't parse it, fall back to one attempt with the raw addr.
		host = "127.0.0.1"
		portStr = ""
	}
	basePort, _ := strconv.Atoi(portStr)
	if basePort == 0 {
		basePort = 8091
	}

	var lastErr error
	for i := 0; i < maxRetries; i++ {
		port := basePort + i
		if port <= 0 || port > 65535 {
			lastErr = fmt.Errorf("port %d out of valid range (1-65535)", port)
			break
		}
		tryAddr := net.JoinHostPort(host, strconv.Itoa(port))
		ln, err := net.Listen("tcp", tryAddr)
		if err != nil {
			log.Printf("[samvnc] address %s unavailable (%v); ", tryAddr, err)
			if i+1 < maxRetries {
				log.Printf("retrying on %d...", port+1)
			} else {
				log.Printf("no more retries.")
			}
			lastErr = err
			continue
		}
		return ln, tryAddr, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no available port in range %d-%d", basePort, basePort+maxRetries-1)
	}
	return nil, "", fmt.Errorf("could not bind after %d attempts: %w", maxRetries, lastErr)
}
