package server

// Remote-desktop HTTP API.
//
// This file extends the original /api/* surface with a comprehensive set
// of mouse + keyboard endpoints so that samvnc can be driven as a full
// remote desktop (not just a Chrome-automation gateway). All endpoints
// work both on the local port and behind an aitun path prefix (the router
// in api.go matches "/api/" anywhere in the path).
//
// Each endpoint prefers CDP (Chrome's native input pipeline) when a CDP
// connection is available, and falls back to the X11 injector (XTEST) so
// the same API also works against arbitrary X11 apps (terminals, file
// managers, browser chrome). The "method" field in the response records
// which path was used.
//
// Endpoints (see README.md for full reference):
//
//   Mouse:
//     POST /api/mouse/move      {"x":100,"y":200}
//     POST /api/mouse/down      {"x":100,"y":200,"button":"left"}
//     POST /api/mouse/up        {"x":100,"y":200,"button":"left"}
//     POST /api/mouse/click     {"x":100,"y":200,"button":"left"}
//     POST /api/mouse/double    {"x":100,"y":200,"button":"left"}
//     POST /api/mouse/right     {"x":100,"y":200}
//     POST /api/mouse/middle    {"x":100,"y":200}
//     POST /api/mouse/wheel     {"x":100,"y":200,"dx":0,"dy":-100}
//     POST /api/mouse/drag      {"x1":100,"y1":200,"x2":400,"y2":200,"steps":30,"button":"left"}
//     GET  /api/mouse/position
//
//   Keyboard:
//     POST /api/key/down        {"code":"KeyA","key":"a","modifiers":0}
//     POST /api/key/up          {"code":"KeyA","key":"a","modifiers":0}
//     POST /api/key/press       {"code":"KeyA","key":"a","modifiers":0}
//     POST /api/key/type        {"text":"hello world"}
//     POST /api/key/combo       {"keys":["Control","KeyA"]}
//
//   Batch:
//     POST /api/input/batch     {"events":[ ... ]}

import (
        "context"
        "encoding/json"
        "fmt"
        "net/http"
        "strings"
        "time"

        "github.com/samaidev/samvnc/internal/cdp"
        "github.com/samaidev/samvnc/internal/protocol"
)

// CDP modifier bitmask constants (matches Chrome DevTools Protocol).
const (
        cdpModAlt   = 1
        cdpModCtrl  = 2
        cdpModMeta  = 4
        cdpModShift = 8
)

// domToCDPModifier translates a list of modifier key names (DOM key names
// like "Control", "Shift", "Alt", "Meta") into the CDP modifier bitmask.
func domToCDPModifier(mods []string) int {
        m := 0
        for _, name := range mods {
                switch strings.ToLower(name) {
                case "alt", "altleft", "altright":
                        m |= cdpModAlt
                case "control", "ctrl", "controlleft", "controlright":
                        m |= cdpModCtrl
                case "meta", "metaleft", "metaright", "super", "win", "cmd":
                        m |= cdpModMeta
                case "shift", "shiftleft", "shiftright":
                        m |= cdpModShift
                }
        }
        return m
}

// normalizeDOMKey converts a user-supplied key name into a (DOM code, DOM key)
// pair. Accepts either a DOM code (e.g. "KeyA", "Enter", "ArrowLeft") or a
// single character (e.g. "a", "1"). For single chars the code is derived
// from the char (e.g. "a" -> ("KeyA","a"), "1" -> ("Digit1","1")).
func normalizeDOMKey(name string) (code, key string) {
        if len(name) == 1 {
                c := name[0]
                if c >= 'a' && c <= 'z' {
                        return "Key" + strings.ToUpper(name), name
                }
                if c >= 'A' && c <= 'Z' {
                        return "Key" + name, strings.ToLower(name)
                }
                if c >= '0' && c <= '9' {
                        return "Digit" + name, name
                }
                return name, name
        }
        // Already a DOM code; derive the key from it.
        switch name {
        case "Enter":
                return name, "Enter"
        case "Backspace":
                return name, "Backspace"
        case "Tab":
                return name, "Tab"
        case "Escape":
                return name, "Escape"
        case "Space":
                return name, " "
        case "ArrowLeft", "ArrowUp", "ArrowRight", "ArrowDown":
                return name, name
        case "Home", "End", "PageUp", "PageDown", "Insert", "Delete":
                return name, name
        case "F1", "F2", "F3", "F4", "F5", "F6", "F7", "F8", "F9", "F10", "F11", "F12":
                return name, name
        }
        // KeyA..KeyZ / Digit0..Digit9: derive lowercase key.
        if strings.HasPrefix(name, "Key") && len(name) == 4 {
                return name, strings.ToLower(name[3:])
        }
        if strings.HasPrefix(name, "Digit") && len(name) == 6 {
                return name, name[5:]
        }
        return name, name
}

// domKeyToVK maps common DOM key names to Windows virtual-key codes.
// Used as a hint for CDP Input.dispatchKeyEvent.
func domKeyToVK(key, code string) int {
        // Single ASCII char: use its uppercased code point.
        if len(key) == 1 {
                c := key[0]
                if c >= 'a' && c <= 'z' {
                        return int(c - 32) // VK_A..VK_Z = 0x41..
                }
                if c >= 'A' && c <= 'Z' {
                        return int(c)
                }
                if c >= '0' && c <= '9' {
                        return int(c) // VK_0..VK_9 = 0x30..0x39
                }
                switch c {
                case 0x0D, 0x20, 0x08, 0x09, 0x1B:
                        // handled below by name
                }
        }
        switch key {
        case "Enter":
                return 0x0D
        case "Backspace":
                return 0x08
        case "Tab":
                return 0x09
        case "Escape":
                return 0x1B
        case "Space", " ":
                return 0x20
        case "Shift", "ShiftLeft", "ShiftRight":
                return 0x10
        case "Control", "ControlLeft", "ControlRight":
                return 0x11
        case "Alt", "AltLeft", "AltRight":
                return 0x12
        case "Meta", "MetaLeft", "MetaRight":
                return 0x5B
        case "CapsLock":
                return 0x14
        case "ArrowLeft":
                return 0x25
        case "ArrowUp":
                return 0x26
        case "ArrowRight":
                return 0x27
        case "ArrowDown":
                return 0x28
        case "Home":
                return 0x24
        case "End":
                return 0x23
        case "PageUp":
                return 0x21
        case "PageDown":
                return 0x22
        case "Insert":
                return 0x2D
        case "Delete":
                return 0x2E
        case "F1":
                return 0x70
        case "F2":
                return 0x71
        case "F3":
                return 0x72
        case "F4":
                return 0x73
        case "F5":
                return 0x74
        case "F6":
                return 0x75
        case "F7":
                return 0x76
        case "F8":
                return 0x77
        case "F9":
                return 0x78
        case "F10":
                return 0x79
        case "F11":
                return 0x7A
        case "F12":
                return 0x7B
        }
        _ = code
        return 0
}

