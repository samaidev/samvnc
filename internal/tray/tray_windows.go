//go:build windows

package tray

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ============================================================
// trayLog — debug logging to ~/.samvnc/tray.log
// ============================================================

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

// ============================================================
// Lazy-loaded DLL procs (exactly as pcguy does)
// ============================================================

var (
	g32                     = windows.NewLazySystemDLL("Gdi32.dll")
	pCreateCompatibleBitmap = g32.NewProc("CreateCompatibleBitmap")
	pCreateCompatibleDC     = g32.NewProc("CreateCompatibleDC")
	pDeleteDC               = g32.NewProc("DeleteDC")
	pSelectObject           = g32.NewProc("SelectObject")

	k32                 = windows.NewLazySystemDLL("Kernel32.dll")
	pGetModuleHandle    = k32.NewProc("GetModuleHandleW")
	pGetCurrentThreadId = k32.NewProc("GetCurrentThreadId")

	s32              = windows.NewLazySystemDLL("Shell32.dll")
	pShellNotifyIcon = s32.NewProc("Shell_NotifyIconW")

	u32                       = windows.NewLazySystemDLL("User32.dll")
	pCreateMenu               = u32.NewProc("CreateMenu")
	pCreatePopupMenu          = u32.NewProc("CreatePopupMenu")
	pCreateWindowEx           = u32.NewProc("CreateWindowExW")
	pDefWindowProc            = u32.NewProc("DefWindowProcW")
	pRemoveMenu               = u32.NewProc("RemoveMenu")
	pDestroyWindow            = u32.NewProc("DestroyWindow")
	pDispatchMessage          = u32.NewProc("DispatchMessageW")
	pDrawIconEx               = u32.NewProc("DrawIconEx")
	pAttachThreadInput        = u32.NewProc("AttachThreadInput")
	pBringWindowToTop         = u32.NewProc("BringWindowToTop")
	pGetCursorPos             = u32.NewProc("GetCursorPos")
	pGetDC                    = u32.NewProc("GetDC")
	pGetForegroundWindow      = u32.NewProc("GetForegroundWindow")
	pGetMessage               = u32.NewProc("GetMessageW")
	pGetSystemMetrics         = u32.NewProc("GetSystemMetrics")
	pGetWindowThreadProcessId = u32.NewProc("GetWindowThreadProcessId")
	pIsHungAppWindow          = u32.NewProc("IsHungAppWindow")
	pInsertMenuItem           = u32.NewProc("InsertMenuItemW")
	pLoadCursor               = u32.NewProc("LoadCursorW")
	pLoadIcon                 = u32.NewProc("LoadIconW")
	pLoadImage                = u32.NewProc("LoadImageW")
	pPostMessage              = u32.NewProc("PostMessageW")
	pPostQuitMessage          = u32.NewProc("PostQuitMessage")
	pRegisterClass            = u32.NewProc("RegisterClassExW")
	pRegisterWindowMessage    = u32.NewProc("RegisterWindowMessageW")
	pReleaseDC                = u32.NewProc("ReleaseDC")
	pSetForegroundWindow      = u32.NewProc("SetForegroundWindow")
	pAllowSetForegroundWindow = u32.NewProc("AllowSetForegroundWindow")
	pSetMenuInfo               = u32.NewProc("SetMenuInfo")
	pSetMenuItemInfo           = u32.NewProc("SetMenuItemInfoW")
	pShowWindow               = u32.NewProc("ShowWindow")
	pTrackPopupMenu           = u32.NewProc("TrackPopupMenu")
	pTranslateMessage         = u32.NewProc("TranslateMessage")
	pUnregisterClass          = u32.NewProc("UnregisterClassW")
	pUpdateWindow             = u32.NewProc("UpdateWindow")
	pCreateIconIndirect       = u32.NewProc("CreateIconIndirect")
)

// ============================================================
// Structures (copied from pcguy)
// ============================================================

type wndClassEx struct {
	Size, Style                        uint32
	WndProc                            uintptr
	ClsExtra, WndExtra                 int32
	Instance, Icon, Cursor, Background windows.Handle
	MenuName, ClassName                *uint16
	IconSm                             windows.Handle
}

