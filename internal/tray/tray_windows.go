//go:build windows

package tray

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// trayLog writes debug logs to ~/.samvnc/tray.log
var trayLogFile *os.File

func trayLog(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	fmt.Print(msg)
	if trayLogFile != nil {
		timestamp := time.Now().Format("15:04:05")
		trayLogFile.WriteString(timestamp + " " + msg)
		trayLogFile.Sync()
	}
}

func initTrayLog() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	logPath := filepath.Join(home, ".samvnc", "tray.log")
	os.MkdirAll(filepath.Dir(logPath), 0755)
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		trayLogFile = f
		trayLog("[tray] === Tray log started ===\n")
	}
}

// Windows message constants.
const (
	WM_USER         = 0x0400
	WM_TRAYICON     = WM_USER + 1
	WM_LBUTTONUP    = 0x0202
	WM_LBUTTONDBLCLK = 0x0203
	WM_RBUTTONUP    = 0x0205
	WM_CONTEXTMENU  = 0x007B
	WM_COMMAND      = 0x0111
	WM_DESTROY      = 0x0002
	WM_QUIT         = 0x0012
	WM_CLOSE        = 0x0010

	NIM_ADD     = 0x00000000
	NIM_MODIFY  = 0x00000001
	NIM_DELETE  = 0x00000002
	NIM_SETVERSION = 0x0000000A

	NIF_MESSAGE = 0x00000001
	NIF_ICON    = 0x00000002
	NIF_TIP     = 0x00000004
	NIF_INFO    = 0x00000010
	NIF_GUID    = 0x00000020
	NIF_SHOWTIP = 0x00000080

	NOTIFYICON_VERSION_4 = 4

	IDI_APPLICATION = 32512

	NIIF_INFO    = 0x00000001
	NIIF_WARNING = 0x00000002
	NIIF_ERROR   = 0x00000003

	LR_LOADFROMFILE = 0x00000010
	IMAGE_ICON      = 1

	TPM_RIGHTBUTTON = 0x0002
	TPM_BOTTOMALIGN = 0x0020
	TPM_LEFTALIGN   = 0x0000

	MF_SEPARATOR = 0x0800
	MF_STRING    = 0x0000
	MF_DISABLED  = 0x0002
	MF_GRAYED    = 0x0001
	MF_CHECKED   = 0x0008
	MF_UNCHECKED = 0x0000

	CS_HREDRAW = 0x0002
	CS_VREDRAW = 0x0001

	WS_OVERLAPPEDWINDOW = 0x00CF0000
	SW_HIDE              = 0
	CW_USEDEFAULT        = 0x80000000
)

