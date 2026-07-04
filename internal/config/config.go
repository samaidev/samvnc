// Package config holds runtime configuration for the SamVNC server.
package config

import (
        "fmt"
        "os"
        "strconv"
        "strings"
)

// Config is the resolved server configuration.
type Config struct {
        // ListenAddr is the local address the WebSocket server binds to.
        ListenAddr string

        // Tunnel configuration (aitun.cc).
        Tunnel TunnelConfig

        // Auth token required from clients (empty disables auth).
        AuthToken string

        // Quality / encoding defaults.
        DefaultQuality string
        MaxFPS         int

        // Clipboard sharing.
        Clipboard bool

        // Preferred connection mode: auto | p2p | relay.
        Mode string

        // Static web assets directory. If empty, the embedded UI is used.
        WebDir string

        // Demo forces the synthetic in-memory desktop backend (no real display).
        // Useful on headless servers and for trying SamVNC without a desktop.
        Demo bool

        // Verbose logging.
        Verbose bool

        // CDPAddr is the Chrome DevTools Protocol address (e.g. "127.0.0.1:9222").
        // When set, /api/* endpoints can control Chrome via CDP.
        CDPAddr string
}

// TunnelConfig configures the aitun.cc TCP+TLS transport.
type TunnelConfig struct {
        // Enabled controls whether the server spawns the aitun client.
        Enabled bool

        // Binary path to the aitun client executable. If empty, the server
        // searches PATH for "aitun" / "aitun-client".
        Binary string

        // Token is the aitun.cc account token (gives permanent subdomains).
        // When empty an ephemeral anonymous tunnel is requested.
        Token string

        // LocalPort is the local port forwarded through the tunnel. When 0 it
        // matches the WebSocket server port.
        LocalPort int

        // TCPOnly requests a TCP-only tunnel (no HTTP/WebSocket pass-through).
        // The server then runs its own TLS front-end on top of the relayed port.
        TCPOnly bool

        // Subdomain requests a specific short subdomain (requires a token).
        Subdomain string
}

// Default returns a sane default configuration.
func Default() Config {
        return Config{
                ListenAddr:     "127.0.0.1:8090",
                DefaultQuality: "medium",
                MaxFPS:         15,
                Clipboard:      true,
                Mode:           "auto",
                Tunnel: TunnelConfig{
                        Enabled: true,
                },
        }
}

// FromEnv resolves configuration from environment variables. SAMVNC_* prefix.
func FromEnv() Config {
        c := Default()

        if v := os.Getenv("SAMVNC_LISTEN"); v != "" {
                c.ListenAddr = v
        }
        if v := os.Getenv("SAMVNC_TOKEN"); v != "" {
                c.AuthToken = v
        }
        if v := os.Getenv("SAMVNC_QUALITY"); v != "" {
                c.DefaultQuality = v
        }
        if v := os.Getenv("SAMVNC_FPS"); v != "" {
                if n, err := strconv.Atoi(v); err == nil {
                        c.MaxFPS = n
                }
        }
        if v := os.Getenv("SAMVNC_CLIPBOARD"); v != "" {
                c.Clipboard = parseBool(v)
        }
        if v := os.Getenv("SAMVNC_MODE"); v != "" {
                c.Mode = v
        }
        if v := os.Getenv("SAMVNC_WEBDIR"); v != "" {
                c.WebDir = v
        }
        if v := os.Getenv("SAMVNC_VERBOSE"); v != "" {
                c.Verbose = parseBool(v)
        }
        if v := os.Getenv("SAMVNC_DEMO"); v != "" {
                c.Demo = parseBool(v)
        }

        // Tunnel env.
        if v := os.Getenv("SAMVNC_TUNNEL"); v != "" {
                c.Tunnel.Enabled = parseBool(v)
        }
        if v := os.Getenv("SAMVNC_AITUN_BIN"); v != "" {
                c.Tunnel.Binary = v
        }
        if v := os.Getenv("SAMVNC_AITUN_TOKEN"); v != "" {
                c.Tunnel.Token = v
        }
        if v := os.Getenv("SAMVNC_AITUN_PORT"); v != "" {
                if n, err := strconv.Atoi(v); err == nil {
                        c.Tunnel.LocalPort = n
                }
        }
        if v := os.Getenv("SAMVNC_AITUN_TCP_ONLY"); v != "" {
                c.Tunnel.TCPOnly = parseBool(v)
        }
        if v := os.Getenv("SAMVNC_AITUN_SUBDOMAIN"); v != "" {
                c.Tunnel.Subdomain = v
        }
        if v := os.Getenv("SAMVNC_CDP_ADDR"); v != "" {
                c.CDPAddr = v
        }

        return c
}

// Validate checks the configuration for obvious mistakes.
func (c Config) Validate() error {
        switch strings.ToLower(c.DefaultQuality) {
        case "low", "medium", "high":
        default:
                return fmt.Errorf("invalid quality %q (want low|medium|high)", c.DefaultQuality)
        }
        switch strings.ToLower(c.Mode) {
        case "auto", "p2p", "relay":
        default:
                return fmt.Errorf("invalid mode %q (want auto|p2p|relay)", c.Mode)
        }
        return nil
}

func parseBool(v string) bool {
        switch strings.ToLower(strings.TrimSpace(v)) {
        case "1", "true", "yes", "on", "y", "t":
                return true
        default:
                return false
        }
}