func (w *wndClassEx) register() error {
	w.Size = uint32(unsafe.Sizeof(*w))
	res, _, err := pRegisterClass.Call(uintptr(unsafe.Pointer(w)))
	if res == 0 {
		return err
	}
	return nil
}

func (w *wndClassEx) unregister() error {
	res, _, err := pUnregisterClass.Call(
		uintptr(unsafe.Pointer(w.ClassName)),
		uintptr(w.Instance),
	)
	if res == 0 {
		return err
	}
	return nil
}

// notifyIconData — NOTE: Timeout and Version are separate fields (NOT a union).
// This matches pcguy's definition. The struct is 984 bytes on amd64,
// but CbSize is set to unsafe.Sizeof which Windows accepts.
type notifyIconData struct {
	Size                       uint32
	Wnd                        windows.Handle
	ID, Flags, CallbackMessage uint32
	Icon                       windows.Handle
	Tip                        [128]uint16
	State, StateMask           uint32
	Info                       [256]uint16
	Timeout, Version           uint32
	InfoTitle                  [64]uint16
	InfoFlags                  uint32
	GuidItem                   windows.GUID
	BalloonIcon                windows.Handle
}

func (nid *notifyIconData) add() error {
	const NIM_ADD = 0x00000000
	res, _, err := pShellNotifyIcon.Call(
		uintptr(NIM_ADD),
		uintptr(unsafe.Pointer(nid)),
	)
	if res == 0 {
		return err
	}
	return nil
}

func (nid *notifyIconData) modify() error {
	const NIM_MODIFY = 0x00000001
	res, _, err := pShellNotifyIcon.Call(
		uintptr(NIM_MODIFY),
		uintptr(unsafe.Pointer(nid)),
	)
	if res == 0 {
		return err
	}
	// Re-set version after every modify (pcguy's keepalive trick).
	nid.setVersion()
	return nil
}

func (nid *notifyIconData) delete() error {
	const NIM_DELETE = 0x00000002
	res, _, err := pShellNotifyIcon.Call(
		uintptr(NIM_DELETE),
		uintptr(unsafe.Pointer(nid)),
	)
	if res == 0 {
		return err
	}
	return nil
}

func (nid *notifyIconData) setVersion() error {
	const NIM_SETVERSION = 0x0000000A
	const NOTIFYICON_VERSION_4 = 4
	nid.Version = NOTIFYICON_VERSION_4
	nid.Size = uint32(unsafe.Sizeof(*nid))
	res, _, err := pShellNotifyIcon.Call(
		uintptr(NIM_SETVERSION),
		uintptr(unsafe.Pointer(nid)),
	)
	if res == 0 {
		return err
	}
	return nil
}

type menuItemInfo struct {
	Size, Mask, Type, State     uint32
	ID                          uint32
	SubMenu, Checked, Unchecked windows.Handle
	ItemData                    uintptr
	TypeData                    *uint16
	Cch                         uint32
	BMPItem                     windows.Handle
}

type point struct {
	X, Y int32
}

// ============================================================
// winTray — the tray implementation (mirrors pcguy's winTray)
// ============================================================

type winTray struct {
	instance,
	icon,
	cursor,
	window windows.Handle

	loadedImages   map[string]windows.Handle
	muLoadedImages sync.RWMutex
	menus          map[uint32]windows.Handle
	muMenus        sync.RWMutex
	menuOf         map[uint32]windows.Handle
	muMenuOf       sync.RWMutex
	menuItemIcons   map[uint32]windows.Handle
	muMenuItemIcons sync.RWMutex
	visibleItems   map[uint32][]uint32
	muVisibleItems sync.RWMutex

	nid   *notifyIconData
	muNID sync.RWMutex
	wcex  *wndClassEx

	wmSystrayMessage,
	wmTaskbarCreated uint32

	// SamVNC-specific fields
	menuItems  []MenuItem
	callback   func(string)
	tooltip    string
	menuIDMap  map[uint32]string
	nextMenuID uint32
}

