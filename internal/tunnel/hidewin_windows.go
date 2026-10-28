//go:build windows

package tunnel

import (
        "os/exec"
        "syscall"
)

// hideWindow marks the aitun client process with CREATE_NO_WINDOW.
//
// The samvnc-tray binary is built with -H=windowsgui (GUI subsystem, no
// console of its own). Spawning a console child — the aitun client is one
// — without CREATE_NO_WINDOW makes Windows allocate a fresh, visible
// black console window that stays up for the whole tunnel lifetime.
func hideWindow(cmd *exec.Cmd) {
        const createNoWindow = 0x08000000
        cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
}
