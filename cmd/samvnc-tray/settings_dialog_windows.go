//go:build windows

package main

import (
        "fmt"
        "log"
        "strings"
        "syscall"
        "unsafe"

        "github.com/samaidev/samvnc/internal/settings"
        "golang.org/x/sys/windows"
)

// Win32 constants
const (
        WS_OVERLAPPEDWINDOW = 0x00CF0000
        WS_VISIBLE          = 0x10000000
        WS_CHILD            = 0x40000000
        WS_BORDER           = 0x00800000
        WS_TABSTOP          = 0x00010000
        WS_GROUP            = 0x00020000
        ES_AUTOHSCROLL      = 0x0080
        ES_PASSWORD         = 0x0020
        SS_LEFT             = 0x00000000
        BS_PUSHBUTTON       = 0x00000000
        BS_DEFPUSHBUTTON    = 0x00000001
        BS_AUTOCHECKBOX     = 0x00000003

        // Combo box (language selector).
        CBS_DROPDOWNLIST = 0x0003
        CB_ADDSTRING     = 0x0143
        CB_GETCURSEL     = 0x0147
        CB_SETCURSEL     = 0x014E

        WM_COMMAND    = 0x0111
        WM_CLOSE      = 0x0010
        WM_DESTROY    = 0x0002
        WM_GETTEXT    = 0x000D
        WM_GETTEXTLENGTH = 0x000E
        WM_SETTEXT    = 0x000C
        BM_GETCHECK   = 0x00F0

        STM_SETICON   = 0x0170

        IDC_STATIC    = -1

        SW_SHOW       = 5
        CW_USEDEFAULT = 0x80000000

        DLG_FONT = "Segoe UI"
)

// Control IDs
const (
        idLabelAgentKey   = 1001
        idEditAgentKey    = 1002
        idLabelSubdomain  = 1003
        idEditSubdomain   = 1004
        idCheckAutoStart  = 1005
        idLabelAuthToken  = 1006
        idEditAuthToken   = 1007
        idBtnOK           = 1008
        idBtnCancel       = 1009
        idLabelHint       = 1010
        idBtnCopyAddr     = 1011
        idLabelAddr       = 1012
        idComboLang       = 1013
        idLabelLang       = 1014
)

var (
        modUser32       = windows.NewLazySystemDLL("user32.dll")
        modGdi32        = windows.NewLazySystemDLL("gdi32.dll")

        procCreateWindowExW    = modUser32.NewProc("CreateWindowExW")
        procDestroyWindow      = modUser32.NewProc("DestroyWindow")
        procDefWindowProcW     = modUser32.NewProc("DefWindowProcW")
        procRegisterClassExW   = modUser32.NewProc("RegisterClassExW")
        procShowWindow         = modUser32.NewProc("ShowWindow")
        procGetMessageW        = modUser32.NewProc("GetMessageW")
        procTranslateMessageW  = modUser32.NewProc("TranslateMessage")
        procDispatchMessageW   = modUser32.NewProc("DispatchMessageW")
        procPostQuitMessage    = modUser32.NewProc("PostQuitMessage")
        procIsWindowW          = modUser32.NewProc("IsWindow")
        procSendMessageW       = modUser32.NewProc("SendMessageW")
        procSetWindowTextW     = modUser32.NewProc("SetWindowTextW")
        procGetWindowTextW     = modUser32.NewProc("GetWindowTextW")
        procGetWindowTextLengthW = modUser32.NewProc("GetWindowTextLengthW")
        procEnableWindow       = modUser32.NewProc("EnableWindow")
        procSetFocus           = modUser32.NewProc("SetFocus")
        procCreateFontW        = modGdi32.NewProc("CreateFontW")
        procDeleteObject       = modGdi32.NewProc("DeleteObject")
)

// WNDCLASSEXW for the dialog
type wndClassExW struct {
        Size        uint32
        Style       uint32
        LpfnWndProc uintptr
        CbClsExtra  int32
        CbWndExtra  int32
        HInstance   windows.Handle
        HIcon       windows.Handle
        HCursor     windows.Handle
        HbrBackground windows.Handle
        LpszMenuName *uint16
        LpszClassName *uint16
        HIconSm     windows.Handle
}

// settingsDialogResult holds the result from the dialog
type settingsDialogResult struct {
        agentKey   string
        subdomain  string
        authToken  string
        autoStart  bool
        language   string
        copyAddr   bool
        ok         bool
}

var dialogResult *settingsDialogResult
var dialogHwnd windows.HWND
var hFont windows.Handle
var dialogDone bool // signals the dialog message loop to exit