// buttonName normalizes a mouse button name to one of "left","right","middle".
func buttonName(b string) string {
        switch strings.ToLower(b) {
        case "right", "r":
                return "right"
        case "middle", "m", "mid":
                return "middle"
        default:
                return "left"
        }
}

// protocolButtonMask maps "left/right/middle" to the protocol.Button* mask.
func protocolButtonMask(b string) uint16 {
        switch buttonName(b) {
        case "right":
                return protocol.ButtonRight
        case "middle":
                return protocol.ButtonMiddle
        default:
                return protocol.ButtonLeft
        }
}

// --- Mouse handlers ---------------------------------------------------------

type mouseXYReq struct {
        X         float64 `json:"x"`
        Y         float64 `json:"y"`
        Button    string  `json:"button"`
        Modifiers int     `json:"modifiers"`
}

func (m *mouseXYReq) btn() string {
        if m.Button == "" {
                return "left"
        }
        return buttonName(m.Button)
}

// apiMouseMove moves the pointer to (x, y) without clicking.
// POST /api/mouse/move {"x":100,"y":200}
func (s *Server) apiMouseMove(w http.ResponseWriter, r *http.Request) {
        var req mouseXYReq
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
                return
        }
        method := s.dispatchMouseEither(w, r, "mouseMoved", req, "none", 0)
        if method == "" {
                return
        }
        json.NewEncoder(w).Encode(map[string]interface{}{
                "ok": true, "x": req.X, "y": req.Y, "method": method,
        })
}

// apiMouseDown presses a mouse button at (x, y).
// POST /api/mouse/down {"x":100,"y":200,"button":"left"}
func (s *Server) apiMouseDown(w http.ResponseWriter, r *http.Request) {
        var req mouseXYReq
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
                return
        }
        method := s.dispatchMouseEither(w, r, "mousePressed", req, req.btn(), 1)
        if method == "" {
                return
        }
        json.NewEncoder(w).Encode(map[string]interface{}{
                "ok": true, "x": req.X, "y": req.Y, "button": req.btn(), "method": method,
        })
}

// apiMouseUp releases a mouse button at (x, y).
// POST /api/mouse/up {"x":100,"y":200,"button":"left"}
func (s *Server) apiMouseUp(w http.ResponseWriter, r *http.Request) {
        var req mouseXYReq
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
                return
        }
        method := s.dispatchMouseEither(w, r, "mouseReleased", req, req.btn(), 1)
        if method == "" {
                return
        }
        json.NewEncoder(w).Encode(map[string]interface{}{
                "ok": true, "x": req.X, "y": req.Y, "button": req.btn(), "method": method,
        })
}

// apiMouseClick presses and releases a button at (x, y).
// POST /api/mouse/click {"x":100,"y":200,"button":"left"}
func (s *Server) apiMouseClick(w http.ResponseWriter, r *http.Request) {
        var req mouseXYReq
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
                return
        }
        method := s.clickEither(w, r, req, req.btn(), 1)
        if method == "" {
                return
        }
        json.NewEncoder(w).Encode(map[string]interface{}{
                "ok": true, "x": req.X, "y": req.Y, "button": req.btn(), "method": method,
        })
}

// apiMouseDoubleClick double-clicks at (x, y).
// POST /api/mouse/double {"x":100,"y":200,"button":"left"}
func (s *Server) apiMouseDoubleClick(w http.ResponseWriter, r *http.Request) {
        var req mouseXYReq
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
                return
        }
        method := s.clickEither(w, r, req, req.btn(), 2)
        if method == "" {
                return
        }
        json.NewEncoder(w).Encode(map[string]interface{}{
                "ok": true, "x": req.X, "y": req.Y, "button": req.btn(), "clicks": 2, "method": method,
        })
}

// apiMouseRightClick right-clicks at (x, y).
// POST /api/mouse/right {"x":100,"y":200}
func (s *Server) apiMouseRightClick(w http.ResponseWriter, r *http.Request) {
        var req mouseXYReq
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
                return
        }
        req.Button = "right"
        method := s.clickEither(w, r, req, "right", 1)
        if method == "" {
                return
        }
        json.NewEncoder(w).Encode(map[string]interface{}{
                "ok": true, "x": req.X, "y": req.Y, "button": "right", "method": method,
        })
}

// apiMouseMiddleClick middle-clicks at (x, y).
// POST /api/mouse/middle {"x":100,"y":200}
func (s *Server) apiMouseMiddleClick(w http.ResponseWriter, r *http.Request) {
        var req mouseXYReq
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
                return
        }
        req.Button = "middle"
        method := s.clickEither(w, r, req, "middle", 1)
        if method == "" {
                return
        }
        json.NewEncoder(w).Encode(map[string]interface{}{
                "ok": true, "x": req.X, "y": req.Y, "button": "middle", "method": method,
        })
}

