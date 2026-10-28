//go:build !windows

package main

import "os/exec"

// ignoreConsoleClose is a no-op on non-Windows platforms.
func ignoreConsoleClose() {}

// runHidden is a plain Run on non-Windows platforms: there is no console
// window to suppress.
func runHidden(cmd *exec.Cmd) error { return cmd.Run() }
