// SamVNC browser client.
//
// Two transports are supported:
//   - WebSocket at /ws (best latency; used for local / direct connections).
//   - Single-connection long polling at /poll (used behind aitun.cc free
//     tunnels, which permit only ONE concurrent HTTP connection). The browser
//     POSTs its accumulated input batch plus the last frame seq it has; the
//     server processes the inputs, waits briefly for a fresher frame, and
//     returns the newest frame + state. The browser immediately re-POSTs, so
//     input and frames never need a second concurrent connection.
//
// Both transports carry the same SamVNC protocol: JSON control messages and
// 24-byte-header binary screen frames.
(function () {
  "use strict";

  // ---- Protocol constants (mirror internal/protocol/protocol.go) -----------
  const FRAME_MAGIC = 0x5356;
  const BIN_HEADER_LEN = 24;
  const FLAG_FULL_FRAME = 1;

  const canvas = document.getElementById("screen");
  const ctx = canvas.getContext("2d", { alpha: false });
  const statusEl = document.getElementById("status");
  const statsEl = document.getElementById("stats");
  const overlay = document.getElementById("overlay");
  const overlayTitle = document.getElementById("overlayTitle");
  const overlayMsg = document.getElementById("overlayMsg");
  const reconnectBtn = document.getElementById("reconnect");
  const qSel = document.getElementById("quality");
  const modeSel = document.getElementById("mode");
  // The narrow-screen selects live in the menu panel; the wide-screen ones
  // sit inline in the topbar. Mirror their values across both.
  const qSelInline = document.getElementById("qualityInline");
  const modeSelInline = document.getElementById("modeInline");
  const touchModeSel = document.getElementById("touchMode");
  const fsBtn = document.getElementById("fsBtn");
  const menuBtn = document.getElementById("menuBtn");
  const menuPanel = document.getElementById("menuPanel");
  const clipBar = document.getElementById("clipboardBar");
  const clipText = document.getElementById("clipText");
  const clipSend = document.getElementById("clipSend");
  const clipBtn = document.getElementById("clipBtn");
  const mobileBar = document.getElementById("mobileBar");
  const kbBtn = document.getElementById("kbBtn");
  const fnBtn = document.getElementById("fnBtn");
  const mouseModeBtn = document.getElementById("mouseModeBtn");
  const zoomBtn = document.getElementById("zoomBtn");
  const resetZoom = document.getElementById("resetZoom");
  const kbInput = document.getElementById("kbInput");
  const fnPanel = document.getElementById("fnPanel");
  const fnMods = document.getElementById("fnMods");
  const mousePanel = document.getElementById("mousePanel");
  const stage = document.getElementById("stage");
  const cursor = document.getElementById("cursor");
  const zoomBadge = document.getElementById("zoomBadge");

  let desktopW = 0, desktopH = 0;
  let mouseMask = 0;
  let currentBlobUrl = null;
  let frameCount = 0;
  let fpsTimer = null;
  let fpsCount = 0;

  // Transport state.
  let ws = null;            // WebSocket mode
  let usePoll = false;      // selected transport
  let pollInFlight = false;
  let pollStopped = false;
  let lastSeq = 0;
  let inputQueue = [];      // messages waiting for the next /poll batch
  let lastMouseTs = 0;      // mouse-move throttle

  // Path prefix preserving the aitun.cc tunnel code (e.g. https://aitun.cc/<code>).
  function prefix() {
    let p = location.pathname.replace(/\/+$/, "");
    if (p === "") p = "";
    return p;
  }

  function usePollMode() {
    // aitun.cc free tunnels permit only one concurrent connection, so use the
    // single-connection /poll transport there.
    return location.hostname === "aitun.cc" ||
      location.hostname.endsWith(".aitun.cc");
  }

  function connect() {
    setStatus("connecting…");
    hideOverlay();
    if (usePollMode()) {
      usePoll = true;
      connectPoll();
    } else {
      usePoll = false;
      connectWS();
    }
  }

  // ---- WebSocket transport -------------------------------------------------
  function wsUrl() {
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    return proto + "//" + location.host + prefix() + "/ws";
  }

  function connectWS() {
    ws = new WebSocket(wsUrl());
    ws.binaryType = "arraybuffer";
    ws.onopen = function () {
      setStatus("authorizing…");
      sendHello();
      startFps();
    };
    ws.onmessage = onWSMessage;
    ws.onclose = function () {
      setStatus("disconnected");
      showOverlay("Disconnected", "The remote session ended.");
      stopFps();
    };
    ws.onerror = function () { setStatus("connection error"); };
  }

  function onWSMessage(ev) {
    if (typeof ev.data === "string") {
      handleText(JSON.parse(ev.data));
      return;
    }
    // Binary frame: 24-byte header + image payload.
    const buf = new Uint8Array(ev.data);
    parseFrame(buf);
    fpsCount++;
  }

  // ---- Long-poll transport -------------------------------------------------
  function connectPoll() {
    pollStopped = false;
    lastSeq = 0;
    inputQueue = [];
    startFps();
    // Kick off the poll loop. Each round POSTs the queued inputs and the last
    // seq, then immediately re-posts when the response arrives.
    sendHello();
    pollLoop();
  }

  async function pollLoop() {
    if (pollStopped) return;
    if (pollInFlight) return;
    pollInFlight = true;
    // Snapshot + clear the input queue so new events buffer for the next round.
    const batch = inputQueue;
    inputQueue = [];
    let resp;
    try {
      // If a paste is pending, ask the server to echo the remote clipboard
      // selection in this round so we can sync the local clipboard before
      // injecting Ctrl+V.
      const body = { seq: lastSeq, inputs: batch };
      if (pendingPaste) {
        body.clip_req = pendingPasteSel;
      }
      const r = await fetch(prefix() + "/poll", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
        keepalive: true,
        cache: "no-store",
      });
      if (!r.ok) {
        setStatus("poll error " + r.status);
        pollInFlight = false;
        if (pollStopped) return;
        setTimeout(pollLoop, 500);
        return;
      }
      resp = await r.json();
    } catch (e) {
      setStatus("network error, retrying…");
      pollInFlight = false;
      if (pollStopped) return;
      setTimeout(pollLoop, 800);
      return;
    }

    if (resp.ack) handleText(resp.ack);
    if (resp.frame) {
      const bin = base64ToBytes(resp.frame);
      if (bin) { parseFrame(bin); fpsCount++; }
    }
    // Clipboard sync. If a paste was pending, this round's clip is the
    // requested selection — write it local and inject Ctrl+V. Otherwise it
    // is a steady-state CLIPBOARD push from the remote.
    if (resp.clip !== undefined && resp.clip !== null) {
      if (pendingPaste && (!resp.clip_sel || resp.clip_sel === pendingPasteSel)) {
        pendingPaste = false;
        writeLocalAndPaste(resp.clip);
      } else {
        handleText({ type: "clipboard", text: resp.clip, selection: resp.clip_sel || "clipboard" });
      }
    }
    if (resp.clip_prime !== undefined && resp.clip_prime !== null && resp.clip_prime !== "") {
      handleText({ type: "clipboard", text: resp.clip_prime, selection: "primary" });
    }
    if (resp.stats) handleText(resp.stats);
    if (resp.error) { showOverlay("Session error", resp.error); }
    if (typeof resp.seq === "number") lastSeq = resp.seq;

    pollInFlight = false;
    if (pollStopped) return;
    // Immediately start the next round; inputs queued meanwhile ride along.
    setTimeout(pollLoop, 0);
  }

  function base64ToBytes(b64) {
    try {
      const bin = atob(b64);
      const u = new Uint8Array(bin.length);
      for (let i = 0; i < bin.length; i++) u[i] = bin.charCodeAt(i);
      return u;
    } catch (e) { return null; }
  }

  // ---- Shared frame parser -------------------------------------------------
  function parseFrame(buf) {
    if (buf.length < BIN_HEADER_LEN) return;
    const dv = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
    const magic = dv.getUint16(0, true);
    if (magic !== FRAME_MAGIC) return;
    const format = dv.getUint8(3);
    const flags = dv.getUint8(17);
    const payloadLen = dv.getUint32(18, true);
    const payload = buf.slice(BIN_HEADER_LEN, BIN_HEADER_LEN + payloadLen);
    drawFrame(format, payload, flags);
  }

  // ---- Output: send to server ----------------------------------------------
  function sendJSON(obj) {
    if (usePoll) {
      // Throttle high-frequency mouse moves to ~60/s to keep batches small.
      if (obj.type === "mouse") {
        const now = Date.now();
        if (now - lastMouseTs < 16) return;
        lastMouseTs = now;
      }
      inputQueue.push(obj);
      return;
    }
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify(obj));
    }
  }

  function sendHello() {
    sendJSON({
      type: "hello",
      version: 1,
      quality: parseInt(qSel.value, 10),
      width: 0,
      height: 0,
      mode: parseInt(modeSel.value, 10),
      fps: 0,
      token: getParam("token") || "",
    });
  }

  // ---- Message handling ----------------------------------------------------
  function handleText(msg) {
    switch (msg.type) {
      case "hello_ack":
        desktopW = msg.width;
        desktopH = msg.height;
        canvas.width = desktopW;
        canvas.height = desktopH;
        setStatus(
          "live  " + desktopW + "x" + desktopH + "  " + msg.server +
          (msg.p2p ? "  P2P" : "  relay")
        );
        if (msg.relay_url) {
          console.log("SamVNC public URL:", msg.relay_url);
        }
        clipBar.classList.remove("hidden");
        break;
      case "clipboard":
        // WS mode: a clipboard event may be the reply to a paste request.
        // Poll mode handles paste replies inline in pollLoop, so this only
        // fires for WS-mode clipboard_request responses and server pushes.
        if (!usePoll && pendingPaste && (msg.selection || "clipboard") === pendingPasteSel) {
          pendingPaste = false;
          writeLocalAndPaste(msg.text);
          break;
        }
        if (msg.selection === "primary") {
          // PRIMARY (mouse-select) has no local equivalent; just mirror it
          // into the clipboard textarea so the user can see/copy it.
          if (clipText.value !== msg.text) clipText.value = msg.text;
        } else {
          // CLIPBOARD: mirror to the local system clipboard for seamless
          // local paste.
          if (clipText.value !== msg.text) {
            clipText.value = msg.text;
            if (navigator.clipboard) {
              navigator.clipboard.writeText(msg.text).catch(function () {});
            }
          }
        }
        break;
      case "stats":
        statsEl.textContent =
          "fps " + msg.fps.toFixed(0) + (msg.p2p ? "  P2P" : "  relay");
        break;
      case "pong":
        break;
      case "error":
        showOverlay("Session error", msg.reason);
        break;
    }
  }

  // ---- Rendering -----------------------------------------------------------
  function drawFrame(format, payload, flags) {
    const mime = format === 2 ? "image/png" : "image/jpeg";
    const blob = new Blob([payload], { type: mime });
    if (currentBlobUrl) URL.revokeObjectURL(currentBlobUrl);
    currentBlobUrl = URL.createObjectURL(blob);
    const img = new Image();
    img.onload = function () {
      ctx.drawImage(img, 0, 0, canvas.width, canvas.height);
      URL.revokeObjectURL(currentBlobUrl);
      currentBlobUrl = null;
    };
    img.onerror = function () {
      if (currentBlobUrl) { URL.revokeObjectURL(currentBlobUrl); currentBlobUrl = null; }
    };
    img.src = currentBlobUrl;
  }

  // ---- Input: keyboard -----------------------------------------------------
  // Listen on window (not canvas) so keys work without first clicking the
  // canvas, and so arrows/Ctrl+C are captured even when focus drifts. Skip
  // form fields so the clipboard textarea stays usable.
  //
  // Ctrl/Cmd+C and Ctrl/Cmd+V are intercepted for seamless clipboard sync:
  //   - Copy: read the LOCAL clipboard and push it to the remote CLIPBOARD so
  //     a subsequent paste in the remote desktop yields the local content.
  //   - Paste: pull the REMOTE clipboard, write it to the LOCAL clipboard,
  //     then inject Ctrl+V into the remote so the remote app pastes its own
  //     (just-synced) CLIPBOARD. This avoids shipping the local clipboard to
  //     the remote and back.
  function isFormField(el) {
    const tag = (el && el.tagName || "").toLowerCase();
    return tag === "input" || tag === "textarea" || tag === "select";
  }

  function isCopyKey(e) {
    return (e.ctrlKey || e.metaKey) && (e.key === "c" || e.key === "C") &&
           !e.altKey && !e.shiftKey;
  }
  function isPasteKey(e) {
    return (e.ctrlKey || e.metaKey) && (e.key === "v" || e.key === "V") &&
           !e.altKey && !e.shiftKey;
  }
  function isCutKey(e) {
    return (e.ctrlKey || e.metaKey) && (e.key === "x" || e.key === "X") &&
           !e.altKey && !e.shiftKey;
  }

  // Push local clipboard -> remote. Used on copy/cut intercept.
  function pushLocalClipboardToRemote(sel) {
    if (!navigator.clipboard) return;
    navigator.clipboard.readText().then(function (t) {
      if (t !== null && t !== undefined) {
        sendJSON({ type: "clipboard", text: t, selection: sel || "clipboard" });
      }
    }).catch(function () {});
  }

  // Pull remote clipboard -> local, then inject Ctrl+V into the remote.
  // Poll mode: set pendingPaste so the next /poll carries clip_req; when the
  // response arrives, writeLocalAndPaste() is called. WS mode: send a
  // clipboard_request; the clipboard event handler calls writeLocalAndPaste().
  let pendingPaste = false;
  let pendingPasteSel = "clipboard";

  function requestRemotePaste(sel) {
    pendingPaste = true;
    pendingPasteSel = sel || "clipboard";
    if (!usePoll) {
      // WS mode: ask the server for the selection; reply arrives as a
      // clipboard event handled in handleText().
      sendJSON({ type: "clipboard_request", selection: pendingPasteSel });
    }
    // Poll mode: the next pollLoop iteration will include clip_req.
  }

  function writeLocalAndPaste(text) {
    // Write remote content to the local clipboard so a quick local paste
    // also works, then inject the text directly into the remote desktop
    // via `type` (xdotool type on the server). This works for ALL apps
    // (xterm, xfce4-terminal, browsers, editors) regardless of whether
    // they bind Ctrl+V to paste. The previous approach of injecting
    // Ctrl+V failed because many X apps (xterm especially) don't bind
    // Ctrl+V to paste by default.
    const done = function () {
      if (text) {
        sendJSON({ type: "text", text: text });
      }
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text || "").then(done).catch(done);
    } else {
      done();
    }
  }

  window.addEventListener("keydown", function (e) {
    if (isFormField(e.target)) return;
    // Intercept copy/cut/paste for seamless clipboard sync.
    if (isCopyKey(e) || isCutKey(e)) {
      e.preventDefault();
      if (e.repeat) return;
      pushLocalClipboardToRemote("clipboard");
      // For cut, also send Ctrl+X to the remote so the remote app cuts its
      // selection (the clipboard push above keeps the remote CLIPBOARD in
      // sync with what the user just copied locally).
      if (isCutKey(e)) {
        sendJSON({ type: "key", key: "Control", code: "ControlLeft", down: true,
                   repeat: false, ctrl: true, alt: false, shift: false, meta: false });
        sendJSON({ type: "key", key: "x", code: "KeyX", down: true,
                   repeat: false, ctrl: true, alt: false, shift: false, meta: false });
        sendJSON({ type: "key", key: "x", code: "KeyX", down: false,
                   repeat: false, ctrl: true, alt: false, shift: false, meta: false });
        sendJSON({ type: "key", key: "Control", code: "ControlLeft", down: false,
                   repeat: false, ctrl: false, alt: false, shift: false, meta: false });
      }
      return;
    }
    if (isPasteKey(e)) {
      e.preventDefault();
      if (e.repeat) return;
      requestRemotePaste("clipboard");
      return;
    }
    e.preventDefault();
    // Drop OS auto-repeat keydowns: xdotool keydown sends an XKeyPress each
    // call, and X11 treats every XKeyPress as a typed character, so letting
    // browser repeats through produces "sssssss" bursts. Only the initial
    // press + the final keyup are injected.
    if (e.repeat) return;
    sendJSON({
      type: "key", key: e.key, code: e.code, down: true, repeat: false,
      ctrl: e.ctrlKey, alt: e.altKey, shift: e.shiftKey, meta: e.metaKey,
    });
  });
  window.addEventListener("keyup", function (e) {
    if (isFormField(e.target)) return;
    // Don't forward keyup for copy/cut/paste keys we already intercepted on
    // keydown: the remote Ctrl state was managed inside the intercept and a
    // stray keyup would release a modifier that was never pressed remotely.
    if (isCopyKey(e) || isPasteKey(e) || isCutKey(e)) {
      e.preventDefault();
      return;
    }
    e.preventDefault();
    sendJSON({
      type: "key", key: e.key, code: e.code, down: false, repeat: false,
      ctrl: e.ctrlKey, alt: e.altKey, shift: e.shiftKey, meta: e.metaKey,
    });
  });

  // ---- Input: mouse + touch (unified gesture engine) ----------------------
  //
  // Touch gestures (when the canvas receives a touch):
  //   1 finger tap               -> left click
  //   1 finger double-tap        -> left double-click
  //   1 finger long-press (350ms)-> right click
  //   1 finger drag              -> mouse move with left button held
  //   2 finger tap               -> right click
  //   2 finger drag (vertical)   -> scroll wheel
  //   2 finger pinch            -> zoom the local canvas view (not remote)
  //   3 finger tap              -> middle click
  //
  // In trackpad mode the canvas acts like a laptop touchpad: finger move
  // moves the (visible) cursor by a relative amount, taps click. Useful when
  // the remote desktop is high-resolution and direct-touch targets are tiny.
  //
  // Mouse events are forwarded unchanged on devices that have a real mouse.
  const BTN = { 0: 1, 1: 2, 2: 4, 3: 8, 4: 16 }; // JS button -> X11 button mask

  // Active mouse-button override from the on-screen chooser (mobileBar).
  // Default 1 = left. When the user taps Right in the chooser, the next
  // single-tap is a right click and the chooser reverts to Left.
  let activeBtn = 1;        // 1=left, 2=middle, 4=right
  let activeBtnSticky = false; // if true, keep the chosen button for every tap
  let dragLock = false;     // hold left button down between taps
  let scrollMode = false;   // any 1-finger drag scrolls instead of moving

  // Pinch-zoom state.
  let viewScale = 1.0;
  let viewScaleMin = 0.5, viewScaleMax = 4.0;

  // Virtual cursor: a small dot we draw on the canvas at the last-known
  // mouse position. The remote X cursor isn't captured by GetImage, so on
  // touch devices (where there's no local mouse pointer) we draw our own
  // so the user can see where their taps are landing.
  let lastMouseX = 0, lastMouseY = 0;
  let cursorVisible = false; // true once we've seen a mouse/touch event
  let cursorHideTimer = null;

  function updateVirtualCursor(x, y) {
    lastMouseX = x; lastMouseY = y;
    cursorVisible = true;
    // Position the cursor div (which is in the stage coordinate space).
    const rect = canvas.getBoundingClientRect();
    const stageRect = stage.getBoundingClientRect();
    const cx = rect.left - stageRect.left + (x / canvas.width) * rect.width;
    const cy = rect.top - stageRect.top + (y / canvas.height) * rect.height;
    cursor.style.left = cx + "px";
    cursor.style.top = cy + "px";

    const isTouch = window.matchMedia("(pointer: coarse)").matches;
    if (touchModeSel.value === "trackpad") {
      // Persistent cursor in trackpad mode.
      cursor.classList.remove("hidden");
    } else if (isTouch) {
      // In direct-touch mode, briefly flash the cursor (300ms) after each
      // tap/click so the user gets positional feedback without the cursor
      // lingering under their finger.
      cursor.classList.remove("hidden");
      if (cursorHideTimer) clearTimeout(cursorHideTimer);
      cursorHideTimer = setTimeout(function () {
        cursor.classList.add("hidden");
      }, 350);
    } else {
      // Real mouse: hide our cursor, the OS cursor is enough.
      cursor.classList.add("hidden");
    }
  }

  function desktopCoords(clientX, clientY) {
    const rect = canvas.getBoundingClientRect();
    // Use the *untransformed* canvas size to map to desktop pixels. The CSS
    // transform (pinch-zoom) only affects the visual size, not the underlying
    // coordinate space.
    const sx = (clientX - rect.left) / rect.width * canvas.width;
    const sy = (clientY - rect.top) / rect.height * canvas.height;
    return { x: Math.round(sx), y: Math.round(sy) };
  }

  function sendMouse(x, y, mask, wheel) {
    sendJSON({ type: "mouse", x: x, y: y, mask: mask, wheel: wheel || 0 });
    // Update the virtual cursor position so the user sees where their
    // input is landing (helpful on touch devices).
    if (x !== 0 || y !== 0) updateVirtualCursor(x, y);
  }

  function applyViewScale() {
    canvas.style.transform = "scale(" + viewScale + ")";
    if (viewScale !== 1.0) {
      zoomBadge.classList.remove("hidden");
      zoomBadge.textContent = Math.round(viewScale * 100) + "%";
    } else {
      zoomBadge.classList.add("hidden");
    }
  }

  // ----- Mouse (real) -----
  // Track whether we've seen a non-synthetic mouse event recently. If a
  // touch just ended, the browser will fire a synthesized mousedown ~300ms
  // later; we want to swallow that to avoid double-clicking.
  let lastTouchEndTime = 0;

  canvas.addEventListener("mousedown", function (e) {
    if (Date.now() - lastTouchEndTime < 500) return; // swallow synthesized
    e.preventDefault();
    canvas.focus();
    mouseMask |= BTN[e.button] || 0;
    const c = desktopCoords(e.clientX, e.clientY);
    sendMouse(c.x, c.y, mouseMask, 0);
  });
  canvas.addEventListener("mouseup", function (e) {
    if (Date.now() - lastTouchEndTime < 500) return;
    e.preventDefault();
    mouseMask &= ~(BTN[e.button] || 0);
    const c = desktopCoords(e.clientX, e.clientY);
    sendMouse(c.x, c.y, mouseMask, 0);
  });
  canvas.addEventListener("mousemove", function (e) {
    if (Date.now() - lastTouchEndTime < 500) return;
    const c = desktopCoords(e.clientX, e.clientY);
    sendMouse(c.x, c.y, mouseMask, 0);
  });
  canvas.addEventListener("contextmenu", function (e) { e.preventDefault(); });
  canvas.addEventListener("wheel", function (e) {
    e.preventDefault();
    const c = desktopCoords(e.clientX, e.clientY);
    let ticks = Math.round(e.deltaY / 100);
    if (ticks === 0) ticks = e.deltaY > 0 ? 1 : -1;
    sendMouse(c.x, c.y, mouseMask, ticks);
  }, { passive: false });

  // ----- Touch gesture engine -----
  //
  // We track up to 3 simultaneous touches. The first one is the "primary"
  // (drives mouse position / drag). When a second finger lands we switch to
  // pinch-zoom + scroll mode and pause the primary's mouse events.
  const touches = new Map(); // identifier -> {x, y, startX, startY, startTime}
  let gestureMode = "none";  // none | tap | drag | scroll | pinch
  let longPressTimer = null;
  let lastTapTime = 0;
  let lastTapX = 0, lastTapY = 0;
  let pinchStartDist = 0;
  let pinchStartScale = 1.0;
  let twoFingerStartY = 0;

  // Trackpad-mode cursor position (in desktop coordinates).
  let trackpadX = 0, trackpadY = 0;
  const TRACKPAD_SPEED = 1.6; // multiplier for finger-move -> cursor-move

  function clearLongPress() {
    if (longPressTimer) {
      clearTimeout(longPressTimer);
      longPressTimer = null;
    }
  }

  function distance(t1, t2) {
    const dx = t1.x - t2.x, dy = t1.y - t2.y;
    return Math.sqrt(dx * dx + dy * dy);
  }

  function setCursor(x, y, pressed) {
    // Convert desktop coords back to stage pixel coords for the cursor div.
    const rect = canvas.getBoundingClientRect();
    const stageRect = stage.getBoundingClientRect();
    const cx = rect.left - stageRect.left + (x / canvas.width) * rect.width;
    const cy = rect.top - stageRect.top + (y / canvas.height) * rect.height;
    cursor.style.left = cx + "px";
    cursor.style.top = cy + "px";
    if (pressed) cursor.classList.add("pressed");
    else cursor.classList.remove("pressed");
  }

  function doClick(btn, x, y) {
    // Press + release a single mouse button at (x,y).
    mouseMask |= btn;
    sendMouse(x, y, mouseMask, 0);
    mouseMask &= ~btn;
    sendMouse(x, y, mouseMask, 0);
  }

  function doDoubleClick(btn, x, y) {
    doClick(btn, x, y);
    setTimeout(function () { doClick(btn, x, y); }, 60);
  }

  function startLongPressForRightClick(t) {
    clearLongPress();
    longPressTimer = setTimeout(function () {
      longPressTimer = null;
      // Long-press => right click at the finger position.
      const c = desktopCoords(t.x, t.y);
      doClick(4, c.x, c.y);
      // Cancel any in-progress drag by entering "consumed" state.
      gestureMode = "consumed";
    }, 380);
  }

  canvas.addEventListener("touchstart", function (e) {
    e.preventDefault();
    for (let i = 0; i < e.changedTouches.length; i++) {
      const t = e.changedTouches[i];
      touches.set(t.identifier, {
        x: t.clientX, y: t.clientY,
        startX: t.clientX, startY: t.clientY,
        startTime: Date.now(),
      });
    }

    if (touches.size === 1) {
      // Single finger. Decide tap vs drag later; arm long-press now.
      const t = touches.values().next().value;
      if (touchModeSel.value === "trackpad") {
        gestureMode = "trackpad";
        cursor.classList.remove("hidden");
        setCursor(trackpadX, trackpadY, false);
      } else {
        gestureMode = "tap-pending";
        // Move cursor immediately so the user sees where the click will land.
        const c = desktopCoords(t.x, t.y);
        sendMouse(c.x, c.y, mouseMask, 0);
        if (!dragLock) startLongPressForRightClick(t);
      }
    } else if (touches.size === 2) {
      // Two fingers. Cancel any pending long-press / tap.
      clearLongPress();
      const ts = Array.from(touches.values());
      pinchStartDist = distance(ts[0], ts[1]);
      pinchStartScale = viewScale;
      twoFingerStartY = (ts[0].y + ts[1].y) / 2;
      // If fingers stay roughly stationary, treat as a two-finger tap (right
      // click). If they move vertically, treat as scroll. If they converge/
      // diverge, treat as pinch-zoom.
      gestureMode = "two-finger-pending";
    } else if (touches.size === 3) {
      clearLongPress();
      gestureMode = "three-finger-pending";
    }
  }, { passive: false });

  canvas.addEventListener("touchmove", function (e) {
    e.preventDefault();
    for (let i = 0; i < e.changedTouches.length; i++) {
      const t = e.changedTouches[i];
      if (touches.has(t.identifier)) {
        const entry = touches.get(t.identifier);
        entry.x = t.clientX;
        entry.y = t.clientY;
      }
    }

    if (touches.size === 1) {
      const t = touches.values().next().value;
      const moved = Math.hypot(t.x - t.startX, t.y - t.startY);

      if (gestureMode === "trackpad") {
        // Relative movement.
        const last = t.lastX !== undefined ? t.lastX : t.startX;
        const lastY = t.lastY !== undefined ? t.lastY : t.startY;
        const dx = (t.x - last) * TRACKPAD_SPEED;
        const dy = (t.y - lastY) * TRACKPAD_SPEED;
        trackpadX = Math.max(0, Math.min(canvas.width, trackpadX + dx));
        trackpadY = Math.max(0, Math.min(canvas.height, trackpadY + dy));
        setCursor(trackpadX, trackpadY, mouseMask & 1);
        sendMouse(trackpadX, trackpadY, mouseMask, 0);
        t.lastX = t.x;
        t.lastY = t.y;
        return;
      }

      if (gestureMode === "tap-pending") {
        if (moved > 12) {
          // Promote to drag (or scroll mode).
          clearLongPress();
          if (scrollMode) {
            gestureMode = "scroll";
            twoFingerStartY = t.y;
          } else {
            gestureMode = "drag";
            // Press the active button if dragLock isn't already holding it.
            if (!(mouseMask & activeBtn)) {
              mouseMask |= activeBtn;
              const c = desktopCoords(t.x, t.y);
              sendMouse(c.x, c.y, mouseMask, 0);
            }
          }
        }
        return;
      }

      if (gestureMode === "drag") {
        const c = desktopCoords(t.x, t.y);
        sendMouse(c.x, c.y, mouseMask, 0);
        return;
      }

      if (gestureMode === "scroll") {
        const dy = t.y - twoFingerStartY;
        const ticks = Math.round(-dy / 20);
        if (ticks !== 0) {
          const c = desktopCoords(t.x, t.y);
          sendMouse(c.x, c.y, mouseMask, ticks);
          twoFingerStartY = t.y;
        }
        return;
      }
    } else if (touches.size === 2) {
      const ts = Array.from(touches.values());
      const curDist = distance(ts[0], ts[1]);

      if (gestureMode === "two-finger-pending") {
        // Classify: pinch if dist changed >15%, scroll if Y moved >15px.
        const pinchRatio = Math.abs(curDist - pinchStartDist) / pinchStartDist;
        const curY = (ts[0].y + ts[1].y) / 2;
        const yMoved = Math.abs(curY - twoFingerStartY);
        if (pinchRatio > 0.12) {
          gestureMode = "pinch";
        } else if (yMoved > 12) {
          gestureMode = "scroll";
          twoFingerStartY = curY;
        }
      }

      if (gestureMode === "pinch") {
        let newScale = pinchStartScale * (curDist / pinchStartDist);
        newScale = Math.max(viewScaleMin, Math.min(viewScaleMax, newScale));
        viewScale = newScale;
        applyViewScale();
      } else if (gestureMode === "scroll") {
        const curY = (ts[0].y + ts[1].y) / 2;
        const dy = curY - twoFingerStartY;
        const ticks = Math.round(-dy / 20);
        if (ticks !== 0) {
          const mid = desktopCoords((ts[0].x + ts[1].x) / 2,
                                     (ts[0].y + ts[1].y) / 2);
          sendMouse(mid.x, mid.y, mouseMask, ticks);
          twoFingerStartY = curY;
        }
      }
    }
  }, { passive: false });

  canvas.addEventListener("touchend", function (e) {
    e.preventDefault();
    lastTouchEndTime = Date.now();
    for (let i = 0; i < e.changedTouches.length; i++) {
      touches.delete(e.changedTouches[i].identifier);
    }
    clearLongPress();

    if (e.targetTouches.length === 0) {
      // All fingers lifted.
      if (gestureMode === "tap-pending") {
        const t = e.changedTouches[0];
        const c = desktopCoords(t.clientX, t.clientY);
        const now = Date.now();
        const isDouble = (now - lastTapTime < 320) &&
                         Math.hypot(c.x - lastTapX, c.y - lastTapY) < 20;
        if (activeBtn === 1 && !activeBtnSticky && dragLock) {
          // Drag-lock on: tap toggles button-down/up, no click.
          if (mouseMask & 1) { mouseMask &= ~1; }
          else { mouseMask |= 1; }
          sendMouse(c.x, c.y, mouseMask, 0);
        } else if (isDouble) {
          doDoubleClick(activeBtn, c.x, c.y);
          lastTapTime = 0;
        } else {
          doClick(activeBtn, c.x, c.y);
          lastTapTime = now;
          lastTapX = c.x; lastTapY = c.y;
        }
        // Reset non-sticky button choice after one tap.
        if (!activeBtnSticky && activeBtn !== 1) {
          activeBtn = 1;
          updateMousePanelButtons();
        }
      } else if (gestureMode === "drag") {
        // Release the button (unless drag-locked).
        if (!dragLock) {
          const t = e.changedTouches[0];
          const c = desktopCoords(t.clientX, t.clientY);
          mouseMask &= ~activeBtn;
          sendMouse(c.x, c.y, mouseMask, 0);
        }
      } else if (gestureMode === "two-finger-pending") {
        // Two-finger tap = right click at midpoint.
        const t = e.changedTouches[0];
        const c = desktopCoords(t.clientX, t.clientY);
        doClick(4, c.x, c.y);
      } else if (gestureMode === "three-finger-pending") {
        // Three-finger tap = middle click.
        const t = e.changedTouches[0];
        const c = desktopCoords(t.clientX, t.clientY);
        doClick(2, c.x, c.y);
      }
      gestureMode = "none";
      if (touchModeSel.value === "trackpad") {
        // Keep cursor visible in trackpad mode.
        setCursor(trackpadX, trackpadY, false);
      }
    } else if (e.targetTouches.length === 1 && gestureMode === "pinch") {
      // Pinch ended (one finger left). Fall back to single-finger drag/tap.
      const t = e.targetTouches[0];
      gestureMode = "tap-pending";
      const c = desktopCoords(t.clientX, t.clientY);
      sendMouse(c.x, c.y, mouseMask, 0);
    } else if (e.targetTouches.length === 1 && gestureMode === "scroll") {
      gestureMode = "tap-pending";
    }
  }, { passive: false });

  canvas.addEventListener("touchcancel", function (e) {
    e.preventDefault();
    touches.clear();
    clearLongPress();
    gestureMode = "none";
    // Release any held buttons.
    if (mouseMask) {
      mouseMask = 0;
      sendMouse(0, 0, 0, 0);
    }
  }, { passive: false });

  // ---- Clipboard (manual fallback) ----------------------------------------
  // Copy/paste is normally seamless via the Ctrl+C/V intercepts above. This
  // button stays as a manual fallback for environments where the Clipboard
  // API is denied (e.g. non-HTTPS, no user gesture).
  clipSend.addEventListener("click", function () {
    sendJSON({ type: "clipboard", text: clipText.value, selection: "clipboard" });
  });
  window.addEventListener("focus", function () {
    // On regaining focus, push the local clipboard to the remote so a copy
    // done while the tab was unfocused is not lost.
    if (navigator.clipboard) {
      navigator.clipboard.readText().then(function (t) {
        if (t && t !== clipText.value) {
          sendJSON({ type: "clipboard", text: t, selection: "clipboard" });
        }
      }).catch(function () {});
    }
  });

  // ---- Controls ------------------------------------------------------------
  qSel.addEventListener("change", function () {
    if (qSelInline) qSelInline.value = qSel.value;
    sendJSON({ type: "quality", quality: parseInt(qSel.value, 10) });
  });
  modeSel.addEventListener("change", function () {
    if (modeSelInline) modeSelInline.value = modeSel.value;
    reconnect();
  });
  // Mirror the inline (wide-screen) selects back to the menu-panel selects.
  if (qSelInline) {
    qSelInline.addEventListener("change", function () {
      qSel.value = qSelInline.value;
      sendJSON({ type: "quality", quality: parseInt(qSel.value, 10) });
    });
  }
  if (modeSelInline) {
    modeSelInline.addEventListener("change", function () {
      modeSel.value = modeSelInline.value;
      reconnect();
    });
  }
  // Touch-mode select: toggle the virtual cursor.
  touchModeSel.addEventListener("change", function () {
    if (touchModeSel.value === "trackpad") {
      cursor.classList.remove("hidden");
      // Center the cursor on the desktop.
      trackpadX = Math.floor(canvas.width / 2);
      trackpadY = Math.floor(canvas.height / 2);
      setCursor(trackpadX, trackpadY, false);
    } else {
      // In direct mode we still keep the cursor hidden by default — it
      // only appears briefly after each tap via updateVirtualCursor().
      cursor.classList.add("hidden");
    }
  });
  fsBtn.addEventListener("click", function () {
    if (!document.fullscreenElement) {
      document.documentElement.requestFullscreen();
    } else {
      document.exitFullscreen();
    }
  });
  reconnectBtn.addEventListener("click", reconnect);

  // Menu toggle for narrow screens.
  menuBtn.addEventListener("click", function (e) {
    e.stopPropagation();
    menuPanel.classList.toggle("hidden");
  });
  // Close menu on outside tap.
  document.addEventListener("click", function (e) {
    if (!menuPanel.classList.contains("hidden") &&
        !menuPanel.contains(e.target) && e.target !== menuBtn) {
      menuPanel.classList.add("hidden");
    }
  });

  // Mouse-button chooser panel.
  function updateMousePanelButtons() {
    mousePanel.querySelectorAll("button[data-btn]").forEach(function (b) {
      if (parseInt(b.dataset.btn, 10) === activeBtn) b.classList.add("active");
      else b.classList.remove("active");
    });
  }
  mousePanel.querySelectorAll("button[data-btn]").forEach(function (b) {
    b.addEventListener("click", function () {
      activeBtn = parseInt(b.dataset.btn, 10);
      activeBtnSticky = true; // once chosen, keep until user picks another
      updateMousePanelButtons();
    });
  });
  const mouseDragBtn = document.getElementById("mouseDrag");
  mouseDragBtn.addEventListener("click", function () {
    dragLock = !dragLock;
    mouseDragBtn.textContent = "Drag lock: " + (dragLock ? "on" : "off");
    mouseDragBtn.classList.toggle("active", dragLock);
    // If turning off drag-lock while a button is held, release it.
    if (!dragLock && mouseMask) {
      mouseMask = 0;
      sendMouse(0, 0, 0, 0);
    }
  });
  const mouseScrollBtn = document.getElementById("mouseScroll");
  mouseScrollBtn.addEventListener("click", function () {
    scrollMode = !scrollMode;
    mouseScrollBtn.textContent = "Scroll mode: " + (scrollMode ? "on" : "off");
    mouseScrollBtn.classList.toggle("active", scrollMode);
  });
  mousePanel.querySelector(".mouse-close").addEventListener("click", function () {
    mousePanel.classList.add("hidden");
  });
  mouseModeBtn.addEventListener("click", function () {
    fnPanel.classList.add("hidden");
    kbInput.classList.remove("visible");
    kbInput.blur();
    mousePanel.classList.toggle("hidden");
  });

  // Zoom controls.
  zoomBtn.addEventListener("click", function () {
    // Cycle 100% -> 200% -> 50% -> 100%.
    if (viewScale === 1.0) viewScale = 2.0;
    else if (viewScale === 2.0) viewScale = 0.5;
    else viewScale = 1.0;
    applyViewScale();
  });
  resetZoom.addEventListener("click", function () {
    viewScale = 1.0;
    applyViewScale();
  });

  // Clipboard button (mobile menu).
  clipBtn.addEventListener("click", function () {
    clipBar.classList.toggle("hidden");
  });

  function reconnect() {
    if (ws) { ws.onclose = null; ws.close(); ws = null; }
    pollStopped = true;
    connect();
  }

  // Keep the link alive (WS mode only; poll mode is self-keeping).
  setInterval(function () {
    if (!usePoll) sendJSON({ type: "ping", t: Date.now() });
  }, 15000);

  // ---- Mobile: soft keyboard + function-key panel -------------------------
  //
  // Two on-screen buttons (visible on touch / narrow screens) summon:
  //   1. Keyboard — focuses a hidden <input> so the mobile IME opens. Typed
  //      text (including IME-composed Chinese, etc.) is sent as a "text"
  //      message which the server injects via `xdotool type`. Control keys
  //      (Backspace/Enter/Tab/Arrows) the IME emits are forwarded as key
  //      events.
  //   2. Fn — toggles a panel of function keys with sticky modifiers. Tap a
  //      modifier (Ctrl/Alt/Shift/Super) to arm it; the next key tap is sent
  //      with that modifier state, then the modifier is cleared.

  // Soft keyboard.
  let kbComposing = false;
  kbInput.addEventListener("compositionstart", function () { kbComposing = true; });
  kbInput.addEventListener("compositionend", function (e) {
    kbComposing = false;
    const text = e.data || kbInput.value;
    if (text) sendJSON({ type: "text", text: text });
    kbInput.value = "";
  });
  // Non-IME input: each inserted character is sent immediately and the field
  // is cleared so it never accumulates (we only need it to host the IME).
  kbInput.addEventListener("input", function (e) {
    if (kbComposing) return; // wait for compositionend
    const v = kbInput.value;
    if (v) {
      sendJSON({ type: "text", text: v });
      kbInput.value = "";
    }
  });
  // Control keys from the soft keyboard that are not text.
  kbInput.addEventListener("keydown", function (e) {
    if (kbComposing) return; // let IME handle its own keys
    switch (e.key) {
      case "Backspace":
      case "Enter":
      case "Tab":
        e.preventDefault();
        sendKeyTap(e.key, e.code);
        break;
      case "ArrowLeft": case "ArrowRight": case "ArrowUp": case "ArrowDown":
      case "Escape":
        e.preventDefault();
        sendKeyTap(e.key, e.code);
        break;
    }
  });
  // Hide the input when it loses focus (IME dismissed).
  kbInput.addEventListener("blur", function () {
    setTimeout(function () {
      if (document.activeElement !== kbInput) {
        kbInput.classList.remove("visible");
      }
    }, 200);
  });

  kbBtn.addEventListener("click", function () {
    fnPanel.classList.add("hidden");
    kbInput.classList.add("visible");
    // focus must happen in the same user-gesture tick or mobile Safari
    // refuses to open the IME.
    kbInput.focus({ preventScroll: true });
  });
  fnBtn.addEventListener("click", function () {
    kbInput.classList.remove("visible");
    kbInput.blur();
    fnPanel.classList.toggle("hidden");
  });

  // Function-key panel: sticky modifiers + key taps.
  let stickyMods = { ctrl: false, alt: false, shift: false, meta: false };

  function refreshModButtons() {
    const btns = fnMods.querySelectorAll("button[data-mod]");
    btns.forEach(function (b) {
      if (stickyMods[b.dataset.mod]) b.classList.add("active");
      else b.classList.remove("active");
    });
  }

  function clearStickyMods() {
    stickyMods = { ctrl: false, alt: false, shift: false, meta: false };
    refreshModButtons();
  }

  // Modifier buttons toggle their sticky state.
  fnMods.querySelectorAll("button[data-mod]").forEach(function (b) {
    b.addEventListener("click", function () {
      stickyMods[b.dataset.mod] = !stickyMods[b.dataset.mod];
      refreshModButtons();
    });
  });
  fnPanel.querySelector(".fn-close").addEventListener("click", function () {
    fnPanel.classList.add("hidden");
    clearStickyMods();
  });

  // Send a key tap (keydown + keyup) carrying the current sticky modifiers,
  // then clear the modifiers so the next tap is plain.
  function sendKeyTap(key, code) {
    sendJSON({
      type: "key", key: key, code: code, down: true, repeat: false,
      ctrl: stickyMods.ctrl, alt: stickyMods.alt, shift: stickyMods.shift, meta: stickyMods.meta,
    });
    sendJSON({
      type: "key", key: key, code: code, down: false, repeat: false,
      ctrl: stickyMods.ctrl, alt: stickyMods.alt, shift: stickyMods.shift, meta: stickyMods.meta,
    });
    clearStickyMods();
  }

  fnPanel.querySelectorAll(".fn-row button[data-key]").forEach(function (b) {
    b.addEventListener("click", function () {
      sendKeyTap(b.dataset.key, b.dataset.code);
    });
  });

  // ---- FPS meter -----------------------------------------------------------
  function startFps() {
    stopFps();
    fpsCount = 0;
    fpsTimer = setInterval(function () {
      statsEl.textContent = "~" + fpsCount + " fps";
      fpsCount = 0;
    }, 1000);
  }
  function stopFps() {
    if (fpsTimer) { clearInterval(fpsTimer); fpsTimer = null; }
  }

  // ---- Helpers -------------------------------------------------------------
  function setStatus(t) { statusEl.textContent = t; }
  function showOverlay(title, msg) {
    overlayTitle.textContent = title;
    overlayMsg.textContent = msg;
    overlay.classList.remove("hidden");
  }
  function hideOverlay() { overlay.classList.add("hidden"); }
  function getParam(name) {
    const m = new URLSearchParams(location.search);
    return m.get(name);
  }

  connect();
})();
