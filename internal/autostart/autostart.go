// Package autostart manages cross-platform boot-time auto-start for SamVNC.
package autostart

// Enable registers the executable at execPath to start on system boot.
func Enable(execPath string) error {
	return enable(execPath)
}

// Disable removes the auto-start registration.
func Disable() error {
	return disable()
}

// IsEnabled reports whether auto-start is currently enabled.
func IsEnabled() (bool, error) {
	return isEnabled()
}
