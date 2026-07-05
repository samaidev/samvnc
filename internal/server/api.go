package server

import (
        "context"
        "encoding/json"
        "fmt"
        "image"
        "image/png"
        "io"
        "log"
        "net/http"
        "strings"
        "sync"
        "time"

        "github.com/samaidev/samvnc/internal/cdp"
        "github.com/samaidev/samvnc/internal/capture"
        "github.com/samaidev/samvnc/internal/input"
        "github.com/samaidev/samvnc/internal/protocol"
)

// apiState holds resources for the HTTP API endpoints.
type apiState struct {
        cdpAddr   string
        cdp       *cdp.Client
        capturer  capture.Capturer
        injector  input.Injector
        cdpMu     sync.Mutex
        cdpInited bool
}

// newAPIState creates API state with CDP client and X11 capturer/injector.
func newAPIState(cdpAddr string) *apiState {
        a := &apiState{
                cdpAddr: cdpAddr,
        }

        // Init X11 capturer for screenshots
        cap, err := capture.New()
        if err != nil {
                log.Printf("[api] capturer init failed: %v (screenshot disabled)", err)
        } else {
                a.capturer = cap
        }

        // Init X11 injector for click/type
        inj, err := input.New()
        if err != nil {
                log.Printf("[api] injector init failed: %v (click/type disabled)", err)
        } else {
                a.injector = inj
        }

        // CDP client - initialize synchronously so we see errors
        if cdpAddr != "" {
                log.Printf("[api] connecting to CDP at %s ...", cdpAddr)
                c, err := cdp.New(cdpAddr)
                if err != nil {
                        log.Printf("[api] CDP connect failed: %v", err)
                } else {
                        a.cdp = c
                        log.Printf("[api] CDP connected!")
                }
        }

        return a
}

// initCDP tries to connect to CDP. Safe to call multiple times.
func (a *apiState) initCDP() {
        a.cdpMu.Lock()
        defer a.cdpMu.Unlock()
        if a.cdp != nil || a.cdpAddr == "" {
                return
        }

        log.Printf("[api] lazy CDP connect to %s ...", a.cdpAddr)
        c, err := cdp.New(a.cdpAddr)
        if err != nil {
                log.Printf("[api] CDP connect failed: %v", err)
                return
        }
        a.cdp = c
        log.Printf("[api] CDP connected to %s", a.cdpAddr)
}

// getCDP returns the CDP client, trying to connect if not yet initialized.
func (a *apiState) getCDP() *cdp.Client {
        if a.cdp != nil {
                return a.cdp
        }
        // Try to connect (non-blocking if already trying)
        a.cdpMu.Lock()
        if a.cdp != nil {
                a.cdpMu.Unlock()
                return a.cdp
        }
        a.cdpMu.Unlock()

        // Try connect in this goroutine
        a.initCDP()
        return a.cdp
}

