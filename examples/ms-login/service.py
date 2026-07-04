#!/usr/bin/env python3
"""
ms-login HTTP service.

A FastAPI service that drives the remote Chrome browser on the HPC node
(via samvnc-server exposed through the aitun tunnel) to log into
modelscope.cn using phone + SMS, with manual CAPTCHA fallback.

Run:
    python3 service.py
    # or
    uvicorn service:app --host 127.0.0.1 --port 8765

Endpoints (see API summary below — also served at /):
    GET  /                    — this help page (HTML)
    GET  /health              — ping samvnc + return status
    GET  /config              — show config (phone masked)
    POST /config              — update config {phone, samvnc_base_url, ...}
    POST /login/start         — start login flow; {phone?} → {session_id, state, screenshot}
    POST /login/sms           — submit SMS code; {session_id, code} → {state, screenshot}
    POST /login/continue      — resume after manual CAPTCHA; {session_id} → {state, screenshot}
    GET  /login/status        — check current login state (probes /my/overview)
    GET  /sessions            — list all sessions
    GET  /sessions/{sid}      — get one session
    GET  /screenshot          — current Chrome screenshot (PNG)
    GET  /screenshot.jpg      — same, JPEG-formatted (lazy: actually PNG)
    POST /debug/navigate      — {url} manual nav (debug)
    POST /debug/eval          — {script} manual JS eval (debug)
    POST /debug/iframe/eval   — {iframe, script} (debug)
"""
from __future__ import annotations

import logging
import os
import sys
import time
import uuid
from typing import Optional

# Make sibling modules importable when running as a script
HERE = os.path.dirname(os.path.abspath(__file__))
if HERE not in sys.path:
    sys.path.insert(0, HERE)

from fastapi import FastAPI, HTTPException
from fastapi.responses import HTMLResponse, JSONResponse, Response, FileResponse
from pydantic import BaseModel, Field

from config import Config, load_config, save_config
from samvnc_client import SamVNCClient, SamVNCError
from login_flow import LoginSession, ModelScopeLoginFlow, State, OVERVIEW_URL, LOGIN_URL

# ---- logging ----
logging.basicConfig(
    level=os.environ.get("MS_LOGIN_LOG", "INFO"),
    format="%(asctime)s %(levelname)-7s %(name)s | %(message)s",
    datefmt="%H:%M:%S",
)
log = logging.getLogger("ms_login")

# ---- global state ----
CONFIG: Config = load_config()
SESSIONS: dict[str, LoginSession] = {}
FLOWS: dict[str, ModelScopeLoginFlow] = {}


def get_client() -> SamVNCClient:
    return SamVNCClient(CONFIG.samvnc_base_url)


# ---- Pydantic request models ----
class LoginStartReq(BaseModel):
    phone: Optional[str] = Field(default=None, description="Phone number; defaults to config.phone")


class LoginSmsReq(BaseModel):
    session_id: str
    code: str = Field(..., min_length=4, max_length=8)


class LoginContinueReq(BaseModel):
    session_id: str


class ConfigUpdateReq(BaseModel):
    phone: Optional[str] = None
    samvnc_base_url: Optional[str] = None
    vnc_url: Optional[str] = None
    service_host: Optional[str] = None
    service_port: Optional[int] = None


class DebugNavigateReq(BaseModel):
    url: str


class DebugEvalReq(BaseModel):
    script: str


class DebugIframeEvalReq(BaseModel):
    iframe: str = "passport"
    script: str


# ---- FastAPI app ----
app = FastAPI(
    title="ms-login",
    description="ModelScope.cn phone+SMS login service (drives remote Chrome via samvnc)",
    version="1.0.0",
)


