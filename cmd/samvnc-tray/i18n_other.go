//go:build !windows && !linux && !darwin

package main

// detectSystemLanguage is the stub for platforms without a desktop
// session (the tray runs in server-only mode there); default to English.
func detectSystemLanguage() string {
	return "en"
}
