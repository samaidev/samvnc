// Package cdp provides a minimal Chrome DevTools Protocol client.
//
// It connects to Chrome's --remote-debugging-port, discovers the page target,
// and issues JSON-RPC commands over WebSocket. Supports iframe targets for
// cross-origin frame interaction (e.g. aliyun passport login forms).
package cdp

import (
        "context"
        "encoding/json"
        "errors"
        "fmt"
        "log"
        "net/http"
        "strings"
        "sync"
        "sync/atomic"
        "time"

        "github.com/gorilla/websocket"
)

// Client is a CDP client connected to a Chrome instance.
type Client struct {
        httpBase string // e.g. "http://127.0.0.1:9222"
        dialer   websocket.Dialer

        mu       sync.Mutex
        conn     *websocket.Conn // main page connection
        wsURL    string          // main page WebSocket URL
        nextID   int64
        pending  map[int64]chan *cdpResponse
        closed   bool
        closeCh  chan struct{}

        // iframe target connections (keyed by URL prefix for matching)
        iframeMu  sync.Mutex
        iframes   map[string]*iframeConn // key: target ID
}

type iframeConn struct {
        conn    *websocket.Conn
        url     string
        nextID  int64
        mu      sync.Mutex
        pending map[int64]chan *cdpResponse
}

type cdpResponse struct {
        Result json.RawMessage `json:"result"`
        Error  *cdpError       `json:"error"`
}

type cdpError struct {
        Code    int    `json:"code"`
        Message string `json:"message"`
}

// targetInfo describes a CDP target (page, iframe, etc.).
type targetInfo struct {
        ID           string `json:"id"`
        Type         string `json:"type"`
        Title        string `json:"title"`
        URL          string `json:"url"`
        WebsocketURL string `json:"webSocketDebuggerUrl"`
}

// New connects to Chrome at httpAddr (e.g. "127.0.0.1:9222").
// Retries for up to 30 seconds in case Chrome is still starting.
func New(httpAddr string) (*Client, error) {
        c := &Client{
                httpBase: "http://" + httpAddr,
                dialer:   websocket.Dialer{HandshakeTimeout: 10 * time.Second},
                pending:  map[int64]chan *cdpResponse{},
                closeCh:  make(chan struct{}),
                iframes:  map[string]*iframeConn{},
        }

        var lastErr error
        for i := 0; i < 15; i++ {
                if err := c.connect(); err != nil {
                        lastErr = err
                        log.Printf("[cdp] connect attempt %d failed: %v, retrying in 2s...", i+1, err)
                        time.Sleep(2 * time.Second)
                        continue
                }
                return c, nil
        }
        return nil, fmt.Errorf("cdp: failed after 15 retries: %w", lastErr)
}

// connect discovers the page target and opens a WebSocket.
func (c *Client) connect() error {
        targets, err := c.discoverTargets()
        if err != nil {
                return fmt.Errorf("cdp: discover targets: %w", err)
        }

        var pageTarget *targetInfo
        for i := range targets {
                if targets[i].Type == "page" {
                        pageTarget = &targets[i]
                        break
                }
        }
        if pageTarget == nil {
                return errors.New("cdp: no page target found")
        }
        c.wsURL = pageTarget.WebsocketURL

        conn, _, err := c.dialer.Dial(c.wsURL, nil)
        if err != nil {
                return fmt.Errorf("cdp: dial %s: %w", c.wsURL, err)
        }
        c.conn = conn

        go c.readLoop()

        log.Printf("[cdp] connected to Chrome page target: %s (%s)", pageTarget.Title, pageTarget.URL)
        return nil
}

// discoverTargets fetches /json from Chrome.
func (c *Client) discoverTargets() ([]targetInfo, error) {
        resp, err := http.Get(c.httpBase + "/json")
        if err != nil {
                return nil, err
        }
        defer resp.Body.Close()
        var targets []targetInfo
        if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
                return nil, err
        }
        return targets, nil
}

// ListTargets returns all current CDP targets (pages + iframes).
func (c *Client) ListTargets() ([]targetInfo, error) {
        return c.discoverTargets()
}