func newTray() Tray {
	return &winTray{
		menuIDMap:  map[uint32]string{},
		nextMenuID: 1000,
	}
}

// ============================================================
// Async IPC worker (identical to pcguy)
// ============================================================

var notifyCh chan func()

func asyncNotify(fn func()) {
	if notifyCh == nil {
		fn()
		return
	}
	select {
	case notifyCh <- fn:
	default:
		select {
		case <-notifyCh:
		default:
		}
		select {
		case notifyCh <- fn:
		default:
		}
	}
}

func (t *winTray) asyncModify() {
	asyncNotify(func() {
		t.muNID.Lock()
		if t.nid != nil {
			t.nid.modify()
		}
		t.muNID.Unlock()
	})
}

// ============================================================
// setIcon / setTooltip (from pcguy)
// ============================================================

func (t *winTray) setIcon(src string) error {
	const NIF_ICON = 0x00000002
	h, err := t.loadIconFrom(src)
	if err != nil {
		return err
	}
	t.muNID.Lock()
	t.nid.Icon = h
	t.nid.Flags |= NIF_ICON
	t.nid.Size = uint32(unsafe.Sizeof(*t.nid))
	t.muNID.Unlock()
	t.asyncModify()
	return nil
}

func (t *winTray) setTooltip(src string) error {
	const NIF_TIP = 0x00000004
	b, err := windows.UTF16FromString(src)
	if err != nil {
		return err
	}
	t.muNID.Lock()
	copy(t.nid.Tip[:], b[:])
	t.nid.Flags |= NIF_TIP
	t.nid.Size = uint32(unsafe.Sizeof(*t.nid))
	t.muNID.Unlock()
	t.asyncModify()
	return nil
}

// ============================================================
// wndProc (from pcguy, adapted for SamVNC)
// ============================================================

func (t *winTray) wndProc(hWnd windows.Handle, message uint32, wParam, lParam uintptr) (lResult uintptr) {
	const (
		WM_RBUTTONUP     = 0x0205
		WM_LBUTTONUP     = 0x0202
		WM_LBUTTONDBLCLK = 0x0203
		WM_CONTEXTMENU   = 0x007B
		WM_COMMAND       = 0x0111
		WM_ENDSESSION    = 0x0016
		WM_CLOSE         = 0x0010
		WM_DESTROY       = 0x0002
	)
	switch message {
	case WM_COMMAND:
		menuID := uint32(wParam) & 0xFFFF
		trayLog("[tray] WM_COMMAND: menuID=%d\n", menuID)
		if t.callback != nil {
			if itemID, ok := t.menuIDMap[menuID]; ok {
				trayLog("[tray] callback: itemID=%q\n", itemID)
				t.callback(itemID)
			} else {
				trayLog("[tray] no menuID %d in map\n", menuID)
			}
		}
	case WM_CLOSE:
		pDestroyWindow.Call(uintptr(t.window))
		t.wcex.unregister()
	case WM_DESTROY:
		defer pPostQuitMessage.Call(uintptr(int32(0)))
		fallthrough
	case WM_ENDSESSION:
		t.muNID.RLock()
		nid := t.nid
		t.muNID.RUnlock()
		if nid != nil {
			asyncNotify(func() { nid.delete() })
		}
	case t.wmSystrayMessage:
		trayMsg := lParam & 0xFFFF
		trayLog("[tray] tray msg=0x%04X\n", trayMsg)
		switch trayMsg {
		case WM_LBUTTONDBLCLK:
			// Double-click: could trigger default action
		case WM_LBUTTONUP:
			// Single left-click: show menu (SamVNC behavior)
			trayLog("[tray] left-click -> showMenu\n")
			t.showMenu()
		case WM_RBUTTONUP, WM_CONTEXTMENU:
			trayLog("[tray] right-click -> showMenu\n")
			t.showMenu()
		}
	case t.wmTaskbarCreated:
		trayLog("[tray] TaskbarCreated -> re-add icon\n")
		t.muNID.RLock()
		nid := t.nid
		t.muNID.RUnlock()
		if nid != nil {
			asyncNotify(func() {
				nid.add()
				if err := nid.setVersion(); err != nil {
					trayLog("[tray] taskbarCreated: setVersion err: %v\n", err)
				} else {
					trayLog("[tray] taskbarCreated: icon re-added\n")
				}
			})
		}
	default:
		lResult, _, _ = pDefWindowProc.Call(
			uintptr(hWnd),
			uintptr(message),
			uintptr(wParam),
			uintptr(lParam),
		)
	}
	return
}