// handleAPI routes /api/* requests.
func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
        p := r.URL.Path
        // Strip any aitun path prefix (e.g. /ABCD123/api/click -> /api/click)
        if idx := strings.Index(p, "/api/"); idx >= 0 {
                p = p[idx:]
        }

        w.Header().Set("Content-Type", "application/json")
        w.Header().Set("Access-Control-Allow-Origin", "*")
        w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
        w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

        if r.Method == "OPTIONS" {
                w.WriteHeader(http.StatusOK)
                return
        }

        switch {
        case p == "/api/status":
                s.apiStatus(w, r)
        case p == "/api/click":
                s.apiClick(w, r)
        case p == "/api/type":
                s.apiType(w, r)
        case p == "/api/scroll":
                s.apiScroll(w, r)
        case p == "/api/screenshot":
                s.apiScreenshot(w, r)
        case p == "/api/eval":
                s.apiEval(w, r)
        case p == "/api/navigate":
                s.apiNavigate(w, r)
        case p == "/api/cookies":
                s.apiCookies(w, r)
        case p == "/api/dom":
                s.apiDOM(w, r)
        case p == "/api/targets":
                s.apiTargets(w, r)
        case p == "/api/iframe/eval":
                s.apiIframeEval(w, r)
        case p == "/api/iframe/dom":
                s.apiIframeDOM(w, r)
        case p == "/api/iframe/click":
                s.apiIframeClick(w, r)
        case p == "/api/iframe/type":
                s.apiIframeType(w, r)
        case p == "/api/drag":
                s.apiDrag(w, r)
        case p == "/api/cdp/drag":
                s.apiCdpDrag(w, r)
        case p == "/api/cdp/click":
                s.apiCdpClick(w, r)
        case p == "/api/cdp/type":
                s.apiCdpType(w, r)
        // --- Remote desktop API (full mouse/keyboard) ---
        case p == "/api/mouse/move":
                s.apiMouseMove(w, r)
        case p == "/api/mouse/down":
                s.apiMouseDown(w, r)
        case p == "/api/mouse/up":
                s.apiMouseUp(w, r)
        case p == "/api/mouse/click":
                s.apiMouseClick(w, r)
        case p == "/api/mouse/double":
                s.apiMouseDoubleClick(w, r)
        case p == "/api/mouse/right":
                s.apiMouseRightClick(w, r)
        case p == "/api/mouse/middle":
                s.apiMouseMiddleClick(w, r)
        case p == "/api/mouse/wheel":
                s.apiMouseWheel(w, r)
        case p == "/api/mouse/drag":
                s.apiMouseDrag(w, r)
        case p == "/api/mouse/position":
                s.apiMousePosition(w, r)
        case p == "/api/key/down":
                s.apiKeyDown(w, r)
        case p == "/api/key/up":
                s.apiKeyUp(w, r)
        case p == "/api/key/press":
                s.apiKeyPress(w, r)
        case p == "/api/key/type":
                s.apiKeyType(w, r)
        case p == "/api/key/combo":
                s.apiKeyCombo(w, r)
        case p == "/api/input/batch":
                s.apiInputBatch(w, r)
        default:
                http.Error(w, `{"error":"unknown api endpoint"}`, http.StatusNotFound)
        }
}

// apiStatus returns server status.
func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
        cdpClient := s.api.getCDP()
        status := map[string]interface{}{
                "ok":       true,
                "cdp":      cdpClient != nil,
                "capture":  s.api != nil && s.api.capturer != nil,
                "injector": s.api != nil && s.api.injector != nil,
        }
        json.NewEncoder(w).Encode(status)
}