// FindIframeTarget finds an iframe target whose URL contains the given substring.
// Uses Target.getTargets CDP command (includes OOPIF that /json doesn't list).
func (c *Client) FindIframeTarget(urlContains string) (*targetInfo, error) {
        // Use CDP Target.getTargets which includes all frames
        ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()

        result, err := c.send(ctx, "Target.getTargets", nil)
        if err != nil {
                // Fallback to HTTP /json
                targets, err2 := c.discoverTargets()
                if err2 != nil {
                        return nil, err2
                }
                for i := range targets {
                        if strings.Contains(targets[i].URL, urlContains) {
                                return &targets[i], nil
                        }
                }
                return nil, fmt.Errorf("cdp: no iframe target matching %q", urlContains)
        }

        var resp struct {
                TargetInfos []struct {
                        TargetID         string `json:"targetId"`
                        Type             string `json:"type"`
                        Title            string `json:"title"`
                        URL              string `json:"url"`
                        Attached         bool   `json:"attached"`
                        OpenerID         string `json:"openerId"`
                        CanAccessOpener  bool   `json:"canAccessOpener"`
                        OpenerFrameID    string `json:"openerFrameId"`
                        BrowserContextID string `json:"browserContextId"`
                } `json:"targetInfos"`
        }
        if err := json.Unmarshal(result, &resp); err != nil {
                return nil, fmt.Errorf("cdp: parse Target.getTargets: %w", err)
        }

        for _, t := range resp.TargetInfos {
                if strings.Contains(t.URL, urlContains) {
                        // Need to get the WebSocket URL for this target
                        // Use Target.attachToTarget to get a sessionId, then we can use it
                        // OR use /json to find the webSocketDebuggerUrl
                        wsURL, err := c.getTargetWSURL(t.TargetID)
                        if err != nil {
                                // If we can't get WS URL, try using HTTP /json
                                targets, _ := c.discoverTargets()
                                for _, ht := range targets {
                                        if strings.Contains(ht.URL, urlContains) {
                                                return &ht, nil
                                        }
                                }
                        }
                        return &targetInfo{
                                ID:           t.TargetID,
                                Type:         t.Type,
                                Title:        t.Title,
                                URL:          t.URL,
                                WebsocketURL: wsURL,
                        }, nil
                }
        }
        return nil, fmt.Errorf("cdp: no target matching %q", urlContains)
}

// getTargetWSURL gets the WebSocket URL for a target ID via /json
func (c *Client) getTargetWSURL(targetID string) (string, error) {
        targets, err := c.discoverTargets()
        if err != nil {
                return "", err
        }
        for _, t := range targets {
                if t.ID == targetID {
                        return t.WebsocketURL, nil
                }
        }
        // Target not in /json list (OOPIF) - need to use Target.attachToTarget
        // For OOPIF, we can't directly connect via WebSocket.
        // Instead, use Target.attachToTarget to get a sessionId, then
        // send commands through the main connection with that sessionId.
        return "", fmt.Errorf("target %s not in /json (likely OOPIF)", targetID)
}

// EvalInIframe evaluates JavaScript in an iframe whose URL contains urlContains.
// Uses Page.getFrameTree + Page.createIsolatedWorld for cross-origin iframe support.
func (c *Client) EvalInIframe(ctx context.Context, urlContains, expression string) (interface{}, error) {
        return c.EvalInFrame(ctx, urlContains, expression)
}

// getOrCreateIframeConn connects to an iframe target WebSocket (with caching).
func (c *Client) getOrCreateIframeConn(target *targetInfo) (*iframeConn, error) {
        c.iframeMu.Lock()
        defer c.iframeMu.Unlock()

        // Check cache
        if ifc, ok := c.iframes[target.ID]; ok {
                return ifc, nil
        }

        // Connect
        if target.WebsocketURL == "" {
                return nil, fmt.Errorf("cdp: iframe %s has no websocket URL", target.ID)
        }
        conn, _, err := c.dialer.Dial(target.WebsocketURL, nil)
        if err != nil {
                return nil, fmt.Errorf("cdp: dial iframe %s: %w", target.ID, err)
        }

        ifc := &iframeConn{
                conn:    conn,
                url:     target.URL,
                pending: map[int64]chan *cdpResponse{},
        }
        c.iframes[target.ID] = ifc

        // Start reader for this iframe
        go c.readIframeLoop(target.ID, ifc)

        log.Printf("[cdp] connected to iframe target: %s (%s)", target.ID, target.URL[:min(len(target.URL), 60)])
        return ifc, nil
}

