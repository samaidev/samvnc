// Package protocol defines the wire format for SamVNC remote-desktop sessions.
//
// SamVNC multiplexes a remote-desktop stream (screen frames, cursor, clipboard,
// input) over a single WebSocket. The WebSocket is itself carried over an
// aitun.cc TCP+TLS tunnel, so everything here is designed to be frugal with
// bandwidth and tolerant of latency.
//
// Two frame kinds are used:
//
//   - Text frames: JSON control messages (hello, key, mouse, clipboard, ...).
//   - Binary frames: a fixed-size binary header followed by an image payload.
//
// The binary header is host-byte-order independent (always little-endian) so a
// browser DataView can parse it directly.
package protocol

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Version is the protocol version negotiated in the hello handshake.
const Version = 1

// Frame format magic for binary screen frames.
const FrameMagic uint16 = 0x5356 // "SV"

// Binary header length in bytes.
const BinHeaderLen = 24

// ImageFormat enumerates the pixel encodings a server may emit.
type ImageFormat uint8

const (
	FormatJPEG ImageFormat = 1 // lossy, best for low/high quality streaming
	FormatPNG  ImageFormat = 2 // lossless, used for sharp text when bandwidth allows
	FormatRaw  ImageFormat = 3 // uncompressed BGRA, debug only
)

// Quality presets requested by the client.
type Quality uint8

const (
	QualityLow    Quality = 1 // max compression, grayscale-ish, ~10 fps target
	QualityMedium Quality = 2 // balanced
	QualityHigh   Quality = 3 // sharp, high fps
)

// ConnMode describes how the browser reached the server.
type ConnMode uint8

const (
	ModeAuto  ConnMode = 0 // server picks (P2P if reachable, else relay)
	ModeP2P   ConnMode = 1 // direct IPv4/IPv6
	ModeRelay ConnMode = 2 // via aitun.cc relay
)

// --- Client -> Server messages ---------------------------------------------

// Hello is the first message from the browser after the WS upgrade.
type Hello struct {
	Type      string  `json:"type"`       // "hello"
	Version   int     `json:"version"`    // protocol version
	Quality   Quality `json:"quality"`    // requested quality preset
	Width     int     `json:"width"`      // desired viewport width (0 = native)
	Height    int     `json:"height"`     // desired viewport height (0 = native)
	Mode      ConnMode `json:"mode"`      // preferred connection mode
	FPS       int     `json:"fps"`        // desired fps cap (0 = unlimited)
	Token     string  `json:"token"`      // session auth token (optional)
}

// KeyEvent models a keyboard event.
type KeyEvent struct {
	Type     string `json:"type"`     // "key"
	Key      string `json:"key"`      // e.g. "a", "Enter", "Shift"
	Code     string `json:"code"`     // DOM code e.g. "KeyA", "Enter"
	Down     bool   `json:"down"`     // true=press, false=release
	Repeat   bool   `json:"repeat"`
	Ctrl     bool   `json:"ctrl"`
	Alt      bool   `json:"alt"`
	Shift    bool   `json:"shift"`
	Meta     bool   `json:"meta"`
}

// MouseEvent models a pointer event.
type MouseEvent struct {
	Type  string `json:"type"`  // "mouse"
	X     int    `json:"x"`     // absolute coords relative to desktop
	Y     int    `json:"y"`
	Mask  uint16 `json:"mask"`  // button bitmask (see Button* constants)
	Wheel int16  `json:"wheel"` // vertical scroll delta (ticks)
}

// Button bitmask for MouseEvent.Mask.
const (
	ButtonLeft   uint16 = 1 << 0
	ButtonMiddle uint16 = 1 << 1
	ButtonRight  uint16 = 1 << 2
	ButtonX1     uint16 = 1 << 3
	ButtonX2     uint16 = 1 << 4
)

// ClipboardEvent carries shared clipboard contents.
type ClipboardEvent struct {
	Type      string `json:"type"`      // "clipboard"
	Text      string `json:"text"`
	Selection string `json:"selection"` // "clipboard" (default) or "primary"
}

// ClipboardRequest asks the server to return the current remote clipboard
// contents immediately (used by the browser on Ctrl+V: pull remote clip, write
// it to the local clipboard, then inject the Ctrl+V so the remote app pastes
// its own just-read content). Selection mirrors ClipboardEvent.Selection.
type ClipboardRequest struct {
	Type      string `json:"type"` // "clipboard_request"
	Selection string `json:"selection"`
}

// QualityChange dynamically switches quality mid-session.
type QualityChange struct {
	Type    string  `json:"type"`    // "quality"
	Quality Quality `json:"quality"`
}

