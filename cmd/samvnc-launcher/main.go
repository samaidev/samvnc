package main

import (
	"fmt"
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

func main() {
	exePath := `C:\Program Files\samvnc\samvnc-tray.exe`
	if len(os.Args) > 1 {
		exePath = os.Args[1]
	}

	cmdLineUTF16, _ := syscall.UTF16PtrFromString(exePath)

	// DETACHED_PROCESS = 0x00000008
	// CREATE_NEW_PROCESS_GROUP = 0x00000200
	// CREATE_UNICODE_ENVIRONMENT = 0x00000400
	creationFlags := uint32(0x00000008 | 0x00000200)

	si := startupInfo{
		Cb:         uint32(unsafe.Sizeof(startupInfo{})),
		Flags:      0x00000001, // STARTF_USESHOWWINDOW
		ShowWindow: 0,          // SW_HIDE
	}
	var pi processInfo

	ret, _, err := procCreateProc.Call(
		0,
		uintptr(unsafe.Pointer(cmdLineUTF16)),
		0, 0,
		0,
		uintptr(creationFlags),
		0,
		0,
		uintptr(unsafe.Pointer(&si)),
		uintptr(unsafe.Pointer(&pi)),
	)
	if ret == 0 {
		fmt.Fprintf(os.Stderr, "CreateProcessW failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Launched %s PID=%d\n", exePath, pi.ProcessId)
	procCloseHandle.Call(pi.Process)
	procCloseHandle.Call(pi.Thread)
}
