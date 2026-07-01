package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/samaidev/samvnc/internal/capture"
	"github.com/samaidev/samvnc/internal/clipboard"
	"github.com/samaidev/samvnc/internal/compress"
	"github.com/samaidev/samvnc/internal/config"
	"github.com/samaidev/samvnc/internal/demo"
	"github.com/samaidev/samvnc/internal/input"
	"github.com/samaidev/samvnc/internal/protocol"
	"github.com/samaidev/samvnc/internal/relay"
)

// HTTPSession is a remote-desktop session carried over either:
//   - SSE + POST (/stream + /input), for tunnels that allow a long-lived
//     response alongside short POSTs; or
//   - single-connection long polling (/poll), for tunnels that permit only
//     one concurrent connection (e.g. aitun.cc free subdomain tunnels).
//
// Both transports carry the same SamVNC protocol: JSON control messages and
// 24-byte-header binary screen frames.
type HTTPSession struct {
	srv      *Server
	capturer capture.Capturer
	injector input.Injector
	clip     clipboard.Manager
	enc      *compress.Encoder

	quality protocol.Quality
	fps     int
	mode    protocol.ConnMode
	p2p     bool

	writeCh chan sseMsg
	w       http.ResponseWriter
	flusher http.Flusher

	// poll-mode state. captureLoop always refreshes latestFrame; a /poll
	// request reads it. frameSig is a buffered-1 "new frame available" signal.
	// qualGen is bumped on quality change; captureLoop stamps each produced
	// frame with the current qualGen so a /poll can wait for a frame actually
	// encoded at the new quality instead of a stale one racing in.
	frameMu     sync.Mutex
	latestFrame []byte // binHeader + payload
	latestSeq   uint32
	frameGen    uint64 // qualGen when this frame was encoded
	qualGen     uint64 // bumped on quality change
	frameSig    chan struct{}
	lastPoll    time.Time
	pollMode    bool
	// lastPayload/lastPayloadGen dedupe identical consecutive frames (static
	// desktop) so /poll returns an empty frame instead of re-sending ~20KB.
	lastPayload     []byte
	lastPayloadGen  uint64

	done      chan struct{}
	closeOnce sync.Once
}

type sseMsg struct {
	event string // "ack" | "frame" | "clipboard" | "stats" | "error" | "" (comment)
	data  []byte // raw bytes: frame = binHeader+payload (base64-encoded on write); others = JSON
}

func newHTTPSession(s *Server, w http.ResponseWriter, f http.Flusher) *HTTPSession {
	var cap capture.Capturer
	var inj input.Injector
	var clip clipboard.Manager
	if s.cfg.Demo {
		cap = demo.NewCapturer()
		inj = demo.NewInjector()
		clip = demo.NewManager()
	} else {
		c, err := capture.New()
		if err != nil {
			s.logf("capture: native unavailable (%v), falling back to demo", err)
			c = demo.NewCapturer()
		}
		cap = c
		i, err := input.New()
		if err != nil {
			s.logf("input: native unavailable (%v), falling back to demo", err)
			i = demo.NewInjector()
		}
		inj = i
		m, err := clipboard.New()
		if err != nil {
			s.logf("clipboard: native unavailable (%v), falling back to demo", err)
			m = demo.NewManager()
		}
		clip = m
	}
	q := parseQuality(s.cfg.DefaultQuality)
	dec := s.picker.Decide("")
	return &HTTPSession{
		srv:      s,
		capturer: cap,
		injector: inj,
		clip:     clip,
		enc:      compress.New(q),
		quality:  q,
		fps:      s.cfg.MaxFPS,
		mode:     dec.Mode,
		p2p:      dec.P2P,
		writeCh:  make(chan sseMsg, 64),
		w:        w,
		flusher:  f,
		frameSig: make(chan struct{}, 1),
		lastPoll: time.Now(),
		done:     make(chan struct{}),
	}
}

// handleStream serves the SSE channel: hello_ack first, then a stream of
// screen frames / clipboard / stats until the browser closes the connection.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sess := newHTTPSession(s, w, flusher)
	s.setCurrentHTTP(sess)
	defer func() {
		s.setCurrentHTTP(nil)
		sess.close()
	}()

	s.logf("sse: session start backend=%s", sess.capturer.Name())

	// Sole SSE writer goroutine.
	go sess.writer()

	// hello_ack
	sess.sendAck()

	// Capture / clipboard / stats loops. They exit when `done` closes.
	go sess.captureLoop()
	if s.cfg.Clipboard {
		go sess.clipboardLoop()
	}
	go sess.statsLoop()
	go sess.heartbeatLoop()

	// Block until the browser disconnects (EventSource close / tab close).
	<-r.Context().Done()
	s.logf("sse: client disconnected")
}

