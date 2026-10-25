//go:build !windows

package main

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"
)

// copyToClipboard copies text to the system clipboard on macOS/Linux:
// pbcopy on darwin; wl-copy (Wayland), xclip, xsel on Linux — tried in
// order, and a tool that exists but fails (e.g. xclip with no X display
// on Wayland) falls through to the next candidate instead of aborting.
//
// Note: wl-copy self-daemonizes (its parent exits after the fork, the
// child keeps serving the selection), and xclip backgrounds itself the
// same way by default, so a plain cmd.Run() returns promptly for both.
func copyToClipboard(text string) error {
	candidates := []string{"wl-copy", "xclip", "xsel"}
	if runtime.GOOS == "darwin" {
		candidates = []string{"pbcopy", "xclip", "xsel"}
	}
	var lastErr error
	for _, tool := range candidates {
		if _, err := exec.LookPath(tool); err != nil {
			continue
		}
		cmd := exec.Command(tool)
		cmd.Stdin = strings.NewReader(text)
		switch tool {
		case "xclip":
			cmd.Args = append(cmd.Args, "-selection", "clipboard")
		case "xsel":
			cmd.Args = append(cmd.Args, "--clipboard", "--input")
		}
		if err := cmd.Run(); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	if lastErr != nil {
		return errors.New("clipboard tools failed: " + lastErr.Error())
	}
	return errors.New("no clipboard tool available")
}
