//go:build darwin

package capture

// macOS screen capture via the system `screencapture` utility.
//
// CGWindowList/CGDisplayCreateImage through cgo would tie the server build
// to CGO (the CI cross-compiles darwin with CGO_ENABLED=0), so we shell out
// to /usr/sbin/screencapture which is present on every macOS installation:
//
//	screencapture -x -m <file>   (-x: no sound, -m: main display only)
//
// The PNG it writes is decoded into an *image.RGBA.
//
// NOTE (macOS 10.15+): the first capture triggers the Screen Recording
// permission prompt (TCC). Once the user approves the responsible app
// (e.g. the samvnc-tray or the terminal that spawned samvnc-server),
// captures succeed without further interaction.

import (
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

type darwinCapturer struct {
	mu      sync.Mutex
	tmpFile string
	lastW   int
	lastH   int
}

func newCapturer() (Capturer, error) {
	dir, err := os.MkdirTemp("", "samvnc-cap")
	if err != nil {
		return nil, fmt.Errorf("capture: temp dir: %w", err)
	}
	return &darwinCapturer{
		tmpFile: filepath.Join(dir, "shot.png"),
	}, nil
}

func (c *darwinCapturer) Name() string { return "darwin-screencapture" }

func (c *darwinCapturer) Size() (int, int) {
	// Best-effort: dimensions of the last successful capture.
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastW, c.lastH
}

func (c *darwinCapturer) Capture() (*image.RGBA, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// -x no sound, -m main display only, -C no cursor shadow noise.
	cmd := exec.Command("/usr/sbin/screencapture", "-x", "-m", c.tmpFile)
	// Fall back to PATH lookup if /usr/sbin is not it.
	if _, err := exec.LookPath("/usr/sbin/screencapture"); err != nil {
		cmd = exec.Command("screencapture", "-x", "-m", c.tmpFile)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(c.tmpFile)
		return nil, errors.New("capture: screencapture: " +
			string(out) + " (" + err.Error() + ")")
	}

	f, err := os.Open(c.tmpFile)
	if err != nil {
		return nil, fmt.Errorf("capture: open shot: %w", err)
	}
	img, err := png.Decode(f)
	f.Close()
	if err != nil {
		os.Remove(c.tmpFile)
		return nil, fmt.Errorf("capture: decode shot: %w", err)
	}
	os.Remove(c.tmpFile)

	// Convert to *image.RGBA (screencapture writes NRGBA or RGBA PNGs).
	b := img.Bounds()
	rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	drawCopy(rgba, img)
	c.lastW, c.lastH = b.Dx(), b.Dy()
	return rgba, nil
}

func (c *darwinCapturer) Close() error {
	return nil
}

// drawCopy converts any image to RGBA without importing image/draw at
// package scope twice (kept local for clarity).
func drawCopy(dst *image.RGBA, src image.Image) {
	b := src.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			dst.Set(x, y, src.At(x, y))
		}
	}
}
