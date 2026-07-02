//go:build linux

package input

import (
        "fmt"
        "os"
        "os/exec"
        "strings"
        "sync"

        x "github.com/BurntSushi/xgb"
        "github.com/BurntSushi/xgb/xproto"
        "github.com/BurntSushi/xgb/xtest"

        "github.com/samaidev/samvnc/internal/protocol"
)

// debugInput echoes every input call to stderr when set (env SAMVNC_DEBUG_INPUT).
var debugInput = os.Getenv("SAMVNC_DEBUG_INPUT") != ""

// xtestInjector drives the X server directly via the XTEST extension.
//
// This is the native Linux input path: it uses xgb (already a dependency
// for screen capture) to issue XTEST FakeInput requests for key presses,
// key releases, mouse motion, and button presses. Compared to the previous
// xdotool-based implementation, this is:
//   - Lower latency (no process fork per event).
//   - More reliable (no window-manager focus races).
//   - Self-contained (no external binary dependency).
//
// For Unicode text input (mobile IME, composed CJK characters) we still
// fall back to `xdotool type` because converting arbitrary Unicode to
// per-key keysym sequences is complex and xdotool handles it correctly.
type xtestInjector struct {
        conn      *x.Conn
        root      xproto.Window
        mouseMask uint16 // last button mask, to emit clean press/release deltas
        mu        sync.Mutex
        held      map[string]bool // keyed by ev.Code; prevents repeat keydowns
}

func newInjector() (Injector, error) {
        inj := &xtestInjector{held: map[string]bool{}}
        if err := inj.reconnect(); err != nil {
                return nil, err
        }
        return inj, nil
}

// reconnect (re-)establishes the X server connection and re-inits XTEST.
// Called on startup and whenever an injection call fails (the X server
// may have been restarted by the base image's entrypoint, or the
// connection may have timed out after a period of inactivity).
func (i *xtestInjector) reconnect() error {
        i.mu.Lock()
        defer i.mu.Unlock()
        if i.conn != nil {
                i.conn.Close()
                i.conn = nil
        }
        conn, err := x.NewConn()
        if err != nil {
                return fmt.Errorf("input: x11 reconnect: %w (is DISPLAY set?)", err)
        }
        if err := xtest.Init(conn); err != nil {
                conn.Close()
                return fmt.Errorf("input: XTEST init: %w", err)
        }
        setup := xproto.Setup(conn)
        i.conn = conn
        i.root = setup.DefaultScreen(conn).Root
        // Clear held keys — the new X server doesn't know about any modifiers
        // we thought were pressed on the old connection.
        i.held = map[string]bool{}
        if debugInput {
                fmt.Fprintf(os.Stderr, "[input] X server reconnected OK\n")
        }
        return nil
}

// withReconnect runs an injection function; if it fails, reconnects to
// the X server and retries once. This handles the case where the X
// server was restarted (e.g. by the base entrypoint) or the connection
// dropped after a period of inactivity.
func (i *xtestInjector) withReconnect(fn func() error) error {
        err := fn()
        if err == nil {
                return nil
        }
        if debugInput {
                fmt.Fprintf(os.Stderr, "[input] injection failed (%v), reconnecting X...\n", err)
        }
        // Reconnect and retry once.
        if rerr := i.reconnect(); rerr != nil {
                return fmt.Errorf("input: reconnect failed: %w (original error: %v)", rerr, err)
        }
        return fn()
}

func (*xtestInjector) Name() string { return "linux-xtest" }

func (i *xtestInjector) Close() error {
        if i.conn != nil {
                i.conn.Close()
        }
        return nil
}

// fakeKey sends a KeyPress or KeyRelease event for the given keycode.
func (i *xtestInjector) fakeKey(keycode xproto.Keycode, type_ byte) error {
        return i.withReconnect(func() error {
                return xtest.FakeInputChecked(i.conn, type_, byte(keycode), 0, 0, 0, 0, 0).Check()
        })
}

// fakeButton sends a ButtonPress or ButtonRelease event.
func (i *xtestInjector) fakeButton(button byte, type_ byte) error {
        return i.withReconnect(func() error {
                return xtest.FakeInputChecked(i.conn, type_, button, 0, 0, 0, 0, 0).Check()
        })
}