// apiMouseWheel scrolls the wheel at (x, y). dx/dy are pixel deltas
// (positive = right/down). POST /api/mouse/wheel {"x":100,"y":200,"dx":0,"dy":-100}
func (s *Server) apiMouseWheel(w http.ResponseWriter, r *http.Request) {
        var req struct {
                X         float64 `json:"x"`
                Y         float64 `json:"y"`
                DX        float64 `json:"dx"`
                DY        float64 `json:"dy"`
                Modifiers int     `json:"modifiers"`
        }
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
                return
        }

        cdpClient := s.api.getCDP()
        if cdpClient != nil {
                ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
                defer cancel()
                if err := cdpClient.DispatchMouseWheel(ctx, req.X, req.Y, req.DX, req.DY, req.Modifiers); err != nil {
                        http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
                        return
                }
                json.NewEncoder(w).Encode(map[string]interface{}{
                        "ok": true, "x": req.X, "y": req.Y, "dx": req.DX, "dy": req.DY, "method": "cdp",
                })
                return
        }

        // X11 fallback: vertical wheel via protocol.MouseEvent.Wheel (in ticks).
        if s.api == nil || s.api.injector == nil {
                http.Error(w, `{"error":"neither cdp nor injector available"}`, http.StatusServiceUnavailable)
                return
        }
        // Move first.
        moveEv := protocol.MouseEvent{Type: "mouse", X: int(req.X), Y: int(req.Y), Mask: 0}
        if err := s.api.injector.Mouse(moveEv); err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"move: %s"}`, err), http.StatusInternalServerError)
                return
        }
        // Translate pixel delta to wheel ticks (~33px per tick).
        ticks := int16(req.DY / 33)
        if ticks == 0 && req.DY != 0 {
                if req.DY > 0 {
                        ticks = 1
                } else {
                        ticks = -1
                }
        }
        wheelEv := protocol.MouseEvent{Type: "mouse", X: int(req.X), Y: int(req.Y), Mask: 0, Wheel: ticks}
        if err := s.api.injector.Mouse(wheelEv); err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"wheel: %s"}`, err), http.StatusInternalServerError)
                return
        }
        json.NewEncoder(w).Encode(map[string]interface{}{
                "ok": true, "x": req.X, "y": req.Y, "dx": req.DX, "dy": req.DY, "ticks": ticks, "method": "x11",
        })
}