// These are set before opening the dialog, to show the current URL.
var currentPublicURL string
var currentAuthToken string

// settingsDlgProc is the WindowProc for the settings dialog
func settingsDlgProc(hwnd windows.HWND, msg uint32, wParam, lParam uintptr) uintptr {
        switch msg {
        case 0x0001: // WM_CREATE
                return 0

        case WM_COMMAND:
                cmdID := uint32(wParam) & 0xFFFF
                notifCode := uint32(wParam) >> 16
                _ = notifCode

                switch cmdID {
                case idBtnOK:
                        // Read values from controls. Log lengths (never the
                        // secrets themselves) so a stuck OK path is diagnosable
                        // from tray.log alone.
                        dialogResult.agentKey = getWindowText(dialogHwnd, idEditAgentKey)
                        dialogResult.subdomain = getWindowText(dialogHwnd, idEditSubdomain)
                        dialogResult.authToken = getWindowText(dialogHwnd, idEditAuthToken)
                        checked, _, _ := procSendMessageW.Call(
                                uintptr(getDlgItem(hwnd, idCheckAutoStart)),
                                uintptr(BM_GETCHECK), 0, 0,
                        )
                        dialogResult.autoStart = checked != 0
                        // Language combo: 0=auto(""), 1=en, 2=zh. Mapped by
                        // INDEX, not by text, so it works in both UI languages.
                        sel, _, _ := procSendMessageW.Call(
                                uintptr(getDlgItem(hwnd, idComboLang)),
                                uintptr(CB_GETCURSEL), 0, 0,
                        )
                        switch int(sel) {
                        case 1:
                                dialogResult.language = "en"
                        case 2:
                                dialogResult.language = "zh"
                        default:
                                dialogResult.language = ""
                        }
                        dialogResult.ok = true
                        dialogDone = true
                        log.Printf("[settings] OK: key_len=%d token_len=%d autostart=%v lang=%q",
                                len(dialogResult.agentKey), len(dialogResult.authToken), dialogResult.autoStart, dialogResult.language)
                        procDestroyWindow.Call(uintptr(hwnd))
                        return 0

                case idBtnCancel:
                        log.Printf("[settings] Cancel")
                        dialogResult.ok = false
                        dialogDone = true
                        procDestroyWindow.Call(uintptr(hwnd))
                        return 0

                case idBtnCopyAddr:
                        // Copy the current public URL to clipboard
                        if currentPublicURL != "" {
                                fullURL := currentPublicURL
                                if currentAuthToken != "" {
                                        sep := "?"
                                        if strings.Contains(fullURL, "?") {
                                                sep = "&"
                                        }
                                        fullURL = fullURL + sep + "token=" + currentAuthToken
                                }
                                copyToClipboardWindows(fullURL)
                                // Update label to show feedback
                                setWindowText(hwnd, idLabelAddr, tr("dlg.copied")+fullURL)
                        }
                        return 0
                }

        case WM_CLOSE:
                log.Printf("[settings] WM_CLOSE")
                dialogResult.ok = false
                dialogDone = true
                procDestroyWindow.Call(uintptr(hwnd))
                return 0
        }

        r, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wParam, lParam)
        return r
}

func getDlgItem(parentHwnd windows.HWND, id int) windows.HWND {
        // Find child window by ID - use GetDlgItem
        procGetDlgItem := modUser32.NewProc("GetDlgItem")
        h, _, _ := procGetDlgItem.Call(uintptr(parentHwnd), uintptr(id))
        return windows.HWND(h)
}

func getWindowText(parentHwnd windows.HWND, controlID int) string {
        hwnd := getDlgItem(parentHwnd, controlID)
        length, _, _ := procGetWindowTextLengthW.Call(uintptr(hwnd))
        if length == 0 {
                return ""
        }
        buf := make([]uint16, length+1)
        procGetWindowTextW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), uintptr(length+1))
        return windows.UTF16ToString(buf)
}

func setWindowText(parentHwnd windows.HWND, controlID int, text string) {
        hwnd := getDlgItem(parentHwnd, controlID)
        textUTF16, _ := windows.UTF16PtrFromString(text)
        procSetWindowTextW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(textUTF16)))
}

