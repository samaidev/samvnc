//go:build windows

package main

import (
	"log"
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32        = syscall.NewLazyDLL("kernel32.dll")
	procCreateProc  = kernel32.NewProc("CreateProcessW")
	procCloseHandle = kernel32.NewProc("CloseHandle")
)

type startupInfo struct {
	Cb            uint32
	Reserved      *uint16
	Desktop       *uint16
	Title         *uint16
	X             uint32
	Y             uint32
	XSize         uint32
	YSize         uint32
	XCountChars   uint32
	YCountChars   uint32
	FillAttribute uint32
	Flags         uint32
	ShowWindow    uint16
	Reserved2     uint16
	Reserved3     *byte
	StdInput      uintptr
	StdOutput     uintptr
	StdError      uintptr
}

type processInfo struct {
	Process   uintptr
	Thread    uintptr
	ProcessId uint32
	ThreadId  uint32
}

// buildEnvBlock builds a Unicode environment block for CreateProcessW.
func buildEnvBlock(env []string) *uint16 {
	// Each entry is "KEY=VALUE\0", block ends with extra "\0".
	total := 0
	for _, s := range env {
		total += len(s) + 1
	}
	total++ // final null terminator
	buf := make([]uint16, 0, total)
	for _, s := range env {
		u, _ := syscall.UTF16FromString(s)
		buf = append(buf, u...)
	}
	buf = append(buf, 0) // final null
	return &buf[0]
}

func detachSelf() {
	if os.Getenv("SAMVNC_DETACHED") == "1" {
		return
	}

	exePath, err := os.Executable()
	if err != nil {
		return
	}

	cmdLineUTF16, _ := syscall.UTF16PtrFromString(exePath)

	// Build environment with SAMVNC_DETACHED=1 so the child won't re-detach.
	env := append(os.Environ(), "SAMVNC_DETACHED=1")
	envBlock := buildEnvBlock(env)

	// DETACHED_PROCESS = 0x00000008 — no console
	// CREATE_NEW_PROCESS_GROUP = 0x00000200 — independent process group
	// CREATE_UNICODE_ENVIRONMENT = 0x00000400 — env block is UTF-16
	creationFlags := uint32(0x00000008 | 0x00000200 | 0x00000400)

	si := startupInfo{
		Cb: uint32(unsafe.Sizeof(startupInfo{})),
	}
	var pi processInfo

	ret, _, callErr := procCreateProc.Call(
		0,
		uintptr(unsafe.Pointer(cmdLineUTF16)),
		0, 0,
		0,
		uintptr(creationFlags),
		uintptr(unsafe.Pointer(envBlock)),
		0,
		uintptr(unsafe.Pointer(&si)),
		uintptr(unsafe.Pointer(&pi)),
	)
	if ret == 0 {
		log.Printf("[tray] detachSelf: CreateProcessW failed: %v\n", callErr)
		return
	}

	procCloseHandle.Call(pi.Process)
	procCloseHandle.Call(pi.Thread)
	os.Exit(0)
}
