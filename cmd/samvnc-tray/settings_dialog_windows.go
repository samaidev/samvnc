//go:build windows

package main

import (
	"fmt"
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
)

var (
	modUser32       = windows.NewLazySystemDLL("user32.dll")
	modGdi32        = windows.NewLazySystemDLL("gdi32.dll")
	modKernel32     = windows.NewLazySystemDLL("kernel32.dll")

	procCreateWindowExW    = modUser32.NewProc("CreateWindowExW")
	procDestroyWindow      = modUser32.NewProc("DestroyWindow")
	procDefWindowProcW     = modUser32.NewProc("DefWindowProcW")
	procRegisterClassExW   = modUser32.NewProc("RegisterClassExW")
	procShowWindow         = modUser32.NewProc("ShowWindow")
	procGetMessageW        = modUser32.NewProc("GetMessageW")
	procDispatchMessageW   = modUser32.NewProc("DispatchMessageW")
	procPostQuitMessage    = modUser32.NewProc("PostQuitMessage")
	procSendMessageW       = modUser32.NewProc("SendMessageW")
	procSetWindowTextW     = modUser32.NewProc("SetWindowTextW")
	procGetWindowTextW     = modUser32.NewProc("GetWindowTextW")
	procGetWindowTextLengthW = modUser32.NewProc("GetWindowTextLengthW")
	procEnableWindow       = modUser32.NewProc("EnableWindow")
	procSetFocus           = modUser32.NewProc("SetFocus")
	procSendMessageW2      = modUser32.NewProc("SendMessageW")
	procCreateFontW        = modGdi32.NewProc("CreateFontW")
	procDeleteObject       = modGdi32.NewProc("DeleteObject")
	procSendMessageW3     = modUser32.NewProc("SendMessageW")
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
			// Read values from controls
			dialogResult.agentKey = getWindowText(dialogHwnd, idEditAgentKey)
			dialogResult.subdomain = getWindowText(dialogHwnd, idEditSubdomain)
			dialogResult.authToken = getWindowText(dialogHwnd, idEditAuthToken)
			checked, _, _ := procSendMessageW.Call(
				uintptr(getDlgItem(hwnd, idCheckAutoStart)),
				uintptr(BM_GETCHECK), 0, 0,
			)
			dialogResult.autoStart = checked != 0
			dialogResult.ok = true
			dialogDone = true
			procDestroyWindow.Call(uintptr(hwnd))
			return 0

		case idBtnCancel:
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
				setWindowText(hwnd, idLabelAddr, "✓ Copied: "+fullURL)
			}
			return 0
		}

	case WM_CLOSE:
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
func openSettingsDialog(s *settings.Settings, publicURL, authToken string) (newSettings *settings.Settings, copyAddr bool, ok bool) {
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

	// Create main window
	titleUTF16, _ := windows.UTF16PtrFromString("SamVNC Settings")
	hwnd, _, _ := procCreateWindowExW.Call(
		0, // exStyle
		uintptr(unsafe.Pointer(classNameUTF16)),
		uintptr(unsafe.Pointer(titleUTF16)),
		uintptr(WS_OVERLAPPEDWINDOW & ^uintptr(0x00040000) & ^uintptr(0x00010000)), // OVERLAPPEDWINDOW without maximizebox and sizebox (fixed dialog)
		uintptr(CW_USEDEFAULT), uintptr(CW_USEDEFAULT),
		uintptr(540), uintptr(370), // width, height
		0, 0, 0, 0,
	)
	dialogHwnd = windows.HWND(hwnd)

	// Create controls
	const margin = 20
	const labelW = 130
	const editW = 310
	const editH = 24
	const lineH = 35

	y := 20

	// Agent Key label
	createControl("STATIC", "Aitun.cc Key:", uintptr(WS_CHILD|WS_VISIBLE|SS_LEFT), idLabelAgentKey, margin, y+3, labelW, 20, dialogHwnd)
	// Agent Key edit
	createControl("EDIT", s.AgentToken, uintptr(WS_CHILD|WS_VISIBLE|WS_BORDER|WS_TABSTOP|ES_AUTOHSCROLL), idEditAgentKey, margin+labelW+10, y, editW, editH, dialogHwnd)
	y += lineH

	// Auth token label
	createControl("STATIC", "Access Token:", uintptr(WS_CHILD|WS_VISIBLE|SS_LEFT), idLabelAuthToken, margin, y+3, labelW, 20, dialogHwnd)
	// Auth token edit
	createControl("EDIT", s.AuthToken, uintptr(WS_CHILD|WS_VISIBLE|WS_BORDER|WS_TABSTOP|ES_AUTOHSCROLL), idEditAuthToken, margin+labelW+10, y, editW, editH, dialogHwnd)
	y += lineH

	// Auto-start checkbox
	checkText := " Start SamVNC on system boot"
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

	// Hint text
	hintText := "Enter your aitun.cc key to bind a subdomain.\nLeave empty to use the built-in aitun proxy address."
	createControl("STATIC", hintText, uintptr(WS_CHILD|WS_VISIBLE|SS_LEFT), idLabelHint, margin, y, labelW+editW+10, 40, dialogHwnd)
	y += 50

	// Current address display + Copy button
	btnW := 100
	btnH := 30
	addrLabel := "Address: (not available)"
	if publicURL != "" {
		fullURL := publicURL
		if authToken != "" {
			fullURL = fullURL + "?token=" + authToken
		}
		addrLabel = fullURL
	}
	createControl("STATIC", addrLabel, uintptr(WS_CHILD|WS_VISIBLE|SS_LEFT), idLabelAddr, margin, y, labelW+editW+10-btnW-10, editH, dialogHwnd)
	createControl("BUTTON", "Copy Address", uintptr(WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_PUSHBUTTON), idBtnCopyAddr, margin+labelW+editW+10-btnW, y-2, btnW, btnH, dialogHwnd)
	if publicURL == "" {
		// Disable copy button if no URL
		hwndCopy := getDlgItem(dialogHwnd, idBtnCopyAddr)
		procEnableWindow.Call(uintptr(hwndCopy), 0)
	}
	y += lineH

	// OK button
	createControl("BUTTON", "OK", uintptr(WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_DEFPUSHBUTTON), idBtnOK, margin+labelW+10+editW-btnW*2-10, y, btnW, btnH, dialogHwnd)
	// Cancel button
	createControl("BUTTON", "Cancel", uintptr(WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_PUSHBUTTON), idBtnCancel, margin+labelW+10+editW-btnW, y, btnW, btnH, dialogHwnd)

	// Show window
	procShowWindow.Call(uintptr(dialogHwnd), uintptr(SW_SHOW))

	// Message loop — use PeekMessage so we can check dialogDone
	procPeekMessageW := modUser32.NewProc("PeekMessageW")
	PM_REMOVE := uint32(0x0001)
	for {
		if dialogDone {
			break
		}
		var msg struct {
			HWnd    windows.HWND
			Message uint32
			WParam  uintptr
			LParam  uintptr
			Time    uint32
			Pt      struct{ X, Y int32 }
		}
		ret, _, _ := procPeekMessageW.Call(
			uintptr(unsafe.Pointer(&msg)),
			0, 0, 0,
			uintptr(PM_REMOVE),
		)
		if ret != 0 {
			procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
		} else {
			// No message — sleep briefly to avoid busy loop
			procSleep := modKernel32.NewProc("Sleep")
			procSleep.Call(uintptr(10))
		}
	}

	// Cleanup (window already destroyed in proc, but just in case)
	procDestroyWindow.Call(uintptr(dialogHwnd))

	if dialogResult.ok {
		return &settings.Settings{
			AgentToken: dialogResult.agentKey,
			Subdomain:  dialogResult.subdomain,
			AuthToken:  dialogResult.authToken,
			AutoStart:  dialogResult.autoStart,
		}, false, true
	}
	return nil, false, false
}

// Ensure imports are used
var _ = fmt.Sprintf