// handleInput accepts a single client->server JSON message via POST and
// dispatches it to the active HTTP session's input injector / clipboard.
func (s *Server) handleInput(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sess := s.currentHTTP()
	if sess == nil {
		http.Error(w, "no active session", http.StatusConflict)
		return
	}
	var buf [1 << 16]byte
	n, _ := r.Body.Read(buf[:])
	if n == 0 {
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}
	msg, err := protocol.DecodeClientMessage(buf[:n])
	if err != nil {
		http.Error(w, "bad message: "+err.Error(), http.StatusBadRequest)
		return
	}
	sess.handleClientMessage(msg)
	w.WriteHeader(http.StatusNoContent)
}

// handleState returns the hello_ack JSON so a browser can learn the desktop
// size / backend before opening the SSE stream (useful for diagnostics).
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	sess := s.currentHTTP()
	if sess == nil {
		// Build a throwaway session just to report backend info.
		tmp := newHTTPSession(s, w, nil)
		defer tmp.capturer.Close()
		b, _ := json.Marshal(tmp.helloAck())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
		return
	}
	b, _ := json.Marshal(sess.helloAck())
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

// pollReq is the body of a /poll request. Inputs is a batch of client->server
// messages accumulated since the last poll, so input and frame transport share
// a single connection (required by single-concurrency tunnels like aitun free).
// ClipReq asks the server to echo the current remote clipboard in Clip.
type pollReq struct {
	Seq     int               `json:"seq"`
	Inputs  []json.RawMessage `json:"inputs"`
	ClipReq string            `json:"clip_req,omitempty"` // "" | "clipboard" | "primary"
}

// pollResp carries one round-trip: the newest frame (if any), the hello_ack on
// the first poll, current clipboard text, telemetry, and a pong if a ping was
// in the batch.
type pollResp struct {
	Seq       int                `json:"seq"`
	Ack       *protocol.HelloAck `json:"ack,omitempty"`
	Frame     string             `json:"frame,omitempty"` // base64(binHeader+payload)
	Clip      string             `json:"clip,omitempty"`
	ClipSel   string             `json:"clip_sel,omitempty"` // selection of Clip
	ClipPrime string             `json:"clip_prime,omitempty"` // current PRIMARY text
	Stats     *protocol.Stats    `json:"stats,omitempty"`
	Pong      *protocol.Pong     `json:"pong,omitempty"`
	Error     string             `json:"error,omitempty"`
}

// handlePoll implements single-connection long polling. The browser POSTs its
// accumulated inputs plus the last frame seq it has; the server processes the
// inputs, waits up to a short timeout for a fresher frame, and returns the
// newest frame + state. The browser immediately re-POSTs, so input and frames
// never need a second concurrent connection.
func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req pollReq
	body, err := io.ReadAll(r.Body)
	if err == nil && len(body) > 0 {
		_ = json.Unmarshal(body, &req)
	}

	// Find or create the poll-mode session.
	sess := s.currentHTTP()
	if sess == nil || !sess.pollMode {
		// A stale SSE session may be occupying the slot; let /poll take over.
		if sess != nil {
			sess.close()
		}
		sess = newHTTPSession(s, w, nil)
		sess.pollMode = true
		s.setCurrentHTTP(sess)
		s.logf("poll: session start backend=%s", sess.capturer.Name())
		go sess.captureLoop()
	}
	sess.frameMu.Lock()
	sess.lastPoll = time.Now()
	sess.frameMu.Unlock()

	// Auth + dispatch all accumulated inputs.
	for _, raw := range req.Inputs {
		if len(raw) == 0 {
			continue
		}
		msg, err := protocol.DecodeClientMessage(raw)
		if err != nil {
			continue
		}
		if h, ok := msg.(protocol.Hello); ok && s.cfg.AuthToken != "" && h.Token != s.cfg.AuthToken {
			resp := pollResp{Error: "invalid token"}
			writeJSON(w, resp)
			return
		}
		sess.handleClientMessage(msg)
	}

	// Wait for a frame newer than the client's last seq, or time out. The first
	// poll (seq=0) waits longer so the browser gets an initial frame in the
	// same round-trip as the hello_ack instead of a blank canvas. Subsequent
	// polls use a shorter wait that covers one capture tick (~66ms @ 15fps)
	// without padding latency on a slow tunnel.
	waitTimeout := 120 * time.Millisecond
	if req.Seq == 0 {
		waitTimeout = 500 * time.Millisecond
	}
	frame, fseq := sess.waitFrame(uint32(req.Seq), waitTimeout)

	resp := pollResp{Seq: int(fseq)}
	if req.Seq == 0 {
		ack := sess.helloAck()
		resp.Ack = &ack
		s.logf("session hello: %dx%d mode=%v p2p=%v server=%s", ack.Width, ack.Height, sess.mode, sess.p2p, sess.capturer.Name())
	}
	if frame != nil {
		resp.Frame = base64.StdEncoding.EncodeToString(frame)
	}
	if s.cfg.Clipboard {
		// If the client asked for a specific selection (Ctrl+V intercept),
		// echo it back so the browser can sync its local clipboard before
		// injecting the paste keystroke. Otherwise push the current
		// CLIPBOARD (the normal steady-state sync) and PRIMARY (mouse-select).
		sel := clipboard.SelClipboard
		if req.ClipReq == "primary" {
			sel = clipboard.SelPrimary
		}
		cur, _ := sess.clip.Read(sel)
		resp.Clip = cur
		resp.ClipSel = sel.String()
		if prime, err := sess.clip.Read(clipboard.SelPrimary); err == nil {
			resp.ClipPrime = prime
		}
	}
	st := &protocol.Stats{Type: "stats", FPS: float64(sess.fps), Mode: sess.mode}
	resp.Stats = st

	writeJSON(w, resp)
}

