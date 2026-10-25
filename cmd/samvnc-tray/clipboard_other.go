//go:build !windows

package main

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"
)

// copyToClipboard copies text to the system clipboard on macOS/Linux:
// pbcopy on darwin, xclip/xsel (with fallbacks) on Linux.
func copyToClipboard(text string) error {
	candidates := []string{"xclip", "xsel"}
	if runtime.GOOS == "darwin" {
		candidates = []string{"pbcopy", "xclip", "xsel"}
	}
	for _, tool := range candidates {
		if p, err := exec.LookPath(tool); err == nil {
			cmd := exec.Command(p)
			cmd.Stdin = strings.NewReader(text)
			if tool == "xclip" {
				cmd.Args = append(cmd.Args, "-selection", "clipboard")
			}
			return cmd.Run()
		}
	}
	return errors.New("no clipboard tool available")
}
