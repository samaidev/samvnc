package server

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/gorilla/websocket"
)

//go:embed assets/*
var assetsFS embed.FS

// assetFiles is the set of embedded static files served by basename. Because
// aitun.cc publishes each tunnel under a path prefix (e.g. https://aitun.cc/abc123),
// SamVNC serves assets by basename regardless of the prefix so the UI works
// both on the local port and behind the tunnel.
var assetFiles = map[string]bool{
	"index.html": true,
	"client.js":  true,
	"style.css":  true,
}

// handleIndex routes all HTTP traffic. The WebSocket endpoint is matched by
// path suffix ("/ws"), which makes it work under any path prefix.
func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path

	// WebSocket upgrade — matches "/ws" and ".../<prefix>/ws".
	if p == "/ws" || strings.HasSuffix(p, "/ws") {
		s.handleWS(w, r)
		return
	}

	// SSE / POST transport (used behind aitun.cc where visitor WS is blocked).
	if p == "/stream" || strings.HasSuffix(p, "/stream") {
		s.handleStream(w, r)
		return
	}
	if p == "/input" || strings.HasSuffix(p, "/input") {
		s.handleInput(w, r)
		return
	}
	if p == "/poll" || strings.HasSuffix(p, "/poll") {
		s.handlePoll(w, r)
		return
	}
	if p == "/state" || strings.HasSuffix(p, "/state") {
		s.handleState(w, r)
		return
	}

	base := path.Base(p)
	if base == "." || base == "/" || p == "/" {
		base = "index.html"
	}

	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		http.Error(w, "asset error", http.StatusInternalServerError)
		return
	}

	switch {
	case strings.HasSuffix(base, ".html"):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case strings.HasSuffix(base, ".js"):
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	case strings.HasSuffix(base, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	}

	if assetFiles[base] {
		data, err := fs.ReadFile(sub, base)
		if err == nil {
			_, _ = w.Write(data)
			return
		}
	}

	// Unknown route → serve index.html (SPA fallback) so deep links work.
	data, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

// keep websocket import referenced even if upgrader moves.
var _ = websocket.Upgrader{}
