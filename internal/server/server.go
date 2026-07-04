// Package server hosts the SamVNC WebSocket server and the embedded browser UI.
//
// It wires the capture / input / clipboard / compress / relay / tunnel pieces
// together into a live remote-desktop session. A browser connects to the public
// aitun.cc URL (or directly to the local port), opens a WebSocket at /ws, and
// exchanges the SamVNC protocol defined in internal/protocol.
package server

import (
        "log"
        "net/http"
        "sync"
        "time"

        "github.com/gorilla/websocket"
        "github.com/samaidev/samvnc/internal/capture"
        "github.com/samaidev/samvnc/internal/clipboard"
        "github.com/samaidev/samvnc/internal/compress"
        "github.com/samaidev/samvnc/internal/demo"
        "github.com/samaidev/samvnc/internal/config"
        "github.com/samaidev/samvnc/internal/input"
        "github.com/samaidev/samvnc/internal/protocol"
        "github.com/samaidev/samvnc/internal/relay"
)

var upgrader = websocket.Upgrader{
        ReadBufferSize:  4096,
        WriteBufferSize: 4096,
        CheckOrigin:     func(r *http.Request) bool { return true },
}

// Server is the top-level SamVNC server.
type Server struct {
        cfg       config.Config
        relayURL  string // public aitun.cc URL (may be empty until tunnel publishes)
        tunnelURL string
        picker    *relay.Picker
        logf      func(string, ...any)

        mu       sync.Mutex
        sessions map[*Session]struct{}

        httpMu   sync.Mutex
        httpSess *HTTPSession

        api *apiState // HTTP API state (CDP + capturer + injector)
}

// New constructs a Server.
func New(cfg config.Config, relayURL string) *Server {
        s := &Server{
                cfg:      cfg,
                relayURL: relayURL,
                picker:   relay.New(cfg.Mode),
                sessions: map[*Session]struct{}{},
                logf:     log.Printf,
        }
        if cfg.Verbose {
                s.logf = func(format string, a ...any) { log.Printf("[samvnc] "+format, a...) }
        } else {
                s.logf = func(string, ...any) {}
        }
        // Initialize API state (CDP client + X11 capturer/injector)
        s.api = newAPIState(cfg.CDPAddr)
        return s
}

// SetRelayURL updates the public URL (called when the tunnel publishes it).
func (s *Server) SetRelayURL(u string) {
        s.mu.Lock()
        s.relayURL = u
        s.mu.Unlock()
}

// Handler returns the HTTP handler serving the UI and the /ws endpoint.
// A single catch-all handler is used so the UI works behind aitun.cc's
// path-prefixed tunnel URLs (e.g. https://aitun.cc/<code>).
func (s *Server) Handler() http.Handler {
        return http.HandlerFunc(s.handleHTTP)
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
        conn, err := upgrader.Upgrade(w, r, nil)
        if err != nil {
                s.logf("ws upgrade: %v", err)
                return
        }
        sess, err := newSession(s, conn)
        if err != nil {
                s.logf("session init: %v", err)
                _ = conn.Close()
                return
        }

        s.mu.Lock()
        s.sessions[sess] = struct{}{}
        s.mu.Unlock()

        sess.run()
}

// Session is a single client remote-desktop session.
type Session struct {
        srv  *Server
        conn *websocket.Conn

        capturer capture.Capturer
        injector input.Injector
        clip     clipboard.Manager
        enc      *compress.Encoder

        quality protocol.Quality
        fps     int
        mode    protocol.ConnMode
        p2p     bool

        // single-writer discipline over the WebSocket.
        writeCh chan outMsg
        closeMu sync.Mutex
        closed  bool
        done    chan struct{}
}

type outMsg struct {
        mt   int // websocket.TextMessage / BinaryMessage
        data []byte
}

