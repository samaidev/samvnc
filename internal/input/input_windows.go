//go:build windows

package input

import (
	"fmt"
	"syscall"
	"unsafe"

	"github.com/samaidev/samvnc/internal/protocol"
)

// sendInputInjector injects events via the Win32 SendInput API (native, no CGO).
type sendInputInjector struct {
	mouseMask uint16
}

var (
	user32           = syscall.NewLazyDLL("user32.dll")
	procSendInput    = user32.NewProc("SendInput")
	procVkKeyScan    = user32.NewProc("VkKeyScanW")
	procMapVirtKey   = user32.NewProc("MapVirtualKeyW")
	procSetCursorPos = user32.NewProc("SetCursorPos")
)

// SendInput input types.
const (
	inputMouse    = 0
	inputKeyboard = 1
)

// Mouse button flags.
const (
	mouseMove        = 0x0001
	mouseLeftDown    = 0x0002
	mouseLeftUp      = 0x0004
	mouseRightDown   = 0x0008
	mouseRightUp     = 0x0010
	mouseMiddleDown  = 0x0020
	mouseMiddleUp    = 0x0040
	mouseXDown       = 0x0080
	mouseXUp         = 0x0100
	mouseWheel       = 0x0800
	mouseAbsolute    = 0x8000
)

// Keyboard flags.
const (
	keyDown    = 0x0000
	keyUp      = 0x0002
	keyUnicode = 0x0004
)

// Virtual key codes for special keys.
const (
	vkBack   = 0x08
	vkTab    = 0x09
	vkReturn = 0x0D
	vkShift  = 0x10
	vkCtrl   = 0x11
	vkMenu   = 0x12 // Alt
	vkCapital = 0x14
	vkEscape = 0x1B
	vkSpace  = 0x20
	vkPrior  = 0x21
	vkNext   = 0x22
	vkEnd    = 0x23
	vkHome   = 0x24
	vkLeft   = 0x25
	vkUp     = 0x26
	vkRight  = 0x27
	vkDown   = 0x28
	vkInsert = 0x2D
	vkDelete = 0x2E
	vkLwin   = 0x5B
	vkF1     = 0x70
	vkF12    = 0x7B
)

type mouseInput struct {
	Dx        int32
	Dy        int32
	MouseData uint32
	DwFlags   uint32
	Time      uint32
	DwExtra   uintptr
}

type keybdInput struct {
	WVk     uint16
	WScan   uint16
	DwFlags uint32
	Time    uint32
	DwExtra uintptr
}

// INPUT union (the MOUSEINPUT variant is 32 bytes on amd64 due to the
// trailing ULONG_PTR). We size Mi for the largest variant.
type input struct {
	Type uint32
	_    [4]byte // padding to align the union on an 8-byte boundary
	Mi   [32]byte
}

func newInjector() (Injector, error) { return &sendInputInjector{}, nil }

func (*sendInputInjector) Name() string { return "windows-sendinput" }

func (i *sendInputInjector) Close() error { return nil }

func (i *sendInputInjector) Mouse(ev protocol.MouseEvent) error {
	// Absolute positioning requires normalized 0..65535 coordinates.
	sx := int32(0)
	sy := int32(0)
	// We move with SetCursorPos for simplicity (relative to primary monitor).
	procSetCursorPos.Call(uintptr(ev.X), uintptr(ev.Y))
	_ = sx
	_ = sy

	type btn struct {
		mask uint16
		down uint32
		up   uint32
	}
	buttons := []btn{
		{protocol.ButtonLeft, mouseLeftDown, mouseLeftUp},
		{protocol.ButtonMiddle, mouseMiddleDown, mouseMiddleUp},
		{protocol.ButtonRight, mouseRightDown, mouseRightUp},
		{protocol.ButtonX1, mouseXDown, mouseXUp},
		{protocol.ButtonX2, mouseXDown, mouseXUp},
	}
	for _, b := range buttons {
		was := i.mouseMask & b.mask
		now := ev.Mask & b.mask
		if now != 0 && was == 0 {
			sendMouse(b.down, 0)
		} else if now == 0 && was != 0 {
			sendMouse(b.up, 0)
		}
	}
	i.mouseMask = ev.Mask

	if ev.Wheel != 0 {
		// WHEEL_DELTA = 120.
		sendMouse(mouseWheel, uint32(int32(ev.Wheel)*120))
	}
	return nil
}