// apiMouseDrag performs a drag from (x1,y1) to (x2,y2).
// POST /api/mouse/drag {"x1":100,"y1":200,"x2":400,"y2":200,"steps":30,"button":"left"}
func (s *Server) apiMouseDrag(w http.ResponseWriter, r *http.Request) {
        var req struct {
                X1     float64 `json:"x1"`
                Y1     float64 `json:"y1"`
                X2     float64 `json:"x2"`
                Y2     float64 `json:"y2"`
                Steps  int     `json:"steps"`
                Button string  `json:"button"`
        }
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
                return
        }
        if req.Steps <= 0 {
                req.Steps = 30
        }
        btn := buttonName(req.Button)
        if btn == "" {
                btn = "left"
        }

        cdpClient := s.api.getCDP()
        if cdpClient != nil {
                ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
                defer cancel()
                // CDP only supports left-button drag via DispatchMouseDrag. For other
                // buttons we fall back to X11.
                if btn == "left" {
                        if err := cdpClient.DispatchMouseDrag(ctx, req.X1, req.Y1, req.X2, req.Y2, req.Steps); err != nil {
                                http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
                                return
                        }
                        json.NewEncoder(w).Encode(map[string]interface{}{
                                "ok": true, "x1": req.X1, "y1": req.Y1, "x2": req.X2, "y2": req.Y2,
                                "steps": req.Steps, "button": btn, "method": "cdp",
                        })
                        return
                }
        }

        // X11 fallback (supports any button).
        if s.api == nil || s.api.injector == nil {
                http.Error(w, `{"error":"neither cdp nor injector available"}`, http.StatusServiceUnavailable)
                return
        }
        mask := protocolButtonMask(btn)

        // 1. Move to start.
        ev := protocol.MouseEvent{Type: "mouse", X: int(req.X1), Y: int(req.Y1), Mask: 0}
        if err := s.api.injector.Mouse(ev); err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"move: %s"}`, err), http.StatusInternalServerError)
                return
        }
        time.Sleep(50 * time.Millisecond)
        // 2. Press.
        ev.Mask = mask
        if err := s.api.injector.Mouse(ev); err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"press: %s"}`, err), http.StatusInternalServerError)
                return
        }
        time.Sleep(50 * time.Millisecond)
        // 3. Drag in steps.
        for i := 1; i <= req.Steps; i++ {
                x := int(req.X1) + (int(req.X2)-int(req.X1))*i/req.Steps
                y := int(req.Y1) + (int(req.Y2)-int(req.Y1))*i/req.Steps
                jitter := (i % 3) - 1
                stepEv := protocol.MouseEvent{Type: "mouse", X: x, Y: y + jitter, Mask: mask}
                if err := s.api.injector.Mouse(stepEv); err != nil {
                        // continue on step error
                }
                time.Sleep(15 * time.Millisecond)
        }
        // 4. Release.
        ev = protocol.MouseEvent{Type: "mouse", X: int(req.X2), Y: int(req.Y2), Mask: 0}
        if err := s.api.injector.Mouse(ev); err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"release: %s"}`, err), http.StatusInternalServerError)
                return
        }
        json.NewEncoder(w).Encode(map[string]interface{}{
                "ok": true, "x1": req.X1, "y1": req.Y1, "x2": req.X2, "y2": req.Y2,
                "steps": req.Steps, "button": btn, "method": "x11",
        })
}

// apiMousePosition returns the current pointer position. CDP doesn't expose
// this directly, so when no X11 injector is available we report via JS.
// GET /api/mouse/position
func (s *Server) apiMousePosition(w http.ResponseWriter, r *http.Request) {
        // Prefer X11 (returns real cursor position).
        if s.api != nil && s.api.injector != nil {
                // The injector doesn't expose a getter; we use xdotool via the X11
                // path only when CDP isn't available. Fall through to JS-based query.
        }

        cdpClient := s.api.getCDP()
        if cdpClient == nil {
                http.Error(w, `{"error":"cdp not available (cannot query cursor position)"}`, http.StatusServiceUnavailable)
                return
        }
        ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
        defer cancel()
        // Use the page's last-known pointer coordinates if available; otherwise
        // return the viewport center. We attempt a JS snippet that reads a
        // window-level cursor tracker; if not set, returns null.
        script := `(function(){
                if (window.__samvncCursor) { return window.__samvncCursor; }
                return null;
        })()`
        result, err := cdpClient.Evaluate(ctx, script)
        if err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
                return
        }
        json.NewEncoder(w).Encode(map[string]interface{}{
                "ok":     true,
                "result": result,
                "note":   "CDP does not expose the host cursor; track via JS mousemove listeners for accurate coords",
        })
}

// --- Keyboard handlers ------------------------------------------------------

type keyReq struct {
        Code          string   `json:"code"`           // DOM code, e.g. "KeyA", "Enter"
        Key           string   `json:"key"`            // DOM key, e.g. "a", "Enter"
        Text          string   `json:"text"`           // text to insert (for /api/key/type)
        Modifiers     int      `json:"modifiers"`      // CDP bitmask
        ModifiersList []string `json:"modifiers_list"` // friendly names
}

func (k *keyReq) modMask() int {
        m := k.Modifiers
        if len(k.ModifiersList) > 0 {
                m |= domToCDPModifier(k.ModifiersList)
        }
        return m
}

// apiKeyDown presses a key (keyDown only, no release).
// POST /api/key/down {"code":"KeyA","key":"a"}
func (s *Server) apiKeyDown(w http.ResponseWriter, r *http.Request) {
        var req keyReq
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
                return
        }
        method := s.keyEither(w, r, "keyDown", req)
        if method == "" {
                return
        }
        json.NewEncoder(w).Encode(map[string]interface{}{
                "ok": true, "code": req.Code, "key": req.Key, "method": method,
        })
}

// apiKeyUp releases a key.
// POST /api/key/up {"code":"KeyA","key":"a"}
func (s *Server) apiKeyUp(w http.ResponseWriter, r *http.Request) {
        var req keyReq
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
                return
        }
        method := s.keyEither(w, r, "keyUp", req)
        if method == "" {
                return
        }
        json.NewEncoder(w).Encode(map[string]interface{}{
                "ok": true, "code": req.Code, "key": req.Key, "method": method,
        })
}

// apiKeyPress presses and releases a key (atomic).
// POST /api/key/press {"code":"Enter","key":"Enter"}
func (s *Server) apiKeyPress(w http.ResponseWriter, r *http.Request) {
        var req keyReq
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
                return
        }
        method := s.keyPressEither(w, r, req)
        if method == "" {
                return
        }
        json.NewEncoder(w).Encode(map[string]interface{}{
                "ok": true, "code": req.Code, "key": req.Key, "method": method,
        })
}

// apiKeyType types an arbitrary Unicode string (paste-like).
// POST /api/key/type {"text":"hello world 你好"}
func (s *Server) apiKeyType(w http.ResponseWriter, r *http.Request) {
        var req keyReq
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
                return
        }
        if req.Text == "" {
                http.Error(w, `{"error":"text is required"}`, http.StatusBadRequest)
                return
        }

        cdpClient := s.api.getCDP()
        if cdpClient != nil {
                ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
                defer cancel()
                if err := cdpClient.InsertText(ctx, req.Text); err != nil {
                        http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
                        return
                }
                json.NewEncoder(w).Encode(map[string]interface{}{
                        "ok": true, "text": req.Text, "method": "cdp",
                })
                return
        }

        // X11 fallback.
        if s.api == nil || s.api.injector == nil {
                http.Error(w, `{"error":"neither cdp nor injector available"}`, http.StatusServiceUnavailable)
                return
        }
        if err := s.api.injector.Type(req.Text); err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
                return
        }
        json.NewEncoder(w).Encode(map[string]interface{}{
                "ok": true, "text": req.Text, "method": "x11",
        })
}

// apiKeyCombo presses a chord of keys in sequence: hold each in order, then
// release in reverse order. e.g. {"keys":["Control","KeyA"]} -> Ctrl+A.
// POST /api/key/combo {"keys":["Control","Shift","KeyA"]}
func (s *Server) apiKeyCombo(w http.ResponseWriter, r *http.Request) {
        var req struct {
                Keys []string `json:"keys"`
        }
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
                return
        }
        if len(req.Keys) == 0 {
                http.Error(w, `{"error":"keys is required"}`, http.StatusBadRequest)
                return
        }

        method, err := s.dispatchCombo(r.Context(), req.Keys)
        if err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
                return
        }
        json.NewEncoder(w).Encode(map[string]interface{}{
                "ok": true, "keys": req.Keys, "method": method,
        })
}

// dispatchCombo is the HTTP-agnostic core of /api/key/combo. It returns the
// method used ("cdp" or "x11"). CDP path: hold each modifier (rawKeyDown),
// tap the last non-modifier key (keyDown+keyUp), release modifiers (keyUp).
// X11 path: single KeyEvent with modifier flags.
func (s *Server) dispatchCombo(ctx context.Context, names []string) (string, error) {
        // Build resolved key list. Modifier names become modifier flags;
        // everything else is treated as a DOM code or single char.
        var resolved []resolvedKey
        for _, name := range names {
                switch strings.ToLower(name) {
                case "control", "ctrl":
                        resolved = append(resolved, resolvedKey{code: "ControlLeft", key: "Control", vk: 0x11, mod: cdpModCtrl})
                case "shift":
                        resolved = append(resolved, resolvedKey{code: "ShiftLeft", key: "Shift", vk: 0x10, mod: cdpModShift})
                case "alt":
                        resolved = append(resolved, resolvedKey{code: "AltLeft", key: "Alt", vk: 0x12, mod: cdpModAlt})
                case "meta", "super", "win", "cmd":
                        resolved = append(resolved, resolvedKey{code: "MetaLeft", key: "Meta", vk: 0x5B, mod: cdpModMeta})
                default:
                        code, key := normalizeDOMKey(name)
                        resolved = append(resolved, resolvedKey{code: code, key: key, vk: domKeyToVK(key, code), mod: 0})
                }
        }

        cdpClient := s.api.getCDP()
        if cdpClient != nil {
                // Press modifiers first (rawKeyDown so no char event), then main key.
                for _, k := range resolved {
                        if k.mod == 0 {
                                continue
                        }
                        if err := cdpClient.DispatchKeyEvent(ctx, "rawKeyDown", k.code, k.key, "", k.vk, k.mod, false); err != nil {
                                return "cdp", fmt.Errorf("mod down %s: %w", k.key, err)
                        }
                }
                // Main key (last non-modifier).
                for i := len(resolved) - 1; i >= 0; i-- {
                        if resolved[i].mod != 0 {
                                continue
                        }
                        k := resolved[i]
                        if err := cdpClient.DispatchKeyPress(ctx, k.code, k.key, k.vk, combinedMod(resolved)); err != nil {
                                return "cdp", fmt.Errorf("main key %s: %w", k.key, err)
                        }
                        break
                }
                // Release modifiers in reverse.
                for i := len(resolved) - 1; i >= 0; i-- {
                        if resolved[i].mod == 0 {
                                continue
                        }
                        k := resolved[i]
                        if err := cdpClient.DispatchKeyEvent(ctx, "keyUp", k.code, k.key, "", k.vk, k.mod, false); err != nil {
                                return "cdp", fmt.Errorf("mod up %s: %w", k.key, err)
                        }
                }
                return "cdp", nil
        }

        // X11 fallback: build a single protocol.KeyEvent with modifier flags.
        if s.api == nil || s.api.injector == nil {
                return "", fmt.Errorf("neither cdp nor injector available")
        }
        var mainKey keyReq
        var mods int
        for _, name := range names {
                switch strings.ToLower(name) {
                case "control", "ctrl":
                        mods |= cdpModCtrl
                case "shift":
                        mods |= cdpModShift
                case "alt":
                        mods |= cdpModAlt
                case "meta", "super", "win", "cmd":
                        mods |= cdpModMeta
                default:
                        mainKey = keyReq{Code: name, Key: name}
                }
        }
        ev := protocol.KeyEvent{
                Type:  "key",
                Code:  mainKey.Code,
                Key:   mainKey.Key,
                Down:  true,
                Ctrl:  mods&cdpModCtrl != 0,
                Alt:   mods&cdpModAlt != 0,
                Shift: mods&cdpModShift != 0,
                Meta:  mods&cdpModMeta != 0,
        }
        if err := s.api.injector.Key(ev); err != nil {
                return "x11", err
        }
        return "x11", nil
}

// resolvedKey is a parsed key from a combo request: either a modifier
// (mod != 0) or a main key (mod == 0).
type resolvedKey struct {
        code string
        key  string
        vk   int
        mod  int
}

// combinedMod sums the modifier bits of a resolved key list.
func combinedMod(keys []resolvedKey) int {
        m := 0
        for _, k := range keys {
                m |= k.mod
        }
        return m
}

// --- Batch handler ----------------------------------------------------------

// batchEvent is a single event in an /api/input/batch request. The Type
// field selects the kind: "mouse_move", "mouse_down", "mouse_up",
// "mouse_click", "mouse_double", "mouse_wheel", "mouse_drag",
// "key_down", "key_up", "key_press", "key_type", "key_combo", "wait".
type batchEvent struct {
        Type      string   `json:"type"`
        X         float64  `json:"x"`
        Y         float64  `json:"y"`
        X1        float64  `json:"x1"`
        Y1        float64  `json:"y1"`
        X2        float64  `json:"x2"`
        Y2        float64  `json:"y2"`
        Steps     int      `json:"steps"`
        Button    string   `json:"button"`
        Clicks    int      `json:"clicks"`
        DX        float64  `json:"dx"`
        DY        float64  `json:"dy"`
        Code      string   `json:"code"`
        Key       string   `json:"key"`
        Text      string   `json:"text"`
        Keys      []string `json:"keys"`
        Modifiers int      `json:"modifiers"`
        Mods      []string `json:"mods"`
        WaitMs    int      `json:"wait_ms"`
}

// apiInputBatch dispatches a sequence of input events in order. Useful for
// complex gestures (multi-step drags, key chords, etc.) that would
// otherwise require many round-trips.
// POST /api/input/batch {"events":[...]}
func (s *Server) apiInputBatch(w http.ResponseWriter, r *http.Request) {
        var req struct {
                Events []batchEvent `json:"events"`
        }
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
                return
        }
        if len(req.Events) == 0 {
                http.Error(w, `{"error":"events is required"}`, http.StatusBadRequest)
                return
        }

        cdpClient := s.api.getCDP()
        results := make([]map[string]interface{}, 0, len(req.Events))

        for i, ev := range req.Events {
                entry := map[string]interface{}{"index": i, "type": ev.Type}
                switch ev.Type {
                case "mouse_move":
                        err := s.batchMouse(cdpClient, "mouseMoved", ev, "none", 0)
                        entry["ok"] = err == nil
                        if err != nil {
                                entry["error"] = err.Error()
                        }
                case "mouse_down":
                        btn := buttonName(ev.Button)
                        err := s.batchMouse(cdpClient, "mousePressed", ev, btn, defaultClicks(ev.Clicks, 1))
                        entry["ok"] = err == nil
                        if err != nil {
                                entry["error"] = err.Error()
                        }
                case "mouse_up":
                        btn := buttonName(ev.Button)
                        err := s.batchMouse(cdpClient, "mouseReleased", ev, btn, defaultClicks(ev.Clicks, 1))
                        entry["ok"] = err == nil
                        if err != nil {
                                entry["error"] = err.Error()
                        }
                case "mouse_click":
                        btn := buttonName(ev.Button)
                        err := s.batchClick(cdpClient, ev, btn, defaultClicks(ev.Clicks, 1))
                        entry["ok"] = err == nil
                        if err != nil {
                                entry["error"] = err.Error()
                        }
                case "mouse_double":
                        btn := buttonName(ev.Button)
                        err := s.batchClick(cdpClient, ev, btn, 2)
                        entry["ok"] = err == nil
                        if err != nil {
                                entry["error"] = err.Error()
                        }
                case "mouse_wheel":
                        err := s.batchWheel(cdpClient, ev)
                        entry["ok"] = err == nil
                        if err != nil {
                                entry["error"] = err.Error()
                        }
                case "mouse_drag":
                        btn := buttonName(ev.Button)
                        if btn == "" {
                                btn = "left"
                        }
                        err := s.batchDrag(cdpClient, ev, btn)
                        entry["ok"] = err == nil
                        if err != nil {
                                entry["error"] = err.Error()
                        }
                case "key_down":
                        err := s.batchKey(cdpClient, "keyDown", ev)
                        entry["ok"] = err == nil
                        if err != nil {
                                entry["error"] = err.Error()
                        }
                case "key_up":
                        err := s.batchKey(cdpClient, "keyUp", ev)
                        entry["ok"] = err == nil
                        if err != nil {
                                entry["error"] = err.Error()
                        }
                case "key_press":
                        err := s.batchKeyPress(cdpClient, ev)
                        entry["ok"] = err == nil
                        if err != nil {
                                entry["error"] = err.Error()
                        }
                case "key_type":
                        err := s.batchType(cdpClient, ev)
                        entry["ok"] = err == nil
                        if err != nil {
                                entry["error"] = err.Error()
                        }
                case "key_combo":
                        err := s.batchCombo(cdpClient, ev.Keys)
                        entry["ok"] = err == nil
                        if err != nil {
                                entry["error"] = err.Error()
                        }
                case "wait":
                        ms := ev.WaitMs
                        if ms <= 0 {
                                ms = 100
                        }
                        time.Sleep(time.Duration(ms) * time.Millisecond)
                        entry["ok"] = true
                default:
                        entry["ok"] = false
                        entry["error"] = "unknown event type"
                }
                results = append(results, entry)

                if ev.WaitMs > 0 && ev.Type != "wait" {
                        time.Sleep(time.Duration(ev.WaitMs) * time.Millisecond)
                }
        }

        json.NewEncoder(w).Encode(map[string]interface{}{
                "ok":     true,
                "count":  len(req.Events),
                "events": results,
        })
}

func defaultClicks(c, def int) int {
        if c <= 0 {
                return def
        }
        return c
}

// --- Shared dispatch helpers (CDP-first with X11 fallback) ------------------

// dispatchMouseEither dispatches a single mouse event via CDP if available,
// else via the X11 injector. On error it writes the HTTP error and returns "".
// On success it returns the method used ("cdp" or "x11").
func (s *Server) dispatchMouseEither(w http.ResponseWriter, r *http.Request, eventType string, req mouseXYReq, button string, clickCount int) string {
        cdpClient := s.api.getCDP()
        if cdpClient != nil {
                ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
                defer cancel()
                if err := cdpClient.DispatchMouseFull(ctx, eventType, req.X, req.Y, button, clickCount, req.Modifiers); err != nil {
                        http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
                        return ""
                }
                return "cdp"
        }

        if s.api == nil || s.api.injector == nil {
                http.Error(w, `{"error":"neither cdp nor injector available"}`, http.StatusServiceUnavailable)
                return ""
        }
        mask := uint16(0)
        switch eventType {
        case "mousePressed":
                mask = protocolButtonMask(button)
        case "mouseReleased":
                mask = 0
        case "mouseMoved":
                mask = 0
        }
        ev := protocol.MouseEvent{Type: "mouse", X: int(req.X), Y: int(req.Y), Mask: mask}
        if err := s.api.injector.Mouse(ev); err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
                return ""
        }
        return "x11"
}

// clickEither performs a press+release click. Returns "cdp" or "x11".
func (s *Server) clickEither(w http.ResponseWriter, r *http.Request, req mouseXYReq, button string, clickCount int) string {
        cdpClient := s.api.getCDP()
        if cdpClient != nil {
                ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
                defer cancel()
                // Move first.
                if err := cdpClient.DispatchMouseFull(ctx, "mouseMoved", req.X, req.Y, "none", 0, req.Modifiers); err != nil {
                        http.Error(w, fmt.Sprintf(`{"error":"move: %s"}`, err), http.StatusInternalServerError)
                        return ""
                }
                time.Sleep(20 * time.Millisecond)
                if err := cdpClient.DispatchMouseFull(ctx, "mousePressed", req.X, req.Y, button, clickCount, req.Modifiers); err != nil {
                        http.Error(w, fmt.Sprintf(`{"error":"press: %s"}`, err), http.StatusInternalServerError)
                        return ""
                }
                time.Sleep(20 * time.Millisecond)
                if err := cdpClient.DispatchMouseFull(ctx, "mouseReleased", req.X, req.Y, button, clickCount, req.Modifiers); err != nil {
                        http.Error(w, fmt.Sprintf(`{"error":"release: %s"}`, err), http.StatusInternalServerError)
                        return ""
                }
                return "cdp"
        }

        if s.api == nil || s.api.injector == nil {
                http.Error(w, `{"error":"neither cdp nor injector available"}`, http.StatusServiceUnavailable)
                return ""
        }
        mask := protocolButtonMask(button)
        // Move.
        ev := protocol.MouseEvent{Type: "mouse", X: int(req.X), Y: int(req.Y), Mask: 0}
        if err := s.api.injector.Mouse(ev); err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"move: %s"}`, err), http.StatusInternalServerError)
                return ""
        }
        time.Sleep(20 * time.Millisecond)
        // Press.
        ev.Mask = mask
        if err := s.api.injector.Mouse(ev); err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"press: %s"}`, err), http.StatusInternalServerError)
                return ""
        }
        time.Sleep(20 * time.Millisecond)
        // Release.
        ev.Mask = 0
        if err := s.api.injector.Mouse(ev); err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"release: %s"}`, err), http.StatusInternalServerError)
                return ""
        }
        return "x11"
}