// apiClick clicks at (x, y).
// POST /api/click {"x":100,"y":200}
func (s *Server) apiClick(w http.ResponseWriter, r *http.Request) {
        if s.api == nil || s.api.injector == nil {
                http.Error(w, `{"error":"injector not available"}`, http.StatusServiceUnavailable)
                return
        }
        var req struct {
                X int `json:"x"`
                Y int `json:"y"`
        }
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
                return
        }

        // Move + press + release left button
        ev := protocol.MouseEvent{
                Type: "mouse",
                X:    req.X,
                Y:    req.Y,
                Mask: 0, // move
        }
        if err := s.api.injector.Mouse(ev); err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"move: %s"}`, err), http.StatusInternalServerError)
                return
        }
        time.Sleep(20 * time.Millisecond)
        // Press left button
        ev.Mask = protocol.ButtonLeft
        if err := s.api.injector.Mouse(ev); err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"press: %s"}`, err), http.StatusInternalServerError)
                return
        }
        time.Sleep(20 * time.Millisecond)
        // Release
        ev.Mask = 0
        if err := s.api.injector.Mouse(ev); err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"release: %s"}`, err), http.StatusInternalServerError)
                return
        }
        json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "x": req.X, "y": req.Y})
}

// apiType types text (supports Unicode via xdotool).
// POST /api/type {"text":"hello"}
func (s *Server) apiType(w http.ResponseWriter, r *http.Request) {
        if s.api == nil || s.api.injector == nil {
                http.Error(w, `{"error":"injector not available"}`, http.StatusServiceUnavailable)
                return
        }
        var req struct {
                Text string `json:"text"`
        }
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
                return
        }
        if err := s.api.injector.Type(req.Text); err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
                return
        }
        json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "typed": len(req.Text)})
}

// apiScroll scrolls the page.
// POST /api/scroll {"dx":0,"dy":300}
func (s *Server) apiScroll(w http.ResponseWriter, r *http.Request) {
        if s.api == nil || s.api.getCDP() == nil {
                http.Error(w, `{"error":"cdp not available"}`, http.StatusServiceUnavailable)
                return
        }
        var req struct {
                DX int `json:"dx"`
                DY int `json:"dy"`
        }
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
                return
        }
        ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
        defer cancel()
        script := fmt.Sprintf("window.scrollBy(%d, %d); document.title", req.DX, req.DY)
        _, err := s.api.getCDP().Evaluate(ctx, script)
        if err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
                return
        }
        json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "dx": req.DX, "dy": req.DY})
}

// apiScreenshot returns a PNG screenshot.
// GET /api/screenshot
func (s *Server) apiScreenshot(w http.ResponseWriter, r *http.Request) {
        if s.api == nil || s.api.capturer == nil {
                http.Error(w, `{"error":"capturer not available"}`, http.StatusServiceUnavailable)
                return
        }
        img, err := s.api.capturer.Capture()
        if err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"capture: %s"}`, err), http.StatusInternalServerError)
                return
        }
        w.Header().Set("Content-Type", "image/png")
        // Encode as PNG
        if err := encodePNG(w, img); err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"encode: %s"}`, err), http.StatusInternalServerError)
                return
        }
}

// apiEval executes JavaScript in the page.
// POST /api/eval {"script":"document.title"}
func (s *Server) apiEval(w http.ResponseWriter, r *http.Request) {
        if s.api == nil || s.api.getCDP() == nil {
                http.Error(w, `{"error":"cdp not available"}`, http.StatusServiceUnavailable)
                return
        }
        var req struct {
                Script string `json:"script"`
        }
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
                return
        }
        ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
        defer cancel()
        result, err := s.api.getCDP().Evaluate(ctx, req.Script)
        if err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
                return
        }
        json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "result": result})
}

// apiNavigate navigates Chrome to a URL.
// POST /api/navigate {"url":"https://example.com"}
func (s *Server) apiNavigate(w http.ResponseWriter, r *http.Request) {
        if s.api == nil || s.api.getCDP() == nil {
                http.Error(w, `{"error":"cdp not available"}`, http.StatusServiceUnavailable)
                return
        }
        var req struct {
                URL string `json:"url"`
        }
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
                return
        }
        ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
        defer cancel()
        if err := s.api.getCDP().Navigate(ctx, req.URL); err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
                return
        }
        json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "url": req.URL})
}

// apiCookies returns Chrome cookies.
// GET /api/cookies
func (s *Server) apiCookies(w http.ResponseWriter, r *http.Request) {
        if s.api == nil || s.api.getCDP() == nil {
                http.Error(w, `{"error":"cdp not available"}`, http.StatusServiceUnavailable)
                return
        }
        ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
        defer cancel()
        cookies, err := s.api.getCDP().GetCookies(ctx)
        if err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
                return
        }
        json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "cookies": cookies, "count": len(cookies)})
}

// apiDOM returns the outerHTML of the first element matching selector.
// GET /api/dom?selector=.card   or   POST /api/dom {"selector":".card"}
func (s *Server) apiDOM(w http.ResponseWriter, r *http.Request) {
        if s.api == nil || s.api.getCDP() == nil {
                http.Error(w, `{"error":"cdp not available"}`, http.StatusServiceUnavailable)
                return
        }
        selector := r.URL.Query().Get("selector")
        if selector == "" && r.Method == "POST" {
                var req struct {
                        Selector string `json:"selector"`
                }
                if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
                        selector = req.Selector
                }
        }
        if selector == "" {
                selector = "body"
        }
        ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
        defer cancel()
        // Escape selector for JS string
        escaped := strings.ReplaceAll(selector, `\`, `\\`)
        escaped = strings.ReplaceAll(escaped, `'`, `\'`)
        script := fmt.Sprintf("document.querySelector('%s')?.outerHTML || null", escaped)
        result, err := s.api.getCDP().Evaluate(ctx, script)
        if err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
                return
        }
        json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "selector": selector, "html": result})
}