// Resize requests a viewport size change.
type Resize struct {
	Type   string `json:"type"` // "resize"
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// Ping keeps the link alive and measures RTT.
type Ping struct {
	Type string `json:"type"` // "ping"
	T    int64  `json:"t"`     // client millis
}

// --- Server -> Client messages ---------------------------------------------

// HelloAck is the server's reply to Hello.
type HelloAck struct {
	Type     string  `json:"type"`      // "hello_ack"
	Version  int     `json:"version"`
	Width    int     `json:"width"`     // desktop size
	Height   int     `json:"height"`
	Format   ImageFormat `json:"format"`
	Mode     ConnMode `json:"mode"`     // actual mode chosen
	Server   string  `json:"server"`    // "linux-x11" | "windows-gdi"
	P2P      bool    `json:"p2p"`       // whether P2P is in use
	RelayURL string  `json:"relay_url"` // public aitun.cc url
}

// CursorEvent notifies the client of the remote cursor shape/position.
type CursorEvent struct {
	Type    string `json:"type"` // "cursor"
	X       int    `json:"x"`
	Y       int    `json:"y"`
	Visible bool   `json:"visible"`
}

// Pong answers a Ping.
type Pong struct {
	Type string `json:"type"` // "pong"
	T    int64  `json:"t"`     // echoed client millis
	Ts   int64  `json:"ts"`    // server millis
}

// Stats reports session telemetry.
type Stats struct {
	Type      string  `json:"type"`
	FPS       float64 `json:"fps"`
	Kbps      float64 `json:"kbps"`
	LatencyMs int64   `json:"latency_ms"`
	Mode      ConnMode `json:"mode"`
}

// ErrorMsg terminates a session with a reason.
type ErrorMsg struct {
	Type   string `json:"type"` // "error"
	Reason string `json:"reason"`
}

// --- Binary frame header ---------------------------------------------------
//
// Layout (little-endian, 24 bytes):
//
//	0  magic    uint16  // 0x5356
//	2  reserved uint8
//	3  format   uint8   // ImageFormat
//	4  seq      uint32  // monotonic frame number
//	8  x        uint16
//	10 y        uint16
//	12 w        uint16
//	14 h        uint16
//	16 quality  uint8
//	17 flags    uint8   // bit0: full-frame (vs incremental)
//	18 payload  uint32  // payload length following the header
//	22 crc32    uint16  // lower 16 bits of crc32(payload) (0 to skip)

// FrameFlags
const (
	FlagFullFrame uint8 = 1 << 0
)

// BinHeader is the parsed binary screen-frame header.
type BinHeader struct {
	Magic    uint16
	Reserved uint8
	Format   ImageFormat
	Seq      uint32
	X        uint16
	Y        uint16
	W        uint16
	H        uint16
	Quality  Quality
	Flags    uint8
	Payload  uint32
	CRC      uint16
}

// Encode writes the binary header to w.
func (h *BinHeader) Encode(w io.Writer) error {
	var b [BinHeaderLen]byte
	binary.LittleEndian.PutUint16(b[0:2], h.Magic)
	b[2] = h.Reserved
	b[3] = byte(h.Format)
	binary.LittleEndian.PutUint32(b[4:8], h.Seq)
	binary.LittleEndian.PutUint16(b[8:10], h.X)
	binary.LittleEndian.PutUint16(b[10:12], h.Y)
	binary.LittleEndian.PutUint16(b[12:14], h.W)
	binary.LittleEndian.PutUint16(b[14:16], h.H)
	b[16] = byte(h.Quality)
	b[17] = h.Flags
	binary.LittleEndian.PutUint32(b[18:22], h.Payload)
	binary.LittleEndian.PutUint16(b[22:24], h.CRC)
	_, err := w.Write(b[:])
	return err
}

// DecodeBinHeader reads and parses a binary header from r.
func DecodeBinHeader(r io.Reader) (BinHeader, error) {
	var b [BinHeaderLen]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return BinHeader{}, err
	}
	h := BinHeader{
		Magic:    binary.LittleEndian.Uint16(b[0:2]),
		Reserved: b[2],
		Format:   ImageFormat(b[3]),
		Seq:      binary.LittleEndian.Uint32(b[4:8]),
		X:        binary.LittleEndian.Uint16(b[8:10]),
		Y:        binary.LittleEndian.Uint16(b[10:12]),
		W:        binary.LittleEndian.Uint16(b[12:14]),
		H:        binary.LittleEndian.Uint16(b[14:16]),
		Quality:  Quality(b[16]),
		Flags:    b[17],
		Payload:  binary.LittleEndian.Uint32(b[18:22]),
		CRC:      binary.LittleEndian.Uint16(b[22:24]),
	}
	if h.Magic != FrameMagic {
		return h, fmt.Errorf("protocol: bad frame magic %#x", h.Magic)
	}
	return h, nil
}

// EncodeJSON marshals a control message to a text frame payload.
func EncodeJSON(msg any) ([]byte, error) {
	return json.Marshal(msg)
}

// ErrUnknownMessage is returned when a control message has no recognised type.
var ErrUnknownMessage = errors.New("protocol: unknown message type")

// DecodeClientMessage parses a client text-frame payload into a typed value.
func DecodeClientMessage(data []byte) (any, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, err
	}
	switch probe.Type {
	case "hello":
		var m Hello
		err := json.Unmarshal(data, &m)
		return m, err
	case "key":
		var m KeyEvent
		err := json.Unmarshal(data, &m)
		return m, err
	case "mouse":
		var m MouseEvent
		err := json.Unmarshal(data, &m)
		return m, err
	case "clipboard":
		var m ClipboardEvent
		err := json.Unmarshal(data, &m)
		return m, err
	case "clipboard_request":
		var m ClipboardRequest
		err := json.Unmarshal(data, &m)
		return m, err
	case "quality":
		var m QualityChange
		err := json.Unmarshal(data, &m)
		return m, err
	case "resize":
		var m Resize
		err := json.Unmarshal(data, &m)
		return m, err
	case "ping":
		var m Ping
		err := json.Unmarshal(data, &m)
		return m, err
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnknownMessage, probe.Type)
	}
}