// ============================================================
// initInstance (from pcguy — creates window, NIM_ADD, setVersion)
// ============================================================

func (t *winTray) initInstance() error {
	const IDI_APPLICATION = 32512
	const IDC_ARROW = 32512
	const SW_HIDE = 0
	const CW_USEDEFAULT = 0x80000000
	const (
		WS_CAPTION     = 0x00C00000
		WS_MAXIMIZEBOX = 0x00010000
		WS_MINIMIZEBOX = 0x00020000
		WS_OVERLAPPED  = 0x00000000
		WS_SYSMENU     = 0x00080000
		WS_THICKFRAME  = 0x00040000

		WS_OVERLAPPEDWINDOW = WS_OVERLAPPED | WS_CAPTION | WS_SYSMENU | WS_THICKFRAME | WS_MINIMIZEBOX | WS_MAXIMIZEBOX
	)
	const (
		CS_HREDRAW = 0x0002
		CS_VREDRAW = 0x0001
	)
	const NIF_MESSAGE = 0x00000001
	const WM_USER = 0x0400

	const (
		className  = "SamVNCTrayClass"
		windowName = ""
	)

	t.wmSystrayMessage = WM_USER + 1
	t.visibleItems = make(map[uint32][]uint32)
	t.menus = make(map[uint32]windows.Handle)
	t.menuOf = make(map[uint32]windows.Handle)
	t.menuItemIcons = make(map[uint32]windows.Handle)

	taskbarEventNamePtr, _ := windows.UTF16PtrFromString("TaskbarCreated")
	res, _, _ := pRegisterWindowMessage.Call(
		uintptr(unsafe.Pointer(taskbarEventNamePtr)),
	)
	t.wmTaskbarCreated = uint32(res)
	trayLog("[tray] wmTaskbarCreated=%d wmSystrayMessage=%d\n", t.wmTaskbarCreated, t.wmSystrayMessage)

	t.loadedImages = make(map[string]windows.Handle)

	instanceHandle, _, _ := pGetModuleHandle.Call(0)
	if instanceHandle == 0 {
		return fmt.Errorf("GetModuleHandleW failed")
	}
	t.instance = windows.Handle(instanceHandle)

	iconHandle, _, _ := pLoadIcon.Call(0, uintptr(IDI_APPLICATION))
	if iconHandle == 0 {
		return fmt.Errorf("LoadIconW failed")
	}
	t.icon = windows.Handle(iconHandle)

	cursorHandle, _, _ := pLoadCursor.Call(0, uintptr(IDC_ARROW))
	if cursorHandle == 0 {
		return fmt.Errorf("LoadCursorW failed")
	}
	t.cursor = windows.Handle(cursorHandle)

	classNamePtr, _ := windows.UTF16PtrFromString(className)
	windowNamePtr, _ := windows.UTF16PtrFromString(windowName)

	t.wcex = &wndClassEx{
		Style:      CS_HREDRAW | CS_VREDRAW,
		WndProc:    windows.NewCallback(t.wndProc),
		Instance:   t.instance,
		Icon:       t.icon,
		Cursor:     t.cursor,
		Background: windows.Handle(6),
		ClassName:  classNamePtr,
		IconSm:     t.icon,
	}
	if err := t.wcex.register(); err != nil {
		return fmt.Errorf("RegisterClassExW failed: %v", err)
	}

	windowHandle, _, _ := pCreateWindowEx.Call(
		uintptr(0),
		uintptr(unsafe.Pointer(classNamePtr)),
		uintptr(unsafe.Pointer(windowNamePtr)),
		uintptr(WS_OVERLAPPEDWINDOW),
		uintptr(CW_USEDEFAULT),
		uintptr(CW_USEDEFAULT),
		uintptr(CW_USEDEFAULT),
		uintptr(CW_USEDEFAULT),
		uintptr(0),
		uintptr(0),
		uintptr(t.instance),
		uintptr(0),
	)
	if windowHandle == 0 {
		return fmt.Errorf("CreateWindowExW failed")
	}
	t.window = windows.Handle(windowHandle)

	pShowWindow.Call(uintptr(t.window), uintptr(SW_HIDE))
	pUpdateWindow.Call(uintptr(t.window))

	// NIM_ADD with ONLY NIF_MESSAGE (like pcguy).
	t.muNID.Lock()
	defer t.muNID.Unlock()
	t.nid = &notifyIconData{
		Wnd:             windows.Handle(t.window),
		ID:              100,
		Flags:           NIF_MESSAGE,
		CallbackMessage: t.wmSystrayMessage,
	}
	t.nid.Size = uint32(unsafe.Sizeof(*t.nid))

	if err := t.nid.add(); err != nil {
		return fmt.Errorf("NIM_ADD failed: %v", err)
	}
	trayLog("[tray] NIM_ADD success\n")

	// Set version to 4 (critical for right-click messages).
	if err := t.nid.setVersion(); err != nil {
		trayLog("[tray] setVersion failed: %v\n", err)
	} else {
		trayLog("[tray] setVersion success\n")
	}

	return nil
}

