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
  const fsBtn = document.getElementById("fsBtn");
  const clipBar = document.getElementById("clipboardBar");
  const clipText = document.getElementById("clipText");
  const clipSend = document.getElementById("clipSend");
  const mobileBar = document.getElementById("mobileBar");
  const kbBtn = document.getElementById("kbBtn");
  const fnBtn = document.getElementById("fnBtn");
  const kbInput = document.getElementById("kbInput");
  const fnPanel = document.getElementById("fnPanel");
  const fnMods = document.getElementById("fnMods");

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
    // also works, then inject Ctrl+V into the remote desktop.
    const done = function () {
      // Inject Ctrl+V into the remote: press ctrl, press v, release v,
      // release ctrl. Non-modifier 'v' uses atomic key on keydown.
      sendJSON({ type: "key", key: "Control", code: "ControlLeft", down: true,
                 repeat: false, ctrl: true, alt: false, shift: false, meta: false });
      sendJSON({ type: "key", key: "v", code: "KeyV", down: true,
                 repeat: false, ctrl: true, alt: false, shift: false, meta: false });
      sendJSON({ type: "key", key: "v", code: "KeyV", down: false,
                 repeat: false, ctrl: true, alt: false, shift: false, meta: false });
      sendJSON({ type: "key", key: "Control", code: "ControlLeft", down: false,
                 repeat: false, ctrl: false, alt: false, shift: false, meta: false });
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

  // ---- Input: mouse --------------------------------------------------------
  const BTN = { 0: 1, 1: 2, 2: 4, 3: 8, 4: 16 };

  function desktopCoords(e) {
    const rect = canvas.getBoundingClientRect();
    const sx = (e.clientX - rect.left) / rect.width * canvas.width;
    const sy = (e.clientY - rect.top) / rect.height * canvas.height;
    return { x: Math.round(sx), y: Math.round(sy) };
  }

  canvas.addEventListener("mousedown", function (e) {
    e.preventDefault();
    canvas.focus();
    mouseMask |= BTN[e.button] || 0;
    const c = desktopCoords(e);
    sendJSON({ type: "mouse", x: c.x, y: c.y, mask: mouseMask, wheel: 0 });
  });
  canvas.addEventListener("mouseup", function (e) {
    e.preventDefault();
    mouseMask &= ~(BTN[e.button] || 0);
    const c = desktopCoords(e);
    sendJSON({ type: "mouse", x: c.x, y: c.y, mask: mouseMask, wheel: 0 });
  });
  canvas.addEventListener("mousemove", function (e) {
    const c = desktopCoords(e);
    sendJSON({ type: "mouse", x: c.x, y: c.y, mask: mouseMask, wheel: 0 });
  });
  canvas.addEventListener("contextmenu", function (e) { e.preventDefault(); });
  canvas.addEventListener("wheel", function (e) {
    e.preventDefault();
    const c = desktopCoords(e);
    let ticks = Math.round(e.deltaY / 100);
    if (ticks === 0) ticks = e.deltaY > 0 ? 1 : -1;
    sendJSON({ type: "mouse", x: c.x, y: c.y, mask: mouseMask, wheel: ticks });
  }, { passive: false });

  // Touch -> mouse (basic single-touch support).
  canvas.addEventListener("touchstart", function (e) {
    e.preventDefault();
    if (!e.touches[0]) return;
    mouseMask |= 1;
    const c = desktopCoords(e.touches[0]);
    sendJSON({ type: "mouse", x: c.x, y: c.y, mask: mouseMask, wheel: 0 });
  }, { passive: false });
  canvas.addEventListener("touchmove", function (e) {
    e.preventDefault();
    if (!e.touches[0]) return;
    const c = desktopCoords(e.touches[0]);
    sendJSON({ type: "mouse", x: c.x, y: c.y, mask: mouseMask, wheel: 0 });
  }, { passive: false });
  canvas.addEventListener("touchend", function (e) {
    e.preventDefault();
    mouseMask &= ~1;
    sendJSON({ type: "mouse", x: 0, y: 0, mask: mouseMask, wheel: 0 });
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
    sendJSON({ type: "quality", quality: parseInt(qSel.value, 10) });
  });
  modeSel.addEventListener("change", function () {
    reconnect();
  });
  fsBtn.addEventListener("click", function () {
    if (!document.fullscreenElement) {
      document.documentElement.requestFullscreen();
    } else {
      document.exitFullscreen();
    }
  });
  reconnectBtn.addEventListener("click", reconnect);

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
