//go:build darwin && !cgo

package main

import (
	"github.com/samaidev/samvnc/internal/settings"
)

// The native Cocoa settings window (settings_dialog_darwin.go/.m) needs
// cgo. Without cgo (cross-compiled darwin binaries) the tray itself is a
// stub too, so a no-op dialog matches: report "cancelled" and keep the
// original settings.
func openSettingsDialog(s *settings.Settings, publicURL, authToken string,
) (newSettings *settings.Settings, copyAddr bool, ok bool) {
	return s, false, false
}

func showCopyFallback(text string) {}
