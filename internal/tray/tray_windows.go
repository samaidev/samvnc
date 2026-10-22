//go:build windows

package tray

import (
	"fmt"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows message constants.
const (
	WM_USER       = 0x0400
	WM_TRAYICON   = WM_USER + 0
	WM_LBUTTONUP  = 0x0202
	WM_RBUTTONUP  = 0x0205
	WM_COMMAND    = 0x0111
	WM_DESTROY    = 0x0002
	WM_QUIT       = 0x0012
	WM_CLOSE      = 0x0010

	NIM_ADD     = 0x00000000
	NIM_MODIFY  = 0x00000001
	NIM_DELETE  = 0x00000002

	NIF_MESSAGE = 0x00000001
	NIF_ICON   = 0x00000002
	NIF_TIP    = 0x00000004

	IDI_APPLICATION = 32512

	TPM_RIGHTBUTTON  = 0x0002
	TPM_BOTTOMALIGN  = 0x0020
	TPM_RETURNCMD    = 0x0100
	TPM_NONOTIFY     = 0x0080

	MF_SEPARATOR = 0x0800
	MF_STRING    = 0x0000
	MF_DISABLED  = 0x0002
	MF_GRAYED    = 0x0001
	MF_CHECKED   = 0x0008
	MF_UNCHECKED = 0x0000
)

// NOTIFYICONDATAW for amd64.
type NOTIFYICONDATAW struct {
	HWnd             windows.HWND
	UID              uint32
	Flags            uint32
	CallbackMessage  uint32
	HIcon            windows.Handle
	Tip              [128]uint16
	State            uint32
	StateMask        uint32
	Info             [256]uint16
	TimeoutOrVersion uint32
	InfoTitle        [64]uint16
	InfoFlags        uint32
}

// MSG is the Windows message structure.
type MSG struct {
	HWnd    windows.HWND
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

// WNDCLASSEXW matches the Windows WNDCLASSEXW structure for amd64.
// Field alignment matters: the pointer fields need 8-byte alignment,
// which means there's padding after the 4-byte fields.
type WNDCLASSEXW struct {
	Size          uint32         // cbSize       (4 bytes)
	Style         uint32         // style        (4 bytes)
	LpfnWndProc   uintptr        // lpfnWndProc  (8 bytes, 8-aligned)
	CbClsExtra    int32          // cbClsExtra   (4 bytes)
	CbWndExtra    int32          // cbWndExtra   (4 bytes)
	HInstance     windows.Handle // hInstance    (8 bytes, 8-aligned)
	HIcon         windows.Handle // hIcon        (8 bytes)
	HCursor       windows.Handle // hCursor      (8 bytes)
	HbrBackground windows.Handle // hbrBackground(8 bytes)
	LpszMenuName  *uint16        // lpszMenuName (8 bytes)
	LpszClassName *uint16        // lpszClassName(8 bytes)
	HIconSm       windows.Handle // hIconSm      (8 bytes)
}

var (
	modShell32  = windows.NewLazySystemDLL("shell32.dll")
	modUser32    = windows.NewLazySystemDLL("user32.dll")
	modKernel32  = windows.NewLazySystemDLL("kernel32.dll")

	procShellNotifyIcon   = modShell32.NewProc("Shell_NotifyIconW")
	procCreatePopupMenu   = modUser32.NewProc("CreatePopupMenu")
	procAppendMenuW       = modUser32.NewProc("AppendMenuW")
	procTrackPopupMenuEx  = modUser32.NewProc("TrackPopupMenuEx")
	procDestroyMenu       = modUser32.NewProc("DestroyMenu")
	procLoadIconW         = modUser32.NewProc("LoadIconW")
	procRegisterClassExW  = modUser32.NewProc("RegisterClassExW")
	procCreateWindowExW   = modUser32.NewProc("CreateWindowExW")
	procGetMessageW       = modUser32.NewProc("GetMessageW")
	procDispatchMessageW  = modUser32.NewProc("DispatchMessageW")
	procDefWindowProcW    = modUser32.NewProc("DefWindowProcW")
	procPostQuitMessage   = modUser32.NewProc("PostQuitMessage")
	procPostMessageW      = modUser32.NewProc("PostMessageW")
	procGetCursorPos      = modUser32.NewProc("GetCursorPos")
	procSetForegroundWindow = modUser32.NewProc("SetForegroundWindow")
)

// Global registry to route WindowProc callbacks to the correct Tray instance.
var (
	registryMu sync.Mutex
	registry   = map[windows.HWND]*winTray{}
)

const className = "SamVNCTrayClass\x00"
var classOnce sync.Once

// winTray implements Tray on Windows.
type winTray struct {
	hwnd       windows.HWND
	menuItems []MenuItem
	callback  func(string)
	tooltip   string
	iconAdded bool
	menuIDMap map[uint32]string // menu command ID -> item ID string
	nextMenuID uint32
}

func newTray() Tray {
	return &winTray{
		menuIDMap: map[uint32]string{},
		nextMenuID: 1000,
	}
}

// windowProc is the global WindowProc callback.
func windowProc(hwnd windows.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	registryMu.Lock()
	t := registry[hwnd]
	registryMu.Unlock()

	if t == nil {
		return defWindowProc(hwnd, msg, wParam, lParam)
	}

	switch msg {
	case WM_TRAYICON:
		switch lParam & 0xFFFF {
		case WM_RBUTTONUP:
			t.showMenu()
			return 0
		case WM_LBUTTONUP:
			// Left click could do something, but we just show menu too
			t.showMenu()
			return 0
		}
	case WM_COMMAND:
		menuID := uint32(wParam) & 0xFFFF
		if t.callback != nil {
			if itemID, ok := t.menuIDMap[menuID]; ok {
				t.callback(itemID)
			}
		}
		return 0
	case WM_DESTROY:
		// Remove the tray icon
		t.removeIcon()
		procPostQuitMessage.Call(0)
		return 0
	}

	return defWindowProc(hwnd, msg, wParam, lParam)
}

func defWindowProc(hwnd windows.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	r, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wParam, lParam)
	return r
}

// Run creates the hidden window, adds the tray icon, and runs the message loop.
func (t *winTray) Run() error {
	// Create the window class (once globally).
	wproc := syscall.NewCallback(windowProc)

	classNameUTF16, _ := windows.UTF16PtrFromString("SamVNCTrayClass")

	wc := WNDCLASSEXW{
		Size:          uint32(unsafe.Sizeof(WNDCLASSEXW{})),
		LpfnWndProc:   wproc,
		LpszClassName: classNameUTF16,
	}

	// Register the window class.
	ret, _, _ := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	if ret == 0 {
		err := windows.GetLastError()
		// Class may already be registered from a previous run — that's OK.
		_ = err
	}

	// Create a hidden message-only window (parent = HWND_MESSAGE = -3).
	hwnd, _, callErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(classNameUTF16)),
		uintptr(unsafe.Pointer(classNameUTF16)),
		0, // style - not visible
		0, 0, 0, 0,
		^uintptr(2), // HWND_MESSAGE = -3 = ^uintptr(2)
		0, 0, 0,
	)
	if hwnd == 0 {
		return fmt.Errorf("CreateWindowExW failed: %v (callErr: %v)", windows.GetLastError(), callErr)
	}
	t.hwnd = windows.HWND(hwnd)

	// Register this tray instance so windowProc can find it.
	registryMu.Lock()
	registry[t.hwnd] = t
	registryMu.Unlock()

	// Add the tray icon.
	t.addIcon()

	// Update menu/tooltip if they were set before Run.
	if len(t.menuItems) > 0 {
		// menu is shown on demand, nothing to pre-set
	}
	if t.tooltip != "" {
		t.updateTooltip(t.tooltip)
	}

	// Message loop.
	for {
		var msg MSG
		ret, _, _ := procGetMessageW.Call(
			uintptr(unsafe.Pointer(&msg)),
			0, 0, 0,
		)
		if ret == 0 { // WM_QUIT
			break
		}
		if ret == 0xFFFFFFFF {
			return windows.GetLastError()
		}
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}

	// Cleanup
	t.removeIcon()
	registryMu.Lock()
	delete(registry, t.hwnd)
	registryMu.Unlock()
	return nil
}