// keyEither dispatches a single keyDown / keyUp. Returns "cdp" or "x11".
func (s *Server) keyEither(w http.ResponseWriter, r *http.Request, eventType string, req keyReq) string {
        mods := req.modMask()
        vk := domKeyToVK(req.Key, req.Code)

        cdpClient := s.api.getCDP()
        if cdpClient != nil {
                ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
                defer cancel()
                text := ""
                if eventType == "keyDown" && len(req.Key) == 1 && req.Key[0] >= 0x20 && req.Key[0] < 0x7f && mods == 0 {
                        text = req.Key
                }
                kind := eventType
                if eventType == "keyDown" && vk != 0 && text == "" {
                        kind = "rawKeyDown"
                }
                if err := cdpClient.DispatchKeyEvent(ctx, kind, req.Code, req.Key, text, vk, mods, false); err != nil {
                        http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
                        return ""
                }
                return "cdp"
        }

        if s.api == nil || s.api.injector == nil {
                http.Error(w, `{"error":"neither cdp nor injector available"}`, http.StatusServiceUnavailable)
                return ""
        }
        ev := protocol.KeyEvent{
                Type:  "key",
                Code:  req.Code,
                Key:   req.Key,
                Down:  eventType == "keyDown",
                Ctrl:  mods&cdpModCtrl != 0,
                Alt:   mods&cdpModAlt != 0,
                Shift: mods&cdpModShift != 0,
                Meta:  mods&cdpModMeta != 0,
        }
        if err := s.api.injector.Key(ev); err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
                return ""
        }
        return "x11"
}

