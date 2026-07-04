ms-login — ModelScope.cn Phone+SMS Login Service
=================================================

A small HTTP service that drives the remote Chrome browser on the HPC
login node (via samvnc-server behind the aitun tunnel) to log into
modelscope.cn using phone + SMS. Designed so an AI assistant (or any
external script) can trigger and supervise the login flow.

Architecture
------------

    ┌──────────────────────────────┐
    │ Local machine                │
    │  ms-login service (FastAPI)  │  ← you call /login/start etc.
    │  http://127.0.0.1:8765       │
    └────────────┬─────────────────┘
                 │ HTTPS POST
                 ▼
       https://aitun.cc/GMCLNGFZ/api/*
                 │ aitun relay
                 ▼
    ┌──────────────────────────────┐
    │ HPC login07 (CentOS 7)       │
    │  samvnc-server (port 8128)   │  ← Go HTTP API
    │  google-chrome (CDP 9222)    │  ← real Chrome inside proot
    │  fresh_profile (cookies)     │  ← login session persists here
    └──────────────────────────────┘

The SamVNC server exposes /api/navigate, /api/eval, /api/iframe/* etc.
This service orchestrates a phone+SMS login by chaining those calls.

Quick start
-----------

    # 1. Start the service
    cd ms_login
    ./run.sh
    # → listening on http://127.0.0.1:8765

    # 2. Set your modelscope phone number
    curl -X POST http://127.0.0.1:8765/config \
      -H 'Content-Type: application/json' \
      -d '{"phone": "138xxxxxxxx"}'

    # 3. Start a login flow
    curl -X POST http://127.0.0.1:8765/login/start
    # → {"session_id": "abc123def456", "state": "sms_sent", ...}
    #   (or "manual_action_required" if a CAPTCHA appeared)

    # 4. When you receive the SMS, submit the code
    curl -X POST http://127.0.0.1:8765/login/sms \
      -H 'Content-Type: application/json' \
      -d '{"session_id": "abc123def456", "code": "1234"}'
    # → {"state": "logged_in", ...}

    # 5. (If CAPTCHA required) Open VNC URL in a browser, complete the
    #    slider/captcha manually, then resume:
    curl -X POST http://127.0.0.1:8765/login/continue \
      -H 'Content-Type: application/json' \
      -d '{"session_id": "abc123def456"}'

    # 6. Check login status
    curl http://127.0.0.1:8765/login/status

Endpoints
---------

  GET  /                     HTML help page
  GET  /health               Ping samvnc + report status
  GET  /config               Show config (phone masked)
  POST /config               Update config {phone, samvnc_base_url, ...}
  POST /login/start          Start login flow {phone?} → {session_id, state, screenshot}
  POST /login/sms            Submit SMS code {session_id, code}
  POST /login/continue       Resume after manual CAPTCHA {session_id}
  GET  /login/status         Check current login state (probes /my/overview)
  GET  /sessions             List all sessions
  GET  /sessions/{sid}       Get one session
  GET  /screenshot           Current Chrome screenshot (PNG)
  POST /debug/navigate       Manual navigate {url}
  POST /debug/eval           Manual JS eval {script}
  POST /debug/iframe/eval    Manual iframe JS eval {iframe, script}

Configuration
-------------

Config is loaded from (in order):

  1. $MS_LOGIN_CONFIG
  2. ~/.config/ms_login/credentials.json
  3. /home/z/my-project/.config/ms_login/credentials.json
  4. /etc/ms_login/credentials.json

Schema:

  {
    "phone": "138xxxxxxxx",
    "samvnc_base_url": "https://aitun.cc/GMCLNGFZ",
    "vnc_url": "https://aitun.cc/GMCLNGFZ/",
    "service_host": "127.0.0.1",
    "service_port": 8765
  }

The `phone` field is the only required one. Use `POST /config` to set
it without editing the file directly.

CAPTCHA / Slider fallback
-------------------------

If the Alibaba NoCaptcha slider, image captcha, or Baxia container
appears after clicking "Get code", the flow pauses in state
`manual_action_required`. The response includes:

  - `vnc_url`: a link to the SamVNC web client (`https://aitun.cc/GMCLNGFZ/`)
    — open it in any browser to see and control the remote Chrome directly.
  - `screenshot_base64`: a snapshot of the current page so you can see
    what's needed without opening VNC.

After completing the verification in VNC, call `POST /login/continue`
and the flow will resume (it re-checks for SMS sent state and moves on).

Files
-----

  service.py        — FastAPI app + all HTTP endpoints
  samvnc_client.py  — SamVNC /api/* HTTP client wrapper
  login_flow.py     — ModelScope login state machine (navigate → SMS → verify)
  config.py         — JSON config file management
  run.sh            — launcher script

Logs
----

The service logs to stdout. Set `MS_LOGIN_LOG=DEBUG` for verbose logging
of every SamVNC API call.

Notes
-----

- The samvnc-server on the HPC node must be running and the aitun tunnel
  must be active. Check `GET /health` if anything is wrong.
- The aitun tunnel URL (currently `https://aitun.cc/GMCLNGFZ`) is a free
  relay and changes daily (each tunnel has 24h validity). If the URL
  stops working, check the latest aitun log on the HPC node
  (`~/aitun_8128.log`) for the new code.
- The Chrome on the HPC node uses `--user-data-dir=/root/fresh_profile`,
  so cookies persist across restarts. Once logged in, subsequent visits
  to modelscope.cn should be authenticated without re-login.
