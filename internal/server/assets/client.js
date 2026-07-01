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
      const r = await fetch(prefix() + "/poll", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ seq: lastSeq, inputs: batch }),
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
    if (resp.clip !== undefined && resp.clip !== null) {
      handleText({ type: "clipboard", text: resp.clip });
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
        if (clipText.value !== msg.text) {
          clipText.value = msg.text;
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

  // ---- Clipboard -----------------------------------------------------------
  clipSend.addEventListener("click", function () {
    sendJSON({ type: "clipboard", text: clipText.value });
  });
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