func createFont() windows.Handle {
        fontName, _ := windows.UTF16PtrFromString("Segoe UI")
        h, _, _ := procCreateFontW.Call(
                uintptr(16),              // nHeight
                uintptr(0),               // nWidth
                uintptr(0),               // nEscapement
                uintptr(0),               // nOrientation
                uintptr(400),             // fnWeight (FW_NORMAL)
                uintptr(0),               // fdwItalic
                uintptr(0),               // fdwUnderline
                uintptr(0),               // fdwStrikeOut
                uintptr(1),               // fdwCharSet (DEFAULT_CHARSET)
                uintptr(0),               // fdwOutputPrecision
                uintptr(0),               // fdwClipPrecision
                uintptr(5),               // fdwQuality (CLEARTYPE_QUALITY)
                uintptr(0),               // fdwPitchAndFamily
                uintptr(unsafe.Pointer(fontName)), // lpszFace
        )
        return windows.Handle(h)
}

func createControl(class string, text string, style uintptr, id int, x, y, w, h int, parentHwnd windows.HWND) windows.HWND {
        classNameUTF16, _ := windows.UTF16PtrFromString(class)
        textUTF16, _ := windows.UTF16PtrFromString(text)
        hwnd, _, _ := procCreateWindowExW.Call(
                0,
                uintptr(unsafe.Pointer(classNameUTF16)),
                uintptr(unsafe.Pointer(textUTF16)),
                style,
                uintptr(x), uintptr(y), uintptr(w), uintptr(h),
                uintptr(parentHwnd),
                uintptr(id),
                0, 0,
        )
        hwndResult := windows.HWND(hwnd)
        // Set font
        if hFont != 0 {
                procSendMessageW.Call(hwnd, 0x0030, uintptr(hFont), 0) // WM_SETFONT
        }
        return hwndResult
}