// NOTIFYICONDATAW matches the Windows NOTIFYICONDATAW structure for amd64.
type NOTIFYICONDATAW struct {
	CbSize           uint32
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
	GuidItem         windows.GUID
	BalloonIcon      windows.Handle
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
type WNDCLASSEXW struct {
	Size          uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     windows.Handle
	HIcon         windows.Handle
	HCursor       windows.Handle
	HbrBackground windows.Handle
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       windows.Handle
}

var (
	modShell32 = windows.NewLazySystemDLL("shell32.dll")
	modUser32   = windows.NewLazySystemDLL("user32.dll")
	modKernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procShellNotifyIcon      = modShell32.NewProc("Shell_NotifyIconW")
	procCreatePopupMenu      = modUser32.NewProc("CreatePopupMenu")
	procAppendMenuW          = modUser32.NewProc("AppendMenuW")
	procTrackPopupMenu       = modUser32.NewProc("TrackPopupMenu")
	procDestroyMenu          = modUser32.NewProc("DestroyMenu")
	procLoadIconW            = modUser32.NewProc("LoadIconW")
	procLoadImageW           = modUser32.NewProc("LoadImageW")
	procRegisterClassExW     = modUser32.NewProc("RegisterClassExW")
	procCreateWindowExW      = modUser32.NewProc("CreateWindowExW")
	procGetMessageW          = modUser32.NewProc("GetMessageW")
	procDispatchMessageW     = modUser32.NewProc("DispatchMessageW")
	procTranslateMessage     = modUser32.NewProc("TranslateMessage")
	procDefWindowProcW       = modUser32.NewProc("DefWindowProcW")
	procPostQuitMessage      = modUser32.NewProc("PostQuitMessage")
	procPostMessageW         = modUser32.NewProc("PostMessageW")
	procGetCursorPos         = modUser32.NewProc("GetCursorPos")
	procSetForegroundWindow  = modUser32.NewProc("SetForegroundWindow")
	procGetForegroundWindow  = modUser32.NewProc("GetForegroundWindow")
	procAllowSetForegroundWindow = modUser32.NewProc("AllowSetForegroundWindow")
	procAttachThreadInput    = modUser32.NewProc("AttachThreadInput")
	procGetWindowThreadProcessId = modUser32.NewProc("GetWindowThreadProcessId")
	procBringWindowToTop     = modUser32.NewProc("BringWindowToTop")
	procIsHungAppWindow      = modUser32.NewProc("IsHungAppWindow")
	procGetCurrentThreadId   = modKernel32.NewProc("GetCurrentThreadId")
	procRegisterWindowMessageW = modUser32.NewProc("RegisterWindowMessageW")
	procShowWindow           = modUser32.NewProc("ShowWindow")
	procUpdateWindow         = modUser32.NewProc("UpdateWindow")
	procGetModuleHandleW     = modKernel32.NewProc("GetModuleHandleW")
	procLoadCursorW          = modUser32.NewProc("LoadCursorW")
	procCreateIconIndirect   = modUser32.NewProc("CreateIconIndirect")
)

// Global registry to route WindowProc callbacks to the correct Tray instance.
var (
	registryMu sync.Mutex
	registry   = map[windows.HWND]*winTray{}
)

// winTray implements Tray on Windows.
type winTray struct {
	hwnd            windows.HWND
	menuItems       []MenuItem
	callback        func(string)
	tooltip         string
	iconAdded       bool
	menuIDMap       map[uint32]string
	nextMenuID      uint32
	nid             *NOTIFYICONDATAW
	muNID           sync.Mutex
	wmTaskbarCreated uint32
}

func newTray() Tray {
	return &winTray{
		menuIDMap:  map[uint32]string{},
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

	// Log ALL messages for debugging (throttled: skip WM_MOUSEMOVE=0x0200 spam)
	if msg != 0x0200 {
		trayLog("[tray] wndProc msg=0x%04X wParam=0x%X lParam=0x%X\n", msg, wParam, lParam)
	}

	switch msg {
	case t.nidCallbackMessage():
		// NOTIFYICON_VERSION_4: notification code is in lParam low 16 bits
		trayMsg := lParam & 0xFFFF
		trayLog("[tray] tray msg=0x%04X\n", trayMsg)
		switch trayMsg {
		case WM_RBUTTONUP, WM_CONTEXTMENU:
			// v4 sends WM_CONTEXTMENU; v0 sends WM_RBUTTONUP. Handle both.
			trayLog("[tray] right-click -> showMenu\n")
			t.showMenu()
			return 0
		case WM_LBUTTONUP:
			trayLog("[tray] left-click -> showMenu\n")
			t.showMenu()
			return 0
		case WM_LBUTTONDBLCLK:
			trayLog("[tray] double-click\n")
			return 0
		}
	case WM_COMMAND:
		menuID := uint32(wParam) & 0xFFFF
		trayLog("[tray] WM_COMMAND: menuID=%d\n", menuID)
		if t.callback != nil {
			if itemID, ok := t.menuIDMap[menuID]; ok {
				trayLog("[tray] callback: itemID=%q\n", itemID)
				t.callback(itemID)
			} else {
				trayLog("[tray] no menuID %d in map (map has %d entries)\n", menuID, len(t.menuIDMap))
			}
		}
		return 0
	case WM_DESTROY:
		t.removeIcon()
		procPostQuitMessage.Call(0)
		return 0
	case t.wmTaskbarCreated:
		// Explorer restarted — re-add icon.
		trayLog("[tray] TaskbarCreated -> re-add icon\n")
		t.reAddIcon()
		return 0
	}

	return defWindowProc(hwnd, msg, wParam, lParam)
}

func (t *winTray) nidCallbackMessage() uint32 {
	t.muNID.Lock()
	defer t.muNID.Unlock()
	if t.nid != nil {
		return t.nid.CallbackMessage
	}
	return WM_TRAYICON
}

func defWindowProc(hwnd windows.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	r, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wParam, lParam)
	return r
}

// Run creates the hidden window, adds the tray icon, and runs the message loop.
func (t *winTray) Run() error {
	initTrayLog()

	// Register TaskbarCreated message.
	taskbarCreatedName, _ := windows.UTF16PtrFromString("TaskbarCreated")
	res, _, _ := procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(taskbarCreatedName)))
	t.wmTaskbarCreated = uint32(res)

	// Get module instance handle.
	instanceHandle, _, _ := procGetModuleHandleW.Call(0)

	// Load default icon and cursor.
	iconHandle, _, _ := procLoadIconW.Call(0, uintptr(IDI_APPLICATION))
	cursorHandle, _, _ := procLoadCursorW.Call(0, uintptr(32512)) // IDC_ARROW

	// Register window class.
	wproc := syscall.NewCallback(windowProc)
	classNameUTF16, _ := windows.UTF16PtrFromString("SamVNCTrayClass")

	wc := WNDCLASSEXW{
		Size:          uint32(unsafe.Sizeof(WNDCLASSEXW{})),
		Style:         CS_HREDRAW | CS_VREDRAW,
		LpfnWndProc:   wproc,
		HInstance:     windows.Handle(instanceHandle),
		HIcon:         windows.Handle(iconHandle),
		HCursor:       windows.Handle(cursorHandle),
		HbrBackground: windows.Handle(6), // COLOR_WINDOW + 1
		LpszClassName: classNameUTF16,
		HIconSm:       windows.Handle(iconHandle),
	}
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	// Create a normal hidden window (NOT message-only — TrackPopupMenu needs foreground).
	windowName, _ := windows.UTF16PtrFromString("")
	hwnd, _, callErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(classNameUTF16)),
		uintptr(unsafe.Pointer(windowName)),
		uintptr(WS_OVERLAPPEDWINDOW),
		uintptr(CW_USEDEFAULT), uintptr(CW_USEDEFAULT),
		uintptr(CW_USEDEFAULT), uintptr(CW_USEDEFAULT),
		0, 0,
		uintptr(instanceHandle),
		0,
	)
	if hwnd == 0 {
		return fmt.Errorf("CreateWindowExW failed: %v", callErr)
	}
	t.hwnd = windows.HWND(hwnd)

	// Hide the window.
	procShowWindow.Call(uintptr(t.hwnd), uintptr(SW_HIDE))
	procUpdateWindow.Call(uintptr(t.hwnd))

	// Register this tray instance.
	registryMu.Lock()
	registry[t.hwnd] = t
	registryMu.Unlock()

	// Add the tray icon.
	t.addIcon()

	// Set tooltip if provided.
	if t.tooltip != "" {
		t.updateTooltip(t.tooltip)
	}

	// Start keepalive goroutine: periodically re-set NOTIFYICON_VERSION_4
	// to prevent version reset after RDP reconnect, DPI change, etc.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			t.muNID.Lock()
			nid := t.nid
			t.muNID.Unlock()
			if nid != nil {
				t.setVersion()
			}
		}
	}()

	// Message loop.
	trayLog("[tray] entering message loop\n")
	for {
		var msg MSG
		ret, _, _ := procGetMessageW.Call(
			uintptr(unsafe.Pointer(&msg)),
			0, 0, 0,
		)
		if ret == 0 {
			trayLog("[tray] WM_QUIT received, exiting message loop\n")
			break
		}
		if ret == 0xFFFFFFFF {
			err := windows.GetLastError()
			trayLog("[tray] GetMessageW error: %v\n", err)
			return err
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
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

	hIcon := t.loadIconFromFile()
	if hIcon == 0 {
		hIcon = t.createFallbackIcon()
	}

	tipText := "SamVNC"
	if t.tooltip != "" {
		tipText = t.tooltip
	}
	tipUTF16, _ := windows.UTF16FromString(tipText)

	nid := &NOTIFYICONDATAW{
		CbSize:          uint32(unsafe.Sizeof(NOTIFYICONDATAW{})),
		HWnd:            t.hwnd,
		UID:             100,
		Flags:           NIF_MESSAGE | NIF_ICON | NIF_TIP,
		CallbackMessage: WM_TRAYICON,
		HIcon:           windows.Handle(hIcon),
	}
	copy(nid.Tip[:], tipUTF16)

	t.muNID.Lock()
	t.nid = nid
	t.muNID.Unlock()

	r, _, _ := procShellNotifyIcon.Call(NIM_ADD, uintptr(unsafe.Pointer(nid)))
	if r == 0 {
		trayLog("[tray] Shell_NotifyIcon(NIM_ADD) failed\n")
	} else {
		trayLog("[tray] Shell_NotifyIcon(NIM_ADD) success\n")
	}

	// CRITICAL: Set version to 4 so Explorer sends proper mouse messages
	// (WM_CONTEXTMENU for right-click, WM_LBUTTONDBLCLK for double-click).
	// Without this, right-click menu may never appear!
	t.setVersion()

	t.iconAdded = true
}

// setVersion sets the notification icon callback version to 4.
// This must be called after NIM_ADD and after TaskbarCreated.
func (t *winTray) setVersion() {
	t.muNID.Lock()
	defer t.muNID.Unlock()
	if t.nid == nil {
		return
	}
	t.nid.TimeoutOrVersion = NOTIFYICON_VERSION_4
	t.nid.CbSize = uint32(unsafe.Sizeof(*t.nid))
	r, _, _ := procShellNotifyIcon.Call(NIM_SETVERSION, uintptr(unsafe.Pointer(t.nid)))
	trayLog("[tray] setVersion: NIM_SETVERSION ret=%d\n", r)
}

// reAddIcon re-adds the icon after Explorer restart.
func (t *winTray) reAddIcon() {
	t.muNID.Lock()
	nid := t.nid
	t.muNID.Unlock()
	if nid == nil {
		return
	}
	procShellNotifyIcon.Call(NIM_ADD, uintptr(unsafe.Pointer(nid)))
	t.setVersion()
}

func (t *winTray) removeIcon() {
	if !t.iconAdded {
		return
	}
	t.muNID.Lock()
	nid := t.nid
	t.muNID.Unlock()
	if nid != nil {
		procShellNotifyIcon.Call(NIM_DELETE, uintptr(unsafe.Pointer(nid)))
	}
	t.iconAdded = false
}

func (t *winTray) updateTooltip(text string) {
	t.muNID.Lock()
	defer t.muNID.Unlock()
	if t.nid == nil {
		return
	}
	t.nid.Flags = NIF_TIP
	t.nid.CbSize = uint32(unsafe.Sizeof(*t.nid))
	tipUTF16, _ := windows.UTF16FromString(text)
	copy(t.nid.Tip[:], tipUTF16)
	procShellNotifyIcon.Call(NIM_MODIFY, uintptr(unsafe.Pointer(t.nid)))
}

// loadIconFromFile tries to load tray_icon.ico from several candidate locations.
func (t *winTray) loadIconFromFile() uintptr {
	candidates := []string{}

	if exePath, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exePath)
		candidates = append(candidates,
			filepath.Join(exeDir, "tray_icon.ico"),
			filepath.Join(exeDir, "assets", "tray_icon.ico"),
		)
	}

	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".samvnc", "tray_icon.ico"))
	}

	for _, path := range candidates {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		pathUTF16, err := windows.UTF16PtrFromString(path)
		if err != nil {
			continue
		}
		hIcon, _, _ := procLoadImageW.Call(
			0,
			uintptr(unsafe.Pointer(pathUTF16)),
			uintptr(IMAGE_ICON),
			0, 0,
			uintptr(LR_LOADFROMFILE),
		)
		if hIcon != 0 {
			trayLog("[tray] Loaded icon from %s\n", path)
			return hIcon
		}
	}
	return 0
}

