package server

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed assets/*
var assetsFS embed.FS

// handleIndex serves the embedded browser UI. Static files live under
// assets/ and are served at the root path (e.g. /client.js, /style.css).
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "index.html"
	}
	// Security: prevent path traversal.
	if strings.Contains(path, "..") {
		http.NotFound(w, r)
		return
	}
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		http.Error(w, "asset error", http.StatusInternalServerError)
		return
	}
	// Default content type.
	switch {
	case strings.HasSuffix(path, ".html"):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case strings.HasSuffix(path, ".js"):
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	case strings.HasSuffix(path, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	}
	data, err := fs.ReadFile(sub, path)
	if err != nil {
		// Fall back to index.html for unknown routes (SPA-style).
		data, err = fs.ReadFile(sub, "index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	_, _ = w.Write(data)
}
