//go:build !windows

package main

// FreeConsole is a no-op on non-Windows platforms.
func FreeConsole() {}

// ignoreConsoleClose is a no-op on non-Windows platforms.
func ignoreConsoleClose() {}
