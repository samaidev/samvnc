//go:build !linux

package main

import (
    "log"

    "github.com/samaidev/samvnc/internal/settings"
)

// settingsSelftest mirrors the linux-only flag in
// settings_dialog_linux.go; --settings-selftest is a no-op elsewhere.
var settingsSelftest bool

// runSettingsSelftest is only implemented on Linux (the GTK dialog lives
// there). Other platforms exit quietly with a note.
func runSettingsSelftest(s *settings.Settings) int {
    _ = s
    log.Printf("--settings-selftest is only implemented on Linux")
    return 0
}