// ============================================================
// createMenu (from pcguy)
// ============================================================

func (t *winTray) createMenu() error {
	const MIM_APPLYTOSUBMENUS = 0x80000000

	menuHandle, _, _ := pCreatePopupMenu.Call()
	if menuHandle == 0 {
		return fmt.Errorf("CreatePopupMenu failed")
	}
	t.menus[0] = windows.Handle(menuHandle)

	mi := struct {
		Size, Mask, Style, Max uint32
		Background             windows.Handle
		ContextHelpID          uint32
		MenuData               uintptr
	}{
		Mask: MIM_APPLYTOSUBMENUS,
	}
	mi.Size = uint32(unsafe.Sizeof(mi))

	res, _, _ := pSetMenuInfo.Call(
		uintptr(t.menus[0]),
		uintptr(unsafe.Pointer(&mi)),
	)
	if res == 0 {
		return fmt.Errorf("SetMenuInfo failed")
	}
	return nil
}

// ============================================================
// Menu item helpers (from pcguy)
// ============================================================

func (t *winTray) convertToSubMenu(menuItemId uint32) (windows.Handle, error) {
	const MIIM_SUBMENU = 0x00000004

	res, _, _ := pCreateMenu.Call()
	if res == 0 {
		return 0, fmt.Errorf("CreateMenu failed")
	}
	menu := windows.Handle(res)

	mi := menuItemInfo{Mask: MIIM_SUBMENU, SubMenu: menu}
	mi.Size = uint32(unsafe.Sizeof(mi))
	t.muMenuOf.RLock()
	hMenu := t.menuOf[menuItemId]
	t.muMenuOf.RUnlock()
	res, _, _ = pSetMenuItemInfo.Call(
		uintptr(hMenu),
		uintptr(menuItemId),
		0,
		uintptr(unsafe.Pointer(&mi)),
	)
	if res == 0 {
		return 0, fmt.Errorf("SetMenuItemInfo failed")
	}
	t.muMenus.Lock()
	t.menus[menuItemId] = menu
	t.muMenus.Unlock()
	return menu, nil
}

