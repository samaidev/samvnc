// SamVNC server entry point.
//
// SamVNC exposes a Linux (X11) or Windows (GDI) desktop to a browser over an
// aitun.cc TCP+TLS tunnel. Run it on the machine you want to control, then
// open the printed https://aitun.cc/<code> URL in any browser.
//
//      Usage:
//        samvnc-server [flags]
//
//      Flags:
//        -listen <addr>        local WebSocket bind address (default 127.0.0.1:8091)
//        -quality low|medium|high
//        -mode auto|p2p|relay
//        -fps <n>              frame rate cap (default 30)
//        -token <token>        client auth token (empty disables auth)
//        -tunnel on|off        spawn the aitun.cc client (default on)
//        -aitun-agent-token <t>  aitun.cc agent token (aitagt_...) for auto subdomain
//        -aitun-subdomain <s>  request a specific short subdomain (auto-generates if empty)
//        -aitun-token <token>  aitun.cc tunnel token (ait_...) — overrides auto-register
//        -tcp-only             use a TCP-only tunnel (no HTTP pass-through)
//        -no-clipboard         disable clipboard sharing
//        -verbose              verbose logging
//        -demo                 use synthetic in-memory desktop (headless)
//        -cdp-addr <addr>      Chrome DevTools Protocol address
//
//      Environment variables (SAMVNC_*) override the defaults; see config.FromEnv.
package main

import (
        "context"
        "crypto/rand"
        "encoding/hex"
        "flag"
        "fmt"
        "log"
        "net"
        "net/http"
        "os"
        "os/signal"
        "strconv"
        "strings"
        "syscall"
        "time"

        "github.com/samaidev/samvnc/internal/aitunmgr"
        "github.com/samaidev/samvnc/internal/config"
        "github.com/samaidev/samvnc/internal/server"
        "github.com/samaidev/samvnc/internal/substore"
        "github.com/samaidev/samvnc/internal/tunnel"
)

