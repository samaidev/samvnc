//go:build linux

package clipboard

import (
	"fmt"
	"os/exec"
	"strings"
)

// xselClipboard shares the X11 CLIPBOARD and PRIMARY selections via xsel/xclip.
type xselClipboard struct{}

func newManager() (Manager, error) { return &xselClipboard{}, nil }

func (*xselClipboard) Name() string  { return "linux-xsel" }
func (*xselClipboard) Close() error  { return nil }

// Read returns the text of the given X11 selection.
func (*xselClipboard) Read(sel Selection) (string, error) {
	if path, err := exec.LookPath("xsel"); err == nil {
		args := []string{"-o"}
		if sel == SelClipboard {
			args = append(args, "-b")
		} else {
			args = append(args, "-p")
		}
		out, err := exec.Command(path, args...).Output()
		if err == nil {
			return string(out), nil
		}
	}
	if path, err := exec.LookPath("xclip"); err == nil {
		selName := "clipboard"
		if sel == SelPrimary {
			selName = "primary"
		}
		out, err := exec.Command(path, "-selection", selName, "-o").Output()
		if err == nil {
			return string(out), nil
		}
	}
	return "", fmt.Errorf("clipboard: no xsel/xclip available")
}

// Write sets the text of the given X11 selection. Ownership is taken so the
// content persists after the xsel process exits.
func (*xselClipboard) Write(text string, sel Selection) error {
	if path, err := exec.LookPath("xsel"); err == nil {
		args := []string{"-i"}
		if sel == SelClipboard {
			args = append(args, "-b")
		} else {
			args = append(args, "-p")
		}
		cmd := exec.Command(path, args...)
		cmd.Stdin = strings.NewReader(text)
		if err := cmd.Run(); err == nil {
			return nil
		}
	}
	if path, err := exec.LookPath("xclip"); err == nil {
		selName := "clipboard"
		if sel == SelPrimary {
			selName = "primary"
		}
		cmd := exec.Command(path, "-selection", selName, "-i")
		cmd.Stdin = strings.NewReader(text)
		if err := cmd.Run(); err == nil {
			return nil
		}
	}
	return fmt.Errorf("clipboard: no xsel/xclip available")
}