// waitFrame returns the newest frame with seq > afterSeq AND encoded at the
// current qualGen, or waits up to timeout for one. This prevents a quality
// change from being masked by a stale frame still racing in from captureLoop.
func (h *HTTPSession) waitFrame(afterSeq uint32, timeout time.Duration) ([]byte, uint32) {
	deadline := time.Now().Add(timeout)
	for {
		h.frameMu.Lock()
		wantGen := h.qualGen
		if h.latestSeq > afterSeq && h.latestFrame != nil && h.frameGen == wantGen {
			f := h.latestFrame
			s := h.latestSeq
			h.frameMu.Unlock()
			return f, s
		}
		cur := h.latestSeq
		h.frameMu.Unlock()
		if !time.Now().Before(deadline) {
			return nil, cur
		}
		// Drain any pending signal, else wait briefly.
		select {
		case <-h.frameSig:
		case <-time.After(40 * time.Millisecond):
		case <-h.done:
			return nil, cur
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	_, _ = w.Write(b)
}

func (s *Server) setCurrentHTTP(h *HTTPSession) {
	s.httpMu.Lock()
	s.httpSess = h
	s.httpMu.Unlock()
}

func (s *Server) currentHTTP() *HTTPSession {
	s.httpMu.Lock()
	defer s.httpMu.Unlock()
	if s.httpSess != nil && !s.httpSess.isClosed() {
		return s.httpSess
	}
	return nil
}

func (h *HTTPSession) isClosed() bool {
	select {
	case <-h.done:
		return true
	default:
		return false
	}
}

func (h *HTTPSession) close() {
	h.closeOnce.Do(func() {
		close(h.done)
		if h.capturer != nil {
			h.capturer.Close()
		}
		if h.injector != nil {
			h.injector.Close()
		}
		if h.clip != nil {
			h.clip.Close()
		}
	})
}

func (h *HTTPSession) helloAck() protocol.HelloAck {
	w, ht := 0, 0
	if h.capturer != nil {
		w, ht = h.capturer.Size()
	}
	return protocol.HelloAck{
		Type:     "hello_ack",
		Version:  protocol.Version,
		Width:    w,
		Height:   ht,
		Format:   protocol.FormatJPEG,
		Mode:     h.mode,
		Server:   h.capturer.Name(),
		P2P:      h.p2p,
		RelayURL: h.srv.relayURL,
	}
}

func (h *HTTPSession) sendAck() {
	b, _ := json.Marshal(h.helloAck())
	h.logf("session hello: %dx%d mode=%v p2p=%v server=%s", h.helloAck().Width, h.helloAck().Height, h.mode, h.p2p, h.capturer.Name())
	select {
	case h.writeCh <- sseMsg{event: "ack", data: b}:
	default:
	}
}

func (h *HTTPSession) handleClientMessage(msg any) {
	switch m := msg.(type) {
	case protocol.Hello:
		if h.srv.cfg.AuthToken != "" && m.Token != h.srv.cfg.AuthToken {
			h.sendError("invalid token")
			return
		}
		if m.Quality >= protocol.QualityLow && m.Quality <= protocol.QualityHigh {
			h.quality = m.Quality
			h.enc.SetQuality(m.Quality)
		}
		if m.FPS > 0 {
			h.fps = m.FPS
		}
		if m.Mode == protocol.ModeP2P {
			h.mode = protocol.ModeP2P
			h.p2p = true
		} else if m.Mode == protocol.ModeRelay {
			h.mode = protocol.ModeRelay
			h.p2p = false
		}
		h.sendAck()
	case protocol.KeyEvent:
		if err := h.injector.Key(m); err != nil {
			h.logf("key inject: %v", err)
		}
	case protocol.MouseEvent:
		if err := h.injector.Mouse(m); err != nil {
			h.logf("mouse inject: %v", err)
		}
	case protocol.TextEvent:
		if err := h.injector.Type(m.Text); err != nil {
			h.logf("text inject: %v", err)
		}
	case protocol.ClipboardEvent:
		if h.srv.cfg.Clipboard {
			sel := clipboard.SelClipboard
			if m.Selection == "primary" {
				sel = clipboard.SelPrimary
			}
			if err := h.clip.Write(m.Text, sel); err != nil {
				h.logf("clipboard write: %v", err)
			}
		}
	case protocol.ClipboardRequest:
		// SSE mode: reply with the requested selection as a clipboard event.
		// (Poll mode handles this inline in handlePoll via ClipReq.)
		if h.srv.cfg.Clipboard {
			sel := clipboard.SelClipboard
			if m.Selection == "primary" {
				sel = clipboard.SelPrimary
			}
			cur, _ := h.clip.Read(sel)
			b, _ := json.Marshal(protocol.ClipboardEvent{
				Type: "clipboard", Text: cur, Selection: sel.String(),
			})
			select {
			case h.writeCh <- sseMsg{event: "clipboard", data: b}:
			default:
			}
		}
	case protocol.QualityChange:
		if m.Quality >= protocol.QualityLow && m.Quality <= protocol.QualityHigh {
			h.quality = m.Quality
			h.enc.SetQuality(m.Quality)
			h.logf("quality -> %d", m.Quality)
			// Bump qualGen so waitFrame ignores frames encoded at the old
			// quality that may still be racing in from captureLoop.
			h.frameMu.Lock()
			h.qualGen++
			h.latestFrame = nil
			h.frameMu.Unlock()
		}
	case protocol.Ping:
		pong, _ := json.Marshal(protocol.Pong{Type: "pong", T: m.T, Ts: time.Now().UnixMilli()})
		select {
		case h.writeCh <- sseMsg{event: "pong", data: pong}:
		default:
		}
	}
}

func (h *HTTPSession) sendError(reason string) {
	b, _ := json.Marshal(protocol.ErrorMsg{Type: "error", Reason: reason})
	select {
	case h.writeCh <- sseMsg{event: "error", data: b}:
	default:
	}
}

// writer is the sole goroutine that writes to the SSE ResponseWriter.
func (h *HTTPSession) writer() {
	for m := range h.writeCh {
		var line string
		if m.event == "" {
			line = fmt.Sprintf(": %s\n\n", string(m.data))
		} else if m.event == "frame" {
			line = fmt.Sprintf("event: frame\ndata: %s\n\n", base64.StdEncoding.EncodeToString(m.data))
		} else {
			line = fmt.Sprintf("event: %s\ndata: %s\n\n", m.event, string(m.data))
		}
		if _, err := h.w.Write([]byte(line)); err != nil {
			h.close()
			// drain remaining
			for range h.writeCh {
			}
			return
		}
		h.flusher.Flush()
	}
}

func (h *HTTPSession) captureLoop() {
	var seq uint32
	var lastLoggedQ protocol.Quality = 255
	frameDur := time.Second / time.Duration(max1(h.fps))
	ticker := time.NewTicker(frameDur)
	defer ticker.Stop()
	idleCheck := time.NewTicker(5 * time.Second)
	defer idleCheck.Stop()
	for {
		select {
		case <-h.done:
			return
		case <-ticker.C:
		case <-idleCheck.C:
			// Poll-mode sessions self-close after 30s of no /poll requests so
			// a browser tab left closed does not keep capturing forever.
			if h.pollMode && time.Since(h.lastPoll) > 30*time.Second {
				h.logf("poll session idle, closing")
				h.close()
				return
			}
			continue
		}
		want := time.Second / time.Duration(max1(h.fps))
		if want != frameDur {
			frameDur = want
			ticker.Reset(frameDur)
		}
		img, err := h.capturer.Capture()
		if err != nil {
			h.logf("capture: %v", err)
			continue
		}
		// Snapshot qualGen before encoding so we can detect a quality change
		// racing in during Encode and discard the stale-quality frame.
		h.frameMu.Lock()
		genBefore := h.qualGen
		h.frameMu.Unlock()
		payload, opt, err := h.enc.Encode(img)
		if err != nil {
			h.logf("encode: %v", err)
			continue
		}
		// Skip frames whose encoded payload is identical to the previous one
		// (desktop is static). This is the single biggest win on a slow tunnel:
		// an unchanged 1280x800 desktop would otherwise re-send ~20KB every
		// tick; with this, /poll returns an empty frame and the browser just
		// re-displays what it already has.
		if bytes.Equal(payload, h.lastPayload) && genBefore == h.lastPayloadGen {
			continue
		}
		h.lastPayload = payload
		h.lastPayloadGen = genBefore
		if opt.Quality != lastLoggedQ {
			h.logf("capture: quality=%d scale=%d format=%d payload=%dB", opt.Quality, opt.Scale, opt.Format, len(payload))
			lastLoggedQ = opt.Quality
		}
		seq++
		w := img.Bounds().Dx()
		ht := img.Bounds().Dy()
		if opt.Scale > 1 {
			w /= opt.Scale
			ht /= opt.Scale
		}
		hdr := protocol.BinHeader{
			Magic:   protocol.FrameMagic,
			Format:  opt.Format,
			Seq:     seq,
			W:       uint16(w),
			H:       uint16(ht),
			Quality: opt.Quality,
			Flags:   protocol.FlagFullFrame,
			Payload: uint32(len(payload)),
		}
		buf := make([]byte, protocol.BinHeaderLen+len(payload))
		_ = hdr.Encode(byteWriter(buf[:protocol.BinHeaderLen]))
		copy(buf[protocol.BinHeaderLen:], payload)

		// Refresh the latest-frame cache for /poll clients. If quality changed
		// during Encode (qualGen bumped), discard this stale-quality frame so
		// waitFrame keeps waiting for one encoded at the new quality.
		h.frameMu.Lock()
		if h.qualGen == genBefore {
			h.latestFrame = buf
			h.latestSeq = seq
			h.frameGen = genBefore
		}
		h.frameMu.Unlock()
		select {
		case h.frameSig <- struct{}{}:
		default:
		}

		// SSE clients read from writeCh; if no SSE reader, just drop.
		select {
		case h.writeCh <- sseMsg{event: "frame", data: buf}:
		default:
		}
	}
}

func (h *HTTPSession) clipboardLoop() {
	lastClip, _ := h.clip.Read(clipboard.SelClipboard)
	lastPrime, _ := h.clip.Read(clipboard.SelPrimary)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-h.done:
			return
		case <-ticker.C:
		}
		// CLIPBOARD (Ctrl+C/V).
		if cur, err := h.clip.Read(clipboard.SelClipboard); err == nil && cur != lastClip {
			lastClip = cur
			b, _ := json.Marshal(protocol.ClipboardEvent{
				Type: "clipboard", Text: cur, Selection: "clipboard",
			})
			select {
			case h.writeCh <- sseMsg{event: "clipboard", data: b}:
			default:
			}
		}
		// PRIMARY (mouse-select).
		if cur, err := h.clip.Read(clipboard.SelPrimary); err == nil && cur != lastPrime {
			lastPrime = cur
			b, _ := json.Marshal(protocol.ClipboardEvent{
				Type: "clipboard", Text: cur, Selection: "primary",
			})
			select {
			case h.writeCh <- sseMsg{event: "clipboard", data: b}:
			default:
			}
		}
	}
}

func (h *HTTPSession) statsLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-h.done:
			return
		case <-ticker.C:
		}
		st, _ := json.Marshal(protocol.Stats{Type: "stats", FPS: float64(h.fps), Mode: h.mode})
		select {
		case h.writeCh <- sseMsg{event: "stats", data: st}:
		default:
		}
	}
}

// heartbeatLoop emits an SSE comment every 15s to keep proxies from closing
// the idle-looking long poll.
func (h *HTTPSession) heartbeatLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-h.done:
			return
		case <-ticker.C:
		}
		select {
		case h.writeCh <- sseMsg{event: "", data: []byte("keepalive")}:
		default:
		}
	}
}

func (h *HTTPSession) logf(format string, a ...any) { h.srv.logf(format, a...) }

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// keep references so unused imports don't break the build if config/relay move.
var _ = config.Config{}
var _ = relay.Picker{}
