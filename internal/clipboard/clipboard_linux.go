//go:build linux

package clipboard

import (
	"fmt"
	"os/exec"
	"strings"
)

// xselClipboard shares the X11 CLIPBOARD selection via `xsel` (or `xclip`).
type xselClipboard struct{}

func newManager() (Manager, error) { return &xselClipboard{}, nil }

func (*xselClipboard) Name() string { return "linux-xsel" }

func (*xselClipboard) Close() error { return nil }

func (*xselClipboard) Read() (string, error) {
	// Prefer xsel, fall back to xclip.
	if path, err := exec.LookPath("xsel"); err == nil {
		out, err := exec.Command(path, "-ob").Output()
		if err == nil {
			return string(out), nil
		}
	}
	if path, err := exec.LookPath("xclip"); err == nil {
		out, err := exec.Command(path, "-selection", "clipboard", "-o").Output()
		if err == nil {
			return string(out), nil
		}
	}
	return "", fmt.Errorf("clipboard: no xsel/xclip available")
}

func (*xselClipboard) Write(text string) error {
	if path, err := exec.LookPath("xsel"); err == nil {
		cmd := exec.Command(path, "-bi")
		cmd.Stdin = strings.NewReader(text)
		if err := cmd.Run(); err == nil {
			return nil
		}
	}
	if path, err := exec.LookPath("xclip"); err == nil {
		cmd := exec.Command(path, "-selection", "clipboard", "-i")
		cmd.Stdin = strings.NewReader(text)
		if err := cmd.Run(); err == nil {
			return nil
		}
	}
	return fmt.Errorf("clipboard: no xsel/xclip available")
}
