//go:build !windows

package main

// On non-Windows platforms, the clipboard is handled in main.go via pbcopy/xclip.
// This file exists so the package compiles on non-Windows.
