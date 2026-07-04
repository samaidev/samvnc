#!/usr/bin/env python3
"""
ModelScope login flow — phone + SMS, with manual CAPTCHA fallback.

States:
  init            — flow just created
  navigating      — navigating to /login
  sms_tab_clicked — switched to SMS login tab
  phone_filled    — phone number entered
  sms_requested   — clicked "Get code" button
  sms_sent        — confirmed SMS sent (or fell through to manual)
  sms_submitted   — SMS code submitted
  verifying       — checking login status
  logged_in       — verified logged in
  failed          — unrecoverable failure
  manual_action_required — needs human intervention (CAPTCHA, slider, etc.)

The flow uses samvnc-server's /api/iframe/* endpoints, which talk CDP
directly to the Chrome instance running inside the proot container on
the HPC node. The iframe matching substring is "passport" — that matches
the alibaba-login-box iframe whose src is
https://passport.modelscope.cn/mini_login.htm?...
"""
from __future__ import annotations

import logging
import time
from dataclasses import dataclass, field
from enum import Enum
from typing import Optional

from samvnc_client import SamVNCClient, SamVNCError

log = logging.getLogger("ms_login.flow")

# iframe URL substring that matches the Alibaba passport login iframe
PASSPORT_IFRAME = "passport"

LOGIN_URL = "https://modelscope.cn/login"
OVERVIEW_URL = "https://modelscope.cn/my/overview"


class State(str, Enum):
    INIT = "init"
    NAVIGATING = "navigating"
    SMS_TAB_CLICKED = "sms_tab_clicked"
    PHONE_FILLED = "phone_filled"
    SMS_REQUESTED = "sms_requested"
    SMS_SENT = "sms_sent"
    SMS_SUBMITTED = "sms_submitted"
    VERIFYING = "verifying"
    LOGGED_IN = "logged_in"
    FAILED = "failed"
    MANUAL = "manual_action_required"


@dataclass
class LoginSession:
    """Holds the state of one login attempt."""

    session_id: str
    phone: str
    state: State = State.INIT
    error: Optional[str] = None
    message: Optional[str] = None
    last_screenshot_b64: Optional[str] = None
    vnc_url: Optional[str] = None
    created_at: float = field(default_factory=time.time)
    updated_at: float = field(default_factory=time.time)

    # Internal: SMS code provided by the user via /login/sms
    sms_code: Optional[str] = None

    def touch(self):
        self.updated_at = time.time()

    def to_dict(self) -> dict:
        return {
            "session_id": self.session_id,
            "phone": self.phone,
            "state": self.state.value,
            "error": self.error,
            "message": self.message,
            "vnc_url": self.vnc_url,
            "created_at": self.created_at,
            "updated_at": self.updated_at,
            "has_screenshot": self.last_screenshot_b64 is not None,
        }


