// SamVNC browser client.
//
// Connects to /ws over ws:// or wss:// (so it works both on the local port and
// through the aitun.cc TCP+TLS tunnel). Parses the SamVNC binary protocol and
// renders frames to a canvas, while sending keyboard / mouse / clipboard events
// back to the server.
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

  let ws = null;
  let desktopW = 0, desktopH = 0;
  let mouseMask = 0;
  let currentBlobUrl = null;
  let frameCount = 0;
  let fpsTimer = null;
  let fpsCount = 0;

  // ---- WebSocket plumbing --------------------------------------------------
  function wsUrl() {
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    return proto + "//" + location.host + "/ws";
  }

  function connect() {
    setStatus("connecting…");
    hideOverlay();
    ws = new WebSocket(wsUrl());
    ws.binaryType = "arraybuffer";

    ws.onopen = function () {
      setStatus("authorizing…");
      sendHello();
      startFps();
    };
    ws.onmessage = onMessage;
    ws.onclose = function () {
      setStatus("disconnected");
      showOverlay("Disconnected", "The remote session ended.");
      stopFps();
    };
    ws.onerror = function () {
      setStatus("connection error");
    };
  }

  function sendJSON(obj) {
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
  function onMessage(ev) {
    if (typeof ev.data === "string") {
      handleText(JSON.parse(ev.data));
      return;
    }
    // Binary frame: 24-byte header + image payload.
    const buf = new Uint8Array(ev.data);
    if (buf.length < BIN_HEADER_LEN) return;
    const dv = new DataView(ev.data);
    const magic = dv.getUint16(0, true);
    if (magic !== FRAME_MAGIC) return;
    const format = dv.getUint8(3);
    const flags = dv.getUint8(17);
    const payloadLen = dv.getUint32(18, true);
    const payload = buf.slice(BIN_HEADER_LEN, BIN_HEADER_LEN + payloadLen);
    drawFrame(format, payload, flags);
    fpsCount++;
  }

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
          // Surface the public tunnel URL for convenience.
          console.log("SamVNC public URL:", msg.relay_url);
        }
        clipBar.classList.remove("hidden");
        break;
      case "clipboard":
        if (clipText.value !== msg.text) {
          clipText.value = msg.text;
          // Also push into the local clipboard when focused.
          if (navigator.clipboard && document.hasFocus()) {
            navigator.clipboard.writeText(msg.text).catch(function () {});
          }
        }
        break;
      case "stats":
        statsEl.textContent =
          "fps " + msg.fps.toFixed(0) + (msg.p2p ? "  P2P" : "  relay");
        break;
      case "pong":
        // latency could be computed from msg.t; ignored for now.
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
  canvas.addEventListener("keydown", function (e) {
    e.preventDefault();
    sendJSON({
      type: "key", key: e.key, code: e.code, down: true, repeat: e.repeat,
      ctrl: e.ctrlKey, alt: e.altKey, shift: e.shiftKey, meta: e.metaKey,
    });
  });
  canvas.addEventListener("keyup", function (e) {
    e.preventDefault();
    sendJSON({
      type: "key", key: e.key, code: e.code, down: false, repeat: false,
      ctrl: e.ctrlKey, alt: e.altKey, shift: e.shiftKey, meta: e.metaKey,
    });
  });

  // ---- Input: mouse --------------------------------------------------------
  const BTN = { 0: 1, 1: 2, 2: 4, 3: 8, 4: 16 }; // button index -> mask

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

  // ---- Clipboard -----------------------------------------------------------
  clipSend.addEventListener("click", function () {
    sendJSON({ type: "clipboard", text: clipText.value });
  });
  // Auto-sync local clipboard into the remote when the window gains focus.
  window.addEventListener("focus", function () {
    if (navigator.clipboard) {
      navigator.clipboard.readText().then(function (t) {
        if (t && t !== clipText.value) {
          sendJSON({ type: "clipboard", text: t });
        }
      }).catch(function () {});
    }
  });

  // ---- Controls ------------------------------------------------------------
  qSel.addEventListener("change", function () {
    sendJSON({ type: "quality", quality: parseInt(qSel.value, 10) });
  });
  modeSel.addEventListener("change", function () {
    // Mode change requires a fresh hello / reconnect.
    if (ws) ws.close();
    setTimeout(connect, 100);
  });
  fsBtn.addEventListener("click", function () {
    if (!document.fullscreenElement) {
      document.documentElement.requestFullscreen();
    } else {
      document.exitFullscreen();
    }
  });
  reconnectBtn.addEventListener("click", connect);

  // Keep the link alive.
  setInterval(function () {
    sendJSON({ type: "ping", t: Date.now() });
  }, 15000);

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
