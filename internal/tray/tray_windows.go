//go:build windows

package tray

import (
        "fmt"
        "os"
        "path/filepath"
        "runtime"
        "sync"
        "syscall"
        "time"
        "unsafe"

        "golang.org/x/sys/windows"
)

// trayIconGUID is the stable identity of the SamVNC tray icon.
//
// The icon is registered with NIF_GUID so Windows identifies it by GUID
// instead of (hwnd, uID). Without a stable identity, every process restart
// registers a brand-new icon: Windows 11 keeps one "NotifyIconSettings"
// entry per registration, the taskbar accumulates stale ("ghost") icons of
// dead processes, and clicking a ghost produces no callback at all — the
// "menu sometimes does not appear" symptom. With NIF_GUID a re-add replaces
// the same logical icon and the user's taskbar/overflow preference sticks.
var trayIconGUID = windows.GUID{
        Data1: 0x8F7B2A51,
        Data2: 0x4C3D,
        Data3: 0x49E2,
        Data4: [8]byte{0xB1, 0x0A, 0x6E, 0x5C, 0x3D, 0x9F, 0x2B, 0x74},
}

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
        pCreateMutex        = k32.NewProc("CreateMutexW")
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
        pDestroyMenu              = u32.NewProc("DestroyMenu")
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