@app.get("/", response_class=HTMLResponse)
def index() -> HTMLResponse:
    """Render a small help page with current state."""
    sessions_html = "".join(
        f"<tr><td>{s.session_id[:8]}</td><td>{s.phone}</td>"
        f"<td>{s.state.value}</td><td>{s.error or s.message or ''}</td>"
        f"<td>{time.strftime('%H:%M:%S', time.localtime(s.updated_at))}</td></tr>"
        for s in SESSIONS.values()
    )
    return HTMLResponse(
        f"""<!doctype html>
<html><head>
<meta charset="utf-8">
<title>ms-login</title>
<style>
  body {{ font: 14px/1.5 -apple-system, BlinkMacSystemFont, 'PingFang SC', sans-serif;
         max-width: 960px; margin: 2em auto; padding: 0 1em; color: #222; }}
  h1 {{ color: #624AFF; }}
  h2 {{ margin-top: 2em; border-bottom: 1px solid #eee; padding-bottom: .3em; }}
  code, pre {{ background: #f6f6f6; padding: 1px 4px; border-radius: 3px;
              font-family: 'SF Mono', Menlo, Consolas, monospace; }}
  pre {{ padding: .8em; overflow: auto; }}
  table {{ border-collapse: collapse; width: 100%; margin: 1em 0; }}
  td, th {{ border: 1px solid #ddd; padding: 4px 8px; text-align: left; }}
  th {{ background: #f6f6f6; }}
  .ok {{ color: #2c8a3a; }}
  .err {{ color: #c0341c; }}
  .warn {{ color: #b27912; }}
</style>
</head><body>
<h1>ms-login <small>v1.0</small></h1>
<p>Drive the remote Chrome on the HPC node (via samvnc) to log into
<a href="https://modelscope.cn">modelscope.cn</a> using phone + SMS.</p>

<h2>Current config</h2>
<pre>{__import__('json').dumps(CONFIG.to_masked_dict(), indent=2, ensure_ascii=False)}</pre>
<p><a href="/config">/config</a> · <a href="/health">/health</a> ·
<a href="/login/status">/login/status</a> · <a href="/sessions">/sessions</a> ·
<a href="{CONFIG.vnc_url}" target="_blank">Open VNC viewer</a> (manual control)</p>

<h2>Quick start</h2>
<pre>
# 1. Set phone number
curl -X POST http://{CONFIG.service_host}:{CONFIG.service_port}/config \\
  -H 'Content-Type: application/json' \\
  -d '{{"phone": "138xxxxxxxx"}}'

# 2. Start login
curl -X POST http://{CONFIG.service_host}:{CONFIG.service_port}/login/start

# 3. (After receiving SMS) Submit code
curl -X POST http://{CONFIG.service_host}:{CONFIG.service_port}/login/sms \\
  -H 'Content-Type: application/json' \\
  -d '{{"session_id": "&lt;from step 2&gt;", "code": "1234"}}'

# 4. If CAPTCHA: open VNC URL in browser, complete manually, then:
curl -X POST http://{CONFIG.service_host}:{CONFIG.service_port}/login/continue \\
  -H 'Content-Type: application/json' \\
  -d '{{"session_id": "&lt;from step 2&gt;"}}'

# 5. Check status
curl http://{CONFIG.service_host}:{CONFIG.service_port}/login/status
</pre>

<h2>Active sessions ({len(SESSIONS)})</h2>
<table>
<tr><th>ID</th><th>Phone</th><th>State</th><th>Message/Error</th><th>Updated</th></tr>
{sessions_html or '<tr><td colspan=5><i>none</i></td></tr>'}
</table>

<h2>API endpoints</h2>
<table>
<tr><th>Method</th><th>Path</th><th>Description</th></tr>
<tr><td>GET</td><td>/</td><td>this page</td></tr>
<tr><td>GET</td><td>/health</td><td>ping samvnc, return {{"ok": true/false, "samvnc": {...}}}</td></tr>
<tr><td>GET</td><td>/config</td><td>show config (phone masked)</td></tr>
<tr><td>POST</td><td>/config</td><td>update config</td></tr>
<tr><td>POST</td><td>/login/start</td><td>start login flow</td></tr>
<tr><td>POST</td><td>/login/sms</td><td>submit SMS code</td></tr>
<tr><td>POST</td><td>/login/continue</td><td>resume after manual CAPTCHA</td></tr>
<tr><td>GET</td><td>/login/status</td><td>probe /my/overview, return logged_in</td></tr>
<tr><td>GET</td><td>/sessions</td><td>list all sessions</td></tr>
<tr><td>GET</td><td>/sessions/{{sid}}</td><td>get one session</td></tr>
<tr><td>GET</td><td>/screenshot</td><td>current Chrome screenshot (PNG)</td></tr>
<tr><td>POST</td><td>/debug/navigate</td><td>manual navigate (debug)</td></tr>
<tr><td>POST</td><td>/debug/eval</td><td>manual JS eval (debug)</td></tr>
<tr><td>POST</td><td>/debug/iframe/eval</td><td>manual iframe JS eval (debug)</td></tr>
</table>
</body></html>"""
    )