func (t *winTray) addOrUpdateMenuItem(menuItemId uint32, parentId uint32, title string, disabled, checked bool) error {
	const (
		MIIM_FTYPE   = 0x00000100
		MIIM_STRING  = 0x00000040
		MIIM_SUBMENU = 0x00000004
		MIIM_ID      = 0x00000002
		MIIM_STATE   = 0x00000001
	)
	const MFT_STRING = 0x00000000
	const (
		MFS_CHECKED  = 0x00000008
		MFS_DISABLED = 0x00000003
	)
	titlePtr, _ := windows.UTF16PtrFromString(title)

	mi := menuItemInfo{
		Mask:     MIIM_FTYPE | MIIM_STRING | MIIM_ID | MIIM_STATE,
		Type:     MFT_STRING,
		ID:       uint32(menuItemId),
		TypeData: titlePtr,
		Cch:      uint32(len(title)),
	}
	mi.Size = uint32(unsafe.Sizeof(mi))
	if disabled {
		mi.State |= MFS_DISABLED
	}
	if checked {
		mi.State |= MFS_CHECKED
	}

	var res uintptr
	t.muMenus.RLock()
	menu, exists := t.menus[parentId]
	t.muMenus.RUnlock()
	if !exists {
		var err error
		menu, err = t.convertToSubMenu(parentId)
		if err != nil {
			return err
		}
		t.muMenus.Lock()
		t.menus[parentId] = menu
		t.muMenus.Unlock()
	} else if t.getVisibleItemIndex(parentId, menuItemId) != -1 {
		res, _, _ = pSetMenuItemInfo.Call(
			uintptr(menu),
			uintptr(menuItemId),
			0,
			uintptr(unsafe.Pointer(&mi)),
		)
	}

	if res == 0 {
		t.muMenus.RLock()
		submenu, exists := t.menus[menuItemId]
		t.muMenus.RUnlock()
		if exists {
			mi.Mask |= MIIM_SUBMENU
			mi.SubMenu = submenu
		}
		t.addToVisibleItems(parentId, menuItemId)
		position := t.getVisibleItemIndex(parentId, menuItemId)
		res, _, _ = pInsertMenuItem.Call(
			uintptr(menu),
			uintptr(position),
			1,
			uintptr(unsafe.Pointer(&mi)),
		)
		if res == 0 {
			t.delFromVisibleItems(parentId, menuItemId)
			return fmt.Errorf("InsertMenuItemW failed")
		}
		t.muMenuOf.Lock()
		t.menuOf[menuItemId] = menu
		t.muMenuOf.Unlock()
	}

	return nil
}

func (t *winTray) addSeparatorMenuItem(menuItemId, parentId uint32) error {
	const (
		MIIM_FTYPE = 0x00000100
		MIIM_ID    = 0x00000002
		MIIM_STATE = 0x00000001
	)
	const MFT_SEPARATOR = 0x00000800

	mi := menuItemInfo{
		Mask: MIIM_FTYPE | MIIM_ID | MIIM_STATE,
		Type: MFT_SEPARATOR,
		ID:   uint32(menuItemId),
	}
	mi.Size = uint32(unsafe.Sizeof(mi))

	t.addToVisibleItems(parentId, menuItemId)
	position := t.getVisibleItemIndex(parentId, menuItemId)
	t.muMenus.RLock()
	menu := uintptr(t.menus[parentId])
	t.muMenus.RUnlock()
	res, _, _ := pInsertMenuItem.Call(
		menu,
		uintptr(position),
		1,
		uintptr(unsafe.Pointer(&mi)),
	)
	if res == 0 {
		return fmt.Errorf("InsertMenuItemW separator failed")
	}
	return nil
}

func (t *winTray) hideMenuItem(menuItemId, parentId uint32) error {
	const MF_BYCOMMAND = 0x00000000
	const ERROR_SUCCESS syscall.Errno = 0

	t.muMenus.RLock()
	menu := uintptr(t.menus[parentId])
	t.muMenus.RUnlock()
	res, _, err := pRemoveMenu.Call(
		menu,
		uintptr(menuItemId),
		MF_BYCOMMAND,
	)
	if res == 0 && err.(syscall.Errno) != ERROR_SUCCESS {
		return err
	}
	t.delFromVisibleItems(parentId, menuItemId)
	return nil
}

func (t *winTray) delFromVisibleItems(parent, val uint32) {
	t.muVisibleItems.Lock()
	defer t.muVisibleItems.Unlock()
	visibleItems := t.visibleItems[parent]
	for i, itemval := range visibleItems {
		if val == itemval {
			t.visibleItems[parent] = append(visibleItems[:i], visibleItems[i+1:]...)
			break
		}
	}
}

