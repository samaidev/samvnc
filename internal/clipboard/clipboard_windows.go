//go:build windows

package clipboard

import (
	"syscall"
	"unsafe"
)

var (
	kernel32            = syscall.NewLazyDLL("kernel32.dll")
	procGlobalAlloc     = kernel32.NewProc("GlobalAlloc")
	procGlobalLock      = kernel32.NewProc("GlobalLock")
	procGlobalUnlock    = kernel32.NewProc("GlobalUnlock")
	procOpenClipboard   = user32Clip.NewProc("OpenClipboard")
	procCloseClipboard  = user32Clip.NewProc("CloseClipboard")
	procEmptyClipboard  = user32Clip.NewProc("EmptyClipboard")
	procSetClipboard    = user32Clip.NewProc("SetClipboardData")
	procGetClipboard    = user32Clip.NewProc("GetClipboardData")
)

var user32Clip = syscall.NewLazyDLL("user32.dll")

const (
	gmemMoveable = 0x0002
	cfUnicodeText = 13
)

type winClipboard struct{}

func newManager() (Manager, error) { return &winClipboard{}, nil }

func (*winClipboard) Name() string { return "windows-clipboard" }

func (*winClipboard) Close() error { return nil }

// Windows has no PRIMARY selection; map it to the clipboard.
func (*winClipboard) Read(sel Selection) (string, error) {
	_ = sel
	if r, _, _ := procOpenClipboard.Call(0); r == 0 {
		return "", nil
	}
	defer procCloseClipboard.Call()

	h, _, _ := procGetClipboard.Call(cfUnicodeText)
	if h == 0 {
		return "", nil
	}
	ptr, _, _ := procGlobalLock.Call(h)
	if ptr == 0 {
		return "", nil
	}
	defer procGlobalUnlock.Call(h)
	// UTF-16 string until null.
	s := utf16PtrToString(ptr)
	return s, nil
}

func (*winClipboard) Write(text string, sel Selection) error {
	_ = sel
	utf16, _ := syscall.UTF16FromString(text)
	size := len(utf16) * 2
	h, _, _ := procGlobalAlloc.Call(gmemMoveable, uintptr(size))
	if h == 0 {
		return nil
	}
	ptr, _, _ := procGlobalLock.Call(h)
	if ptr == 0 {
		return nil
	}
	copy((*[1 << 16]uint16)(unsafe.Pointer(ptr))[:len(utf16)], utf16)
	procGlobalUnlock.Call(h)

	procOpenClipboard.Call(0)
	procEmptyClipboard.Call()
	procSetClipboard.Call(cfUnicodeText, h)
	procCloseClipboard.Call()
	return nil
}

func utf16PtrToString(p uintptr) string {
	if p == 0 {
		return ""
	}
	var out []uint16
	for i := 0; ; i++ {
		c := *(*uint16)(unsafe.Pointer(p + uintptr(i*2)))
		if c == 0 {
			break
		}
		out = append(out, c)
	}
	return string(utf16Decode(out))
}

func utf16Decode(s []uint16) []rune {
	r := make([]rune, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 0xD800 && c <= 0xDBFF && i+1 < len(s):
			lo := s[i+1]
			r = append(r, rune(0x10000)+rune(c-0xD800)<<10+rune(lo-0xDC00))
			i++
		case c >= 0xDC00 && c <= 0xDFFF:
			// lone low surrogate, skip
		default:
			r = append(r, rune(c))
		}
	}
	return r
}