func main() {
        cfg := config.FromEnv()

        listen := flag.String("listen", cfg.ListenAddr, "local WebSocket bind address")
        quality := flag.String("quality", cfg.DefaultQuality, "quality preset: low|medium|high")
        mode := flag.String("mode", cfg.Mode, "connection mode: auto|p2p|relay")
        fps := flag.Int("fps", cfg.MaxFPS, "frame rate cap")
        token := flag.String("token", cfg.AuthToken, "client auth token (empty disables)")
        tunnelOn := flag.Bool("tunnel", cfg.Tunnel.Enabled, "spawn the aitun.cc client")
        aitunAgentToken := flag.String("aitun-agent-token", "", "aitun.cc agent token (aitagt_...) for auto subdomain registration")
        aitunSub := flag.String("aitun-subdomain", cfg.Tunnel.Subdomain, "aitun.cc subdomain name (auto-generated if empty)")
        aitunToken := flag.String("aitun-token", cfg.Tunnel.Token, "aitun.cc tunnel token (ait_...) — overrides auto-register")
        tcpOnly := flag.Bool("tcp-only", cfg.Tunnel.TCPOnly, "use a TCP-only tunnel")
        noClip := flag.Bool("no-clipboard", !cfg.Clipboard, "disable clipboard sharing")
        verbose := flag.Bool("verbose", cfg.Verbose, "verbose logging")
        demo := flag.Bool("demo", cfg.Demo, "use the synthetic in-memory desktop backend (headless)")
        cdpAddr := flag.String("cdp-addr", cfg.CDPAddr, "Chrome DevTools Protocol address (e.g. 127.0.0.1:9222)")
        dataDir := flag.String("data-dir", cfg.DataDir, "directory for persistent state (subdomain info). Defaults to ~/.samvnc. Env: SAMVNC_DATA_DIR")
        flag.Parse()

        cfg.ListenAddr = *listen
        cfg.DefaultQuality = *quality
        cfg.Mode = *mode
        cfg.MaxFPS = *fps
        cfg.AuthToken = *token
        cfg.Tunnel.Enabled = *tunnelOn
        cfg.Tunnel.Token = *aitunToken
        cfg.Tunnel.Subdomain = *aitunSub
        cfg.Tunnel.TCPOnly = *tcpOnly
        cfg.Clipboard = !*noClip
        cfg.Verbose = *verbose
        cfg.Demo = *demo
        cfg.CDPAddr = *cdpAddr
        cfg.DataDir = *dataDir

        if err := cfg.Validate(); err != nil {
                fmt.Fprintln(os.Stderr, "config error:", err)
                os.Exit(2)
        }

        // Default DataDir to ~/.samvnc if not set.
        if cfg.DataDir == "" {
                if home, err := os.UserHomeDir(); err == nil {
                        cfg.DataDir = home + "/.samvnc"
                } else {
                        cfg.DataDir = ".samvnc"
                }
        }

        log.SetFlags(log.LstdFlags | log.Lmicroseconds)
        log.Printf("SamVNC starting (quality=%s mode=%s fps=%d clipboard=%v cdp=%s)",
                cfg.DefaultQuality, cfg.Mode, cfg.MaxFPS, cfg.Clipboard, cfg.CDPAddr)

        srv := server.New(cfg, "")

        // Bind the listening socket up front so we can retry on a busy port.
        // If the configured port (default 8091) is already in use, we try
        // port+1, port+2, ... up to 5 attempts before giving up (mirroring
        // samcommand's behavior). The actual port that succeeded is propagated
        // back into cfg.ListenAddr / cfg.Tunnel.LocalPort so the aitun tunnel
        // connects to the right local port.
        const portRetryCount = 5
        ln, actualAddr, err := pickListener(cfg.ListenAddr, portRetryCount)
        if err != nil {
                log.Fatalf("bind local WebSocket address (tried %s + %d attempts): %v",
                        cfg.ListenAddr, portRetryCount, err)
        }
        if actualAddr != cfg.ListenAddr {
                log.Printf("[samvnc] address %s was busy, using %s instead", cfg.ListenAddr, actualAddr)
                cfg.ListenAddr = actualAddr
                // Also update the tunnel's local port so aitun forwards to the right place.
                if cfg.Tunnel.LocalPort == 0 {
                        cfg.Tunnel.LocalPort = portFromAddr(actualAddr)
                }
        }
        log.Printf("local UI:   http://%s", actualAddr)

        // HTTP server. We Serve() on the already-bound listener so that the
        // retry logic above can pick a free port.
        httpSrv := &http.Server{
                Handler: srv.Handler(),
        }

        // Determine the local port for the tunnel to forward.
        port := portFromAddr(cfg.ListenAddr)
        if cfg.Tunnel.LocalPort != 0 {
                port = cfg.Tunnel.LocalPort
        }

        // Start the HTTP server on the bound listener.
        go func() {
                if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
                        log.Fatalf("http: %v", err)
                }
        }()

        if cfg.Tunnel.Enabled {
                startTunnel(cfg, port, srv, *aitunAgentToken)
        } else {
                log.Printf("tunnel disabled; open http://%s in a browser on this host", cfg.ListenAddr)
        }

        // Wait for interrupt.
        stop := make(chan os.Signal, 1)
        signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
        <-stop
        log.Printf("shutting down…")

        ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
        defer cancel()
        _ = httpSrv.Shutdown(ctx)
}

func startTunnel(cfg config.Config, port int, srv *server.Server, agentToken string) {
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
                srv.SetRelayURL("https://" + sub.DomainURL)
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
                Logf: func(format string, args ...any) {
                        log.Printf("[aitun] "+format, args...)
                },
        }
        t, err := tunnel.Start(tcfg, 90*time.Second)
        if err != nil {
                log.Printf("tunnel: %v (continuing without public URL; use --tunnel off or install aitun)", err)
                return
        }
        if u := t.URL(); u != "" {
                srv.SetRelayURL(u)
                log.Printf("public URL: %s", u)
                log.Printf("open this URL in any browser to control the desktop remotely")
        }
        go func() {
                <-t.Done()
                log.Printf("tunnel exited")
        }()
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
