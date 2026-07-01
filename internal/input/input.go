// Package input abstracts keyboard and mouse injection into the desktop.
//
// Implementations exist for Linux (XTEST) and Windows (SendInput). A no-op
// stub is used on unsupported platforms.
package input

import "github.com/samaidev/samvnc/internal/protocol"

// Injector injects keyboard and mouse events into the local desktop.
type Injector interface {
	// Key injects a keyboard event. The DOM key/code and modifier state are
	// translated to a platform-native keysym + keycode internally.
	Key(ev protocol.KeyEvent) error

	// Type injects an arbitrary Unicode string (used by the mobile soft
	// keyboard so IME/composed text reaches the remote app without per-key
	// keysym mapping). The string is sent through the platform's native
	// text-input path (e.g. xdotool type on X11).
	Type(text string) error

	// Mouse injects a pointer event with absolute coordinates and a button
	// bitmask (see protocol.Button* constants).
	Mouse(ev protocol.MouseEvent) error

	// Close releases platform resources.
	Close() error

	// Name returns a short backend identifier e.g. "linux-xtest".
	Name() string
}

// New returns the platform-native Injector, or a stub on unsupported OSes.
func New() (Injector, error) { return newInjector() }