func min(a, b int) int {
        if a < b {
                return a
        }
        return b
}

// readLoop reads CDP responses from the main page connection.
func (c *Client) readLoop() {
        for {
                select {
                case <-c.closeCh:
                        return
                default:
                }
                _, data, err := c.conn.ReadMessage()
                if err != nil {
                        if !c.closed {
                                log.Printf("[cdp] read error: %v, reconnecting...", err)
                                c.reconnect()
                        }
                        return
                }

                var msg struct {
                        ID     int64           `json:"id"`
                        Result json.RawMessage `json:"result"`
                        Error  *cdpError       `json:"error"`
                        Method string          `json:"method"`
                }
                if err := json.Unmarshal(data, &msg); err != nil {
                        continue
                }

                if msg.ID != 0 {
                        c.mu.Lock()
                        ch, ok := c.pending[msg.ID]
                        if ok {
                                delete(c.pending, msg.ID)
                        }
                        c.mu.Unlock()
                        if ok {
                                ch <- &cdpResponse{Result: msg.Result, Error: msg.Error}
                        }
                }
        }
}

// readIframeLoop reads CDP responses from an iframe connection.
func (c *Client) readIframeLoop(targetID string, ifc *iframeConn) {
        for {
                _, data, err := ifc.conn.ReadMessage()
                if err != nil {
                        log.Printf("[cdp] iframe %s read error: %v", targetID, err)
                        c.iframeMu.Lock()
                        delete(c.iframes, targetID)
                        c.iframeMu.Unlock()
                        return
                }

                var msg struct {
                        ID     int64           `json:"id"`
                        Result json.RawMessage `json:"result"`
                        Error  *cdpError       `json:"error"`
                }
                if err := json.Unmarshal(data, &msg); err != nil {
                        continue
                }

                if msg.ID != 0 {
                        ifc.mu.Lock()
                        ch, ok := ifc.pending[msg.ID]
                        if ok {
                                delete(ifc.pending, msg.ID)
                        }
                        ifc.mu.Unlock()
                        if ok {
                                ch <- &cdpResponse{Result: msg.Result, Error: msg.Error}
                        }
                }
        }
}

// reconnect attempts to re-establish the CDP connection.
func (c *Client) reconnect() {
        c.mu.Lock()
        if c.conn != nil {
                c.conn.Close()
        }
        c.mu.Unlock()

        for i := 0; i < 5; i++ {
                time.Sleep(2 * time.Second)
                log.Printf("[cdp] reconnect attempt %d...", i+1)
                if err := c.connect(); err == nil {
                        return
                }
        }
        log.Printf("[cdp] failed to reconnect after 5 attempts")
}

// sendWithSession sends a CDP command with a sessionId (for OOPIF targets).
func (c *Client) sendWithSession(ctx context.Context, method string, params interface{}, sessionID string) (json.RawMessage, error) {
        id := atomic.AddInt64(&c.nextID, 1)

        cmd := map[string]interface{}{
                "id":        id,
                "method":    method,
                "sessionId": sessionID,
        }
        if params != nil {
                cmd["params"] = params
        }

        data, err := json.Marshal(cmd)
        if err != nil {
                return nil, err
        }

        ch := make(chan *cdpResponse, 1)
        c.mu.Lock()
        if c.conn == nil {
                c.mu.Unlock()
                return nil, errors.New("cdp: not connected")
        }
        c.pending[id] = ch
        if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
                delete(c.pending, id)
                c.mu.Unlock()
                return nil, fmt.Errorf("cdp: write: %w", err)
        }
        c.mu.Unlock()

        select {
        case resp := <-ch:
                if resp.Error != nil {
                        return nil, fmt.Errorf("cdp: %s: %s (code %d)", method, resp.Error.Message, resp.Error.Code)
                }
                return resp.Result, nil
        case <-ctx.Done():
                c.mu.Lock()
                delete(c.pending, id)
                c.mu.Unlock()
                return nil, ctx.Err()
        case <-time.After(30 * time.Second):
                c.mu.Lock()
                delete(c.pending, id)
                c.mu.Unlock()
                return nil, errors.New("cdp: timeout waiting for response")
        }
}