func (t *winTray) addToVisibleItems(parent, val uint32) {
	t.muVisibleItems.Lock()
	defer t.muVisibleItems.Unlock()
	if visibleItems, exists := t.visibleItems[parent]; !exists {
		t.visibleItems[parent] = []uint32{val}
	} else {
		newvisible := append(visibleItems, val)
		sort.Slice(newvisible, func(i, j int) bool { return newvisible[i] < newvisible[j] })
		t.visibleItems[parent] = newvisible
	}
}

func (t *winTray) getVisibleItemIndex(parent, val uint32) int {
	t.muVisibleItems.RLock()
	defer t.muVisibleItems.RUnlock()
	for i, itemval := range t.visibleItems[parent] {
		if val == itemval {
			return i
		}
	}
	return -1
}

// ============================================================
// loadIconFrom (from pcguy)
// ============================================================

func (t *winTray) loadIconFrom(src string) (windows.Handle, error) {
	const IMAGE_ICON = 1
	const LR_LOADFROMFILE = 0x00000010
	const LR_DEFAULTSIZE = 0x00000040

	t.muLoadedImages.RLock()
	h, ok := t.loadedImages[src]
	t.muLoadedImages.RUnlock()
	if !ok {
		srcPtr, err := windows.UTF16PtrFromString(src)
		if err != nil {
			return 0, err
		}
		res, _, err := pLoadImage.Call(
			0,
			uintptr(unsafe.Pointer(srcPtr)),
			IMAGE_ICON,
			0,
			0,
			LR_LOADFROMFILE|LR_DEFAULTSIZE,
		)
		if res == 0 {
			return 0, err
		}
		h = windows.Handle(res)
		t.muLoadedImages.Lock()
		t.loadedImages[src] = h
		t.muLoadedImages.Unlock()
	}
	return h, nil
}

// ============================================================
// showMenu (from pcguy — exact copy)
// ============================================================

func (t *winTray) showMenu() {
	const (
		TPM_RIGHTBUTTON = 0x0002
		TPM_BOTTOMALIGN = 0x0020
		TPM_LEFTALIGN   = 0x0000
	)
	p := point{}
	res, _, _ := pGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	if res == 0 {
		trayLog("[tray] showMenu: GetCursorPos failed\n")
		return
	}

	const ASFW_ANY = 0xFFFFFFFF
	pAllowSetForegroundWindow.Call(uintptr(ASFW_ANY))

	fg, _, _ := pGetForegroundWindow.Call()
	if fg != 0 {
		if hung, _, _ := pIsHungAppWindow.Call(fg); hung != 0 {
			trayLog("[tray] showMenu: foreground window hung, skip fg switch\n")
		} else if fg != uintptr(t.window) {
			me, _, _ := pGetCurrentThreadId.Call()
			fgTid, _, _ := pGetWindowThreadProcessId.Call(fg, 0)
			if fgTid != 0 && fgTid != me {
				pAttachThreadInput.Call(me, fgTid, 1)
				pSetForegroundWindow.Call(uintptr(t.window))
				pBringWindowToTop.Call(uintptr(t.window))
				pAttachThreadInput.Call(me, fgTid, 0)
			} else {
				pSetForegroundWindow.Call(uintptr(t.window))
			}
		}
	}
	pPostMessage.Call(uintptr(t.window), 0, 0, 0)

	res, _, _ = pTrackPopupMenu.Call(
		uintptr(t.menus[0]),
		uintptr(TPM_RIGHTBUTTON|TPM_BOTTOMALIGN|TPM_LEFTALIGN),
		uintptr(p.X),
		uintptr(p.Y),
		0,
		uintptr(t.window),
		0,
	)
	trayLog("[tray] showMenu: track res=0x%x at (%d,%d)\n", res, p.X, p.Y)

	pPostMessage.Call(uintptr(t.window), 0, 0, 0)
}

// ============================================================
// Run — main entry (mirrors pcguy's registerSystray + nativeLoop)
// ============================================================