func (t *winTray) addIcon() {
	if t.iconAdded {
		return
	}
	// Load default application icon.
	hIcon, _, _ := procLoadIconW.Call(0, IDI_APPLICATION)

	tipUTF16, _ := windows.UTF16FromString("SamVNC")
	if t.tooltip != "" {
		tipUTF16, _ = windows.UTF16FromString(t.tooltip)
	}

	nid := NOTIFYICONDATAW{
		HWnd:            t.hwnd,
		UID:             1,
		Flags:           NIF_MESSAGE | NIF_ICON | NIF_TIP,
		CallbackMessage: WM_TRAYICON,
		HIcon:           windows.Handle(hIcon),
	}
	copy(nid.Tip[:], tipUTF16)

	procShellNotifyIcon.Call(NIM_ADD, uintptr(unsafe.Pointer(&nid)))
	t.iconAdded = true
}

func (t *winTray) removeIcon() {
	if !t.iconAdded {
		return
	}
	nid := NOTIFYICONDATAW{
		HWnd: t.hwnd,
		UID:  1,
	}
	procShellNotifyIcon.Call(NIM_DELETE, uintptr(unsafe.Pointer(&nid)))
	t.iconAdded = false
}

func (t *winTray) updateTooltip(text string) {
	nid := NOTIFYICONDATAW{
		HWnd:  t.hwnd,
		UID:   1,
		Flags: NIF_TIP,
	}
	tipUTF16, _ := windows.UTF16FromString(text)
	copy(nid.Tip[:], tipUTF16)
	procShellNotifyIcon.Call(NIM_MODIFY, uintptr(unsafe.Pointer(&nid)))
}

