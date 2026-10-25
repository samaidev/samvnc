//go:build darwin

// Package tray — macOS implementation backed by NSStatusItem + NSMenu
// (Cocoa, via cgo). The main goroutine is pinned to the process main
// thread in init() because NSApplication requires it; later SetMenu /
// SetTooltip / Stop calls from other goroutines are marshalled onto the
// main queue with dispatch_async.
package tray

import (
	"runtime"
	"sync"
	"unsafe"
)

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa

#include <stdlib.h>

typedef struct { char* label; char* id; int flags; } NativeMenuItem;

void nativeTrayRun(void);
void nativeTrayQuit(void);
void nativeTraySetTooltip(const char* text);
void nativeTraySetMenu(NativeMenuItem* items, int n);
*/
import "C"

func init() {
	// NSApplication/NSStatusItem must be used from the process main
	// thread. Package init runs on the main goroutine, so locking here
	// pins the whole tray lifecycle (New → Run) to the main thread.
	runtime.LockOSThread()
}

// darwinTray implements Tray via Cocoa.
type darwinTray struct {
	mu sync.Mutex
	// ids mirrors the menu passed to SetMenu; menu item tags index it.
	ids []string
	cb  func(itemID string)
}

// curTray is the singleton created by newTray, read from the cgo export
// callback (menu clicks arrive on the main thread).
var curTray *darwinTray

func newTray() Tray {
	t := &darwinTray{}
	curTray = t
	return t
}

func (t *darwinTray) Run() error {
	// Blocks in [NSApp run] on the main thread (we are pinned there).
	C.nativeTrayRun()
	return nil
}

func (t *darwinTray) SetMenu(items []MenuItem) {
	t.mu.Lock()
	t.ids = make([]string, len(items))
	for i, m := range items {
		t.ids[i] = m.ID
	}
	t.mu.Unlock()

	n := len(items)
	if n == 0 {
		C.nativeTraySetMenu(nil, 0)
		return
	}
	citems := make([]C.NativeMenuItem, n)
	cstrs := make([]*C.char, 0, 2*n)
	for i, m := range items {
		l := C.CString(m.Label)
		id := C.CString(m.ID)
		cstrs = append(cstrs, l, id)
		citems[i].label = l
		citems[i].id = id
		f := C.int(0)
		if m.Disabled {
			f |= 1
		}
		if m.Checked {
			f |= 2
		}
		if m.Separator {
			f |= 4
		}
		citems[i].flags = f
	}
	C.nativeTraySetMenu(&citems[0], C.int(n))
	for _, p := range cstrs {
		C.free(unsafe.Pointer(p))
	}
}

func (t *darwinTray) SetTooltip(text string) {
	c := C.CString(text)
	C.nativeTraySetTooltip(c)
	C.free(unsafe.Pointer(c))
}

func (t *darwinTray) SetCallback(cb func(itemID string)) {
	t.mu.Lock()
	t.cb = cb
	t.mu.Unlock()
}

func (t *darwinTray) Stop() {
	C.nativeTrayQuit()
}

// dispatchClick maps a menu item tag back to the logical item ID and
// invokes the callback. Runs on the main thread via the ObjC handler.
func (t *darwinTray) dispatchClick(idx int) {
	t.mu.Lock()
	var goID string
	if idx >= 0 && idx < len(t.ids) {
		goID = t.ids[idx]
	}
	cb := t.cb
	t.mu.Unlock()
	if goID != "" && cb != nil {
		cb(goID)
	}
}

//export trayMenuClickedC
func trayMenuClickedC(idx C.int) {
	if curTray != nil {
		curTray.dispatchClick(int(idx))
	}
}