func sendMouse(flags uint32, data uint32) {
	mi := mouseInput{MouseData: data, DwFlags: flags}
	var raw [32]byte
	*(*mouseInput)(unsafe.Pointer(&raw[0])) = mi
	in := input{Type: inputMouse, Mi: raw}
	procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}

func (i *sendInputInjector) Key(ev protocol.KeyEvent) error {
	vk := domToVK(ev.Code, ev.Key)
	if vk == 0 {
		return nil
	}
	flags := uint32(keyDown)
	if !ev.Down {
		flags = keyUp
	}
	ki := keybdInput{WVk: uint16(vk), DwFlags: flags}
	var raw [32]byte
	*(*keybdInput)(unsafe.Pointer(&raw[0])) = ki
	in := input{Type: inputKeyboard, Mi: raw}
	r, _, _ := procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
	if r == 0 {
		return fmt.Errorf("SendInput failed")
	}
	return nil
}

// Type injects a Unicode string via KEYEVENTF_UNICODE SendInput events, one
// per rune. Used by the mobile soft keyboard for IME/composed text.
func (i *sendInputInjector) Type(text string) error {
	for _, r := range text {
		sc := uint16(r)
		for _, flags := range []uint32{keyDown | keyUnicode, keyUp | keyUnicode} {
			ki := keybdInput{WScan: sc, DwFlags: flags}
			var raw [32]byte
			*(*keybdInput)(unsafe.Pointer(&raw[0])) = ki
			in := input{Type: inputKeyboard, Mi: raw}
			procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
		}
	}
	return nil
}

func domToVK(code, key string) uint16 {
	switch code {
	case "Enter":
		return vkReturn
	case "Backspace":
		return vkBack
	case "Tab":
		return vkTab
	case "Escape":
		return vkEscape
	case "Space":
		return vkSpace
	case "ShiftLeft", "ShiftRight":
		return vkShift
	case "ControlLeft", "ControlRight":
		return vkCtrl
	case "AltLeft", "AltRight":
		return vkMenu
	case "MetaLeft", "MetaRight":
		return vkLwin
	case "CapsLock":
		return vkCapital
	case "ArrowLeft":
		return vkLeft
	case "ArrowRight":
		return vkRight
	case "ArrowUp":
		return vkUp
	case "ArrowDown":
		return vkDown
	case "Home":
		return vkHome
	case "End":
		return vkEnd
	case "PageUp":
		return vkPrior
	case "PageDown":
		return vkNext
	case "Insert":
		return vkInsert
	case "Delete":
		return vkDelete
	case "F1":
		return vkF1
	case "F2":
		return vkF1 + 1
	case "F3":
		return vkF1 + 2
	case "F4":
		return vkF1 + 3
	case "F5":
		return vkF1 + 4
	case "F6":
		return vkF1 + 5
	case "F7":
		return vkF1 + 6
	case "F8":
		return vkF1 + 7
	case "F9":
		return vkF1 + 8
	case "F10":
		return vkF1 + 9
	case "F11":
		return vkF1 + 10
	case "F12":
		return vkF1 + 11
	}
	if len(code) >= 4 {
		if c := code[:3]; c == "Key" {
			ch := code[3:]
			if len(ch) == 1 {
				return uint16(ch[0]) // 'A'..'Z' map to VK
			}
		}
		if c := code[:5]; c == "Digit" {
			d := code[5:]
			if len(d) == 1 {
				return uint16(d[0]) // '0'..'9'
			}
		}
	}
	if len(key) == 1 {
		return uint16(toupper(key[0]))
	}
	return 0
}

func toupper(b byte) byte {
	if b >= 'a' && b <= 'z' {
		return b - 32
	}
	return b
}

// Ensure vkey scan path compiles; referenced for future unicode input.
var _ = procVkKeyScan
var _ = procMapVirtKey