@app.get("/health")
def health() -> JSONResponse:
    """Ping samvnc and report status."""
    try:
        client = get_client()
        s = client.status()
        return JSONResponse({"ok": True, "samvnc": s, "config_loaded": bool(CONFIG.phone)})
    except SamVNCError as e:
        return JSONResponse(
            {"ok": False, "error": str(e), "status": e.status, "body": e.body},
            status_code=502,
        )
    except Exception as e:
        return JSONResponse({"ok": False, "error": f"{type(e).__name__}: {e}"}, status_code=500)


@app.get("/config")
def get_config() -> JSONResponse:
    return JSONResponse(CONFIG.to_masked_dict())


@app.post("/config")
def update_config(req: ConfigUpdateReq) -> JSONResponse:
    global CONFIG
    if req.phone is not None:
        CONFIG.phone = req.phone
    if req.samvnc_base_url is not None:
        CONFIG.samvnc_base_url = req.samvnc_base_url
    if req.vnc_url is not None:
        CONFIG.vnc_url = req.vnc_url
    if req.service_host is not None:
        CONFIG.service_host = req.service_host
    if req.service_port is not None:
        CONFIG.service_port = req.service_port
    # Recompute vnc_url if base changed and vnc_url wasn't explicitly set
    if req.samvnc_base_url and not req.vnc_url:
        CONFIG.vnc_url = CONFIG.samvnc_base_url.rstrip("/") + "/"
    path = save_config(CONFIG)
    log.info("config updated and saved to %s", path)
    return JSONResponse({"ok": True, "config": CONFIG.to_masked_dict(), "saved_to": str(path)})


def _session_response(s: LoginSession, include_screenshot: bool = True) -> dict:
    """Build a JSON-serializable session dict, optionally with screenshot."""
    out = s.to_dict()
    if include_screenshot and s.last_screenshot_b64:
        out["screenshot_base64"] = s.last_screenshot_b64
    return out


@app.post("/login/start")
def login_start(req: LoginStartReq) -> JSONResponse:
    """Start a new phone+SMS login flow."""
    phone = req.phone or CONFIG.phone
    if not phone:
        raise HTTPException(400, "no phone number provided (set via /config or in request body)")
    if not CONFIG.samvnc_base_url:
        raise HTTPException(500, "samvnc_base_url not configured")

    sid = uuid.uuid4().hex[:12]
    session = LoginSession(session_id=sid, phone=phone)
    SESSIONS[sid] = session
    flow = ModelScopeLoginFlow(get_client(), session, vnc_url=CONFIG.vnc_url)
    FLOWS[sid] = flow
    log.info("starting login flow sid=%s phone=%s", sid, phone)
    # Run the flow synchronously — this returns after either SMS_SENT or
    # MANUAL or FAILED. Subsequent steps are triggered by /login/sms and
    # /login/continue.
    flow.step_navigate()
    return JSONResponse(_session_response(session))


@app.post("/login/sms")
def login_sms(req: LoginSmsReq) -> JSONResponse:
    """Submit the SMS code received by the user."""
    session = SESSIONS.get(req.session_id)
    if not session:
        raise HTTPException(404, f"session not found: {req.session_id}")
    flow = FLOWS.get(req.session_id)
    if not flow:
        raise HTTPException(500, "flow object missing")
    if session.state not in (State.SMS_SENT, State.MANUAL):
        raise HTTPException(
            409,
            f"session is in state {session.state.value}, "
            f"cannot submit SMS (expected sms_sent or manual_action_required)",
        )
    flow.step_submit_sms(req.code)
    return JSONResponse(_session_response(session))


@app.post("/login/continue")
def login_continue(req: LoginContinueReq) -> JSONResponse:
    """Resume the flow after the user completed a manual CAPTCHA / slider."""
    session = SESSIONS.get(req.session_id)
    if not session:
        raise HTTPException(404, f"session not found: {req.session_id}")
    flow = FLOWS.get(req.session_id)
    if not flow:
        raise HTTPException(500, "flow object missing")
    if session.state != State.MANUAL:
        return JSONResponse(
            _session_response(session),
            status_code=200,
        )
    flow.step_continue_after_manual()
    return JSONResponse(_session_response(session))


