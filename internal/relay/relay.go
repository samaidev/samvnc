// Package relay implements the "smart relay or P2P" connection policy.
//
// aitun.cc supports both IPv6/IPv4 P2P direct connection and server relay.
// SamVNC wraps that with a small decision layer so a session can be steered
// toward the lowest-latency path automatically:
//
//   - "p2p"   : force direct P2P (best latency, needs reachable endpoints).
//   - "relay" : force aitun.cc relay (always works, a bit more latency).
//   - "auto"  : prefer P2P when the local network looks reachable, else relay.
//
// The picker is intentionally heuristic and dependency-free: it probes the
// local interface addresses and an optional peer hint, then emits a Decision.
package relay

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/samaidev/samvnc/internal/protocol"
)

// Decision is the outcome of the smart connection picker.
type Decision struct {
	Mode   protocol.ConnMode
	P2P    bool
	Reason string
}

// Picker decides the connection mode for a session.
type Picker struct {
	Preferred protocol.ConnMode
}

// New returns a Picker honouring the given mode string (auto|p2p|relay).
func New(mode string) *Picker {
	return &Picker{Preferred: parseMode(mode)}
}

// Decide evaluates the current environment and returns a Decision.
//
// peerHint may be "" or "ip:port"/"ip" supplied by the client; when the peer
// shares a private subnet with us we prefer P2P.
func (p *Picker) Decide(peerHint string) Decision {
	switch p.Preferred {
	case protocol.ModeP2P:
		return Decision{Mode: protocol.ModeP2P, P2P: true, Reason: "p2p forced by config"}
	case protocol.ModeRelay:
		return Decision{Mode: protocol.ModeRelay, P2P: false, Reason: "relay forced by config"}
	default:
		return p.decideAuto(peerHint)
	}
}

func (p *Picker) decideAuto(peerHint string) Decision {
	// Gather our local addresses.
	addrs := localAddrs()
	hasPublic := false
	hasPrivate := false
	for _, a := range addrs {
		if isPublic(a) {
			hasPublic = true
		} else {
			hasPrivate = true
		}
	}

	// If we have a public IPv4/v6 the endpoint is directly reachable -> P2P.
	if hasPublic {
		return Decision{Mode: protocol.ModeP2P, P2P: true, Reason: "public IP present, prefer direct P2P"}
	}

	// No public address on our side: can we reach the peer directly?
	if peer := net.ParseIP(hostOnly(peerHint)); peer != nil {
		if sharesSubnet(peer, addrs) {
			return Decision{Mode: protocol.ModeP2P, P2P: true, Reason: "peer shares local subnet, direct P2P"}
		}
		if peer.To4() == nil {
			// IPv6 peer is often directly reachable.
			return Decision{Mode: protocol.ModeP2P, P2P: true, Reason: "ipv6 peer, attempt direct P2P"}
		}
	}

	_ = hasPrivate
	// Fall back to the aitun.cc relay.
	return Decision{Mode: protocol.ModeRelay, P2P: false, Reason: "no direct path, using aitun.cc relay"}
}

// ProbeP2P performs a quick TCP dial to the peer to confirm reachability.
// Returns true on success within the timeout. Used to validate an auto choice.
func ProbeP2P(addr string, timeout time.Duration) bool {
	if addr == "" {
		return false
	}
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func parseMode(s string) protocol.ConnMode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "p2p":
		return protocol.ModeP2P
	case "relay":
		return protocol.ModeRelay
	default:
		return protocol.ModeAuto
	}
}

func localAddrs() []net.IP {
	out := []net.IP{}
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			out = append(out, ip)
		}
	}
	return out
}

func isPublic(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsPrivate() {
		return false
	}
	return true
}

func sharesSubnet(peer net.IP, addrs []net.IP) bool {
	for _, a := range addrs {
		if a.To4() == nil || peer.To4() == nil {
			continue
		}
		// Same /24 heuristic (good enough for LAN P2P discovery).
		if a.Mask(net.IPv4Mask(255, 255, 255, 0)).Equal(peer.Mask(net.IPv4Mask(255, 255, 255, 0))) {
			return true
		}
	}
	return false
}

func hostOnly(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// String renders a Decision for logs.
func (d Decision) String() string {
	mode := "auto"
	switch d.Mode {
	case protocol.ModeP2P:
		mode = "p2p"
	case protocol.ModeRelay:
		mode = "relay"
	}
	return fmt.Sprintf("relay: mode=%s p2p=%v (%s)", mode, d.P2P, d.Reason)
}
