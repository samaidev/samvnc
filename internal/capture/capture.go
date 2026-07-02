// Package capture abstracts desktop screen capture across platforms.
//
// A Capturer produces BGRA pixel buffers of the current desktop. Implementations
// exist for Linux (X11 via xgb) and Windows (GDI BitBlt). A no-op stub is used
// on platforms without a backend so the project still compiles for development.
package capture

import (
	"image"
)

// Capturer captures the desktop framebuffer.
//
// All methods must be safe to call from a single goroutine (the capture loop).
type Capturer interface {
	// Size returns the current desktop dimensions in pixels.
	Size() (int, int)

	// Capture grabs the full desktop into an RGBA image. The returned image is
	// owned by the caller. Implementations may reuse an internal buffer and
	// return the same *image.RGBA across calls; callers must copy if they need
	// to retain it.
	Capture() (*image.RGBA, error)

	// Close releases platform resources.
	Close() error

	// Name returns a short backend identifier e.g. "linux-x11".
	Name() string
}

// New returns the platform-native Capturer, or a stub on unsupported OSes.
func New() (Capturer, error) { return newCapturer() }
