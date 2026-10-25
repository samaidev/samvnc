//go:build !windows

package main

// detectSystemLanguage is the non-Windows stub; the tray is Windows-only
// in practice, so default to English.
func detectSystemLanguage() string {
	return "en"
}