// fakeMove moves the pointer to (x, y) on the root window.
func (i *xtestInjector) fakeMove(x, y int16) error {
        return i.withReconnect(func() error {
                return xtest.FakeInputChecked(i.conn, 6, 0, 0, i.root, x, y, 0).Check()
        })
}

// Type injects an arbitrary Unicode string. We use xdotool for this
// because it correctly handles multi-byte / composed characters that
// don't map to a single keycode.
func (i *xtestInjector) Type(text string) error {
        if text == "" {
                return nil
        }
        if debugInput {
                fmt.Fprintf(os.Stderr, "[input] xdotool type %q\n", text)
        }
        // If xdotool isn't available, fall back to per-character XTEST with
        // keysym lookup. This works for ASCII but not for CJK.
        if _, err := exec.LookPath("xdotool"); err != nil {
                return i.typeViaXTEST(text)
        }
        cmd := exec.Command("xdotool", "type", "--clearmodifiers", "--delay", "0", "--", text)
        out, err := cmd.CombinedOutput()
        if err != nil {
                return fmt.Errorf("xdotool type: %w (%s)", err, strings.TrimSpace(string(out)))
        }
        return nil
}

// typeViaXTEST is a fallback for when xdotool is unavailable. It maps each
// character to a keysym, looks up the keycode, and sends a key press +
// release. Non-ASCII characters that don't map to a keycode are skipped.
func (i *xtestInjector) typeViaXTEST(text string) error {
        for _, r := range text {
                // Convert rune to keysym (Latin-1 range maps directly).
                keysym := xproto.Keysym(r)
                if r >= 0x100 {
                        // For chars above Latin-1, try the unicode keysym (0x01000000 + r).
                        keysym = xproto.Keysym(0x01000000) | xproto.Keysym(r)
                }
                keycode := i.keysymToKeycode(keysym)
                if keycode == 0 {
                        if debugInput {
                                fmt.Fprintf(os.Stderr, "[input] no keycode for %q\n", r)
                        }
                        continue
                }
                if err := i.fakeKey(keycode, xproto.KeyPress); err != nil {
                        return err
                }
                if err := i.fakeKey(keycode, xproto.KeyRelease); err != nil {
                        return err
                }
        }
        return nil
}

// keysymToKeycode maps a keysym to the first keycode that produces it.
// Uses the X server's keymap (GetKeyboardMapping).
func (i *xtestInjector) keysymToKeycode(keysym xproto.Keysym) xproto.Keycode {
        min, max := i.keycodeRange()
        if max < min {
                return 0
        }
        // GetKeyboardMapping returns a list of keysyms per keycode, in groups
        // of KeysymsPerKeycode. We check the first group (no modifiers).
        cookie := xproto.GetKeyboardMapping(i.conn, min, byte(max-min+1))
        reply, err := cookie.Reply()
        if err != nil || reply == nil {
                return 0
        }
        perKC := int(reply.KeysymsPerKeycode)
        if perKC < 1 {
                return 0
        }
        for idx := 0; idx < len(reply.Keysyms); idx += perKC {
                for g := 0; g < perKC; g++ {
                        if xproto.Keysym(reply.Keysyms[idx+g]) == keysym {
                                return xproto.Keycode(min + xproto.Keycode(idx/perKC))
                        }
                }
        }
        return 0
}

// keycodeRange returns the min/max keycode range from the X server setup.
func (i *xtestInjector) keycodeRange() (xproto.Keycode, xproto.Keycode) {
        setup := xproto.Setup(i.conn)
        min := setup.MinKeycode
        max := setup.MaxKeycode
        return min, max
}

