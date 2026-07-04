# samvnc

A lightweight remote-desktop + Chrome-automation gateway that exposes a
real Chrome browser (with full CDP control) over HTTP, designed to run
behind NAT-tunnel relays like [aitun.cc](https://aitun.cc).

## Why

You have a headless Chrome (or a real X11 desktop) running on a machine
behind NAT (e.g. an HPC login node, a home server, a CI worker). You want
to drive it programmatically — navigate, click, type, drag sliders, read
DOM, run JS in iframes, take screenshots — from any client, through a
single HTTPS endpoint.

Existing tools (Selenium, Playwright) require a direct TCP connection to
the WebDriver port. samvnc wraps Chrome's CDP protocol into a simpler
HTTP API and adds X11 input injection as a fallback for cases where CDP
isn't enough (e.g. Alibaba NoCaptcha slider verification needs real
mouse events, not synthetic JS events).

## Architecture

```
┌────────────────────────────────────────────────────────────┐
│ Client (curl, Python, AI agent, browser)                   │
│   ┌──────────────────────────────────────────────────┐     │
│   │ HTTP requests to /api/*                          │     │
│   └────────────────────┬─────────────────────────────┘     │
└────────────────────────│───────────────────────────────────┘
                         │ HTTPS
                         ▼
              ┌──────────────────────────┐
              │ aitun.cc relay (or LAN)  │
              └──────────┬───────────────┘
                         │
┌────────────────────────▼───────────────────────────────────┐
│ Target machine (e.g. HPC login node, no root, no Docker)   │
│   ┌────────────────────────────────────────────────────┐   │
│   │ samvnc-server  (Go binary, no deps)                │   │
│   │   • HTTP API on 127.0.0.1:8128                     │   │
│   │   • X11 capture (XGetImage)                        │   │
│   │   • X11 input  (XTEST FakeInput + xdotool)         │   │
│   │   • CDP client (WebSocket to Chrome 9222)          │   │
│   └────────┬─────────────────────────┬──────────────────┘   │
│            │                         │                     │
│            ▼                         ▼                     │
│   ┌─────────────────┐      ┌──────────────────────┐        │
│   │ Xvfb :101       │      │ google-chrome        │        │
│   │ 1280x720x24     │      │ --remote-debugging   │        │
│   └─────────────────┘      │   -port=9222         │        │
│                            │ --user-data-dir=...  │        │
│                            └──────────────────────┘        │
└────────────────────────────────────────────────────────────┘
```

## HTTP API

All endpoints accept JSON and return JSON. They work both on the local
port and behind an aitun path prefix (the server matches `/api/`
anywhere in the path).

### Status & screenshots

| Method | Path                | Body / Query            | Description                          |
|--------|---------------------|-------------------------|--------------------------------------|
| GET    | `/api/status`       |                         | `{ok, cdp, capture, injector}`       |
| GET    | `/api/screenshot`   |                         | PNG screenshot of the X11 display    |
| GET    | `/api/targets`      |                         | List all CDP targets (pages, iframes)|
| GET    | `/api/cookies`      |                         | All Chrome cookies                   |

### Navigation & JS (top-level page)

| Method | Path                | Body                    | Description                          |
|--------|---------------------|-------------------------|--------------------------------------|
| POST   | `/api/navigate`     | `{"url": "..."}`        | Navigate active tab                  |
| POST   | `/api/eval`         | `{"script": "..."}`     | Run JS, return value                 |
| POST   | `/api/dom`          | `{"selector": "..."}`   | Get outerHTML of first match         |
| POST   | `/api/scroll`       | `{"dx": 0, "dy": 300}`  | Scroll page                          |

### Iframe JS (CDP-level, for cross-origin frames like Alibaba passport)

| Method | Path                  | Body                                          |
|--------|-----------------------|-----------------------------------------------|
| POST   | `/api/iframe/eval`    | `{"iframe": "passport", "script": "..."}`     |
| POST   | `/api/iframe/dom`     | `{"iframe": "passport", "selector": "..."}`   |
| POST   | `/api/iframe/click`   | `{"iframe": "passport", "selector": "..."}`   |
| POST   | `/api/iframe/type`    | `{"iframe": "passport", "selector": "...", "text": "..."}` |

The `iframe` field is a URL substring used to match the iframe
(e.g. `"passport"` matches `https://passport.modelscope.cn/...`).

### X11 input (screen coordinates, 1280x720)

| Method | Path          | Body                                  | Description                          |
|--------|---------------|---------------------------------------|--------------------------------------|
| POST   | `/api/click`  | `{"x": 100, "y": 200}`                | Single left click                    |
| POST   | `/api/type`   | `{"text": "hello"}`                   | Type text via xdotool (Unicode OK)   |
| POST   | `/api/drag`   | `{"x1": 100, "y1": 200, "x2": 400, "y2": 200, "steps": 30}` | Drag with left button held |

### CDP input (Chrome's internal input pipeline — more reliable for anti-bot)

| Method | Path              | Body                                  | Description                          |
|--------|-------------------|---------------------------------------|--------------------------------------|
| POST   | `/api/cdp/click`  | `{"x": 100, "y": 200}`                | CDP `Input.dispatchMouseEvent` click |
| POST   | `/api/cdp/type`   | `{"text": "hello"}`                   | CDP `Input.insertText` (paste-like)  |
| POST   | `/api/cdp/drag`   | `{"x1": 100, "y1": 200, "x2": 400, "y2": 200, "steps": 25}` | CDP drag (mousePressed → mouseMoved* → mouseReleased) |