// keyPressEither performs an atomic keyDown + keyUp. Returns "cdp" or "x11".
func (s *Server) keyPressEither(w http.ResponseWriter, r *http.Request, req keyReq) string {
        mods := req.modMask()
        vk := domKeyToVK(req.Key, req.Code)

        cdpClient := s.api.getCDP()
        if cdpClient != nil {
                ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
                defer cancel()
                if err := cdpClient.DispatchKeyPress(ctx, req.Code, req.Key, vk, mods); err != nil {
                        http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
                        return ""
                }
                return "cdp"
        }

        if s.api == nil || s.api.injector == nil {
                http.Error(w, `{"error":"neither cdp nor injector available"}`, http.StatusServiceUnavailable)
                return ""
        }
        ev := protocol.KeyEvent{
                Type:  "key",
                Code:  req.Code,
                Key:   req.Key,
                Down:  true,
                Ctrl:  mods&cdpModCtrl != 0,
                Alt:   mods&cdpModAlt != 0,
                Shift: mods&cdpModShift != 0,
                Meta:  mods&cdpModMeta != 0,
        }
        if err := s.api.injector.Key(ev); err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
                return ""
        }
        return "x11"
}

// --- Batch helpers (no HTTP writing; return errors) -------------------------

func (s *Server) batchMouse(c *cdp.Client, eventType string, ev batchEvent, button string, clickCount int) error {
        mods := ev.Modifiers
        if len(ev.Mods) > 0 {
                mods |= domToCDPModifier(ev.Mods)
        }
        if c != nil {
                ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
                defer cancel()
                return c.DispatchMouseFull(ctx, eventType, ev.X, ev.Y, button, clickCount, mods)
        }
        if s.api == nil || s.api.injector == nil {
                return fmt.Errorf("neither cdp nor injector available")
        }
        mask := uint16(0)
        if eventType == "mousePressed" {
                mask = protocolButtonMask(button)
        }
        mouseEv := protocol.MouseEvent{Type: "mouse", X: int(ev.X), Y: int(ev.Y), Mask: mask}
        return s.api.injector.Mouse(mouseEv)
}