// encodePNG writes an image.RGBA as PNG.
func encodePNG(w io.Writer, img interface{}) error {
        rgba, ok := img.(*image.RGBA)
        if !ok {
                return fmt.Errorf("expected *image.RGBA, got %T", img)
        }
        return png.Encode(w, rgba)
}

// apiDrag performs a mouse drag (mousedown -> move -> mouseup) via X11.
// POST /api/drag {"x1":100,"y1":200,"x2":400,"y2":200,"steps":30}
// This is essential for slider captcha verification — JS event simulation
// in isolated worlds doesn't trigger anti-bot JS listeners, but real X11
// input goes through the normal browser input path.
func (s *Server) apiDrag(w http.ResponseWriter, r *http.Request) {
        if s.api == nil || s.api.injector == nil {
                http.Error(w, `{"error":"injector not available"}`, http.StatusServiceUnavailable)
                return
        }
        var req struct {
                X1    int `json:"x1"`
                Y1    int `json:"y1"`
                X2    int `json:"x2"`
                Y2    int `json:"y2"`
                Steps int `json:"steps"`
        }
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
                return
        }
        if req.Steps <= 0 {
                req.Steps = 30
        }

        log.Printf("[api] drag from (%d,%d) to (%d,%d) in %d steps", req.X1, req.Y1, req.X2, req.Y2, req.Steps)

        // 1. Move to start position
        ev := protocol.MouseEvent{Type: "mouse", X: req.X1, Y: req.Y1, Mask: 0}
        if err := s.api.injector.Mouse(ev); err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"move: %s"}`, err), http.StatusInternalServerError)
                return
        }
        time.Sleep(100 * time.Millisecond)

        // 2. Press left button
        ev.Mask = protocol.ButtonLeft
        if err := s.api.injector.Mouse(ev); err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"press: %s"}`, err), http.StatusInternalServerError)
                return
        }
        time.Sleep(100 * time.Millisecond)

        // 3. Drag in steps (add slight Y jitter for human-like movement)
        for i := 1; i <= req.Steps; i++ {
                x := req.X1 + (req.X2-req.X1)*i/req.Steps
                y := req.Y1 + (req.Y2-req.Y1)*i/req.Steps
                // Small random jitter
                jitter := (i % 3) - 1
                ev := protocol.MouseEvent{Type: "mouse", X: x, Y: y + jitter, Mask: protocol.ButtonLeft}
                if err := s.api.injector.Mouse(ev); err != nil {
                        log.Printf("[api] drag step %d error: %v", i, err)
                }
                time.Sleep(20 * time.Millisecond)
        }

        // 4. Release at end position
        ev = protocol.MouseEvent{Type: "mouse", X: req.X2, Y: req.Y2, Mask: 0}
        if err := s.api.injector.Mouse(ev); err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"release: %s"}`, err), http.StatusInternalServerError)
                return
        }

        json.NewEncoder(w).Encode(map[string]interface{}{
                "ok":    true,
                "x1":    req.X1, "y1": req.Y1,
                "x2":    req.X2, "y2": req.Y2,
                "steps": req.Steps,
        })
}

// --- iframe API handlers ---

// apiTargets lists all CDP targets (pages + iframes).
// GET /api/targets
func (s *Server) apiTargets(w http.ResponseWriter, r *http.Request) {
        cdpClient := s.api.getCDP()
        if cdpClient == nil {
                http.Error(w, `{"error":"cdp not available"}`, http.StatusServiceUnavailable)
                return
        }
        targets, err := cdpClient.ListTargets()
        if err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
                return
        }
        // Simplify output
        result := make([]map[string]string, 0, len(targets))
        for _, t := range targets {
                result = append(result, map[string]string{
                        "id":    t.ID,
                        "type":  t.Type,
                        "title": t.Title,
                        "url":   t.URL,
                })
        }
        json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "targets": result, "count": len(result)})
}

// apiIframeEval executes JS in an iframe.
// POST /api/iframe/eval {"iframe":"passport","script":"document.title"}
func (s *Server) apiIframeEval(w http.ResponseWriter, r *http.Request) {
        cdpClient := s.api.getCDP()
        if cdpClient == nil {
                http.Error(w, `{"error":"cdp not available"}`, http.StatusServiceUnavailable)
                return
        }
        var req struct {
                Iframe  string `json:"iframe"`  // URL substring to match iframe
                Script  string `json:"script"`
        }
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
                return
        }
        ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
        defer cancel()
        result, err := cdpClient.EvalInIframe(ctx, req.Iframe, req.Script)
        if err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
                return
        }
        json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "result": result})
}

// apiIframeDOM gets element HTML from an iframe.
// POST /api/iframe/dom {"iframe":"passport","selector":"input"}
func (s *Server) apiIframeDOM(w http.ResponseWriter, r *http.Request) {
        cdpClient := s.api.getCDP()
        if cdpClient == nil {
                http.Error(w, `{"error":"cdp not available"}`, http.StatusServiceUnavailable)
                return
        }
        var req struct {
                Iframe   string `json:"iframe"`
                Selector string `json:"selector"`
        }
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
                return
        }
        if req.Selector == "" {
                req.Selector = "body"
        }
        ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
        defer cancel()
        escaped := strings.ReplaceAll(req.Selector, `\`, `\\`)
        escaped = strings.ReplaceAll(escaped, `'`, `\'`)
        script := fmt.Sprintf("document.querySelector('%s')?.outerHTML || null", escaped)
        result, err := cdpClient.EvalInIframe(ctx, req.Iframe, script)
        if err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
                return
        }
        json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "selector": req.Selector, "html": result})
}

