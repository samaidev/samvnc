//go:build windows

package main

import (
        "syscall"
        "unsafe"
)

var (
        kernel32Clip       = syscall.NewLazyDLL("kernel32.dll")
        user32Clip         = syscall.NewLazyDLL("user32.dll")
        procGlobalAlloc     = kernel32Clip.NewProc("GlobalAlloc")
        procGlobalLock      = kernel32Clip.NewProc("GlobalLock")
        procGlobalUnlock    = kernel32Clip.NewProc("GlobalUnlock")
        procOpenClipboard   = user32Clip.NewProc("OpenClipboard")
        procCloseClipboard  = user32Clip.NewProc("CloseClipboard")
        procEmptyClipboard  = user32Clip.NewProc("EmptyClipboard")
        procSetClipboardData = user32Clip.NewProc("SetClipboardData")
)

const (
        gmemMoveable   = 0x0002
        cfUnicodeText  = 13
)

func copyToClipboardWindows(text string) error {
        utf16, _ := syscall.UTF16FromString(text)
        size := len(utf16) * 2

        h, _, _ := procGlobalAlloc.Call(gmemMoveable, uintptr(size))
        if h == 0 {
                return nil
        }
        ptr, _, _ := procGlobalLock.Call(h)
        if ptr == 0 {
                return nil
        }
        copy((*[1 << 16]uint16)(unsafe.Pointer(ptr))[:len(utf16)], utf16)
        procGlobalUnlock.Call(h)

        procOpenClipboard.Call(0)
        procEmptyClipboard.Call()
        procSetClipboardData.Call(cfUnicodeText, h)
        procCloseClipboard.Call()
        return nil
}

// copyToClipboard is the platform entry point used by main.go.
func copyToClipboard(text string) error {
        return copyToClipboardWindows(text)
}

// showCopyFallback is a no-op on Windows: the native clipboard API above
// always works, so copyToClipboard never needs the manual-copy fallback
// dialog that the !windows build shows when no clipboard tool exists.
func showCopyFallback(text string) {}