func (s *Server) batchClick(c *cdp.Client, ev batchEvent, button string, clickCount int) error {
        mods := ev.Modifiers
        if len(ev.Mods) > 0 {
                mods |= domToCDPModifier(ev.Mods)
        }
        if c != nil {
                ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
                defer cancel()
                if err := c.DispatchMouseFull(ctx, "mouseMoved", ev.X, ev.Y, "none", 0, mods); err != nil {
                        return err
                }
                time.Sleep(20 * time.Millisecond)
                if err := c.DispatchMouseFull(ctx, "mousePressed", ev.X, ev.Y, button, clickCount, mods); err != nil {
                        return err
                }
                time.Sleep(20 * time.Millisecond)
                return c.DispatchMouseFull(ctx, "mouseReleased", ev.X, ev.Y, button, clickCount, mods)
        }
        if s.api == nil || s.api.injector == nil {
                return fmt.Errorf("neither cdp nor injector available")
        }
        mask := protocolButtonMask(button)
        moveEv := protocol.MouseEvent{Type: "mouse", X: int(ev.X), Y: int(ev.Y), Mask: 0}
        if err := s.api.injector.Mouse(moveEv); err != nil {
                return err
        }
        pressEv := protocol.MouseEvent{Type: "mouse", X: int(ev.X), Y: int(ev.Y), Mask: mask}
        if err := s.api.injector.Mouse(pressEv); err != nil {
                return err
        }
        releaseEv := protocol.MouseEvent{Type: "mouse", X: int(ev.X), Y: int(ev.Y), Mask: 0}
        return s.api.injector.Mouse(releaseEv)
}