// send sends a CDP command on the main page connection.
func (c *Client) send(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
        id := atomic.AddInt64(&c.nextID, 1)

        cmd := map[string]interface{}{
                "id":     id,
                "method": method,
        }
        if params != nil {
                cmd["params"] = params
        }

        data, err := json.Marshal(cmd)
        if err != nil {
                return nil, err
        }

        ch := make(chan *cdpResponse, 1)
        c.mu.Lock()
        if c.conn == nil {
                c.mu.Unlock()
                return nil, errors.New("cdp: not connected")
        }
        c.pending[id] = ch
        if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
                delete(c.pending, id)
                c.mu.Unlock()
                return nil, fmt.Errorf("cdp: write: %w", err)
        }
        c.mu.Unlock()

        select {
        case resp := <-ch:
                if resp.Error != nil {
                        return nil, fmt.Errorf("cdp: %s: %s (code %d)", method, resp.Error.Message, resp.Error.Code)
                }
                return resp.Result, nil
        case <-ctx.Done():
                c.mu.Lock()
                delete(c.pending, id)
                c.mu.Unlock()
                return nil, ctx.Err()
        case <-time.After(30 * time.Second):
                c.mu.Lock()
                delete(c.pending, id)
                c.mu.Unlock()
                return nil, errors.New("cdp: timeout waiting for response")
        }
}

// Evaluate executes a JavaScript expression on the main page and returns the result.
func (c *Client) Evaluate(ctx context.Context, expression string) (interface{}, error) {
        params := map[string]interface{}{
                "expression":    expression,
                "returnByValue": true,
                "awaitPromise":  true,
        }
        result, err := c.send(ctx, "Runtime.evaluate", params)
        if err != nil {
                return nil, err
        }

        var evalResult struct {
                Result struct {
                        Type  string          `json:"type"`
                        Value json.RawMessage `json:"value"`
                } `json:"result"`
                ExceptionDetails *struct {
                        Text string `json:"text"`
                } `json:"exceptionDetails"`
        }
        if err := json.Unmarshal(result, &evalResult); err != nil {
                return nil, fmt.Errorf("cdp: parse evaluate result: %w", err)
        }
        if evalResult.ExceptionDetails != nil {
                return nil, fmt.Errorf("cdp: JS exception: %s", evalResult.ExceptionDetails.Text)
        }

        var value interface{}
        if err := json.Unmarshal(evalResult.Result.Value, &value); err != nil {
                return string(evalResult.Result.Value), nil
        }
        return value, nil
}

// Navigate navigates the page to a URL.
func (c *Client) Navigate(ctx context.Context, url string) error {
        params := map[string]interface{}{
                "url": url,
        }
        _, err := c.send(ctx, "Page.navigate", params)
        return err
}

// GetCookies returns all cookies.
func (c *Client) GetCookies(ctx context.Context) ([]map[string]interface{}, error) {
        result, err := c.send(ctx, "Network.getCookies", nil)
        if err != nil {
                return nil, err
        }

        var cookiesResp struct {
                Cookies []map[string]interface{} `json:"cookies"`
        }
        if err := json.Unmarshal(result, &cookiesResp); err != nil {
                return nil, fmt.Errorf("cdp: parse cookies: %w", err)
        }
        return cookiesResp.Cookies, nil
}

// DispatchMouse dispatches a mouse event via CDP Input domain.
// This bypasses X11 and goes directly through Chrome's input pipeline.
// type: "mousePressed", "mouseMoved", or "mouseReleased"
// x, y: coordinates relative to the viewport
// button: "left", "right", "middle", "none"
func (c *Client) DispatchMouse(ctx context.Context, eventType string, x, y float64, button string) error {
        params := map[string]interface{}{
                "type":   eventType,
                "x":      x,
                "y":      y,
                "button": button,
                "clickCount": 1,
        }
        _, err := c.send(ctx, "Input.dispatchMouseEvent", params)
        return err
}

