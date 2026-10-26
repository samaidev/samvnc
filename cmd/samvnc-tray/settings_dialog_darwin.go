//go:build darwin && cgo

package main

// settings_dialog_darwin.go — Go side of the NATIVE macOS settings window.
//
// The old flow was a sequence of osascript `display dialog` prompts (key →
// token → copy → auto-start → language). That buried the Regenerate button
// on the second of four sheets and the language picker on the last one, so
// users never saw either ("no Regenerate button on mac"). This file + its
// Objective-C half (settings_dialog_darwin.m) replace it with ONE native
// Cocoa window that mirrors the Windows settings page:
//
//   Connection:  Aitun.cc Key  [___________]
//                Access Token  [________] [Regenerate]  ← new token + auto
//                [Copy Address]  https://…?token=…       copy + popup note
//   General:     Language: [Auto ▾]   ← switches EVERY label live
//                [x] Start SamVNC on system boot
//                                        [Cancel] [OK]
//
// Cross-language plumbing:
//   - Go → ObjC: nativeSettingsShow / Relocalize / SetToken / CopyClipboard
//     / ShowNote, all called from the main thread (the tray menu callback
//     runs there).
//   - ObjC → Go: traySettingsAction(code, arg) exported below; buttons
//     bounce back into Go so the token generation and the language switch
//     stay in one place (tr(), settings.GenerateToken, ResolveLang…).

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa
#include <stdlib.h>

// All UI strings, re-built from tr() on every language change.
typedef struct {
    const char* title;
    const char* subtitle;
    const char* groupConn;
    const char* keyLabel;
    const char* keyPlaceholder;
    const char* tokenLabel;
    const char* tokenHint;
    const char* btnRegen;
    const char* btnCopy;
    const char* copiedPrefix;
    const char* urlBase;      // public URL WITHOUT token (copy builds on it)
    const char* urlText;      // display text: urlBase + ?token=<current>
    const char* groupGeneral;
    const char* langLabel;
    const char* langAuto;
    const char* langEn;
    const char* langZh;
    const char* autostart;
    const char* btnOK;
    const char* btnCancel;
} SamVNCSettingsTexts;

void nativeSettingsShow(const SamVNCSettingsTexts* t, const char* keyValue,
                        const char* tokenValue, int langIndex,
                        int autostartOn);
void nativeSettingsRelocalize(const SamVNCSettingsTexts* t);
void nativeSettingsSetToken(const char* tok);
void nativeSettingsCopyClipboard(const char* text);
void nativeSettingsShowNote(const char* msg);
const char* nativeSettingsResultKey(void);
const char* nativeSettingsResultToken(void);
int nativeSettingsResultLang(void);
int nativeSettingsResultAutostart(void);
int nativeSettingsResultOK(void);
*/
import "C"

import (
        "bytes"
        "log"
        "os/exec"
        "strconv"
        "strings"
        "time"
        "unsafe"

        "github.com/samaidev/samvnc/internal/settings"
)

// Action codes used by traySettingsAction (must match the .m side).
const (
        settingsActionRegen = 1 // Regenerate pressed
        settingsActionLang  = 2 // language popup changed (arg: auto|en|zh)
        settingsActionCopy  = 3 // Copy Address pressed (info only)
)

// openSettingsDialog shows the native Cocoa settings window. Blocks the
// tray menu callback (main thread) in a modal session until the window is
// closed — same contract as the Windows dialog and the old osascript flow.
func openSettingsDialog(s *settings.Settings, publicURL, authToken string,
) (newSettings *settings.Settings, copyAddr bool, ok bool) {
        urlBase := publicURL
        urlText := publicURL
        if urlText != "" && authToken != "" {
                sep := "?"
                for i := 0; i < len(urlText); i++ { // cheap strings.ContainsRune('?')
                        if urlText[i] == '?' {
                                sep = "&"
                                break
                        }
                }
                urlText += sep + "token=" + authToken
        }

        t := newCSettingsTexts(urlBase, urlText)
        defer t.free()

        keyValue := C.CString(s.AgentToken)
        defer C.free(unsafe.Pointer(keyValue))
        tokenValue := C.CString(s.AuthToken)
        defer C.free(unsafe.Pointer(tokenValue))

        langIndex := 0 // 0 auto, 1 en, 2 zh — popup INDEX, like Windows
        switch ResolveLang(s.Language) {
        case "en":
                langIndex = 1
        case "zh":
                langIndex = 2
        }

        autostart := 0
        if s.AutoStart {
                autostart = 1
        }

        log.Printf("[settings] native dialog open (lang=%s)", CurrentLang())
        C.nativeSettingsShow(&t.c, keyValue, tokenValue,
                C.int(langIndex), C.int(autostart))

        if C.nativeSettingsResultOK() == 0 {
                log.Printf("[settings] native dialog cancelled")
                return s, false, false
        }

        ns := *s
        ns.AgentToken = C.GoString(C.nativeSettingsResultKey())
        ns.AuthToken = C.GoString(C.nativeSettingsResultToken())
        switch C.nativeSettingsResultLang() {
        case 1:
                ns.Language = "en"
        case 2:
                ns.Language = "zh"
        default:
                ns.Language = ""
        }
        ns.AutoStart = C.nativeSettingsResultAutostart() != 0
        log.Printf("[settings] native dialog OK: key_len=%d token_len=%d lang=%q autostart=%v",
                len(ns.AgentToken), len(ns.AuthToken), ns.Language, ns.AutoStart)
        return &ns, false, true
}

// cSettingsTexts owns the C strings packed into a SamVNCSettingsTexts.
// The ObjC side copies everything into NSStrings synchronously, so the
// CStrings can be freed as soon as the call into C returns.
type cSettingsTexts struct {
        c    C.SamVNCSettingsTexts
        ptrs []unsafe.Pointer
}