func (s *Server) batchWheel(c *cdp.Client, ev batchEvent) error {
        mods := ev.Modifiers
        if len(ev.Mods) > 0 {
                mods |= domToCDPModifier(ev.Mods)
        }
        if c != nil {
                ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
                defer cancel()
                return c.DispatchMouseWheel(ctx, ev.X, ev.Y, ev.DX, ev.DY, mods)
        }
        if s.api == nil || s.api.injector == nil {
                return fmt.Errorf("neither cdp nor injector available")
        }
        moveEv := protocol.MouseEvent{Type: "mouse", X: int(ev.X), Y: int(ev.Y), Mask: 0}
        if err := s.api.injector.Mouse(moveEv); err != nil {
                return err
        }
        ticks := int16(ev.DY / 33)
        if ticks == 0 && ev.DY != 0 {
                if ev.DY > 0 {
                        ticks = 1
                } else {
                        ticks = -1
                }
        }
        wheelEv := protocol.MouseEvent{Type: "mouse", X: int(ev.X), Y: int(ev.Y), Mask: 0, Wheel: ticks}
        return s.api.injector.Mouse(wheelEv)
}

func (s *Server) batchKey(c *cdp.Client, eventType string, ev batchEvent) error {
        mods := ev.Modifiers
        if len(ev.Mods) > 0 {
                mods |= domToCDPModifier(ev.Mods)
        }
        vk := domKeyToVK(ev.Key, ev.Code)
        if c != nil {
                ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
                defer cancel()
                text := ""
                if eventType == "keyDown" && len(ev.Key) == 1 && ev.Key[0] >= 0x20 && ev.Key[0] < 0x7f && mods == 0 {
                        text = ev.Key
                }
                kind := eventType
                if eventType == "keyDown" && vk != 0 && text == "" {
                        kind = "rawKeyDown"
                }
                return c.DispatchKeyEvent(ctx, kind, ev.Code, ev.Key, text, vk, mods, false)
        }
        if s.api == nil || s.api.injector == nil {
                return fmt.Errorf("neither cdp nor injector available")
        }
        kev := protocol.KeyEvent{
                Type:  "key",
                Code:  ev.Code,
                Key:   ev.Key,
                Down:  eventType == "keyDown",
                Ctrl:  mods&cdpModCtrl != 0,
                Alt:   mods&cdpModAlt != 0,
                Shift: mods&cdpModShift != 0,
                Meta:  mods&cdpModMeta != 0,
        }
        return s.api.injector.Key(kev)
}

func (s *Server) batchKeyPress(c *cdp.Client, ev batchEvent) error {
        mods := ev.Modifiers
        if len(ev.Mods) > 0 {
                mods |= domToCDPModifier(ev.Mods)
        }
        vk := domKeyToVK(ev.Key, ev.Code)
        if c != nil {
                ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
                defer cancel()
                return c.DispatchKeyPress(ctx, ev.Code, ev.Key, vk, mods)
        }
        if s.api == nil || s.api.injector == nil {
                return fmt.Errorf("neither cdp nor injector available")
        }
        kev := protocol.KeyEvent{
                Type:  "key",
                Code:  ev.Code,
                Key:   ev.Key,
                Down:  true,
                Ctrl:  mods&cdpModCtrl != 0,
                Alt:   mods&cdpModAlt != 0,
                Shift: mods&cdpModShift != 0,
                Meta:  mods&cdpModMeta != 0,
        }
        return s.api.injector.Key(kev)
}

func (s *Server) batchType(c *cdp.Client, ev batchEvent) error {
        if ev.Text == "" {
                return nil
        }
        if c != nil {
                ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
                defer cancel()
                return c.InsertText(ctx, ev.Text)
        }
        if s.api == nil || s.api.injector == nil {
                return fmt.Errorf("neither cdp nor injector available")
        }
        return s.api.injector.Type(ev.Text)
}

// batchCombo dispatches a key chord within a batch. It reuses dispatchCombo
// but pins the CDP client to the one already resolved by the batch caller so
// we don't re-resolve per event.
func (s *Server) batchCombo(c *cdp.Client, keys []string) error {
        if len(keys) == 0 {
                return fmt.Errorf("keys is required")
        }
        ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
        defer cancel()
        // dispatchCombo uses s.api.getCDP() internally; that returns the same
        // client as `c` when CDP is connected, so the behaviour is consistent.
        _ = c
        _, err := s.dispatchCombo(ctx, keys)
        return err
}

// batchDrag performs a drag from (x1,y1) to (x2,y2) within a batch.
func (s *Server) batchDrag(c *cdp.Client, ev batchEvent, button string) error {
        steps := ev.Steps
        if steps <= 0 {
                steps = 30
        }
        if c != nil && button == "left" {
                ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
                defer cancel()
                return c.DispatchMouseDrag(ctx, ev.X1, ev.Y1, ev.X2, ev.Y2, steps)
        }
        if s.api == nil || s.api.injector == nil {
                return fmt.Errorf("neither cdp nor injector available")
        }
        mask := protocolButtonMask(button)
        moveEv := protocol.MouseEvent{Type: "mouse", X: int(ev.X1), Y: int(ev.Y1), Mask: 0}
        if err := s.api.injector.Mouse(moveEv); err != nil {
                return err
        }
        time.Sleep(50 * time.Millisecond)
        pressEv := protocol.MouseEvent{Type: "mouse", X: int(ev.X1), Y: int(ev.Y1), Mask: mask}
        if err := s.api.injector.Mouse(pressEv); err != nil {
                return err
        }
        time.Sleep(50 * time.Millisecond)
        for i := 1; i <= steps; i++ {
                x := int(ev.X1) + (int(ev.X2)-int(ev.X1))*i/steps
                y := int(ev.Y1) + (int(ev.Y2)-int(ev.Y1))*i/steps
                jitter := (i % 3) - 1
                stepEv := protocol.MouseEvent{Type: "mouse", X: x, Y: y + jitter, Mask: mask}
                _ = s.api.injector.Mouse(stepEv)
                time.Sleep(15 * time.Millisecond)
        }
        releaseEv := protocol.MouseEvent{Type: "mouse", X: int(ev.X2), Y: int(ev.Y2), Mask: 0}
        return s.api.injector.Mouse(releaseEv)
}