// openSettingsDialog opens a native Windows GUI settings dialog.
// publicURL and authToken are used to show/copy the current address.
// Returns the new settings and whether the user clicked OK.
//
// THREAD MODEL — this MUST run on the tray window thread (openSettings is
// called inline from the menu callback, which the tray wndProc invokes).
// v1.3.2 ran the dialog on its own goroutine; when the Go scheduler pinned
// that goroutine to the tray's already-locked OS thread, the dialog was
// starved forever (the tray goroutine never leaves GetMessage), leaving a
// zombie window (title bar painted by DWM, client never painted) and a
// settingsDialogOpen guard stuck true — every later "Settings..." click was
// silently ignored. On the tray thread the dialog's nested pump shares the
// same guarantee as TrackPopupMenu's modal loop: it dispatches everything,
// so the tray keeps working while the dialog is up.
//
// The pump uses GetMessage + TRANSLATEMESSAGE + Dispatch. TranslateMessage
// is NOT optional: without it no WM_CHAR is ever generated, the edit
// controls receive no keyboard input, and the key field physically cannot
// be edited — the "key cannot be cleared" bug.
func openSettingsDialog(s *settings.Settings, publicURL, authToken string) (newSettings *settings.Settings, copyAddr bool, ok bool) {
        log.Printf("[settings] opening (key_len=%d token_len=%d)", len(s.AgentToken), len(s.AuthToken))

        dialogResult = &settingsDialogResult{}
        dialogDone = false
        currentPublicURL = publicURL
        currentAuthToken = authToken

        // Create font
        hFont = createFont()
        defer func() {
                if hFont != 0 {
                        procDeleteObject.Call(uintptr(hFont))
                }
        }()

        // Register window class
        classNameUTF16, _ := windows.UTF16PtrFromString("SamVNCSettingsDlg")
        wproc := syscall.NewCallback(settingsDlgProc)

        wc := wndClassExW{
                Size:        uint32(unsafe.Sizeof(wndClassExW{})),
                LpfnWndProc: wproc,
                LpszClassName: classNameUTF16,
                HbrBackground: windows.Handle(15), // COLOR_BTNFACE+1 = 15
        }

        procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

        // Create main window. A failed CreateWindowExW used to be ignored —
        // the pump then spun forever on a dialog that could never close and
        // the open-guard stayed true, permanently disabling the settings
        // page. Bail out loudly instead.
        titleUTF16, _ := windows.UTF16PtrFromString(tr("dlg.title"))
        hwnd, _, _ := procCreateWindowExW.Call(
                0, // exStyle
                uintptr(unsafe.Pointer(classNameUTF16)),
                uintptr(unsafe.Pointer(titleUTF16)),
                uintptr(WS_OVERLAPPEDWINDOW & ^uintptr(0x00040000) & ^uintptr(0x00010000)), // OVERLAPPEDWINDOW without maximizebox and sizebox (fixed dialog)
                uintptr(CW_USEDEFAULT), uintptr(CW_USEDEFAULT),
                uintptr(540), uintptr(370), // width, height
                0, 0, 0, 0,
        )
        if hwnd == 0 {
                log.Printf("[settings] CreateWindowExW failed — aborting dialog")
                return nil, false, false
        }
        dialogHwnd = windows.HWND(hwnd)
        log.Printf("[settings] dialog hwnd=0x%X", uintptr(hwnd))

        // Create controls
        const margin = 20
        const labelW = 130
        const editW = 310
        const editH = 24
        const lineH = 35

        y := 20

        // Agent Key label
        createControl("STATIC", tr("dlg.key"), uintptr(WS_CHILD|WS_VISIBLE|SS_LEFT), idLabelAgentKey, margin, y+3, labelW, 20, dialogHwnd)
        // Agent Key edit
        createControl("EDIT", s.AgentToken, uintptr(WS_CHILD|WS_VISIBLE|WS_BORDER|WS_TABSTOP|ES_AUTOHSCROLL), idEditAgentKey, margin+labelW+10, y, editW, editH, dialogHwnd)
        y += lineH

        // Auth token label
        createControl("STATIC", tr("dlg.token"), uintptr(WS_CHILD|WS_VISIBLE|SS_LEFT), idLabelAuthToken, margin, y+3, labelW, 20, dialogHwnd)
        // Auth token edit
        createControl("EDIT", s.AuthToken, uintptr(WS_CHILD|WS_VISIBLE|WS_BORDER|WS_TABSTOP|ES_AUTOHSCROLL), idEditAuthToken, margin+labelW+10, y, editW, editH, dialogHwnd)
        y += lineH

        // Auto-start checkbox
        checkText := tr("dlg.autostart")
        checked := uint32(0)
        if s.AutoStart {
                checked = 1
        }
        createControl("BUTTON", checkText, uintptr(WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_AUTOCHECKBOX), idCheckAutoStart, margin+labelW+10, y, editW, editH, dialogHwnd)
        if checked == 1 {
                hwndCheck := getDlgItem(dialogHwnd, idCheckAutoStart)
                procSendMessageW.Call(uintptr(hwndCheck), 0x00F1, 1, 0) // BM_SETCHECK = BST_CHECKED
        }
        y += lineH

        // Language selector (Auto / English / 中文). The item texts are
        // found by INDEX on OK (0/1/2), never by text.
        createControl("STATIC", tr("dlg.language"), uintptr(WS_CHILD|WS_VISIBLE|SS_LEFT), idLabelLang, margin, y+5, labelW, 20, dialogHwnd)
        hwndCombo, _, _ := procCreateWindowExW.Call(
                0,
                uintptr(unsafe.Pointer(comboClassName)),
                0,
                uintptr(WS_CHILD|WS_VISIBLE|WS_TABSTOP|CBS_DROPDOWNLIST),
                uintptr(margin+labelW+10), uintptr(y), uintptr(200), uintptr(200), // height includes the dropdown list
                uintptr(dialogHwnd),
                uintptr(idComboLang),
                0, 0,
        )
        langItems := []string{tr("lang.auto"), tr("lang.en"), tr("lang.zh")}
        for _, it := range langItems {
                p, _ := windows.UTF16PtrFromString(it)
                procSendMessageW.Call(hwndCombo, uintptr(CB_ADDSTRING), 0, uintptr(unsafe.Pointer(p)))
        }
        initSel := uintptr(0)
        switch s.Language {
        case "en":
                initSel = 1
        case "zh":
                initSel = 2
        }
        procSendMessageW.Call(hwndCombo, uintptr(CB_SETCURSEL), initSel, 0)
        if hFont != 0 {
                procSendMessageW.Call(hwndCombo, 0x0030, uintptr(hFont), 0) // WM_SETFONT
        }
        y += lineH

        // Hint text
        hintText := tr("dlg.hint")
        createControl("STATIC", hintText, uintptr(WS_CHILD|WS_VISIBLE|SS_LEFT), idLabelHint, margin, y, labelW+editW+10, 40, dialogHwnd)
        y += 50

        // Current address display + Copy button
        btnW := 100
        btnH := 30
        addrLabel := tr("dlg.addrNone")
        if publicURL != "" {
                fullURL := publicURL
                if authToken != "" {
                        fullURL = fullURL + "?token=" + authToken
                }
                addrLabel = fullURL
        }
        createControl("STATIC", addrLabel, uintptr(WS_CHILD|WS_VISIBLE|SS_LEFT), idLabelAddr, margin, y, labelW+editW+10-btnW-10, editH, dialogHwnd)
        createControl("BUTTON", tr("dlg.btnCopy"), uintptr(WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_PUSHBUTTON), idBtnCopyAddr, margin+labelW+editW+10-btnW, y-2, btnW, btnH, dialogHwnd)
        if publicURL == "" {
                // Disable copy button if no URL
                hwndCopy := getDlgItem(dialogHwnd, idBtnCopyAddr)
                procEnableWindow.Call(uintptr(hwndCopy), 0)
        }
        y += lineH

        // OK button
        createControl("BUTTON", tr("dlg.ok"), uintptr(WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_DEFPUSHBUTTON), idBtnOK, margin+labelW+10+editW-btnW*2-10, y, btnW, btnH, dialogHwnd)
        // Cancel button
        createControl("BUTTON", tr("dlg.cancel"), uintptr(WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_PUSHBUTTON), idBtnCancel, margin+labelW+10+editW-btnW, y, btnW, btnH, dialogHwnd)

        // Show window and put the caret into the key field so the user can
        // edit immediately without an extra click.
        procShowWindow.Call(uintptr(dialogHwnd), uintptr(SW_SHOW))
        procSetFocus.Call(uintptr(getDlgItem(dialogHwnd, idEditAgentKey)))

        // Modal pump on the tray thread. GetMessage BLOCKS (no busy spin) and
        // dispatches ALL thread messages, so tray interaction keeps working
        // while the dialog is up. dialogDone is set by the wndProc below, on
        // this same thread, so the check after DispatchMessage is safe.
        const (
                WM_QUIT    = 0x0012
                VK_RETURN  = 0x0D
                VK_ESCAPE  = 0x1B
        )
        pump := func() {
                for {
                        var msg struct {
                                HWnd    windows.HWND
                                Message uint32
                                WParam  uintptr
                                LParam  uintptr
                                Time    uint32
                                Pt      struct{ X, Y int32 }
                        }
                        ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
                        switch int32(ret) {
                        case -1:
                                log.Printf("[settings] GetMessage error — closing dialog")
                                return
                        case 0: // WM_QUIT: forward it to the outer loop, then leave
                                procPostQuitMessage.Call(msg.WParam)
                                log.Printf("[settings] WM_QUIT seen — closing dialog")
                                return
                        }
                        // A key autorepeat (or queued click) can arrive AFTER
                        // OK/Cancel destroyed the window; re-running the
                        // handler then would re-read fields from a dead window
                        // and clobber the result. Stop at the first done.
                        if dialogDone {
                                return
                        }
                        // Enter = OK, Esc = Cancel (standard dialog behavior;
                        // without IsDialogMessage nothing maps these keys).
                        if msg.Message == 0x0100 { // WM_KEYDOWN
                                switch msg.WParam & 0xFFFF {
                                case VK_RETURN:
                                        procSendMessageW.Call(uintptr(dialogHwnd), WM_COMMAND,
                                                uintptr(uint32(idBtnOK)&0xFFFF), 0)
                                        continue
                                case VK_ESCAPE:
                                        procSendMessageW.Call(uintptr(dialogHwnd), WM_COMMAND,
                                                uintptr(uint32(idBtnCancel)&0xFFFF), 0)
                                        continue
                                }
                        }
                        procTranslateMessageW.Call(uintptr(unsafe.Pointer(&msg)))
                        procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
                        if dialogDone {
                                return
                        }
                        // Safety net: if the window was destroyed out-of-band
                        // (external CloseMainWindow, session teardown), the
                        // handlers never ran, dialogDone stays false and this
                        // pump would spin forever while the open-guard stays
                        // true — bricking the settings page. Detect a dead
                        // window and bail out.
                        if w, _, _ := procIsWindowW.Call(uintptr(dialogHwnd)); w == 0 {
                                log.Printf("[settings] dialog window vanished — closing pump")
                                return
                        }
                }
        }
        pump()

        // Cleanup (window already destroyed in proc, but just in case)
        procDestroyWindow.Call(uintptr(dialogHwnd))
        log.Printf("[settings] dialog closed ok=%v", dialogResult.ok)

        if dialogResult.ok {
                return &settings.Settings{
                        AgentToken: dialogResult.agentKey,
                        Subdomain:  dialogResult.subdomain,
                        AuthToken:  dialogResult.authToken,
                        AutoStart:  dialogResult.autoStart,
                        Language:   dialogResult.language,
                }, false, true
        }
        return nil, false, false
}

// comboClassName is the window class of the combo box control.
var comboClassName, _ = windows.UTF16PtrFromString("COMBOBOX")

// Ensure imports are used
var _ = fmt.Sprintf