// notifyIconData mirrors the Win32 NOTIFYICONDATAW layout EXACTLY.
//
// CRITICAL: in C, uTimeout and uVersion are a UNION occupying the same
// 4 bytes (offset 816 on amd64). They must be a SINGLE field here.
// The previous definition declared them as two separate uint32 fields,
// which shifted every following member by +4 (struct size 984 instead of
// 976). Windows then read uVersion=0 from the union when setVersion wrote
// 4 into the wrong slot, so NIM_SETVERSION failed with "Unspecified error"
// and the icon never entered NOTIFYICON_VERSION_4 mode.
type notifyIconData struct {
        Size                       uint32
        Wnd                        windows.Handle
        ID, Flags, CallbackMessage uint32
        Icon                       windows.Handle
        Tip                        [128]uint16
        State, StateMask           uint32
        Info                       [256]uint16
        TimeoutVersion             uint32 // union { UINT uTimeout; UINT uVersion; }
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
        // NIM_SETVERSION is 0x04 per shellapi.h (NOT 0x0A — that value is
        // invalid and made Shell_NotifyIcon fail with "Unspecified error").
        const NIM_SETVERSION = 0x00000004
        const NOTIFYICON_VERSION_4 = 4
        nid.TimeoutVersion = NOTIFYICON_VERSION_4
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

        // menuOpen/pendingRebuild are only touched on the window thread.
        // TrackPopupMenu runs a modal loop that still dispatches posted
        // messages, so a SetMenu() arriving while the menu is open must be
        // deferred — rebuilding (destroying) a menu that is being tracked
        // makes the open menu vanish or future clicks hit a freed handle.
        menuOpen       bool
        pendingRebuild bool

        // Menu-trigger dedup. With NOTIFYICON_VERSION_4 the shell reports a
        // single physical right-click as BOTH the raw WM_RBUTTONUP and the
        // synthetic WM_CONTEXTMENU (and a left-click as NIN_SELECT plus
        // WM_LBUTTONUP). Each of those used to call showMenu(), producing
        // TWO overlapping TrackPopupMenu modal loops on the same menu.
        // A single click then ends both loops and EACH posts its own
        // WM_COMMAND — two commands for one click. Combined with sequential
        // menu-ID assignment this remapped the second command onto a
        // DIFFERENT item after a rebuild: clicking Stop killed the server,
        // the duplicate command landed on the rebuilt menu's "start" slot
        // and the server instantly restarted ("Stop does not stop").
        // lastMenuTrigger collapses the duplicate notifications; stable
        // per-item menu IDs (see stableMenuIDs) make any stray duplicate
        // command harmless even if it still slips through.
        lastMenuTrigger time.Time

        // SamVNC-specific fields
        muItems      sync.RWMutex
        menuItems    []MenuItem
        callback     func(string)
        tooltip      string
        menuIDMap    map[uint32]string
        nextMenuID   uint32
        wmBuildMenuMessage uint32 // internal: rebuild menu on the window thread
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
                // NOTIFYICON_VERSION_4 reports a plain left-click as
                // NIN_SELECT instead of WM_LBUTTONUP (WM_CONTEXTMENU already
                // covers right-click). Legacy (version 0) still sends the
                // raw button messages, so both spellings are handled.
                NIN_SELECT = 0x0400
        )
        switch message {
        case WM_COMMAND:
                menuID := uint32(wParam) & 0xFFFF
                trayLog("[tray] WM_COMMAND: menuID=%d\n", menuID)
                // buildMenu replaces menuIDMap from the window thread; read
                // it under the same lock (concurrent map access can panic).
                t.muMenuOf.RLock()
                itemID, ok := t.menuIDMap[menuID]
                t.muMenuOf.RUnlock()
                if t.callback != nil {
                        if ok {
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
        case t.wmBuildMenuMessage:
                // Rebuild the menu on the window thread. This serializes
                // menu construction with TrackPopupMenu inside showMenu,
                // eliminating the race where a right-click lands while a
                // background goroutine is mid-rebuild (empty/partial menu).
                if t.menuOpen {
                        trayLog("[tray] wmBuildMenu deferred (menu open)\n")
                        t.pendingRebuild = true
                        return
                }
                trayLog("[tray] wmBuildMenu -> rebuild\n")
                t.buildMenu()
        case t.wmSystrayMessage:
                trayMsg := lParam & 0xFFFF
                trayLog("[tray] tray msg=0x%04X wParam=0x%X\n", trayMsg, wParam)
                switch trayMsg {
                case WM_LBUTTONDBLCLK:
                        // Double-click: could trigger default action
                case WM_LBUTTONUP, NIN_SELECT:
                        // Single left-click: show menu (SamVNC behavior)
                        t.menuTrigger("left-click")
                case WM_RBUTTONUP, WM_CONTEXTMENU:
                        t.menuTrigger("right-click")
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
        const NIF_GUID = 0x00000020
        const WM_USER = 0x0400

        const (
                className  = "SamVNCTrayClass"
                windowName = ""
        )

        t.wmSystrayMessage = WM_USER + 1
        t.wmBuildMenuMessage = WM_USER + 2 // unused slot in our own window class
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
        trayLog("[tray] sizeof(NOTIFYICONDATA)=%d (expect 976 on amd64)\n", unsafe.Sizeof(notifyIconData{}))

        t.muNID.Lock()
        defer t.muNID.Unlock()
        t.nid = &notifyIconData{
                Wnd:             windows.Handle(t.window),
                ID:              100,
                Flags:           NIF_MESSAGE | NIF_GUID,
                CallbackMessage: t.wmSystrayMessage,
                GuidItem:        trayIconGUID,
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
                // IMPORTANT: append in ARRIVAL order — this list mirrors the real
                // menu item positions (getVisibleItemIndex feeds InsertMenuItem).
                // The old sort-by-ID broke the popup layout: separators use the
                // 2090+n range while action items are 2101+, so every separator's
                // "index" resolved ABOVE the first real item and all three lines
                // rendered as a lump of horizontal rules above "Stop".
                t.visibleItems[parent] = append(visibleItems, val)
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
        // Re-entrancy guard FIRST: TrackPopupMenu's modal loop dispatches
        // tray messages, so showMenu can be re-entered while a menu is
        // already tracking. A second nested TrackPopupMenu on the same menu
        // handle yields one WM_COMMAND per tracking loop for a single click.
        if t.menuOpen {
                trayLog("[tray] showMenu: menu already open, ignore\n")
                return
        }
        t.muMenus.RLock()
        menu := t.menus[0]
        t.muMenus.RUnlock()
        if menu == 0 {
                trayLog("[tray] showMenu: no popup menu, rebuilding\n")
                t.buildMenu()
                t.muMenus.RLock()
                menu = t.menus[0]
                t.muMenus.RUnlock()
                if menu == 0 {
                        trayLog("[tray] showMenu: menu still missing, giving up\n")
                        return
                }
        }

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

        // Mark the menu as open: TrackPopupMenu runs a modal message loop
        // that still dispatches posted messages, so buildMenu must not run
        // (and destroy this menu) while tracking. Deferred rebuilds run here.
        t.menuOpen = true
        res, _, _ = pTrackPopupMenu.Call(
                uintptr(menu),
                uintptr(TPM_RIGHTBUTTON|TPM_BOTTOMALIGN|TPM_LEFTALIGN),
                uintptr(p.X),
                uintptr(p.Y),
                0,
                uintptr(t.window),
                0,
        )
        t.menuOpen = false
        trayLog("[tray] showMenu: track res=0x%x at (%d,%d)\n", res, p.X, p.Y)

        pPostMessage.Call(uintptr(t.window), 0, 0, 0)

        if t.pendingRebuild {
                t.pendingRebuild = false
                trayLog("[tray] running deferred rebuild\n")
                t.buildMenu()
        }
}

// ============================================================
// Run — main entry (mirrors pcguy's registerSystray + nativeLoop)
// ============================================================

func (t *winTray) Run() error {
        // CRITICAL: Win32 windows have THREAD AFFINITY. initInstance creates
        // the message window on the CURRENT OS thread, and GetMessage must
        // run on that SAME thread to retrieve messages posted to it.
        // Without LockOSThread the Go scheduler is free to migrate this
        // goroutine to a different OS thread after any syscall — GetMessage
        // then blocks on the WRONG thread's queue forever. The window's own
        // thread stops pumping (IsHungAppWindow reports the window as hung)
        // and every tray click is silently swallowed: the icon is visible
        // but right-click never shows the menu. Whether the goroutine happens
        // to migrate is a scheduling race, which made the bug intermittent.
        runtime.LockOSThread()
        defer runtime.UnlockOSThread()

        // Single instance: a second tray would register a second icon and
        // fight over the tunnel/server subprocess. If another instance is
        // already running, exit quietly.
        mutexNamePtr, _ := windows.UTF16PtrFromString("Local\\SamVNCTraySingleInstance")
        hMutex, _, mutexErr := pCreateMutex.Call(
                0,
                0,
                uintptr(unsafe.Pointer(mutexNamePtr)),
        )
        if hMutex != 0 && mutexErr == syscall.Errno(183) { // ERROR_ALREADY_EXISTS
                return fmt.Errorf("SamVNC tray is already running")
        }

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

        // Keepalive: periodically ping the icon with NIM_MODIFY.
        //
        // IMPORTANT: a successful NIM_MODIFY means the icon is healthy.
        // Do NOT delete/re-add the icon based on user inactivity — there are
        // no tray messages unless the user actually interacts with the icon,
        // so a quiet 30 seconds is perfectly normal. Deleting and re-adding
        // created recurring dead windows in which right-clicks were silently
        // swallowed (the "menu sometimes does not appear" bug).
        //
        // Only when NIM_MODIFY fails repeatedly (icon genuinely lost, e.g.
        // explorer restart that somehow missed TaskbarCreated) do we rebuild
        // the icon once.
        go func() {
                ticker := time.NewTicker(10 * time.Second)
                defer ticker.Stop()
                modifyFails := 0
                for range ticker.C {
                        t.muNID.RLock()
                        nid := t.nid
                        t.muNID.RUnlock()
                        if nid == nil {
                                continue
                        }
                        asyncNotify(func() {
                                // modifyFails is only touched here (the asyncNotify
                                // worker serializes these closures), so no lock needed.
                                if err := nid.modify(); err != nil {
                                        modifyFails++
                                        trayLog("[tray] keepalive: modify failed (%d): %v\n", modifyFails, err)
                                        if modifyFails >= 2 {
                                                trayLog("[tray] keepalive: icon lost, re-adding\n")
                                                nid.delete()
                                                nid.add()
                                                if err := nid.setVersion(); err != nil {
                                                        trayLog("[tray] keepalive: setVersion err: %v\n", err)
                                                }
                                                modifyFails = 0
                                        }
                                } else {
                                        modifyFails = 0
                                }
                        })
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

// menuTrigger funnels all menu-opening tray notifications through one
// dedup gate. See the lastMenuTrigger field comment for why this exists.
func (t *winTray) menuTrigger(kind string) {
        const menuDedupWindow = 600 * time.Millisecond
        now := time.Now()
        if !t.lastMenuTrigger.IsZero() && now.Sub(t.lastMenuTrigger) < menuDedupWindow {
                trayLog("[tray] %s -> showMenu SUPPRESSED (duplicate trigger within %v)\n",
                        kind, menuDedupWindow)
                return
        }
        t.lastMenuTrigger = now
        trayLog("[tray] %s -> showMenu\n", kind)
        t.showMenu()
}

// stableMenuIDs assign FIXED Win32 menu-item identifiers to well-known
// item IDs. Sequential IDs (2000, 2001, ...) shift meaning whenever the
// menu is rebuilt with a different first item (Start <-> Stop swap on
// every state change). A WM_COMMAND that arrives after a rebuild would
// then be interpreted as a DIFFERENT action — the "Stop restarts the
// server" bug. Fixed IDs make a stale command resolve to the SAME action:
// a duplicate "stop" is a harmless no-op instead of a "start".
var stableMenuIDs = map[string]uint32{
        "start":        2101,
        "stop":         2102,
        "autostart_on": 2103,
        "autostart_off": 2104,
        "settings":     2105,
        "copy_address": 2106,
        "exit":         2107,
}

// buildMenu rebuilds the popup menu from t.menuItems.
//
// Thread-safety: this runs on the window thread (invoked either directly
// from Run before the message loop starts, or via the wmBuildMenuMessage
// handler in wndProc). That guarantees menu construction never interleaves
// with TrackPopupMenu in showMenu.
func (t *winTray) buildMenu() {
        // Snapshot the desired items.
        t.muItems.RLock()
        items := make([]MenuItem, len(t.menuItems))
        copy(items, t.menuItems)
        t.muItems.RUnlock()

        // Clear existing menu items.
        t.muVisibleItems.Lock()
        t.visibleItems = make(map[uint32][]uint32)
        t.muVisibleItems.Unlock()

        // Destroy old menu and create new.
        t.muMenus.Lock()
        old := t.menus[0]
        if old != 0 {
                pDestroyMenu.Call(uintptr(old))
                delete(t.menus, 0)
        }
        t.muMenus.Unlock()
        menuHandle, _, _ := pCreatePopupMenu.Call()
        if menuHandle == 0 {
                trayLog("[tray] buildMenu: CreatePopupMenu failed\n")
                return
        }
        t.muMenus.Lock()
        t.menus[0] = windows.Handle(menuHandle)
        t.muMenus.Unlock()

        t.muMenuOf.Lock()
        t.menuIDMap = map[uint32]string{}
        t.muMenuOf.Unlock()
        // Separator IDs are indexed (max 3 separators in our menus);
        // unknown item IDs fall back to a non-colliding dynamic range.
        sepIndex := 0
        nextID := uint32(2200)

        for _, item := range items {
                if item.Separator {
                        sepID := uint32(2090 + sepIndex)
                        sepIndex++
                        t.addSeparatorMenuItem(sepID, 0)
                        continue
                }
                menuID, ok := stableMenuIDs[item.ID]
                if !ok {
                        menuID = nextID
                        nextID++
                }
                t.addOrUpdateMenuItem(menuID, 0, item.Label, item.Disabled, item.Checked)
                t.muMenuOf.Lock()
                t.menuIDMap[menuID] = item.ID
                t.muMenuOf.Unlock()
        }
}

// ============================================================
// Tray interface implementation
// ============================================================

func (t *winTray) SetMenu(items []MenuItem) {
        t.muItems.Lock()
        t.menuItems = items
        t.muItems.Unlock()
        if t.window != 0 {
                // Defer the rebuild to the window thread via a private message,
                // so it can never race with TrackPopupMenu in showMenu.
                pPostMessage.Call(uintptr(t.window), uintptr(t.wmBuildMenuMessage), 0, 0)
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