func (t *winTray) Run() error {
	initTrayLog()

	// Start IPC worker.
	notifyCh = make(chan func(), 32)
	go func() {
		for fn := range notifyCh {
			fn()
		}
	}()

	if err := t.initInstance(); err != nil {
		return fmt.Errorf("initInstance failed: %v", err)
	}

	if err := t.createMenu(); err != nil {
		return fmt.Errorf("createMenu failed: %v", err)
	}

	// Set icon from file.
	if iconPath := t.findIconFile(); iconPath != "" {
		if err := t.setIcon(iconPath); err != nil {
			trayLog("[tray] setIcon err: %v\n", err)
		} else {
			trayLog("[tray] icon set from %s\n", iconPath)
		}
	}

	// Set tooltip.
	if t.tooltip != "" {
		t.setTooltip(t.tooltip)
	}

	// Build initial menu.
	t.buildMenu()

	// Keepalive: periodically re-set NOTIFYICON_VERSION_4.
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			t.muNID.RLock()
			nid := t.nid
			t.muNID.RUnlock()
			if nid != nil {
				asyncNotify(func() {
					// NIM_MODIFY re-establishes the Explorer<->window connection
					// after RDP reconnect / session switch / DPI change.
					// setVersion alone is not enough (it fails on some systems).
					nid.modify()
				})
			}
		}
	}()

	// Message loop (identical to pcguy's nativeLoop).
	trayLog("[tray] entering message loop, window=0x%X\n", uintptr(t.window))
	m := &struct {
		WindowHandle windows.Handle
		Message      uint32
		Wparam       uintptr
		Lparam       uintptr
		Time         uint32
		Pt           point
	}{}
	for {
		ret, _, err := pGetMessage.Call(uintptr(unsafe.Pointer(m)), 0, 0, 0)
		switch int32(ret) {
		case -1:
			trayLog("[tray] message loop error: %v\n", err)
			return err
		case 0:
			trayLog("[tray] WM_QUIT received, exiting\n")
			return nil
		default:
			pTranslateMessage.Call(uintptr(unsafe.Pointer(m)))
			pDispatchMessage.Call(uintptr(unsafe.Pointer(m)))
		}
	}
}

// ============================================================
// SamVNC-specific helpers
// ============================================================

// findIconFile locates tray_icon.ico.
func (t *winTray) findIconFile() string {
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
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

// buildMenu rebuilds the popup menu from t.menuItems.
func (t *winTray) buildMenu() {
	// Clear existing menu items.
	t.muVisibleItems.Lock()
	t.visibleItems = make(map[uint32][]uint32)
	t.muVisibleItems.Unlock()

	// Destroy old menu and create new.
	if old := t.menus[0]; old != 0 {
		pRemoveMenu.Call(uintptr(old), 0, 0) // not ideal but works
	}
	menuHandle, _, _ := pCreatePopupMenu.Call()
	if menuHandle == 0 {
		return
	}
	t.menus[0] = windows.Handle(menuHandle)

	t.menuIDMap = map[uint32]string{}
	menuID := uint32(2000)

	for _, item := range t.menuItems {
		if item.Separator {
			t.addSeparatorMenuItem(menuID, 0)
			menuID++
			continue
		}
		t.addOrUpdateMenuItem(menuID, 0, item.Label, item.Disabled, item.Checked)
		t.menuIDMap[menuID] = item.ID
		menuID++
	}
}

// ============================================================
// Tray interface implementation
// ============================================================

func (t *winTray) SetMenu(items []MenuItem) {
	t.menuItems = items
	if t.window != 0 {
		t.buildMenu()
	}
}

func (t *winTray) SetTooltip(text string) {
	t.tooltip = text
	if t.nid != nil {
		t.setTooltip(text)
	}
}

func (t *winTray) SetCallback(cb func(string)) {
	t.callback = cb
}

func (t *winTray) Stop() {
	if t.window != 0 {
		const WM_CLOSE = 0x0010
		pPostMessage.Call(uintptr(t.window), WM_CLOSE, 0, 0)
	}
}

// Ensure syscall is referenced.
var _ = syscall.Handle(0)