class ModelScopeLoginFlow:
    """Drives one phone+SMS login attempt via the SamVNC API."""

    def __init__(self, client: SamVNCClient, session: LoginSession, vnc_url: str = ""):
        self.client = client
        self.session = session
        self.vnc_url = vnc_url
        # If user has been asked to do manual action, the next /continue call
        # should jump to the appropriate next step.
        self._resume_after_manual: Optional[str] = None

    # ------------------------------------------------------------------
    # Step 1: navigate to /login
    # ------------------------------------------------------------------
    def step_navigate(self) -> None:
        log.info("[%s] navigating to %s", self.session.session_id, LOGIN_URL)
        self.session.state = State.NAVIGATING
        self.session.touch()
        try:
            self.client.navigate(LOGIN_URL, wait=0)
        except SamVNCError as e:
            self._fail(f"navigate failed: {e}")
            return
        # Wait for iframe to load
        time.sleep(6)
        self._snapshot()
        # Verify iframe present
        try:
            info = self.client.iframe_eval_json(
                PASSPORT_IFRAME,
                "JSON.stringify({title: document.title, url: location.href, "
                "ready: document.readyState})",
            )
            log.info("iframe info: %s", info)
            if not info or "passport" not in (info.get("url") or "").lower():
                self._fail("passport iframe not detected on /login")
                return
        except SamVNCError as e:
            self._fail(f"iframe probe failed: {e}")
            return
        # Iframe OK — go to next step
        self.step_switch_to_sms_tab()

    # ------------------------------------------------------------------
    # Step 2: click "SMS verification code login" tab
    # ------------------------------------------------------------------
    def step_switch_to_sms_tab(self) -> None:
        log.info("[%s] switching to SMS tab", self.session.session_id)
        try:
            # The tabs are div.login-tabs-tab; second one is SMS
            self.client.iframe_eval(
                PASSPORT_IFRAME,
                """
                (function() {
                    var tabs = document.querySelectorAll('.login-tabs-tab');
                    if (tabs.length < 2) return 'not enough tabs';
                    if (tabs[1].classList.contains('active')) return 'already active';
                    tabs[1].click();
                    return 'clicked';
                })()
                """,
            )
            time.sleep(2)
            self.session.state = State.SMS_TAB_CLICKED
            self.session.touch()
            self._snapshot()

            # Verify SMS form is now active — look for SMS-related elements
            probe = self.client.iframe_eval_json(
                PASSPORT_IFRAME,
                """
                JSON.stringify({
                    activeTab: (document.querySelector('.login-tabs-tab.active')||{}).textContent,
                    inputs: Array.from(document.querySelectorAll('input')).map(i => ({
                        id: i.id, name: i.name, type: i.type,
                        placeholder: i.placeholder, visible: i.offsetParent !== null
                    })),
                    sendBtns: Array.from(document.querySelectorAll('button, a, span, div'))
                        .filter(b => (b.textContent||'').match(/获取验证码|发送验证码|Get.*code|Send.*code/i))
                        .map(b => ({
                            tag: b.tagName, text: (b.textContent||'').trim().substring(0,40),
                            id: b.id, className: b.className, disabled: b.disabled || false
                        }))
                })
                """,
            )
            log.info("SMS tab probe: %s", probe)
            self.session.message = (
                f"active_tab={probe.get('activeTab', '').strip()!r}, "
                f"inputs={len(probe.get('inputs', []))}, "
                f"send_btns={len(probe.get('sendBtns', []))}"
            )
        except SamVNCError as e:
            self._fail(f"SMS tab switch failed: {e}")
            return

        # Proceed to fill phone
        self.step_fill_phone()

    # ------------------------------------------------------------------
    # Step 3: fill phone number
    # ------------------------------------------------------------------
    def step_fill_phone(self) -> None:
        log.info("[%s] filling phone=%s", self.session.session_id, self.session.phone)
        try:
            # In SMS mode the phone field is #fm-sms-login-id (different
            # from password mode which uses #fm-login-id). Try SMS one first.
            filled = False
            for sel in ("#fm-sms-login-id", "#fm-login-id", 'input[name="fm-sms-login-id"]'):
                try:
                    self.client.iframe_set_value(PASSPORT_IFRAME, sel, self.session.phone)
                    log.info("phone filled via %s", sel)
                    filled = True
                    break
                except SamVNCError as e:
                    log.debug("selector %s failed: %s", sel, e)
            if not filled:
                self._fail("could not find phone input field")
                return
            time.sleep(1)
            # Check the agreement checkbox (must be checked to enable login)
            self.client.iframe_eval(
                PASSPORT_IFRAME,
                """
                (function() {
                    var cb = document.getElementById('fm-agreement-checkbox');
                    if (cb && !cb.checked) { cb.click(); return 'checked'; }
                    return cb && cb.checked ? 'already checked' : 'no checkbox';
                })()
                """,
            )
            time.sleep(1)
            self.session.state = State.PHONE_FILLED
            self.session.touch()
            self._snapshot()
        except SamVNCError as e:
            self._fail(f"phone fill failed: {e}")
            return

        # Click "Get code"
        self.step_request_sms()

    # ------------------------------------------------------------------
    # Step 4: click "Get code" button
    # ------------------------------------------------------------------
    def step_request_sms(self) -> None:
        log.info("[%s] requesting SMS code", self.session.session_id)
        try:
            # In SMS mode the "Get code" button is an <a class="send-btn-link">
            # wrapped in a <div class="send-btn">. Click the <a> first; if that
            # fails, try the wrapping div.
            result = self.client.iframe_eval(
                PASSPORT_IFRAME,
                """
                (function() {
                    // Primary: .send-btn-link (the actual <a> in Alibaba passport SMS form)
                    var link = document.querySelector('.send-btn-link');
                    if (link) { link.click(); return 'clicked .send-btn-link'; }
                    // Fallback 1: .send-btn div
                    var div = document.querySelector('.send-btn');
                    if (div) { div.click(); return 'clicked .send-btn'; }
                    // Fallback 2: any element with class containing 'send'
                    var sels = ['button[class*="send"]', 'a[class*="send"]', '#sendCodeBtn', '.sendCodeBtn'];
                    for (var i = 0; i < sels.length; i++) {
                        var el = document.querySelector(sels[i]);
                        if (el) { el.click(); return 'clicked ' + sels[i]; }
                    }
                    // Fallback 3: text match
                    var all = Array.from(document.querySelectorAll('button, a, span, div'));
                    for (var j = 0; j < all.length; j++) {
                        var t = (all[j].textContent || '').trim();
                        if ((t === 'Get code' || t === '获取验证码' || t === 'Send code')
                            && all[j].offsetParent !== null) {
                            all[j].click();
                            return 'clicked text: ' + t;
                        }
                    }
                    return 'send button not found';
                })()
                """,
            )
            log.info("send-code click result: %s", result)
            self.session.state = State.SMS_REQUESTED
            self.session.message = f"send_code_click: {result}"
            self.session.touch()
            time.sleep(3)
            self._snapshot()
        except SamVNCError as e:
            self._fail(f"send code click failed: {e}")
            return

        # Now check whether the SMS was actually sent, or whether a CAPTCHA
        # / slider / image-captcha appeared.
        self._check_post_sms_request()

    # ------------------------------------------------------------------
    # Step 4b: inspect the page after clicking "Get code"
    # ------------------------------------------------------------------
    def _check_post_sms_request(self) -> None:
        """Detect CAPTCHA / slider / error / success after requesting SMS."""
        try:
            probe = self.client.iframe_eval_json(
                PASSPORT_IFRAME,
                """
                JSON.stringify({
                    // slider (NoCaptcha) — In SMS mode it's nc_2_wrapper, not nc_1
                    sliderVisible: (function() {
                        for (var idx of ['1', '2']) {
                            var w = document.getElementById('nc_' + idx + '_wrapper');
                            if (w) {
                                var s = getComputedStyle(w);
                                if (s.display !== 'none' && s.visibility !== 'hidden' && w.offsetParent !== null) {
                                    return true;
                                }
                            }
                        }
                        return false;
                    })(),
                    sliderText: (document.querySelector('#nc_1__scale_text span, #nc_2__scale_text span')||{}).textContent || '',
                    // image captcha
                    imgCaptchaVisible: (function() {
                        for (var idx of ['1', '2']) {
                            var i = document.getElementById('nc_' + idx + '_imgCaptcha');
                            if (i) {
                                var s = getComputedStyle(i);
                                if (s.display !== 'none' && i.offsetParent !== null) return true;
                            }
                        }
                        return false;
                    })(),
                    // baxia
                    baxiaVisible: (function() {
                        var b = document.getElementById('baxia-password')
                              || document.getElementById('baxia-sms')
                              || document.querySelector('[id^=baxia-]');
                        if (!b) return false;
                        return b.children.length > 0 && b.offsetParent !== null;
                    })(),
                    // error message — only count if #login-error is visible
                    errorBoxVisible: (function() {
                        var eb = document.getElementById('login-error');
                        if (!eb) return false;
                        var s = getComputedStyle(eb);
                        return s.display !== 'none' && s.visibility !== 'hidden' && eb.offsetParent !== null;
                    })(),
                    errorMsg: (function() {
                        var eb = document.getElementById('login-error');
                        if (!eb) return '';
                        var s = getComputedStyle(eb);
                        if (s.display === 'none' || s.visibility === 'hidden') return '';
                        var msg = eb.querySelector('.login-error-msg');
                        return (msg && msg.textContent || '').trim();
                    })(),
                    // send-code button text (becomes "X seconds" while counting down)
                    sendBtnText: (function() {
                        var link = document.querySelector('.send-btn-link');
                        if (link) return (link.textContent || '').trim().substring(0, 40);
                        var div = document.querySelector('.send-btn');
                        if (div) return (div.textContent || '').trim().substring(0, 40);
                        return '';
                    })(),
                    sendBtnDisabled: (function() {
                        var link = document.querySelector('.send-btn-link');
                        if (!link) return false;
                        // During countdown the link gets a class like 'disabled' or 'counting'
                        return /disabled|counting|count-/.test(link.className)
                            || link.style.pointerEvents === 'none';
                    })(),
                    // any visible input that looks like an SMS code field
                    smsInput: (function() {
                        var inputs = Array.from(document.querySelectorAll('input'));
                        for (var i = 0; i < inputs.length; i++) {
                            var inp = inputs[i];
                            if (!inp.offsetParent) continue;
                            var p = (inp.placeholder||'') + ' ' + (inp.name||'') + ' ' + (inp.id||'');
                            if (p.match(/code|sms|验证码|短信/i) && inp.type !== 'hidden') {
                                return {id: inp.id, name: inp.name, placeholder: inp.placeholder};
                            }
                        }
                        return null;
                    })()
                })
                """,
            )
            log.info("post-send probe: %s", probe)
            self.session.message = (
                f"slider={probe.get('sliderVisible')}, "
                f"imgCaptcha={probe.get('imgCaptchaVisible')}, "
                f"baxia={probe.get('baxiaVisible')}, "
                f"errorBoxVisible={probe.get('errorBoxVisible')}, "
                f"errorMsg={probe.get('errorMsg')!r}, "
                f"sendBtnText={probe.get('sendBtnText')!r}, "
                f"sendBtnDisabled={probe.get('sendBtnDisabled')}, "
                f"smsInput={probe.get('smsInput')}"
            )
        except SamVNCError as e:
            self._fail(f"post-SMS probe failed: {e}")
            return

        # CAPTCHA detection
        if (
            probe.get("sliderVisible")
            or probe.get("imgCaptchaVisible")
            or probe.get("baxiaVisible")
        ):
            self._require_manual(
                "CAPTCHA/slider detected. Please open the VNC URL in a browser, "
                "complete the verification, then call /login/continue.",
                resume_next="wait_for_sms_input",
            )
            return

        # Visible error message (e.g. "phone number format invalid")
        if probe.get("errorBoxVisible") and probe.get("errorMsg"):
            self._fail(f"login form error: {probe['errorMsg']}")
            return

        # If send button shows countdown (e.g. "60s", "60 秒", "Resend(60s)"),
        # or button is marked disabled/counting, SMS was sent.
        send_btn = (probe.get("sendBtnText") or "").strip()
        if probe.get("sendBtnDisabled") or (
            send_btn and any(c.isdigit() for c in send_btn)
            and not send_btn.lower().startswith("get code")
        ):
            self.session.state = State.SMS_SENT
            self.session.message = (
                f"SMS sent (button shows: {send_btn!r}, disabled={probe.get('sendBtnDisabled')})"
            )
            self.session.touch()
            log.info("SMS sent OK: %s", self.session.message)
            return

        # No CAPTCHA, no visible error, no countdown — ambiguous. Could be:
        #   - SMS actually sent but button text didn't update (rare)
        #   - Phone number rejected silently (most likely for invalid numbers)
        #   - Send button click didn't take effect
        # Safer to ask user to verify.
        self._require_manual(
            "Could not confirm SMS was sent (no CAPTCHA, no error, no countdown "
            "on Get-code button). Possible causes: invalid phone number, silent "
            "rejection by Alibaba passport, or click did not register. "
            "Please open the VNC URL, verify the state, and either: "
            "(a) manually click Get code if needed, then call /login/continue; "
            "or (b) if you received the SMS, just call /login/sms with the code.",
            resume_next="wait_for_sms_input",
        )

    # ------------------------------------------------------------------
    # Step 5: submit SMS code (called from /login/sms endpoint)
    # ------------------------------------------------------------------
    def step_submit_sms(self, code: str) -> None:
        if self.session.state not in (State.SMS_SENT, State.MANUAL):
            self._fail(
                f"cannot submit SMS in state {self.session.state.value}; "
                f"expected sms_sent or manual_action_required"
            )
            return

        log.info("[%s] submitting SMS code=%s", self.session.session_id, code)
        self.session.sms_code = code

        # Probe for SMS code input (priority: SMS-mode selectors first).
        try:
            result = self.client.iframe_eval(
                PASSPORT_IFRAME,
                """
                (function() {
                    var selectors = ['#fm-smscode', 'input[name=\"fm-smscode\"]',
                                      '#fm-login-code', 'input[name=\"fm-login-code\"]',
                                      '#smsCode', '#code', 'input[name=\"code\"]'];
                    var el = null;
                    for (var i = 0; i < selectors.length; i++) {
                        el = document.querySelector(selectors[i]);
                        if (el && el.offsetParent) break;
                        el = null;
                    }
                    if (!el) {
                        var inputs = Array.from(document.querySelectorAll('input'));
                        for (var j = 0; j < inputs.length; j++) {
                            if (!inputs[j].offsetParent) continue;
                            var p = (inputs[j].placeholder||'') + ' ' + (inputs[j].name||'') + ' ' + (inputs[j].id||'');
                            if (p.match(/code|sms|验证码|短信/i) && inputs[j].type !== 'hidden') {
                                el = inputs[j]; break;
                            }
                        }
                    }
                    if (!el) return 'sms input not found';
                    return 'found: id=' + el.id + ' name=' + el.name;
                })()
                """,
            )
            log.info("sms input probe: %s", result)
        except SamVNCError as e:
            log.warning("sms input probe failed: %s", e)

        # Set the SMS code value via iframe_set_value (handles React/Vue/jQuery events).
        code_set = False
        for sel in [
            "#fm-smscode",
            'input[name="fm-smscode"]',
            "#fm-login-code",
            'input[name="fm-login-code"]',
            "#smsCode",
            "#code",
            'input[name="code"]',
        ]:
            try:
                self.client.iframe_set_value(PASSPORT_IFRAME, sel, code)
                log.info("sms code set via %s", sel)
                code_set = True
                break
            except SamVNCError as e:
                log.debug("selector %s failed: %s", sel, e)
                continue

        if not code_set:
            self._fail("could not find SMS code input field")
            return

        try:
            time.sleep(1)

            # Click "Log in" — in SMS mode it's .fm-button.fm-submit.sms-login
            click_result = self.client.iframe_eval(
                PASSPORT_IFRAME,
                """
                (function() {
                    var btn = document.querySelector('.fm-button.fm-submit.sms-login')
                           || document.querySelector('.fm-button.fm-submit')
                           || document.querySelector('button[type=submit]');
                    if (!btn) return 'login button not found';
                    if (btn.classList.contains('fm-button-disabled')) {
                        return 'login button is disabled';
                    }
                    btn.click();
                    return 'clicked';
                })()
                """,
            )
            log.info("login button click: %s", click_result)
            if "disabled" in str(click_result).lower():
                self._fail(f"login button is disabled after SMS code: {click_result}")
                return
            self.session.state = State.SMS_SUBMITTED
            self.session.touch()
            time.sleep(5)
            self._snapshot()
        except SamVNCError as e:
            self._fail(f"SMS submit failed: {e}")
            return

        # Now verify
        self.step_verify()

    # ------------------------------------------------------------------
    # Step 6: verify login success
    # ------------------------------------------------------------------
    def step_verify(self) -> None:
        log.info("[%s] verifying login", self.session.session_id)
        self.session.state = State.VERIFYING
        self.session.touch()

        # The browser may have already redirected; check current URL.
        try:
            top_info = self.client.eval_json(
                "JSON.stringify({url: location.href, title: document.title})"
            )
            log.info("top info after submit: %s", top_info)
        except SamVNCError as e:
            self._fail(f"verify probe failed: {e}")
            return

        url = (top_info or {}).get("url", "")
        if "/login" in url:
            # Still on login page — maybe wrong code, or CAPTCHA appeared
            self._snapshot()
            self._require_manual(
                "After SMS submit, still on /login. Possible wrong code or "
                "post-login CAPTCHA. Check the screenshot and VNC URL, complete "
                "manually if needed, then call /login/continue.",
                resume_next="verify",
            )
            return

        # Navigate to /my/overview to confirm logged in
        try:
            self.client.navigate(OVERVIEW_URL, wait=0)
            time.sleep(6)
        except SamVNCError as e:
            self._fail(f"navigate to overview failed: {e}")
            return

        try:
            top_info = self.client.eval_json(
                "JSON.stringify({url: location.href, title: document.title})"
            )
            url2 = (top_info or {}).get("url", "")
            log.info("after overview: %s", top_info)
            if "/login" in url2.lower() or "passport" in url2.lower():
                self._fail(f"redirected to login page: {url2}")
                return

            # Looks logged in
            self.session.state = State.LOGGED_IN
            self.session.message = f"logged in, url={url2}, title={top_info.get('title')}"
            self.session.touch()
            self._snapshot()
            log.info("LOGIN OK: %s", self.session.message)
        except SamVNCError as e:
            self._fail(f"overview probe failed: {e}")

    # ------------------------------------------------------------------
    # Step "manual action done" — resume from MANUAL state
    # ------------------------------------------------------------------
    def step_continue_after_manual(self) -> None:
        if self.session.state != State.MANUAL:
            self.session.message = (
                f"continue called in state {self.session.state.value}; nothing to do"
            )
            return

        resume = self._resume_after_manual or "verify"
        log.info("[%s] resuming after manual, next=%s", self.session.session_id, resume)
        self._snapshot()

        if resume == "wait_for_sms_input":
            # User completed CAPTCHA. Re-check post-SMS state.
            self._check_post_sms_request()
        elif resume == "verify":
            self.step_verify()
        else:
            self._fail(f"unknown resume step: {resume}")

    # ------------------------------------------------------------------
    # Helpers
    # ------------------------------------------------------------------
    def _snapshot(self) -> None:
        try:
            self.session.last_screenshot_b64 = self.client.screenshot_base64()
        except Exception as e:
            log.warning("snapshot failed: %s", e)

    def _fail(self, msg: str) -> None:
        log.error("[%s] FAIL: %s", self.session.session_id, msg)
        self.session.state = State.FAILED
        self.session.error = msg
        self.session.touch()
        self._snapshot()

    def _require_manual(self, msg: str, resume_next: str = "verify") -> None:
        log.warning("[%s] MANUAL: %s", self.session.session_id, msg)
        self.session.state = State.MANUAL
        self.session.message = msg
        self.session.vnc_url = self.vnc_url
        self._resume_after_manual = resume_next
        self.session.touch()
        self._snapshot()
