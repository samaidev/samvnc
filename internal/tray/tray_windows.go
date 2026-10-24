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

	NIF_MESSAGE  = 0x00000001
	NIF_ICON     = 0x00000002
	NIF_TIP      = 0x00000004
	NIF_INFO     = 0x00000010
	NIF_GUID     = 0x00000020
	NIF_SHOWTIP  = 0x00000080

	IDI_APPLICATION = 32512

	// Balloon notification flags
	NIIF_INFO    = 0x00000001
	NIIF_WARNING = 0x00000002
	NIIF_ERROR   = 0x00000003

	// LoadImageW parameters
	LR_LOADFROMFILE = 0x00000010
	IMAGE_ICON      = 1

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

// GUID matches the Windows GUID structure (16 bytes).
type GUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

// NOTIFYICONDATAW matches the Windows NOTIFYICONDATAW structure for amd64.
// Go handles the alignment/padding automatically (pointers get 8-byte align).
type NOTIFYICONDATAW struct {
	CbSize           uint32          // cbSize           (4 bytes)
	HWnd             windows.HWND    // hWnd             (8 bytes, 4 padding before)
	UID              uint32          // uID              (4 bytes)
	Flags            uint32          // uFlags           (4 bytes)
	CallbackMessage  uint32          // uCallbackMessage (4 bytes)
	HIcon            windows.Handle  // hIcon            (8 bytes, 4 padding before)
	Tip              [128]uint16     // szTip[128]       (256 bytes)
	State            uint32          // dwState          (4 bytes)
	StateMask        uint32          // dwStateMask      (4 bytes)
	Info             [256]uint16     // szInfo[256]      (512 bytes)
	TimeoutOrVersion uint32          // uTimeout/uVersion(4 bytes)
	InfoTitle        [64]uint16      // szInfoTitle[64]  (128 bytes)
	InfoFlags        uint32          // dwInfoFlags      (4 bytes)
	GuidItem         GUID            // guidItem         (16 bytes, 4-byte aligned, no padding needed)
	BalloonIcon      windows.Handle  // hBalloonIcon     (8 bytes, 8-byte aligned, no padding needed)
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

// ICONINFO for CreateIconIndirect.
type ICONINFO struct {
	FIcon    uint32 // TRUE for icon, FALSE for cursor
	XHotspot uint32
	YHotspot uint32
	HbmMask  windows.Handle
	HbmColor windows.Handle
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
	procLoadImageW        = modUser32.NewProc("LoadImageW")
	procRegisterClassExW  = modUser32.NewProc("RegisterClassExW")
	procCreateWindowExW   = modUser32.NewProc("CreateWindowExW")
	procGetMessageW       = modUser32.NewProc("GetMessageW")
	procDispatchMessageW  = modUser32.NewProc("DispatchMessageW")
	procTranslateMessage  = modUser32.NewProc("TranslateMessage")
	procDefWindowProcW    = modUser32.NewProc("DefWindowProcW")
	procPostQuitMessage   = modUser32.NewProc("PostQuitMessage")
	procPostMessageW      = modUser32.NewProc("PostMessageW")
	procGetCursorPos      = modUser32.NewProc("GetCursorPos")
	procSetForegroundWindow = modUser32.NewProc("SetForegroundWindow")
	procCreateIconIndirect = modUser32.NewProc("CreateIconIndirect")
	procTrackPopupMenu    = modUser32.NewProc("TrackPopupMenu")
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
		trayLog("[tray] WM_TRAYICON received: lParam=0x%x\n", lParam)
		switch lParam & 0xFFFF {
		case WM_RBUTTONUP:
			trayLog("[tray] WM_RBUTTONUP -> showMenu\n")
			t.showMenu()
			return 0
		case WM_LBUTTONUP:
			trayLog("[tray] WM_LBUTTONUP -> showMenu\n")
			// Left click could do something, but we just show menu too
			t.showMenu()
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
	// Initialize file logging.
	initTrayLog()

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
	trayLog("[tray] entering message loop\n")
	for {
		var msg MSG
		ret, _, _ := procGetMessageW.Call(
			uintptr(unsafe.Pointer(&msg)),
			0, 0, 0,
		)
		if ret == 0 { // WM_QUIT
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

// SamVNC tray GUID — fixed across restarts so Windows 11 can remember
// the icon's visibility preference.
var samvncGUID = GUID{
	Data1: 0x53414D56, // "SAMV"
	Data2: 0x4E43,     // "NC"
	Data3: 0x5452,     // "TR"
	Data4: [8]byte{0x41, 0x59, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, // "AY"
}

func (t *winTray) addIcon() {
	if t.iconAdded {
		return
	}

	// Create a colored icon (blue square) using CreateIconIndirect.
	// LoadIconW(IDI_APPLICATION) gives a generic icon that's hard to spot.
	hIcon := t.createColorIcon()

	tipUTF16, _ := windows.UTF16FromString("SamVNC")
	if t.tooltip != "" {
		tipUTF16, _ = windows.UTF16FromString(t.tooltip)
	}

	nid := NOTIFYICONDATAW{
		CbSize:          uint32(unsafe.Sizeof(NOTIFYICONDATAW{})),
		HWnd:            t.hwnd,
		UID:             1,
		Flags:           NIF_MESSAGE | NIF_ICON | NIF_TIP | NIF_GUID,
		CallbackMessage: WM_TRAYICON,
		HIcon:           windows.Handle(hIcon),
		GuidItem:         samvncGUID,
	}
	copy(nid.Tip[:], tipUTF16)

	r, _, _ := procShellNotifyIcon.Call(NIM_ADD, uintptr(unsafe.Pointer(&nid)))
	if r == 0 {
		trayLog("[tray] Shell_NotifyIcon(NIM_ADD) failed\n")
	} else {
		trayLog("[tray] Shell_NotifyIcon(NIM_ADD) success, GUID=%v\n", samvncGUID)
		// Show a balloon notification to make the icon visible immediately.
		// Windows hides new tray icons in the overflow area by default;
		// a balloon forces it into the visible area.
		t.showBalloon("SamVNC", "Tray icon is now active. Right-click for menu.")
	}
	t.iconAdded = true
}

// showBalloon shows a balloon notification from the tray icon.
func (t *winTray) showBalloon(title, msg string) {
	nid := NOTIFYICONDATAW{
		CbSize: uint32(unsafe.Sizeof(NOTIFYICONDATAW{})),
		HWnd:   t.hwnd,
		UID:    1,
		Flags:  NIF_INFO,
	}
	titleUTF16, _ := windows.UTF16FromString(title)
	msgUTF16, _ := windows.UTF16FromString(msg)
	copy(nid.InfoTitle[:], titleUTF16)
	copy(nid.Info[:], msgUTF16)
	nid.InfoFlags = NIIF_INFO
	nid.TimeoutOrVersion = 3000 // 3 seconds
	procShellNotifyIcon.Call(NIM_MODIFY, uintptr(unsafe.Pointer(&nid)))
}

// createColorIcon loads the tray icon from the ICO file next to the executable.
// If the file is not found, it falls back to a programmatic blue icon.
func (t *winTray) createColorIcon() uintptr {
	// Try to load icon from file (tray_icon.ico next to executable, or in assets/)
	hIcon := t.loadIconFromFile()
	if hIcon != 0 {
		return hIcon
	}
	// Fallback: create a simple blue icon programmatically
	return t.createFallbackIcon()
}

// loadIconFromFile tries to load tray_icon.ico from several candidate locations.
func (t *winTray) loadIconFromFile() uintptr {
	candidates := []string{}

	// 1. Same directory as the executable
	if exePath, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exePath)
		candidates = append(candidates,
			filepath.Join(exeDir, "tray_icon.ico"),
			filepath.Join(exeDir, "assets", "tray_icon.ico"),
		)
	}

	// 2. ~/.samvnc/tray_icon.ico
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
		// LoadImageW: type=IMAGE_ICON, cx=0/cy=0 = use default, flags=LR_LOADFROMFILE
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
		trayLog("[tray] LoadImageW failed for %s\n", path)
	}
	trayLog("[tray] No icon file found, using fallback\n")
	return 0
}

// createFallbackIcon creates a simple 32x32 blue icon with white border.
func (t *winTray) createFallbackIcon() uintptr {
	const w, h = 32, 32

	// XOR mask (color bitmap) as DIB section
	// We'll use CreateBitmap directly.
	modGdi := windows.NewLazySystemDLL("gdi32.dll")
	procCreateBitmap := modGdi.NewProc("CreateBitmap")
	procCreateCompatibleDC := modGdi.NewProc("CreateCompatibleDC")
	procSelectObject := modGdi.NewProc("SelectObject")
	procDeleteObject := modGdi.NewProc("DeleteObject")
	procSetPixel := modGdi.NewProc("SetPixel")

	// Create color bitmap (32bpp, 16x16)
	// Use a simple approach: create a monochrome + color bitmap pair
	// Actually, let's use CreateIconIndirect with HBITMAPs

	// Create a color bitmap by using CreateBitmap with pixel data
	pixels := make([]uint32, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if x == 0 || x == w-1 || y == 0 || y == h-1 {
				pixels[y*w+x] = 0x00FFFFFF // white (BGR)
			} else {
				pixels[y*w+x] = 0x00FF6600 // blue (B=0xFF, G=0x66, R=0x00)
			}
		}
	}

	hColorBitmap, _, _ := procCreateBitmap.Call(
		uintptr(w), uintptr(h), 1, 32,
		uintptr(unsafe.Pointer(&pixels[0])),
	)

	// Create mask bitmap (all zeros = fully opaque)
	maskPixels := make([]byte, (w*h)/8) // 1bpp, all 0 = opaque
	hMaskBitmap, _, _ := procCreateBitmap.Call(
		uintptr(w), uintptr(h), 1, 1,
		uintptr(unsafe.Pointer(&maskPixels[0])),
	)

	ii := ICONINFO{
		FIcon:    1, // TRUE = icon
		HbmMask:  windows.Handle(hMaskBitmap),
		HbmColor: windows.Handle(hColorBitmap),
	}

	hIcon, _, _ := procCreateIconIndirect.Call(uintptr(unsafe.Pointer(&ii)))

	// Clean up bitmaps (icon has its own copy)
	procDeleteObject.Call(hColorBitmap)
	procDeleteObject.Call(hMaskBitmap)

	// Keep references alive to avoid unused warnings
	_ = procCreateCompatibleDC
	_ = procSelectObject
	_ = procSetPixel

	return hIcon
}

func (t *winTray) removeIcon() {
	if !t.iconAdded {
		return
	}
	nid := NOTIFYICONDATAW{
		CbSize:   uint32(unsafe.Sizeof(NOTIFYICONDATAW{})),
		HWnd:     t.hwnd,
		UID:      1,
		Flags:    NIF_GUID,
		GuidItem: samvncGUID,
	}
	procShellNotifyIcon.Call(NIM_DELETE, uintptr(unsafe.Pointer(&nid)))
	t.iconAdded = false
}

func (t *winTray) updateTooltip(text string) {
	nid := NOTIFYICONDATAW{
		CbSize:   uint32(unsafe.Sizeof(NOTIFYICONDATAW{})),
		HWnd:     t.hwnd,
		UID:      1,
		Flags:    NIF_TIP | NIF_GUID,
		GuidItem: samvncGUID,
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

	trayLog("[tray] showMenu at cursor (%d,%d), %d items\n", cursorPos.X, cursorPos.Y, len(t.menuItems))

	// Set foreground window (required for menu to dismiss properly).
	procSetForegroundWindow.Call(uintptr(t.hwnd))

	// Use TrackPopupMenu (not Ex) — matches energye/systray's approach.
	// TPM_LEFTALIGN=0x0000, TPM_BOTTOMALIGN=0x0020, TPM_RIGHTBUTTON=0x0002
	// Do NOT use TPM_RETURNCMD/TPM_NONOTIFY — let WM_COMMAND flow naturally.
	procTrackPopupMenu.Call(
		menu,
		uintptr(TPM_RIGHTBUTTON|0x0000|0x0020), // TPM_LEFTALIGN|TPM_BOTTOMALIGN
		uintptr(int(cursorPos.X)),
		uintptr(int(cursorPos.Y)),
		0,
		uintptr(t.hwnd),
		0,
	)

	// WM_COMMAND is dispatched by the message loop (no manual posting needed).
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