// createFallbackIcon creates a simple 32x32 icon programmatically.
func (t *winTray) createFallbackIcon() uintptr {
	const w, h = 32, 32

	modGdi := windows.NewLazySystemDLL("gdi32.dll")
	procCreateBitmap := modGdi.NewProc("CreateBitmap")
	procDeleteObject := modGdi.NewProc("DeleteObject")

	pixels := make([]uint32, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if x == 0 || x == w-1 || y == 0 || y == h-1 {
				pixels[y*w+x] = 0x00FFFFFF
			} else {
				pixels[y*w+x] = 0x00FF6600
			}
		}
	}

	hColorBitmap, _, _ := procCreateBitmap.Call(
		uintptr(w), uintptr(h), 1, 32,
		uintptr(unsafe.Pointer(&pixels[0])),
	)

	maskPixels := make([]byte, (w*h)/8)
	hMaskBitmap, _, _ := procCreateBitmap.Call(
		uintptr(w), uintptr(h), 1, 1,
		uintptr(unsafe.Pointer(&maskPixels[0])),
	)

	ii := ICONINFO{
		FIcon:    1,
		HbmMask:  windows.Handle(hMaskBitmap),
		HbmColor: windows.Handle(hColorBitmap),
	}

	hIcon, _, _ := procCreateIconIndirect.Call(uintptr(unsafe.Pointer(&ii)))
	procDeleteObject.Call(hColorBitmap)
	procDeleteObject.Call(hMaskBitmap)

	return hIcon
}