// apiIframeClick clicks an element inside an iframe by CSS selector.
// POST /api/iframe/click {"iframe":"passport","selector":"#loginBtn"}
func (s *Server) apiIframeClick(w http.ResponseWriter, r *http.Request) {
        cdpClient := s.api.getCDP()
        if cdpClient == nil {
                http.Error(w, `{"error":"cdp not available"}`, http.StatusServiceUnavailable)
                return
        }
        var req struct {
                Iframe   string `json:"iframe"`
                Selector string `json:"selector"`
        }
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
                return
        }
        ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
        defer cancel()
        escaped := strings.ReplaceAll(req.Selector, `\`, `\\`)
        escaped = strings.ReplaceAll(escaped, `'`, `\'`)
        script := fmt.Sprintf("var el=document.querySelector('%s'); if(el){el.click(); 'clicked'} else {'not found'}", escaped)
        result, err := cdpClient.EvalInIframe(ctx, req.Iframe, script)
        if err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
                return
        }
        json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "selector": req.Selector, "result": result})
}

// apiIframeType types text into a focused element inside an iframe.
// POST /api/iframe/type {"iframe":"passport","selector":"#phone","text":"13528475138"}
func (s *Server) apiIframeType(w http.ResponseWriter, r *http.Request) {
        cdpClient := s.api.getCDP()
        if cdpClient == nil {
                http.Error(w, `{"error":"cdp not available"}`, http.StatusServiceUnavailable)
                return
        }
        var req struct {
                Iframe   string `json:"iframe"`
                Selector string `json:"selector"`
                Text     string `json:"text"`
        }
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
                return
        }
        ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
        defer cancel()

        // Focus the element and set its value
        escaped := strings.ReplaceAll(req.Selector, `\`, `\\`)
        escaped = strings.ReplaceAll(escaped, `'`, `\'`)
        // Escape text for JS string
        textEscaped := strings.ReplaceAll(req.Text, `\`, `\\`)
        textEscaped = strings.ReplaceAll(textEscaped, `'`, `\'`)
        script := fmt.Sprintf(`
                var el=document.querySelector('%s');
                if(el){
                        el.focus();
                        el.value='%s';
                        el.dispatchEvent(new Event('input',{bubbles:true}));
                        el.dispatchEvent(new Event('change',{bubbles:true}));
                        'typed: %s'
                } else {'element not found'}
        `, escaped, textEscaped, textEscaped)
        result, err := cdpClient.EvalInIframe(ctx, req.Iframe, script)
        if err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
                return
        }
        json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "selector": req.Selector, "text": req.Text, "result": result})
}