**When to use CDP vs X11 input:**

- **CDP** goes through Chrome's own input pipeline. Anti-bot JS
  (Alibaba NoCaptcha, Baxia, reCAPTCHA) sees these as "real" events
  because they go through the same code path as user input.
- **X11** (XTEST + xdotool) injects at the X server level. Also looks
  real to the browser, but requires the X server to be running and the
  Chrome window to be focused. Use this when CDP doesn't work (e.g.
  clicking on browser chrome like the URL bar).

For slider captcha (NoCaptcha), `/api/cdp/drag` is the right choice.

### Low-level protocol (`/poll`)

For batches of mouse/keyboard events (e.g. custom drag trajectories),
use `POST /poll` with a batch of protocol messages:

```json
{
  "seq": 99999,
  "inputs": [
    {"type": "mouse", "x": 100, "y": 200, "mask": 0, "wheel": 0},
    {"type": "mouse", "x": 100, "y": 200, "mask": 1, "wheel": 0},
    {"type": "mouse", "x": 200, "y": 200, "mask": 1, "wheel": 0},
    {"type": "mouse", "x": 200, "y": 200, "mask": 0, "wheel": 0},
    {"type": "text", "text": "hello"}
  ]
}
```

Use `seq: 99999` to skip frame waiting (server returns immediately after
dispatching inputs). Use `seq: 0` on the first call to also receive a
screen frame in the response.

## Build

```bash
make
# → bin/samvnc-server
# → bin/desktop-controller (optional, standalone X11 control daemon)
```

Requires Go 1.21+ and libx11-dev (for XTEST/XGetImage).

## Run

```bash
# 1. Start Chrome with CDP (inside Xvfb if headless)
Xvfb :101 -screen 0 1280x720x24 -ac &
export DISPLAY=:101
google-chrome --no-sandbox --disable-gpu --disable-dev-shm-usage \
  --remote-debugging-port=9222 --remote-debugging-address=127.0.0.1 \
  --user-data-dir=/root/chrome_profile \
  about:blank &

# 2. Start samvnc-server
./bin/samvnc-server \
  -listen 127.0.0.1:8128 \
  -tunnel=false \
  -cdp-addr 127.0.0.1:9222 \
  -verbose

# 3. (Optional) Expose via aitun
aitun -p 8128 --host 127.0.0.1 --no-p2p
# → prints public URL like https://aitun.cc/ABC123XY
```

## Examples

See [`examples/`](examples/):

- **[`examples/ms-login/`](examples/ms-login/)** — Full Python HTTP service
  that logs into modelscope.cn via phone+SMS, with manual CAPTCHA fallback.
  Demonstrates the entire API: navigate, iframe eval, iframe type, drag,
  screenshot, cookies, status.
- **[`examples/scripts/`](examples/scripts/)** — Bash one-liners for
  common tasks: drag a slider, full login flow, input-only mode, etc.

## Quick test

```bash
# Assume samvnc is at http://localhost:8128

# Status
curl http://localhost:8128/api/status

# Navigate
curl -X POST http://localhost:8128/api/navigate \
  -H 'Content-Type: application/json' \
  -d '{"url": "https://example.com"}'

# Screenshot
curl http://localhost:8128/api/screenshot -o shot.png

# Eval JS
curl -X POST http://localhost:8128/api/eval \
  -H 'Content-Type: application/json' \
  -d '{"script": "document.title"}'

# Drag a slider (CDP-level)
curl -X POST http://localhost:8128/api/cdp/drag \
  -H 'Content-Type: application/json' \
  -d '{"x1": 731, "y1": 331, "x2": 1061, "y2": 331, "steps": 25}'
```

## Files

```
samvnc/
├── cmd/
│   ├── samvnc-server/main.go    # main server binary
│   ├── desktop-controller/main.go  # standalone X11 control daemon (optional)
│   ├── fwd/main.go              # port-forwarding helper
│   └── wstest/main.go           # WebSocket test client
├── internal/
│   ├── server/
│   │   ├── server.go            # core server, WS session
│   │   ├── http.go              # SSE + /poll HTTP transport
│   │   ├── web.go               # static assets + routing
│   │   ├── api.go               # /api/* HTTP endpoints
│   │   └── assets/              # browser client (HTML/JS/CSS)
│   ├── cdp/
│   │   ├── cdp.go               # CDP client: Eval, Navigate, Cookies, Dispatch*
│   │   └── frame.go             # CDP frame tree → isolated world eval
│   ├── input/                   # X11 XTEST input injection
│   ├── capture/                 # X11 XGetImage screen capture
│   ├── clipboard/               # X11 clipboard sync
│   ├── compress/                # JPEG/PNG encoders
│   ├── protocol/                # wire protocol definitions
│   ├── config/                  # CLI flag parsing
│   ├── relay/                   # aitun relay integration
│   └── tunnel/                  # aitun client
├── examples/
│   ├── ms-login/                # Python ModelScope login service
│   └── scripts/                 # Bash automation examples
├── Makefile
├── go.mod
└── go.sum
```

## License

MIT
