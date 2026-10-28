// SamVNC server entry point (standalone CLI).
//
// NOTE: this command is a thin wrapper over internal/serverapp and is kept
// ONLY for headless/CI/scriptable setups and local development. It is NOT
// shipped as a release asset anymore — the published binary is
// samvnc-tray, which now embeds the full server stack in-process and
// works identically on desktops and headless machines.
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
//        -token <token>        client auth token (auto-generated and persisted in
//                              <data-dir>/auth_token when empty — auth is never disabled)
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
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/samaidev/samvnc/internal/config"
	"github.com/samaidev/samvnc/internal/serverapp"
)

func main() {
	cfg := config.FromEnv()

	listen := flag.String("listen", cfg.ListenAddr, "local WebSocket bind address")
	quality := flag.String("quality", cfg.DefaultQuality, "quality preset: low|medium|high")
	mode := flag.String("mode", cfg.Mode, "connection mode: auto|p2p|relay")
	fps := flag.Int("fps", cfg.MaxFPS, "frame rate cap")
	token := flag.String("token", cfg.AuthToken, "client auth token (auto-generated and persisted in <data-dir>/auth_token when empty)")
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

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	app, err := serverapp.Start(serverapp.Options{
		Config:     cfg,
		AgentToken: *aitunAgentToken,
	})
	if err != nil {
		log.Fatalf("samvnc-server: %v", err)
	}

	// Wait for interrupt, then shut the stack down.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	app.Stop()
}