// Mouse moves the pointer and presses/releases buttons as needed.
func (i *xtestInjector) Mouse(ev protocol.MouseEvent) error {
        i.mu.Lock()
        defer i.mu.Unlock()

        // Move pointer to (x, y).
        if err := i.fakeMove(int16(ev.X), int16(ev.Y)); err != nil {
                return err
        }

        // On a button press, also set the input focus to the window under the
        // pointer. Without this, xfwm4 (which doesn't always honor click-to-focus
        // without a panel) keeps focus on itself, so XTEST-injected keys never
        // reach the app the user clicked.
        if ev.Mask&protocol.ButtonLeft != 0 && i.mouseMask&protocol.ButtonLeft == 0 {
                i.focusWindowUnderPointer(ev.X, ev.Y)
        }

        // Button press/release derived from mask deltas vs. previous state.
        buttons := []struct {
                mask uint16
                btn  byte // XTEST button number
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
                        if err := i.fakeButton(b.btn, xproto.ButtonPress); err != nil {
                                return err
                        }
                case now == 0 && was != 0:
                        if err := i.fakeButton(b.btn, xproto.ButtonRelease); err != nil {
                                return err
                        }
                }
        }
        i.mouseMask = ev.Mask

        // Vertical wheel.
        if ev.Wheel != 0 {
                btn := byte(4) // wheel up
                if ev.Wheel < 0 {
                        btn = 5 // wheel down
                }
                steps := ev.Wheel
                if steps < 0 {
                        steps = -steps
                }
                for k := int16(0); k < steps; k++ {
                        // Wheel events are press + release pairs.
                        if err := i.fakeButton(btn, xproto.ButtonPress); err != nil {
                                return err
                        }
                        if err := i.fakeButton(btn, xproto.ButtonRelease); err != nil {
                                return err
                        }
                }
        }
        return nil
}

// focusWindowUnderPointer queries the window currently under the pointer
// and sets the input focus to it. This is a workaround for window managers
// that don't honor click-to-focus (or where the WM's focus mechanism is
// broken because we're not running a full session).
func (i *xtestInjector) focusWindowUnderPointer(x, y int) {
        // QueryPointer on the root to get the child window under the pointer.
        cookie := xproto.QueryPointer(i.conn, i.root)
        reply, err := cookie.Reply()
        if err != nil || reply == nil {
                if debugInput {
                        fmt.Fprintf(os.Stderr, "[input] focus: querypointer failed: %v\n", err)
                }
                return
        }
        child := reply.Child
        if debugInput {
                fmt.Fprintf(os.Stderr, "[input] focus: root child=0x%x at (%d,%d)\n", child, reply.WinX, reply.WinY)
        }
        if child == 0 {
                return
        }
        // Walk down: if the child has its own child under the pointer, recurse
        // a few levels to find the deepest window (the actual input target).
        for depth := 0; depth < 5; depth++ {
                sub, err := xproto.QueryPointer(i.conn, child).Reply()
                if err != nil || sub == nil || sub.Child == 0 {
                        break
                }
                if debugInput {
                        fmt.Fprintf(os.Stderr, "[input] focus: descend 0x%x -> 0x%x\n", child, sub.Child)
                }
                child = sub.Child
        }
        // Set input focus to that window. Use RevertToParent so focus returns
        // to the parent if the window is destroyed.
        if err := xproto.SetInputFocusChecked(i.conn, xproto.InputFocusParent, child, xproto.TimeCurrentTime).Check(); err != nil {
                if debugInput {
                        fmt.Fprintf(os.Stderr, "[input] focus: setinputfocus to 0x%x failed: %v\n", child, err)
                }
        } else if debugInput {
                fmt.Fprintf(os.Stderr, "[input] focus: setinputfocus to 0x%x OK\n", child)
        }
}

