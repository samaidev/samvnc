//go:build !windows

package main

import (
	"github.com/samaidev/samvnc/internal/settings"
)

// openSettingsDialog opens a settings dialog on non-Windows platforms.
// Currently a stub — returns the original settings unchanged.
func openSettingsDialog(s *settings.Settings, publicURL, authToken string) (newSettings *settings.Settings, copyAddr bool, ok bool) {
	return s, false, false
}
