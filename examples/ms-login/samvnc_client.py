#!/usr/bin/env python3
"""
SamVNC HTTP API client.

Wraps all /api/* endpoints exposed by samvnc-server (running on the HPC node
behind the aitun tunnel at https://aitun.cc/<CODE>).

Reference: https://github.com/samaidev/samvnc/blob/main/internal/server/api.go
"""
from __future__ import annotations

import base64
import json
import logging
import time
from typing import Any, Optional

import requests

log = logging.getLogger("ms_login.samvnc")


class SamVNCError(RuntimeError):
    """Raised when the SamVNC API returns an error."""

    def __init__(self, message: str, status: Optional[int] = None, body: str = ""):
        super().__init__(message)
        self.status = status
        self.body = body


class SamVNCClient:
    """Thin HTTP client for the SamVNC /api/* endpoints."""

    def __init__(self, base_url: str, default_timeout: float = 30.0):
        self.base_url = base_url.rstrip("/")
        self.default_timeout = default_timeout
        self.session = requests.Session()
        # Don't keep-alive too aggressively through aitun relay
        self.session.headers.update({"User-Agent": "ms-login/1.0"})

    # ---- low-level helper ----
    def _call(self, method: str, path: str, retries: int = 3, **kwargs) -> requests.Response:
        url = f"{self.base_url}{path}"
        timeout = kwargs.pop("timeout", self.default_timeout)
        last_exc = None
        for attempt in range(retries):
            log.debug("-> %s %s (attempt %d)", method, url, attempt + 1)
            try:
                r = self.session.request(method, url, timeout=timeout, **kwargs)
            except requests.RequestException as e:
                last_exc = e
                log.debug("   network error: %s, retrying", e)
                time.sleep(1 + attempt)
                continue
            # 500/502/503 — likely transient (samvnc busy or relay hiccup)
            if r.status_code in (500, 502, 503) and attempt < retries - 1:
                log.debug("   HTTP %d, retrying", r.status_code)
                time.sleep(1 + attempt)
                continue
            log.debug("<- %s %s (len=%d)", r.status_code, url, len(r.content))
            return r
        # All retries exhausted
        if last_exc:
            raise SamVNCError(f"network error after {retries} retries: {last_exc}") from last_exc
        raise SamVNCError(f"{method} {path} -> HTTP {r.status_code} after {retries} retries")

    def _call_json(self, method: str, path: str, **kwargs) -> Any:
        r = self._call(method, path, **kwargs)
        if r.status_code >= 400:
            raise SamVNCError(
                f"{method} {path} -> HTTP {r.status_code}",
                status=r.status_code,
                body=r.text[:500],
            )
        try:
            return r.json()
        except ValueError as e:
            raise SamVNCError(f"non-JSON response from {path}: {r.text[:200]}") from e

    # ---- /api/status ----
    def status(self) -> dict:
        """Return samvnc server status: {ok, cdp, capture, injector}."""
        return self._call_json("GET", "/api/status")

    def health(self) -> bool:
        """Lightweight health check."""
        try:
            s = self.status()
            return bool(s.get("ok"))
        except Exception:
            return False

    # ---- /api/screenshot ----
    def screenshot_png(self) -> bytes:
        """Return current screen as PNG bytes."""
        r = self._call("GET", "/api/screenshot")
        if r.status_code != 200 or not r.content:
            raise SamVNCError(
                f"screenshot -> HTTP {r.status_code} ({len(r.content)} bytes)",
                status=r.status_code,
                body=r.text[:200],
            )
        return r.content

    def screenshot_base64(self) -> str:
        """Return current screen as base64-encoded PNG (for JSON embedding)."""
        return base64.b64encode(self.screenshot_png()).decode("ascii")

    # ---- /api/navigate ----
    def navigate(self, url: str, wait: float = 0.0) -> dict:
        """Navigate the active Chrome tab to a URL."""
        out = self._call_json("POST", "/api/navigate", json={"url": url})
        if wait:
            time.sleep(wait)
        return out

    # ---- /api/eval ----
    def eval(self, script: str) -> Any:
        """Run JavaScript in the top-level page. Returns the result value."""
        r = self._call_json("POST", "/api/eval", json={"script": script})
        if r.get("ok"):
            return r.get("result")
        raise SamVNCError(f"eval failed: {r}")

    def eval_json(self, script: str) -> Any:
        """Run JS that returns JSON.stringify(...); parse and return."""
        raw = self.eval(script)
        if raw is None:
            return None
        try:
            return json.loads(raw)
        except (ValueError, TypeError) as e:
            raise SamVNCError(f"eval returned non-JSON: {raw!r}") from e

    # ---- /api/dom ----
    def dom(self, selector: str = "body") -> Optional[str]:
        """Return outerHTML of the first element matching `selector`."""
        r = self._call_json("POST", "/api/dom", json={"selector": selector})
        return r.get("html")

    # ---- /api/targets ----
    def targets(self) -> list[dict]:
        """List all CDP targets (pages, iframes, browser_ui)."""
        r = self._call_json("GET", "/api/targets")
        return r.get("targets", [])

    # ---- /api/cookies ----
    def cookies(self) -> list[dict]:
        """Return all Chrome cookies for current profile."""
        r = self._call_json("GET", "/api/cookies")
        return r.get("cookies", [])

    # ---- /api/click / /api/type / /api/scroll (X11-level) ----
    def click(self, x: int, y: int) -> dict:
        """Click at screen coordinates (X11 injection)."""
        return self._call_json("POST", "/api/click", json={"x": x, "y": y})

    def type_text(self, text: str) -> dict:
        """Type text via xdotool (X11 injection).

        REQUIRES the target input to already have X11 focus.
        Use iframe_eval(..., 'el.focus()') first to set focus.
        """
        return self._call_json("POST", "/api/type", json={"text": text})

    def scroll(self, dx: int = 0, dy: int = 300) -> dict:
        """Scroll page by (dx, dy)."""
        return self._call_json("POST", "/api/scroll", json={"dx": dx, "dy": dy})

    # ---- /poll (low-level protocol message dispatch) ----
    def send_protocol_messages(self, messages: list[dict], wait_frame: bool = False) -> dict:
        """Send a batch of protocol messages via /poll endpoint.

        This is the only way to send MouseEvent sequences (for dragging) or
        TextEvent (for Unicode text input via xdotool). The /api/click and
        /api/type endpoints are wrappers that send a single MouseEvent or
        TextEvent respectively, but they don't support multi-event batches.

        messages: list of protocol dicts, e.g.:
            [{"type": "mouse", "x": 100, "y": 200, "mask": 0, "wheel": 0}, ...]
            [{"type": "text", "text": "hello"}]

        wait_frame: if True, the server waits for a fresh screen frame before
        returning (useful for visual feedback). If False (default), uses a
        high seq number so the server returns immediately after dispatching
        inputs.

        Returns the /poll response (which includes a frame if available).
        """
        seq = 0 if wait_frame else 99999
        payload = {
            "seq": seq,
            "inputs": [json.dumps(m) for m in messages],
        }
        r = self._call("POST", "/poll", json=payload, timeout=60)
        if r.status_code >= 400:
            raise SamVNCError(
                f"POST /poll -> HTTP {r.status_code}",
                status=r.status_code,
                body=r.text[:500],
            )
        try:
            return r.json()
        except ValueError:
            return {"raw": r.text[:500]}

    # ---- high-level: type text via X11 (xdotool) ----
    def type_via_x11(self, text: str) -> dict:
        """Type text via X11 xdotool injection (real keyboard events).

        Use this when iframe_set_value doesn't trigger the form's event
        handlers (common with Alibaba passport). The target input must have
        X11 focus first — call iframe_eval(..., 'el.focus()') before this.

        Sends a Hello message first to establish /poll session, then the
        TextEvent.
        """
        hello = {
            "type": "hello",
            "version": 1,
            "quality": 2,
            "width": 1280,
            "height": 720,
            "mode": 2,  # relay
            "fps": 15,
        }
        text_msg = {"type": "text", "text": text}
        return self.send_protocol_messages([hello, text_msg], wait_frame=False)

    # ---- high-level: drag (mouse press + move + release) ----
    def drag(
        self,
        x1: int,
        y1: int,
        x2: int,
        y2: int,
        steps: int = 25,
        step_delay_ms: int = 30,
        jitter: bool = True,
    ) -> dict:
        """Drag the mouse from (x1, y1) to (x2, y1) with left button held.

        This is required for Alibaba NoCaptcha slider verification, which
        detects mousedown → mousemove* → mouseup sequences. /api/click only
        does press+release at the same position, which NoCaptcha ignores.

        Args:
            x1, y1: start (screen coords, 1280x720)
            x2, y2: end
            steps: number of intermediate mousemove events
            step_delay_ms: delay between batches of events (samvnc processes
                them quickly, but pacing helps NoCaptcha see realistic timing)
            jitter: if True, add small Y-axis jitter to look human

        Returns the last /poll response.
        """
        # ButtonLeft = 1
        BUTTON_LEFT = 1
        events: list[dict] = []
        # 1. Move to start (no button)
        events.append({"type": "mouse", "x": x1, "y": y1, "mask": 0, "wheel": 0})
        # 2. Press left button
        events.append({"type": "mouse", "x": x1, "y": y1, "mask": BUTTON_LEFT, "wheel": 0})
        # 3. Move in steps (button held)
        for i in range(1, steps + 1):
            x = x1 + (x2 - x1) * i // steps
            y = y2 + (y2 - y2) * i // steps  # keep y constant
            if jitter:
                # Small deterministic jitter (no random, for reproducibility)
                y = y1 + ((i * 7) % 5) - 2
            events.append({"type": "mouse", "x": x, "y": y, "mask": BUTTON_LEFT, "wheel": 0})
        # 4. Release at end
        events.append({"type": "mouse", "x": x2, "y": y2, "mask": 0, "wheel": 0})

        log.info("drag (%d,%d) -> (%d,%d) (%d events)", x1, y1, x2, y2, len(events))

        # Send in small batches with delays for realistic timing
        last_resp = {}
        batch_size = 5
        for batch_start in range(0, len(events), batch_size):
            batch = events[batch_start:batch_start + batch_size]
            last_resp = self.send_protocol_messages(batch, wait_frame=False)
            time.sleep(step_delay_ms / 1000.0)
        return last_resp

    # ---- /api/iframe/* (CDP-level, much more reliable than X11) ----
    def iframe_eval(self, iframe: str, script: str) -> Any:
        """Run JS inside an iframe matched by URL substring."""
        r = self._call_json(
            "POST",
            "/api/iframe/eval",
            json={"iframe": iframe, "script": script},
        )
        if r.get("ok"):
            return r.get("result")
        raise SamVNCError(f"iframe_eval failed: {r}")

    def iframe_eval_json(self, iframe: str, script: str) -> Any:
        raw = self.iframe_eval(iframe, script)
        if raw is None:
            return None
        try:
            return json.loads(raw)
        except (ValueError, TypeError) as e:
            raise SamVNCError(f"iframe_eval returned non-JSON: {raw!r}") from e

    def iframe_dom(self, iframe: str, selector: str = "body") -> Optional[str]:
        r = self._call_json(
            "POST", "/api/iframe/dom",
            json={"iframe": iframe, "selector": selector},
        )
        return r.get("html")

    def iframe_click(self, iframe: str, selector: str) -> dict:
        """Click an element inside an iframe by CSS selector."""
        return self._call_json(
            "POST", "/api/iframe/click",
            json={"iframe": iframe, "selector": selector},
        )

    def iframe_type(self, iframe: str, selector: str, text: str) -> dict:
        """Set the value of an input element inside an iframe.

        Dispatches `input` and `change` events so React/Vue pick it up.
        """
        return self._call_json(
            "POST", "/api/iframe/type",
            json={"iframe": iframe, "selector": selector, "text": text},
        )

    # ---- convenience: high-level "fill form field + dispatch events" ----
    def iframe_set_value(self, iframe: str, selector: str, value: str) -> Any:
        """More thorough value setter than iframe_type — handles React/Vue/jQuery.

        Sets value, fires input/change/blur events, and also calls any
        onChange handler attached via jQuery (Alibaba passport uses jQuery).
        """
        # Escape for JS single-quoted string
        esc_sel = selector.replace("\\", "\\\\").replace("'", "\\'")
        esc_val = value.replace("\\", "\\\\").replace("'", "\\'")
        script = f"""
        (function() {{
            var el = document.querySelector('{esc_sel}');
            if (!el) return 'element not found';
            el.focus();
            var nativeSetter = Object.getOwnPropertyDescriptor(
                window.HTMLInputElement.prototype, 'value'
            ).set;
            nativeSetter.call(el, '{esc_val}');
            el.dispatchEvent(new Event('input', {{bubbles: true}}));
            el.dispatchEvent(new Event('change', {{bubbles: true}}));
            el.dispatchEvent(new Event('blur', {{bubbles: true}}));
            // Some legacy forms use jQuery trigger
            if (window.jQuery) {{
                try {{ window.jQuery(el).trigger('change'); }} catch(e) {{}}
            }}
            return 'ok';
        }})()
        """
        return self.iframe_eval(iframe, script)

    # ---- ping helper ----
    def ping(self) -> bool:
        """Quick liveness check — used by /health endpoint."""
        try:
            return self.health()
        except Exception:
            return False
