//go:build linux

package input

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/samaidev/samvnc/internal/protocol"
)

// debugInput echoes every xdotool invocation to stderr when set (env SAMVNC_DEBUG_INPUT).
var debugInput = os.Getenv("SAMVNC_DEBUG_INPUT") != ""

// xdotoolInjector drives the X server through the `xdotool` command-line tool.
//
// This keeps SamVNC free of CGO and fragile XTEST binding signatures while
// still providing real, low-latency input injection. xdotool is the de-facto
// standard on X11 desktops and is packaged by every major distro.
type xdotoolInjector struct {
	mouseMask uint16 // last button mask, to emit clean press/release deltas
	mu        sync.Mutex
	held      map[string]bool // keyed by ev.Code; prevents repeat keydowns from producing duplicate characters
}

func newInjector() (Injector, error) {
	if _, err := exec.LookPath("xdotool"); err != nil {
		return nil, fmt.Errorf("input: xdotool not found in PATH (install it for keyboard/mouse injection): %w", err)
	}
	return &xdotoolInjector{held: map[string]bool{}}, nil
}

func (*xdotoolInjector) Name() string { return "linux-xdotool" }

func (i *xdotoolInjector) Close() error { return nil }

func (i *xdotoolInjector) Mouse(ev protocol.MouseEvent) error {
	root := strings.Builder{}
	root.WriteString("mousemove ")
	root.WriteString(fmt.Sprintf("%d %d", ev.X, ev.Y))
	if err := xdo(root.String()); err != nil {
		return err
	}

	// Button press/release derived from mask deltas vs. previous state.
	buttons := []struct {
		mask uint16
		btn  int // xdotool button number
	}{
		{protocol.ButtonLeft, 1},
		{protocol.ButtonMiddle, 2},
		{protocol.ButtonRight, 3},
		{protocol.ButtonX1, 8},
		{protocol.ButtonX2, 9},
	}
	for _, b := range buttons {
		was := i.mouseMask & b.mask
		now := ev.Mask & b.mask
		switch {
		case now != 0 && was == 0:
			if err := xdo(fmt.Sprintf("mousedown %d", b.btn)); err != nil {
				return err
			}
		case now == 0 && was != 0:
			if err := xdo(fmt.Sprintf("mouseup %d", b.btn)); err != nil {
				return err
			}
		}
	}
	i.mouseMask = ev.Mask

	// Vertical wheel.
	if ev.Wheel != 0 {
		btn := 4 // up
		if ev.Wheel < 0 {
			btn = 5 // down
		}
		steps := ev.Wheel
		if steps < 0 {
			steps = -steps
		}
		for k := int16(0); k < steps; k++ {
			if err := xdo(fmt.Sprintf("click %d", btn)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (i *xdotoolInjector) Key(ev protocol.KeyEvent) error {
	name := xdotoolKeyName(ev.Code, ev.Key)
	if name == "" {
		return nil
	}

	// Dedupe repeats: if a key is already held, ignore further keydowns so a
	// browser OS auto-repeat (or a slow /poll batch replaying the same
	// keydown N times) cannot produce "sssssss" bursts. xdotool keydown sends
	// an XKeyPress each call and X11 treats each as a typed character.
	i.mu.Lock()
	if ev.Down {
		if i.held[ev.Code] {
			i.mu.Unlock()
			return nil
		}
		i.held[ev.Code] = true
	} else {
		if !i.held[ev.Code] {
			i.mu.Unlock()
			return nil
		}
		delete(i.held, ev.Code)
	}
	i.mu.Unlock()

	// Modifier keys themselves: hold/release with keydown/keyup so later
	// keys can stack on top, and so the X server's modifier state matches
	// what the browser reports.
	if isModifier(name) {
		sub := "keydown"
		if !ev.Down {
			sub = "keyup"
		}
		return xdo(sub + " " + name)
	}

	// Non-modifier key.
	if ev.Down {
		// With modifiers held, use xdotool's combo syntax (e.g. "key ctrl+c"):
		// it atomically presses the chord and releases it, which is the only
		// reliable way to deliver Ctrl+C / Ctrl+V / Alt+Tab through xdotool.
		if ev.Ctrl || ev.Alt || ev.Shift || ev.Meta {
			chord := []string{}
			if ev.Ctrl {
				chord = append(chord, "ctrl")
			}
			if ev.Alt {
				chord = append(chord, "alt")
			}
			if ev.Shift {
				chord = append(chord, "shift")
			}
			if ev.Meta {
				chord = append(chord, "super")
			}
			chord = append(chord, name)
			return xdo("key " + strings.Join(chord, "+"))
		}
		// No modifiers: hold the key down (supports arrows, repeat, gaming).
		return xdo("keydown " + name)
	}

	// Key release. For a chord, xdotool `key` already released it, so only
	// send keyup for plain held keys.
	if !ev.Ctrl && !ev.Alt && !ev.Shift && !ev.Meta {
		return xdo("keyup " + name)
	}
	return nil
}

// isModifier reports whether name is a modifier keysym that should be held via
// keydown rather than fired as a one-shot chord.
func isModifier(name string) bool {
	switch name {
	case "Shift_L", "Shift_R", "Control_L", "Control_R",
		"Alt_L", "Alt_R", "Super_L", "Super_R", "Caps_Lock":
		return true
	}
	return false
}

// xdo runs `xdotool <args>` and returns any error.
func xdo(args string) error {
	if debugInput {
		fmt.Fprintf(os.Stderr, "[input] xdotool %s\n", args)
	}
	parts := strings.Fields(args)
	if len(parts) == 0 {
		return nil
	}
	cmd := exec.Command("xdotool", parts...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("xdotool %s: %w (%s)", parts[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

// xdotoolKeyName maps a DOM KeyboardEvent.code (and key fallback) to an
// xdotool keysym name. Modifier keys are returned so they can be held.
func xdotoolKeyName(code, key string) string {
	// DOM code table.
	switch code {
	case "Enter":
		return "Return"
	case "Backspace":
		return "BackSpace"
	case "Tab":
		return "Tab"
	case "Escape":
		return "Escape"
	case "Space":
		return "space"
	case "ShiftLeft", "ShiftRight":
		return "Shift_L"
	case "ControlLeft", "ControlRight":
		return "Control_L"
	case "AltLeft", "AltRight":
		return "Alt_L"
	case "MetaLeft", "MetaRight":
		return "Super_L"
	case "CapsLock":
		return "Caps_Lock"
	case "ArrowLeft":
		return "Left"
	case "ArrowRight":
		return "Right"
	case "ArrowUp":
		return "Up"
	case "ArrowDown":
		return "Down"
	case "Home":
		return "Home"
	case "End":
		return "End"
	case "PageUp":
		return "Prior"
	case "PageDown":
		return "Next"
	case "Insert":
		return "Insert"
	case "Delete":
		return "Delete"
	case "F1":
		return "F1"
	case "F2":
		return "F2"
	case "F3":
		return "F3"
	case "F4":
		return "F4"
	case "F5":
		return "F5"
	case "F6":
		return "F6"
	case "F7":
		return "F7"
	case "F8":
		return "F8"
	case "F9":
		return "F9"
	case "F10":
		return "F10"
	case "F11":
		return "F11"
	case "F12":
		return "F12"
	}
	// Letter / digit keys: DOM code "KeyA" -> "a", "Digit1" -> "1".
	if len(code) >= 4 {
		if strings.HasPrefix(code, "Key") {
			return strings.ToLower(code[3:])
		}
		if strings.HasPrefix(code, "Digit") {
			return code[5:]
		}
	}
	// Fallback to the literal key for printable single chars.
	if len(key) == 1 {
		return key
	}
	return ""
}
