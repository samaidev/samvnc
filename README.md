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

**Auth:** except for `/api/status`, every call requires the access
token (the same one used by the browser session): append
`?token=<token>` or send an `X-SamVNC-Token` header. The token is
auto-generated on first run (`~/.samvnc/settings.json` for the tray,
`~/.samvnc/auth_token` for standalone server runs) and can be
rotated any time via the Regenerate button in the tray settings.

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

### Remote desktop API (full mouse + keyboard)

A complete set of `/api/mouse/*` and `/api/key/*` endpoints for driving the
remote desktop. Each endpoint **prefers CDP** (Chrome's input pipeline) when
a CDP connection is available and **falls back to X11 XTEST** otherwise, so
the same API works against both Chrome pages and arbitrary X11 apps
(terminals, file managers, browser chrome). The response's `method` field
records which path was used (`"cdp"` or `"x11"`).

#### Mouse

| Method | Path                | Body                                          | Description                          |
|--------|---------------------|-----------------------------------------------|--------------------------------------|
| POST   | `/api/mouse/move`   | `{"x":100,"y":200}`                           | Move pointer (no click)              |
| POST   | `/api/mouse/down`   | `{"x":100,"y":200,"button":"left"}`          | Press button (`left`/`right`/`middle`) |
| POST   | `/api/mouse/up`     | `{"x":100,"y":200,"button":"left"}`          | Release button                       |
| POST   | `/api/mouse/click`  | `{"x":100,"y":200,"button":"left"}`          | Single click                         |
| POST   | `/api/mouse/double` | `{"x":100,"y":200,"button":"left"}`          | Double click                         |
| POST   | `/api/mouse/right`  | `{"x":100,"y":200}`                           | Right click                          |
| POST   | `/api/mouse/middle` | `{"x":100,"y":200}`                           | Middle click                         |
| POST   | `/api/mouse/wheel`  | `{"x":100,"y":200,"dx":0,"dy":-100}`         | Scroll wheel (pixel deltas)          |
| POST   | `/api/mouse/drag`   | `{"x1":100,"y1":200,"x2":400,"y2":200,"steps":30,"button":"left"}` | Drag (any button) |
| GET    | `/api/mouse/position` |                                             | Query last-known cursor position     |

The optional `modifiers` field (CDP bitmask: `1`=Alt, `2`=Ctrl, `4`=Meta,
`8`=Shift) can be added to any mouse event to inject a chorded click
(e.g. Shift+click).

#### Keyboard

| Method | Path             | Body                                              | Description                          |
|--------|------------------|---------------------------------------------------|--------------------------------------|
| POST   | `/api/key/down`  | `{"code":"KeyA","key":"a"}`                       | Key down (no release)                |
| POST   | `/api/key/up`    | `{"code":"KeyA","key":"a"}`                       | Key up                               |
| POST   | `/api/key/press` | `{"code":"Enter","key":"Enter"}`                  | Atomic key down + up                 |
| POST   | `/api/key/type`  | `{"text":"hello 你好"}`                           | Type Unicode text (paste-like)       |
| POST   | `/api/key/combo` | `{"keys":["Control","Shift","KeyA"]}`             | Key chord (hold all, tap last)       |

`code` is the DOM `KeyboardEvent.code` (e.g. `KeyA`, `Enter`, `ArrowLeft`,
`F5`), `key` is the DOM `KeyboardEvent.key` (e.g. `a`, `Enter`, `ArrowLeft`).
Optional `modifiers` (CDP bitmask) or `modifiers_list` (e.g.
`["Control","Shift"]`) inject modifier-chorded keys. For full chords use
`/api/key/combo` which holds each modifier, taps the final non-modifier key,
and releases the modifiers in reverse order.

```bash
# Right-click
curl -X POST http://localhost:8128/api/mouse/right \
  -H 'Content-Type: application/json' -d '{"x":400,"y":300}'

# Type Unicode text
curl -X POST http://localhost:8128/api/key/type \
  -H 'Content-Type: application/json' -d '{"text":"你好 world"}'

# Ctrl+Shift+T (reopen closed tab)
curl -X POST http://localhost:8128/api/key/combo \
  -H 'Content-Type: application/json' -d '{"keys":["Control","Shift","KeyT"]}'

# Scroll down 200px
curl -X POST http://localhost:8128/api/mouse/wheel \
  -H 'Content-Type: application/json' -d '{"x":640,"y":360,"dx":0,"dy":200}'
```

#### Batch input (`/api/input/batch`)

Dispatch a sequence of input events in a single HTTP request — useful for
complex gestures (multi-step drags, key sequences with timing) that would
otherwise require many round-trips. Each event has a `type` and the fields
relevant to that type; an optional `wait_ms` pauses before the next event.

```bash
curl -X POST http://localhost:8128/api/input/batch \
  -H 'Content-Type: application/json' -d '{
    "events": [
      {"type":"mouse_move","x":100,"y":200},
      {"type":"mouse_down","button":"left"},
      {"type":"wait","wait_ms":50},
      {"type":"mouse_move","x":400,"y":200},
      {"type":"mouse_up","button":"left"},
      {"type":"key_press","code":"Enter","key":"Enter"},
      {"type":"key_type","text":"hello"}
    ]
  }'
```

Supported event types: `mouse_move`, `mouse_down`, `mouse_up`,
`mouse_click`, `mouse_wheel`, `key_down`, `key_up`, `key_press`,
`key_type`, `wait`. The response includes per-event success/failure so
partial failures can be diagnosed.

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
make            # → bin/samvnc-server
make tray       # → bin/samvnc-tray (system tray app)
make all-build  # → both server and tray
```

Requires Go 1.21+. On Linux, libx11-dev is needed (for XTEST/XGetImage).
The tray app uses the native Win32 API on Windows (no CGO required).

## System Tray (samvnc-tray)

The tray app (`cmd/samvnc-tray`) provides a system tray icon that manages
the `samvnc-server` subprocess. It auto-starts the server on launch and
provides a context menu with:

- **Start / Stop** — mutually exclusive; starts or stops the server subprocess
- **Auto-start on boot** — toggle (mutually exclusive); registers the tray
  binary for boot-time startup via Windows Registry (Win), LaunchAgent (macOS),
  or `.desktop` autostart file (Linux)
- **Settings...** — native settings dialog (graphical on all three platforms)
  where you can set:
  - `agent_token` — your aitun.cc agent token (`aitagt_...`) for a permanent
    subdomain. If empty, the built-in default is used (ephemeral subdomain).
  - `subdomain` — request a specific subdomain name (e.g. `my-pc`). If empty,
    one is auto-generated.
  - `auth_token` — the client auth token. Auto-generated on first run.
  - `auto_start` — whether to start on boot.
  - `language` — UI language (Auto / English / 中文), applied immediately.
- **Copy Address** — copies the public URL with `?token=xxx` to the clipboard,
  ready to paste into any browser
- **Screenshot** — one-click tray screenshot: grabs `/api/screenshot` from the
  running server, saves `~/Pictures/SamVNC/samvnc-YYYYMMDD-HHMMSS.png` and
  opens it with the platform image viewer. No curl, no terminal.
- **Exit** — stops the server and quits

The tray icon is the SAME artwork on every platform (blue rounded square +
white "V", from `assets/tray_icon.png`): Windows loads `tray_icon.ico`
(embedded fallback), macOS renders the embedded PNG as an NSStatusItem
image, and Linux exports multi-size pixmaps over the SNI D-Bus protocol —
native trays on Windows, macOS and Linux.

## Remote Desktop in 30 Seconds

Three steps after install — browser-accessible remote desktop, screenshots
right from the tray.

**1. Launch the tray**

Run `samvnc-tray` (on Windows, double-click the tray exe from the zip).
The SamVNC icon appears in the system tray / menu bar and the server
starts automatically:

```
samvnc-tray
```

Empty key = free proxy-address mode (`https://aitun.cc/<code>`, ~24h);
with an aitun key (Settings dialog) it becomes a permanent subdomain.

**2. Copy the address, open in a browser**

Tray menu → **Copy Address**, paste into any browser — the desktop is
right there.

**3. Screenshot from the tray**

Tray menu → **Screenshot**: the current desktop is saved as a timestamped
PNG under `~/Pictures/SamVNC/` and opened automatically. Programmatic
clients can still use the same endpoint:

```bash
curl https://samvnc-myhost-abc123.t.aitun.cc/api/screenshot -o screen.png
```

## Settings file

Located at `~/.samvnc/settings.json`:

```json
{
  "agent_token": "",
  "subdomain": "",
  "auth_token": "e22542fbfdcffd7d92b6e93cd85a7718",
  "auto_start": false
}
```

When `agent_token` is empty, the built-in default aitun.cc agent token is
used, which gives you an ephemeral subdomain on `t.aitun.cc`. Set your own
agent token to bind a permanent subdomain to your account.

The public URL with token looks like:
`https://samvnc-wei-abc123.t.aitun.cc?token=e22542fbfdcffd7d92b6e93cd85a7718`

Open it in any browser to access the remote desktop.

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
│   ├── samvnc-tray/main.go      # system tray app (manages server subprocess)
│   ├── desktop-controller/main.go  # standalone X11 control daemon (optional)
│   ├── fwd/main.go              # port-forwarding helper
│   └── wstest/main.go           # WebSocket test client
├── internal/
│   ├── server/
│   │   ├── server.go            # core server, WS session
│   │   ├── http.go              # SSE + /poll HTTP transport
│   │   ├── web.go               # static assets + routing
│   │   ├── api.go               # /api/* HTTP endpoints
│   │   ├── api_desktop.go       # /api/mouse/* + /api/key/* remote-desktop API
│   │   └── assets/              # browser client (HTML/JS/CSS)
│   ├── tray/                    # cross-platform system tray (Win32 Shell_NotifyIcon)
│   ├── settings/                # tray settings persistence (~/.samvnc/settings.json)
│   ├── autostart/               # boot-time auto-start (Win Registry / macOS LaunchAgent / Linux .desktop)
│   ├── cdp/
│   │   ├── cdp.go               # CDP client: Eval, Navigate, Cookies, Dispatch* (mouse/key/wheel)
│   │   └── frame.go             # CDP frame tree → isolated world eval
│   ├── input/                   # X11 XTEST / Windows SendInput injection
│   ├── capture/                 # X11 XGetImage / Windows GDI BitBlt capture
│   ├── clipboard/               # X11 / Windows clipboard sync
│   ├── compress/                # JPEG/PNG encoders
│   ├── protocol/                # wire protocol definitions
│   ├── config/                  # CLI flag parsing
│   ├── aitunmgr/                # aitun.cc management API client
│   ├── substore/                # subdomain state persistence
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