// Key injects a key press or release.
//
// MODIFIER KEYS (Shift/Ctrl/Alt/Super): tracked with held state, sent as
// separate KeyPress (keydown) / KeyRelease (keyup) so modifier state
// persists across subsequent key presses.
//
// NON-MODIFIER KEYS: sent as an ATOMIC KeyPress+KeyRelease pair on
// keydown ONLY. The keyup event is ignored. This is critical because:
//
//   - The /poll transport sends keydown and keyup in SEPARATE HTTP
//     requests, potentially hundreds of milliseconds apart.
//   - X11 auto-repeat kicks in ~500ms after a KeyPress with no
//     KeyRelease, generating 30+ duplicate KeyPress events per second.
//   - This caused "niiiiiiiiiiihao" bursts when typing fast over a slow
//     aitun relay connection.
//
// By sending KeyPress+KeyRelease back-to-back in the same Key() call,
// the X server never sees a "held" key, so auto-repeat never triggers.
//
// MODIFIER CHORDS: when ev.Ctrl/Alt/Shift/Meta are set, we inject the
// full chord atomically (modifiers down → key press+release → modifiers
// up) in a single Key() call, also avoiding auto-repeat.
func (i *xtestInjector) Key(ev protocol.KeyEvent) error {
        keycode := i.keycodeForEvent(ev)
        if keycode == 0 {
                return nil
        }

        keysym := domToKeysym(ev.Code, ev.Key)
        isMod := isModifierKeysym(keysym)

        // ── Modifier keys: track held state, send press/release separately ──
        if isMod {
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

                type_ := byte(xproto.KeyPress)
                if !ev.Down {
                        type_ = byte(xproto.KeyRelease)
                }
                if debugInput {
                        fmt.Fprintf(os.Stderr, "[input] XTEST key %s code=%d mod=true\n",
                                map[bool]string{true: "down", false: "up"}[ev.Down], keycode)
                }
                return i.fakeKey(xproto.Keycode(keycode), type_)
        }

        // ── Non-modifier keys: only act on keydown; ignore keyup ──
        // The keyup was already handled by the atomic press+release below.
        if !ev.Down {
                return nil
        }

        // If modifier flags are set, inject as an atomic chord.
        hasMods := ev.Ctrl || ev.Alt || ev.Shift || ev.Meta
        if hasMods {
                return i.injectChord(keycode, ev.Ctrl, ev.Alt, ev.Shift, ev.Meta)
        }

        // Plain key: atomic KeyPress + KeyRelease (no auto-repeat risk).
        if debugInput {
                fmt.Fprintf(os.Stderr, "[input] XTEST key tap code=%d\n", keycode)
        }
        if err := i.fakeKey(xproto.Keycode(keycode), byte(xproto.KeyPress)); err != nil {
                return err
        }
        return i.fakeKey(xproto.Keycode(keycode), byte(xproto.KeyRelease))
}

// injectChord presses the requested modifiers, then the main key, then
// releases everything. This produces a true Ctrl+C / Alt+Tab / etc. on the
// remote X server.
func (i *xtestInjector) injectChord(keycode xproto.Keycode, ctrl, alt, shift, meta bool) error {
        // Build the list of modifier keycodes to press.
        var modKCs []xproto.Keycode
        if ctrl {
                if kc := i.keysymToKeycode(0xffe3); kc != 0 { // Control_L
                        modKCs = append(modKCs, kc)
                }
        }
        if alt {
                if kc := i.keysymToKeycode(0xffe9); kc != 0 { // Alt_L
                        modKCs = append(modKCs, kc)
                }
        }
        if shift {
                if kc := i.keysymToKeycode(0xffe1); kc != 0 { // Shift_L
                        modKCs = append(modKCs, kc)
                }
        }
        if meta {
                if kc := i.keysymToKeycode(0xffeb); kc != 0 { // Super_L
                        modKCs = append(modKCs, kc)
                }
        }

        if debugInput {
                fmt.Fprintf(os.Stderr, "[input] XTEST chord main=%d mods=%v (ctrl=%v alt=%v shift=%v meta=%v)\n",
                        keycode, modKCs, ctrl, alt, shift, meta)
        }

        // Press all modifiers.
        for _, kc := range modKCs {
                if err := i.fakeKey(kc, xproto.KeyPress); err != nil {
                        return err
                }
        }
        // Press + release the main key.
        if err := i.fakeKey(keycode, xproto.KeyPress); err != nil {
                return err
        }
        if err := i.fakeKey(keycode, xproto.KeyRelease); err != nil {
                return err
        }
        // Release all modifiers (in reverse order).
        for j := len(modKCs) - 1; j >= 0; j-- {
                if err := i.fakeKey(modKCs[j], xproto.KeyRelease); err != nil {
                        return err
                }
        }
        return nil
}

// keycodeForEvent maps a DOM KeyboardEvent (code + key) to an X keycode.
// We first convert the DOM code/key to an X keysym, then look up the
// keycode in the server's keymap.
func (i *xtestInjector) keycodeForEvent(ev protocol.KeyEvent) xproto.Keycode {
        keysym := domToKeysym(ev.Code, ev.Key)
        if keysym == 0 {
                return 0
        }
        kc := i.keysymToKeycode(keysym)
        if kc != 0 {
                return kc
        }
        // Some keysyms (e.g. space) may need case-folding.
        if keysym >= 'A' && keysym <= 'Z' {
                kc = i.keysymToKeycode(keysym + 32) // lowercase
        }
        return kc
}

