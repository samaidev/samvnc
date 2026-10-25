//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// copyToClipboard copies text to the system clipboard on macOS/Linux:
// pbcopy on darwin; wl-copy (Wayland), xclip, xsel on Linux — tried in
// order, and a tool that exists but fails (e.g. xclip with no X display
// on Wayland) falls through to the next candidate instead of aborting.
//
// Final fallback on Linux: python3 + PyGObject (GTK), which talks to the
// desktop clipboard directly. It matters on stock GNOME Wayland machines
// that ship NEITHER wl-clipboard NOR xclip (verified on a real Ubuntu
// GNOME box where Copy Address failed with "no clipboard tool
// available") — python3-gi is part of every GNOME desktop, so it is the
// most universal last resort.
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
	if runtime.GOOS != "darwin" {
		if err := clipboardViaPythonGTK(text); err == nil {
			return nil
		}
	}
	if lastErr != nil {
		return errors.New("clipboard tools failed: " + lastErr.Error())
	}
	return errors.New("no clipboard tool available")
}

// clipboardViaPythonGTK sets the clipboard through python3 + PyGObject
// (GTK 3, falling back to GTK 4). Works on both X11 and Wayland GNOME
// sessions. The script is inline so no helper file has to be shipped.
func clipboardViaPythonGTK(text string) error {
	python, err := exec.LookPath("python3")
	if err != nil {
		return err
	}
	script := `
import sys
try:
    import gi
    gi.require_version("Gtk", "3.0")
    from gi.repository import Gtk, Gdk
    cb = Gtk.Clipboard.get(Gdk.SELECTION_CLIPBOARD)
    cb.set_text(sys.argv[1], -1)
    cb.store()
except Exception:
    gi.require_version("Gtk", "4.0")
    from gi.repository import Gdk
    cb = Gdk.Display.get_default().get_clipboard()
    cb.set_content(Gdk.ContentProvider.new_for_value(sys.argv[1]))
print("CLIP_OK")
`
	cmd := exec.Command(python, "-c", script, text)
	out, err := cmd.Output()
	if err != nil {
		return err
	}
	if !strings.Contains(string(out), "CLIP_OK") {
		return fmt.Errorf("clipboard helper exited without confirmation")
	}
	return nil
}