// DispatchMouseDrag performs a drag operation via CDP Input domain.
// mousedown at (x1,y1) -> mousemove in steps -> mouseup at (x2,y2)
func (c *Client) DispatchMouseDrag(ctx context.Context, x1, y1, x2, y2 float64, steps int) error {
        if steps <= 0 {
                steps = 25
        }

        // 1. Move to start
        if err := c.DispatchMouse(ctx, "mouseMoved", x1, y1, "none"); err != nil {
                return fmt.Errorf("move: %w", err)
        }
        time.Sleep(50 * time.Millisecond)

        // 2. Press
        if err := c.DispatchMouse(ctx, "mousePressed", x1, y1, "left"); err != nil {
                return fmt.Errorf("press: %w", err)
        }
        time.Sleep(50 * time.Millisecond)

        // 3. Drag in steps
        for i := 1; i <= steps; i++ {
                x := x1 + (x2-x1)*float64(i)/float64(steps)
                y := y1 + (y2-y1)*float64(i)/float64(steps)
                // Add tiny jitter
                jitter := float64(i%3-1) * 0.5
                if err := c.DispatchMouse(ctx, "mouseMoved", x, y+jitter, "left"); err != nil {
                        log.Printf("[cdp] drag step %d error: %v", i, err)
                }
                time.Sleep(20 * time.Millisecond)
        }

        // 4. Release
        if err := c.DispatchMouse(ctx, "mouseReleased", x2, y2, "left"); err != nil {
                return fmt.Errorf("release: %w", err)
        }
        return nil
}

// DispatchClick performs a click via CDP Input domain.
// mousePressed then mouseReleased at (x, y).
func (c *Client) DispatchClick(ctx context.Context, x, y float64) error {
        if err := c.DispatchMouse(ctx, "mouseMoved", x, y, "none"); err != nil {
                return fmt.Errorf("move: %w", err)
        }
        time.Sleep(50 * time.Millisecond)
        if err := c.DispatchMouse(ctx, "mousePressed", x, y, "left"); err != nil {
                return fmt.Errorf("press: %w", err)
        }
        time.Sleep(50 * time.Millisecond)
        if err := c.DispatchMouse(ctx, "mouseReleased", x, y, "left"); err != nil {
                return fmt.Errorf("release: %w", err)
        }
        return nil
}

// InsertText inserts text via CDP Input.insertText.
// This is the simplest way to type text - works like paste.
// Must be called after focusing the target element (e.g. via DispatchClick).
func (c *Client) InsertText(ctx context.Context, text string) error {
        params := map[string]interface{}{
                "text": text,
        }
        _, err := c.send(ctx, "Input.insertText", params)
        return err
}

// Close closes the CDP connection.
func (c *Client) Close() error {
        c.closed = true
        close(c.closeCh)
        c.mu.Lock()
        defer c.mu.Unlock()
        if c.conn != nil {
                return c.conn.Close()
        }
        return nil
}

// send sends a CDP command on an iframe connection.
func (ifc *iframeConn) send(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
        id := atomic.AddInt64(&ifc.nextID, 1)

        cmd := map[string]interface{}{
                "id":     id,
                "method": method,
        }
        if params != nil {
                cmd["params"] = params
        }

        data, err := json.Marshal(cmd)
        if err != nil {
                return nil, err
        }

        ch := make(chan *cdpResponse, 1)
        ifc.mu.Lock()
        ifc.pending[id] = ch
        if err := ifc.conn.WriteMessage(websocket.TextMessage, data); err != nil {
                delete(ifc.pending, id)
                ifc.mu.Unlock()
                return nil, fmt.Errorf("cdp iframe: write: %w", err)
        }
        ifc.mu.Unlock()

        select {
        case resp := <-ch:
                if resp.Error != nil {
                        return nil, fmt.Errorf("cdp iframe: %s: %s", method, resp.Error.Message)
                }
                return resp.Result, nil
        case <-ctx.Done():
                ifc.mu.Lock()
                delete(ifc.pending, id)
                ifc.mu.Unlock()
                return nil, ctx.Err()
        case <-time.After(30 * time.Second):
                ifc.mu.Lock()
                delete(ifc.pending, id)
                ifc.mu.Unlock()
                return nil, errors.New("cdp iframe: timeout")
        }
}