// ICONINFO for CreateIconIndirect.
type ICONINFO struct {
	FIcon    uint32
	XHotspot uint32
	YHotspot uint32
	HbmMask  windows.Handle
	HbmColor windows.Handle
}

// showMenu shows the popup menu at the cursor position.
// This closely follows pcguy's implementation to ensure the menu
// actually appears (foreground window handling is critical).
func (t *winTray) showMenu() {
	if len(t.menuItems) == 0 {
		return
	}

	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)

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
		cmdID := t.nextMenuID
		t.nextMenuID++
		t.menuIDMap[cmdID] = item.ID

		procAppendMenuW.Call(menu, uintptr(flags), uintptr(cmdID), uintptr(unsafe.Pointer(labelPtr)))
	}

	// Get cursor position.
	type point struct{ X, Y int32 }
	var p point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))

	trayLog("[tray] showMenu at cursor (%d,%d), %d items\n", p.X, p.Y, len(t.menuItems))

	// === Critical foreground window handling (from pcguy) ===
	// TrackPopupMenu requires the window to be in the foreground,
	// otherwise the menu appears and immediately disappears.
	const ASFW_ANY = 0xFFFFFFFF
	procAllowSetForegroundWindow.Call(uintptr(ASFW_ANY))

	fg, _, _ := procGetForegroundWindow.Call()
	if fg != 0 {
		if hung, _, _ := procIsHungAppWindow.Call(fg); hung != 0 {
			trayLog("[tray] showMenu: foreground window hung, skip fg switch\n")
		} else if fg != uintptr(t.hwnd) {
			me, _, _ := procGetCurrentThreadId.Call()
			fgTid, _, _ := procGetWindowThreadProcessId.Call(fg, 0)
			if fgTid != 0 && fgTid != me {
				procAttachThreadInput.Call(me, fgTid, 1)
				procSetForegroundWindow.Call(uintptr(t.hwnd))
				procBringWindowToTop.Call(uintptr(t.hwnd))
				procAttachThreadInput.Call(me, fgTid, 0)
			} else {
				procSetForegroundWindow.Call(uintptr(t.hwnd))
			}
		}
	}

	// Post WM_NULL before TrackPopupMenu (foreground handshake).
	procPostMessageW.Call(uintptr(t.hwnd), 0, 0, 0)

	res, _, err := procTrackPopupMenu.Call(
		menu,
		uintptr(TPM_RIGHTBUTTON|TPM_LEFTALIGN|TPM_BOTTOMALIGN),
		uintptr(p.X),
		uintptr(p.Y),
		0,
		uintptr(t.hwnd),
		0,
	)
	trayLog("[tray] TrackPopupMenu: res=0x%x err=%v\n", res, err)

	// Post WM_NULL after TrackPopupMenu.
	procPostMessageW.Call(uintptr(t.hwnd), 0, 0, 0)
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