func newSession(s *Server, conn *websocket.Conn) (*Session, error) {
        var cap capture.Capturer
        var inj input.Injector
        var clip clipboard.Manager

        if s.cfg.Demo {
                // Forced synthetic backend (headless sandbox / demo).
                cap = demo.NewCapturer()
                inj = demo.NewInjector()
                clip = demo.NewManager()
                s.logf("backend: demo (forced)")
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
        return &Session{
                srv:      s,
                conn:     conn,
                capturer: cap,
                injector: inj,
                clip:     clip,
                enc:      compress.New(q),
                quality:  q,
                fps:      s.cfg.MaxFPS,
                writeCh:  make(chan outMsg, 64),
                done:     make(chan struct{}),
        }, nil
}

func (s *Session) run() {
        defer s.close()

        // Writer goroutine: sole owner of conn.Write.
        go s.writer()

        // Reader goroutine: handles client -> server messages.
        go s.reader()

        // Wait for the hello handshake before streaming.
        if !s.awaitHello(10 * time.Second) {
                s.sendError("hello timeout")
                return
        }

        // Frame capture loop.
        go s.captureLoop()

        // Clipboard sync loop.
        if s.srv.cfg.Clipboard {
                go s.clipboardLoop()
        }

        // Stats loop.
        go s.statsLoop()

        <-s.done
}

func (s *Session) awaitHello(timeout time.Duration) bool {
        deadline := time.Now().Add(timeout)
        s.conn.SetReadDeadline(deadline)
        defer s.conn.SetReadDeadline(time.Time{})

        _, data, err := s.conn.ReadMessage()
        if err != nil {
                return false
        }
        msg, err := protocol.DecodeClientMessage(data)
        if err != nil {
                return false
        }
        h, ok := msg.(protocol.Hello)
        if !ok {
                return false
        }

        // Auth check.
        if s.srv.cfg.AuthToken != "" && h.Token != s.srv.cfg.AuthToken {
                s.sendError("invalid token")
                return false
        }

        if h.Quality >= protocol.QualityLow && h.Quality <= protocol.QualityHigh {
                s.quality = h.Quality
                s.enc.SetQuality(h.Quality)
        }
        if h.FPS > 0 {
                s.fps = h.FPS
        }

        // Smart relay / P2P decision.
        peerHint := ""
        dec := s.srv.picker.Decide(peerHint)
        s.mode = dec.Mode
        s.p2p = dec.P2P
        if h.Mode == protocol.ModeP2P {
                s.mode = protocol.ModeP2P
                s.p2p = true
        } else if h.Mode == protocol.ModeRelay {
                s.mode = protocol.ModeRelay
                s.p2p = false
        }

        w, hgt := s.capturer.Size()
        ack := protocol.HelloAck{
                Type:     "hello_ack",
                Version:  protocol.Version,
                Width:    w,
                Height:   hgt,
                Format:   protocol.FormatJPEG,
                Mode:     s.mode,
                Server:   s.capturer.Name(),
                P2P:      s.p2p,
                RelayURL: s.srv.relayURL,
        }
        s.logf("session hello: %dx%d mode=%v p2p=%v server=%s", w, hgt, s.mode, s.p2p, s.capturer.Name())
        b, _ := protocol.EncodeJSON(ack)
        s.writeCh <- outMsg{mt: websocket.TextMessage, data: b}
        return true
}

func (s *Session) reader() {
        defer s.close()
        for {
                mt, data, err := s.conn.ReadMessage()
                if err != nil {
                        return
                }
                switch mt {
                case websocket.TextMessage:
                        s.handleText(data)
                case websocket.BinaryMessage:
                        // Clients do not send binary frames in this protocol.
                }
        }
}

func (s *Session) handleText(data []byte) {
        msg, err := protocol.DecodeClientMessage(data)
        if err != nil {
                s.logf("msg decode: %v", err)
                return
        }
        switch m := msg.(type) {
        case protocol.KeyEvent:
                if err := s.injector.Key(m); err != nil {
                        s.logf("key inject: %v", err)
                }
        case protocol.MouseEvent:
                if err := s.injector.Mouse(m); err != nil {
                        s.logf("mouse inject: %v", err)
                }
        case protocol.TextEvent:
                if err := s.injector.Type(m.Text); err != nil {
                        s.logf("text inject: %v", err)
                }
        case protocol.ClipboardEvent:
                if s.srv.cfg.Clipboard {
                        sel := clipboard.SelClipboard
                        if m.Selection == "primary" {
                                sel = clipboard.SelPrimary
                        }
                        if err := s.clip.Write(m.Text, sel); err != nil {
                                s.logf("clipboard write: %v", err)
                        }
                }
        case protocol.QualityChange:
                if m.Quality >= protocol.QualityLow && m.Quality <= protocol.QualityHigh {
                        s.quality = m.Quality
                        s.enc.SetQuality(m.Quality)
                        s.logf("quality -> %d", m.Quality)
                }
        case protocol.Resize:
                // Viewport scaling is handled client-side; server streams native size.
        case protocol.Ping:
                pong, _ := protocol.EncodeJSON(protocol.Pong{Type: "pong", T: m.T, Ts: time.Now().UnixMilli()})
                s.writeCh <- outMsg{mt: websocket.TextMessage, data: pong}
        }
}

func (s *Session) writer() {
        for m := range s.writeCh {
                if err := s.conn.WriteMessage(m.mt, m.data); err != nil {
                        s.close()
                        return
                }
        }
}

func (s *Session) captureLoop() {
        var seq uint32
        frameDur := time.Second / time.Duration(max(1, s.fps))
        // Adaptive: if the write channel fills, we back off.
        ticker := time.NewTicker(frameDur)
        defer ticker.Stop()
        for {
                select {
                case <-s.done:
                        return
                case <-ticker.C:
                }
                // Adapt FPS in case quality changed the cap.
                want := time.Second / time.Duration(max(1, s.fps))
                if want != frameDur {
                        frameDur = want
                        ticker.Reset(frameDur)
                }

                img, err := s.capturer.Capture()
                if err != nil {
                        s.logf("capture: %v", err)
                        continue
                }
                payload, opt, err := s.enc.Encode(img)
                if err != nil {
                        s.logf("encode: %v", err)
                        continue
                }
                seq++
                w := img.Bounds().Dx()
                h := img.Bounds().Dy()
                if opt.Scale > 1 {
                        w /= opt.Scale
                        h /= opt.Scale
                }
                hdr := protocol.BinHeader{
                        Magic:   protocol.FrameMagic,
                        Format:  opt.Format,
                        Seq:     seq,
                        W:       uint16(w),
                        H:       uint16(h),
                        Quality: opt.Quality,
                        Flags:   protocol.FlagFullFrame,
                        Payload: uint32(len(payload)),
                }
                buf := make([]byte, protocol.BinHeaderLen+len(payload))
                _ = hdr.Encode(byteWriter(buf[:protocol.BinHeaderLen]))
                copy(buf[protocol.BinHeaderLen:], payload)

                select {
                case s.writeCh <- outMsg{mt: websocket.BinaryMessage, data: buf}:
                default:
                        // Channel full => client is slow; drop this frame to keep latency low.
                }
        }
}

func (s *Session) clipboardLoop() {
        lastClip, _ := s.clip.Read(clipboard.SelClipboard)
        lastPrime, _ := s.clip.Read(clipboard.SelPrimary)
        ticker := time.NewTicker(500 * time.Millisecond)
        defer ticker.Stop()
        for {
                select {
                case <-s.done:
                        return
                case <-ticker.C:
                }
                if cur, err := s.clip.Read(clipboard.SelClipboard); err == nil && cur != lastClip {
                        lastClip = cur
                        b, _ := protocol.EncodeJSON(protocol.ClipboardEvent{Type: "clipboard", Text: cur, Selection: "clipboard"})
                        select {
                        case s.writeCh <- outMsg{mt: websocket.TextMessage, data: b}:
                        default:
                        }
                }
                if cur, err := s.clip.Read(clipboard.SelPrimary); err == nil && cur != lastPrime {
                        lastPrime = cur
                        b, _ := protocol.EncodeJSON(protocol.ClipboardEvent{Type: "clipboard", Text: cur, Selection: "primary"})
                        select {
                        case s.writeCh <- outMsg{mt: websocket.TextMessage, data: b}:
                        default:
                        }
                }
        }
}

func (s *Session) statsLoop() {
        ticker := time.NewTicker(2 * time.Second)
        defer ticker.Stop()
        for {
                select {
                case <-s.done:
                        return
                case <-ticker.C:
                }
                st, _ := protocol.EncodeJSON(protocol.Stats{
                        Type: "stats",
                        FPS:  float64(s.fps),
                        Mode: s.mode,
                })
                select {
                case s.writeCh <- outMsg{mt: websocket.TextMessage, data: st}:
                default:
                }
        }
}

func (s *Session) sendError(reason string) {
        b, _ := protocol.EncodeJSON(protocol.ErrorMsg{Type: "error", Reason: reason})
        select {
        case s.writeCh <- outMsg{mt: websocket.TextMessage, data: b}:
        default:
        }
}

func (s *Session) logf(format string, a ...any) { s.srv.logf(format, a...) }

func (s *Session) close() {
        s.closeMu.Lock()
        if s.closed {
                s.closeMu.Unlock()
                return
        }
        s.closed = true
        s.closeMu.Unlock()

        close(s.done)
        if s.capturer != nil {
                s.capturer.Close()
        }
        if s.injector != nil {
                s.injector.Close()
        }
        if s.clip != nil {
                s.clip.Close()
        }
        _ = s.conn.Close()

        s.srv.mu.Lock()
        delete(s.srv.sessions, s)
        s.srv.mu.Unlock()
}

// byteWriter adapts a []byte to io.Writer for BinHeader.Encode.
type byteWriter []byte

func (b byteWriter) Write(p []byte) (int, error) {
        return copy(b, p), nil
}

func parseQuality(s string) protocol.Quality {
        switch s {
        case "low":
                return protocol.QualityLow
        case "high":
                return protocol.QualityHigh
        default:
                return protocol.QualityMedium
        }
}

func max(a, b int) int {
        if a > b {
                return a
        }
        return b
}