func (t *winTray) showMenu() {
	if len(t.menuItems) == 0 {
		return
	}

	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)

	// Reset the ID map for this showing.
	t.menuIDMap = map[uint32]string{}

	for _, item := range t.menuItems {
		if item.Separator {
			procAppendMenuW.Call(menu, MF_SEPARATOR, 0, 0)
			continue
		}
		flags := MF_STRING
		if item.Disabled {
			flags |= MF_GRAYED
		}
		if item.Checked {
			flags |= MF_CHECKED
		}

		labelPtr, _ := windows.UTF16PtrFromString(item.Label)

		// Assign a unique command ID.
		cmdID := t.nextMenuID
		t.nextMenuID++
		t.menuIDMap[cmdID] = item.ID

		procAppendMenuW.Call(menu, uintptr(flags), uintptr(cmdID), uintptr(unsafe.Pointer(labelPtr)))
	}

	// Get cursor position for menu placement.
	var cursorPos struct{ X, Y int32 }
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&cursorPos)))

	// Set foreground window (required for menu to dismiss properly).
	procSetForegroundWindow.Call(uintptr(t.hwnd))

	// Show the menu and get the selected command.
	cmd, _, _ := procTrackPopupMenuEx.Call(
		menu,
		uintptr(TPM_RIGHTBUTTON|TPM_RETURNCMD|TPM_NONOTIFY),
		uintptr(int(cursorPos.X)),
		uintptr(int(cursorPos.Y)),
		uintptr(t.hwnd),
		0,
	)

	if cmd != 0 {
		// Post WM_COMMAND to ourselves so the callback runs through the
		// normal message dispatch path.
		procPostMessageW.Call(uintptr(t.hwnd), WM_COMMAND, cmd, 0)
	}
}

func (t *winTray) SetMenu(items []MenuItem) {
	t.menuItems = items
}

func (t *winTray) SetTooltip(text string) {
	t.tooltip = text
	if t.hwnd != 0 {
		t.updateTooltip(text)
	}
}

func (t *winTray) SetCallback(cb func(string)) {
	t.callback = cb
}

func (t *winTray) Stop() {
	if t.hwnd != 0 {
		procPostMessageW.Call(uintptr(t.hwnd), WM_CLOSE, 0, 0)
	}
}

// Ensure syscall is referenced.
var _ = syscall.Handle(0)