func cstr(t *cSettingsTexts, s string) *C.char {
        cs := C.CString(s)
        t.ptrs = append(t.ptrs, unsafe.Pointer(cs))
        return cs
}

func newCSettingsTexts(urlBase, urlText string) *cSettingsTexts {
        t := &cSettingsTexts{}
        t.c.title = cstr(t, tr("dlg.title"))
        t.c.subtitle = cstr(t, tr("dlg.subtitle"))
        t.c.groupConn = cstr(t, tr("dlg.groupConn"))
        t.c.keyLabel = cstr(t, tr("dlg.key"))
        t.c.keyPlaceholder = cstr(t, tr("dlg.keyPh"))
        t.c.tokenLabel = cstr(t, tr("dlg.token"))
        t.c.tokenHint = cstr(t, tr("dlg.tokenHint"))
        t.c.btnRegen = cstr(t, tr("dlg.btnRegen"))
        t.c.btnCopy = cstr(t, tr("dlg.btnCopy"))
        t.c.copiedPrefix = cstr(t, tr("dlg.copied"))
        t.c.urlBase = cstr(t, urlBase)
        t.c.urlText = cstr(t, urlText)
        t.c.groupGeneral = cstr(t, tr("dlg.groupGeneral"))
        t.c.langLabel = cstr(t, tr("dlg.language"))
        t.c.langAuto = cstr(t, tr("lang.auto"))
        t.c.langEn = cstr(t, tr("lang.en"))
        t.c.langZh = cstr(t, tr("lang.zh"))
        t.c.autostart = cstr(t, tr("dlg.autostart"))
        t.c.btnOK = cstr(t, tr("dlg.ok"))
        t.c.btnCancel = cstr(t, tr("dlg.cancel"))
        return t
}

func (t *cSettingsTexts) free() {
        for _, p := range t.ptrs {
                C.free(p)
        }
        t.ptrs = nil
}

// traySettingsAction is the single callback the Objective-C window uses to
// reach Go. It ALWAYS runs on the main thread (button actions fire inside
// the AppKit event loop there), so the nested calls back into the ObjC
// helpers below are safe.
//
//export traySettingsAction
func traySettingsAction(code C.int, arg *C.char) {
        switch code {
        case settingsActionRegen:
                // Fresh token → put it into the field, copy it to the clipboard
                // and show the confirmation note (exactly the behaviour the user
                // asked to see on macOS: 自动复制 + 弹窗展示).
                tok := settings.GenerateToken()
                ct := C.CString(tok)
                C.nativeSettingsSetToken(ct)
                C.nativeSettingsCopyClipboard(ct)
                C.free(unsafe.Pointer(ct))
                msg := tr("dlg.regenDone") + "\n" + tok
                cm := C.CString(msg)
                C.nativeSettingsShowNote(cm)
                C.free(unsafe.Pointer(cm))
                log.Printf("[settings] token regenerated in dialog (len=%d)", len(tok))

        case settingsActionLang:
                // Apply the language at once and re-localize every visible label
                // without touching the field values (the Windows Language combo
                // experience). "Auto" resolves through the system language.
                var pref string
                switch C.GoString(arg) {
                case "en":
                        pref = "en"
                case "zh":
                        pref = "zh"
                }
                SetLanguage(ResolveLang(pref))
                t := newCSettingsTexts("", "") // labels only — values stay put
                defer t.free()
                C.nativeSettingsRelocalize(&t.c)
                log.Printf("[settings] language switched in dialog (now %s)", CurrentLang())

        case settingsActionCopy:
                // The copy itself happened in ObjC (it reads the CURRENT token
                // field, so a freshly regenerated token is included).
                log.Printf("[settings] address copied from dialog")
        }
}

// appleQuote escapes a Go string for embedding in a double-quoted
// AppleScript string literal (still used by the copy-address fallback).
func appleQuote(s string) string {
        r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
        return `"` + r.Replace(s) + `"`
}

// showCopyFallback is the last-resort clipboard helper for the tray
// "Copy Address" menu item: open a dialog with the URL in a selectable
// text field so the user can copy it manually (Cmd+C) instead of getting
// a silent failure.
func showCopyFallback(text string) {
        _, _ = runOsaActivated("display dialog " + appleQuote(tr("dlg.copyFallback")+"\n"+text) +
                " with title " + appleQuote(tr("dlg.title")) +
                " default answer " + appleQuote(text) +
                " buttons {" + appleQuote(tr("dlg.ok")) + "}" +
                " default button 1 giving up after 120")
}

// runOsaActivated runs `osascript -e script` and re-activates the dialog
// process by unix id so its text field accepts keyboard input (background
// tray processes never get the key window on their own). Kept for the
// copy-address fallback; the settings window itself is native Cocoa and
// does not need this dance.
func runOsaActivated(script string) (string, error) {
        cmd := exec.Command("osascript", "-e", script)
        var out bytes.Buffer
        cmd.Stdout = &out
        if err := cmd.Start(); err != nil {
                return "", err
        }
        pid := cmd.Process.Pid
        go func() {
                act := "tell application \"System Events\" to tell " +
                        "(first application process whose unix id is " + strconv.Itoa(pid) + ") " +
                        "to activate"
                for _, delay := range []time.Duration{700 * time.Millisecond, 1800 * time.Millisecond} {
                        time.Sleep(delay)
                        // The dialog may already be answered and gone; activating a
                        // dead pid fails without side effects — ignore the error.
                        a := exec.Command("osascript", "-e", act)
                        _ = a.Run()
                }
        }()
        err := cmd.Wait()
        return strings.TrimSpace(out.String()), err
}