// apiCdpDrag performs a drag via CDP Input.dispatchMouseEvent.
// This bypasses X11 and goes through Chrome's internal input pipeline,
// which is more reliable for interacting with anti-bot components.
// POST /api/cdp/drag {"x1":100,"y1":200,"x2":400,"y2":200,"steps":25}
func (s *Server) apiCdpDrag(w http.ResponseWriter, r *http.Request) {
        cdpClient := s.api.getCDP()
        if cdpClient == nil {
                http.Error(w, `{"error":"cdp not available"}`, http.StatusServiceUnavailable)
                return
        }
        var req struct {
                X1    float64 `json:"x1"`
                Y1    float64 `json:"y1"`
                X2    float64 `json:"x2"`
                Y2    float64 `json:"y2"`
                Steps int     `json:"steps"`
        }
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
                return
        }
        if req.Steps <= 0 {
                req.Steps = 25
        }
        ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
        defer cancel()

        log.Printf("[api] CDP drag from (%.0f,%.0f) to (%.0f,%.0f) in %d steps", req.X1, req.Y1, req.X2, req.Y2, req.Steps)

        if err := cdpClient.DispatchMouseDrag(ctx, req.X1, req.Y1, req.X2, req.Y2, req.Steps); err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
                return
        }
        json.NewEncoder(w).Encode(map[string]interface{}{
                "ok":    true,
                "x1":    req.X1, "y1": req.Y1,
                "x2":    req.X2, "y2": req.Y2,
                "steps": req.Steps,
                "method": "cdp",
        })
}

// apiCdpClick performs a click via CDP Input.dispatchMouseEvent.
// POST /api/cdp/click {"x":100,"y":200}
func (s *Server) apiCdpClick(w http.ResponseWriter, r *http.Request) {
        cdpClient := s.api.getCDP()
        if cdpClient == nil {
                http.Error(w, `{"error":"cdp not available"}`, http.StatusServiceUnavailable)
                return
        }
        var req struct {
                X float64 `json:"x"`
                Y float64 `json:"y"`
        }
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
                return
        }
        ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
        defer cancel()
        if err := cdpClient.DispatchClick(ctx, req.X, req.Y); err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
                return
        }
        json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "x": req.X, "y": req.Y, "method": "cdp"})
}

// apiCdpType inserts text via CDP Input.insertText.
// Must be called after focusing the element (e.g. via /api/cdp/click).
// POST /api/cdp/type {"text":"hello"}
func (s *Server) apiCdpType(w http.ResponseWriter, r *http.Request) {
        cdpClient := s.api.getCDP()
        if cdpClient == nil {
                http.Error(w, `{"error":"cdp not available"}`, http.StatusServiceUnavailable)
                return
        }
        var req struct {
                Text string `json:"text"`
        }
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
                return
        }
        ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
        defer cancel()
        if err := cdpClient.InsertText(ctx, req.Text); err != nil {
                http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
                return
        }
        json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "text": req.Text, "method": "cdp"})
}
