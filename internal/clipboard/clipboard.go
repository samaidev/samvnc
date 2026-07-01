// Package clipboard abstracts desktop clipboard read/write for sharing.
//
// Implementations exist for Linux (X11 selection via xsel/xclip) and Windows
// (OpenClipboard API). A no-op stub is used on unsupported platforms.
package clipboard

// Manager provides bi-directional clipboard sharing between the remote desktop
// and the browser client.
type Manager interface {
	// Read returns the current clipboard text. Returns "" if unavailable.
	Read() (string, error)

	// Write sets the clipboard text.
	Write(text string) error

	// Close releases resources.
	Close() error

	// Name returns a short backend identifier.
	Name() string
}

// New returns the platform-native clipboard Manager, or a stub.
func New() (Manager, error) { return newManager() }
