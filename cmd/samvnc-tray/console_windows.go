//go:build windows

package main

import (
	"log"
	"syscall"
)

var (
	kernel32Console    = syscall.NewLazyDLL("kernel32.dll")
	procFreeConsole    = kernel32Console.NewProc("FreeConsole")
	procSetConsoleCtrl = kernel32Console.NewProc("SetConsoleCtrlHandler")
)

// consoleCtrlHandler is the callback that tells Windows "we handled it,
// don't kill us" when the parent shell closes (CTRL_CLOSE_EVENT).
// Returning 1 suppresses the default action (process termination).
func consoleCtrlHandler(ctrlType uint32) uintptr {
	log.Printf("[tray] consoleCtrlHandler: ctrlType=%d\n", ctrlType)
	// CTRL_C_EVENT=0, CTRL_BREAK_EVENT=1,
	// CTRL_CLOSE_EVENT=2, CTRL_LOGOFF_EVENT=5, CTRL_SHUTDOWN_EVENT=6
	if ctrlType == 2 {
		log.Printf("[tray] CTRL_CLOSE_EVENT received, ignoring\n")
		return 1
	}
	return 0
}

// FreeConsole detaches the calling process from its console.
func FreeConsole() {
	procFreeConsole.Call()
}

// ignoreConsoleClose installs a handler that swallows CTRL_CLOSE_EVENT,
// preventing the OS from killing the tray when the parent shell exits.
func ignoreConsoleClose() {
	handler := syscall.NewCallback(consoleCtrlHandler)
	r, _, err := procSetConsoleCtrl.Call(handler, 1)
	log.Printf("[tray] SetConsoleCtrlHandler: ret=%d err=%v\n", r, err)
}