// domToKeysym converts a DOM KeyboardEvent.code (and .key fallback) to an
// X11 keysym. The mapping covers the common keys; unmapped keys return 0.
func domToKeysym(code, key string) xproto.Keysym {
        switch code {
        case "Enter":
                return 0xff0d // Return
        case "Backspace":
                return 0xff08
        case "Tab":
                return 0xff09
        case "Escape":
                return 0xff1b
        case "Space":
                return 0x020
        case "ShiftLeft":
                return 0xffe1
        case "ShiftRight":
                return 0xffe2
        case "ControlLeft":
                return 0xffe3
        case "ControlRight":
                return 0xffe4
        case "AltLeft":
                return 0xffe9
        case "AltRight":
                return 0xffea
        case "MetaLeft":
                return 0xffeb
        case "MetaRight":
                return 0xffec
        case "CapsLock":
                return 0xffe5
        case "ArrowLeft":
                return 0xff51
        case "ArrowRight":
                return 0xff53
        case "ArrowUp":
                return 0xff52
        case "ArrowDown":
                return 0xff54
        case "Home":
                return 0xff50
        case "End":
                return 0xff57
        case "PageUp":
                return 0xff55
        case "PageDown":
                return 0xff56
        case "Insert":
                return 0xff63
        case "Delete":
                return 0xffff
        case "F1":
                return 0xffbe
        case "F2":
                return 0xffbf
        case "F3":
                return 0xffc0
        case "F4":
                return 0xffc1
        case "F5":
                return 0xffc2
        case "F6":
                return 0xffc3
        case "F7":
                return 0xffc4
        case "F8":
                return 0xffc5
        case "F9":
                return 0xffc6
        case "F10":
                return 0xffc7
        case "F11":
                return 0xffc8
        case "F12":
                return 0xffc9
        case "Minus":
                return 0x02d // -
        case "Equal":
                return 0x03d // =
        case "BracketLeft":
                return 0x05b // [
        case "BracketRight":
                return 0x05d // ]
        case "Backslash":
                return 0x05c // \
        case "Semicolon":
                return 0x03b // ;
        case "Quote":
                return 0x027 // '
        case "Comma":
                return 0x02c // ,
        case "Period":
                return 0x02e // .
        case "Slash":
                return 0x02f // /
        case "Backquote":
                return 0x060 // `
        }
        // Letter / digit keys: DOM code "KeyA" -> keysym 'a', "Digit1" -> '1'.
        if len(code) >= 4 {
                if strings.HasPrefix(code, "Key") {
                        ch := code[3]
                        if ch >= 'A' && ch <= 'Z' {
                                return xproto.Keysym(ch) // uppercase keysym; keycode lookup
                                // will handle both cases via the keymap's group 0/1.
                        }
                }
                if strings.HasPrefix(code, "Digit") {
                        ch := code[5]
                        if ch >= '0' && ch <= '9' {
                                return xproto.Keysym(ch)
                        }
                }
        }
        // Numpad keys.
        if strings.HasPrefix(code, "Numpad") {
                tail := code[6:]
                switch tail {
                case "Enter":
                        return 0xff8d
                case "Add":
                        return 0xffab
                case "Subtract":
                        return 0xffad
                case "Multiply":
                        return 0xffaa
                case "Divide":
                        return 0xffaf
                case "Decimal":
                        return 0xffae
                }
                if len(tail) == 1 && tail[0] >= '0' && tail[0] <= '9' {
                        return xproto.Keysym(tail[0])
                }
        }
        // Final fallback: if the key is a single ASCII char, use it as keysym.
        if len(key) == 1 {
                return xproto.Keysym(key[0])
        }
        return 0
}

// isModifier reports whether a keysym corresponds to a modifier key.
// (Currently unused but kept for future modifier-state tracking.)
func isModifierKeysym(keysym xproto.Keysym) bool {
        switch keysym {
        case 0xffe1, 0xffe2, // Shift_L/R
                0xffe3, 0xffe4, // Control_L/R
                0xffe9, 0xffea, // Alt_L/R
                0xffeb, 0xffec, // Super_L/R
                0xffe5: // Caps_Lock
                return true
        }
        return false
}
