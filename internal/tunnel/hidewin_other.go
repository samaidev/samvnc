//go:build !windows

package tunnel

import "os/exec"

// hideWindow is a no-op on non-Windows platforms: console children there
// do not open extra windows.
func hideWindow(cmd *exec.Cmd) {}