@app.get("/login/status")
def login_status() -> JSONResponse:
    """Probe /my/overview and report whether currently logged in.

    Defensive: wraps each step so a single failure (e.g. cookies endpoint
    timing out) doesn't sink the whole status check.
    """
    out: dict = {"ok": True, "logged_in": False}
    try:
        client = get_client()
        # Navigate first
        try:
            client.navigate(OVERVIEW_URL, wait=0)
            time.sleep(5)
        except SamVNCError as e:
            out.setdefault("warnings", []).append(f"navigate: {e}")

        # Check URL — primary signal for logged_in
        try:
            info = client.eval_json(
                "JSON.stringify({url: location.href, title: document.title})"
            )
            url = (info or {}).get("url", "")
            out["url"] = url
            out["title"] = (info or {}).get("title")
            out["logged_in"] = (
                "/login" not in url.lower() and "passport" not in url.lower()
            )
        except SamVNCError as e:
            out.setdefault("warnings", []).append(f"eval: {e}")

        # Cookies (best-effort — /api/cookies can 500 if too many)
        try:
            cookies = client.cookies()
            out["cookies_count"] = len(cookies)
            out["modelscope_cookies"] = sum(
                1 for c in cookies if "modelscope" in c.get("domain", "")
            )
            identity_cookies = [
                c.get("name") for c in cookies
                if c.get("domain", "").endswith("modelscope.cn")
                and c.get("name", "").lower() in
                {"login_user_id", "login_user_name", "ms_session",
                 "ssxmod_itna", "isg", "cna"}
            ]
            out["identity_cookie_hints"] = identity_cookies
        except SamVNCError as e:
            out.setdefault("warnings", []).append(f"cookies: {e}")

        # Try to extract username (best-effort)
        try:
            username = client.eval(
                "(document.querySelector('.user-name, .username, "
                "[class*=user-name]')||{}).textContent || ''"
            )
            out["username_hint"] = username
        except SamVNCError as e:
            out.setdefault("warnings", []).append(f"username: {e}")

        # Screenshot (best-effort)
        try:
            out["screenshot_base64"] = client.screenshot_base64()
        except SamVNCError as e:
            out.setdefault("warnings", []).append(f"screenshot: {e}")

        return JSONResponse(out)
    except Exception as e:
        return JSONResponse(
            {"ok": False, "error": f"{type(e).__name__}: {e}"},
            status_code=500,
        )


@app.get("/sessions")
def list_sessions() -> JSONResponse:
    return JSONResponse({"sessions": [s.to_dict() for s in SESSIONS.values()]})


@app.get("/sessions/{sid}")
def get_session(sid: str) -> JSONResponse:
    s = SESSIONS.get(sid)
    if not s:
        raise HTTPException(404, f"session not found: {sid}")
    return JSONResponse(_session_response(s))


@app.get("/screenshot")
def screenshot() -> Response:
    """Return current Chrome screenshot as PNG."""
    try:
        png = get_client().screenshot_png()
        return Response(content=png, media_type="image/png")
    except SamVNCError as e:
        raise HTTPException(502, str(e))


# ---- debug endpoints ----
@app.post("/debug/navigate")
def debug_navigate(req: DebugNavigateReq) -> JSONResponse:
    try:
        return JSONResponse(get_client().navigate(req.url))
    except SamVNCError as e:
        raise HTTPException(502, str(e))


@app.post("/debug/eval")
def debug_eval(req: DebugEvalReq) -> JSONResponse:
    try:
        return JSONResponse({"ok": True, "result": get_client().eval(req.script)})
    except SamVNCError as e:
        raise HTTPException(502, str(e))


@app.post("/debug/iframe/eval")
def debug_iframe_eval(req: DebugIframeEvalReq) -> JSONResponse:
    try:
        return JSONResponse({"ok": True, "result": get_client().iframe_eval(req.iframe, req.script)})
    except SamVNCError as e:
        raise HTTPException(502, str(e))


# ---- main entry ----
def main():
    import uvicorn
    host = CONFIG.service_host
    port = CONFIG.service_port
    log.info("ms-login service starting on http://%s:%d", host, port)
    log.info("samvnc base URL: %s", CONFIG.samvnc_base_url)
    log.info("VNC URL:         %s", CONFIG.vnc_url)
    log.info("phone:           %s", CONFIG.phone[:3] + "***" + CONFIG.phone[-4:] if CONFIG.phone else "(not set)")
    uvicorn.run(app, host=host, port=port, log_level="info")


if __name__ == "__main__":
    main()
