//go:build !windows && !linux && !darwin

package main

import (
	"github.com/samaidev/samvnc/internal/settings"
)

// Unsupported platforms (freebsd etc.): no dialog backend. The tray is a
// stub there as well, so the tray app runs in server-only mode.
func openSettingsDialog(s *settings.Settings, publicURL, authToken string,
) (newSettings *settings.Settings, copyAddr bool, ok bool) {
	return s, false, false
}

func showCopyFallback(text string) {}
