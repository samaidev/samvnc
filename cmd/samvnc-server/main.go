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
//        -listen <addr>        local WebSocket bind address (default 127.0.0.1:8090)
//        -quality low|medium|high
//        -mode auto|p2p|relay
//        -fps <n>              frame rate cap (default 30)
//        -token <token>        client auth token (empty disables auth)
//        -tunnel on|off        spawn the aitun.cc client (default on)
//        -aitun-token <token>  aitun.cc account token for permanent subdomains
//        -aitun-subdomain <s>  request a specific short subdomain
//        -tcp-only             use a TCP-only tunnel (no HTTP pass-through)
//        -no-clipboard         disable clipboard sharing
//        -verbose              verbose logging
//
//      Environment variables (SAMVNC_*) override the defaults; see config.FromEnv.
package main

import (
        "context"
        "flag"
        "fmt"
        "log"
        "net/http"
        "os"
        "os/signal"
        "strconv"
        "syscall"
        "time"

        "github.com/samaidev/samvnc/internal/config"
        "github.com/samaidev/samvnc/internal/server"
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
        aitunToken := flag.String("aitun-token", cfg.Tunnel.Token, "aitun.cc account token")
        aitunSub := flag.String("aitun-subdomain", cfg.Tunnel.Subdomain, "aitun.cc subdomain")
        tcpOnly := flag.Bool("tcp-only", cfg.Tunnel.TCPOnly, "use a TCP-only tunnel")
        noClip := flag.Bool("no-clipboard", !cfg.Clipboard, "disable clipboard sharing")
        verbose := flag.Bool("verbose", cfg.Verbose, "verbose logging")
        demo := flag.Bool("demo", cfg.Demo, "use the synthetic in-memory desktop backend (headless)")
        cdpAddr := flag.String("cdp-addr", cfg.CDPAddr, "Chrome DevTools Protocol address (e.g. 127.0.0.1:9222)")
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

        if err := cfg.Validate(); err != nil {
                fmt.Fprintln(os.Stderr, "config error:", err)
                os.Exit(2)
        }

        log.SetFlags(log.LstdFlags | log.Lmicroseconds)
        log.Printf("SamVNC starting (quality=%s mode=%s fps=%d clipboard=%v cdp=%s)",
                cfg.DefaultQuality, cfg.Mode, cfg.MaxFPS, cfg.Clipboard, cfg.CDPAddr)

        srv := server.New(cfg, "")

        // HTTP server.
        httpSrv := &http.Server{
                Addr:    cfg.ListenAddr,
                Handler: srv.Handler(),
        }

        // Determine the local port for the tunnel to forward.
        port := portFromAddr(cfg.ListenAddr)
        if cfg.Tunnel.LocalPort != 0 {
                port = cfg.Tunnel.LocalPort
        }

        // Start the aitun.cc tunnel (optional). The HTTP server must be up first
        // so the tunnel has something to forward to.
        go func() {
                if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
                        log.Fatalf("http: %v", err)
                }
        }()
        log.Printf("local UI:   http://%s", cfg.ListenAddr)

        if cfg.Tunnel.Enabled {
                startTunnel(cfg, port, srv)
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

func startTunnel(cfg config.Config, port int, srv *server.Server) {
        tcfg := tunnel.Config{
                Binary:    cfg.Tunnel.Binary,
                Token:     cfg.Tunnel.Token,
                LocalPort: port,
                TCPOnly:   cfg.Tunnel.TCPOnly,
                Subdomain: cfg.Tunnel.Subdomain,
                Logf: func(format string, args ...any) {
                        log.Printf("[aitun] "+format, args...)
                },
        }
        t, err := tunnel.Start(tcfg, 30*time.Second)
        if err != nil {
                log.Printf("tunnel: %v (continuing without public URL; use SAMVNC_TUNNEL=off or install aitun)", err)
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

func portFromAddr(addr string) int {
        for i := len(addr) - 1; i >= 0; i-- {
                if addr[i] == ':' {
                        n, _ := strconv.Atoi(addr[i+1:])
                        return n
                }
        }
        return 8090
}
