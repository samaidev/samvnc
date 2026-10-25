//go:build !windows

package main

import (
        "bytes"
        "log"
        "os/exec"
        "runtime"
        "strconv"
        "strings"
        "time"

        "github.com/samaidev/samvnc/internal/settings"
)

// openSettingsDialog opens a native settings UI on non-Windows platforms:
//   - macOS: AppleScript dialogs via osascript (key + token input, language
//     choice, auto-start toggle, copy-address button that really copies)
//   - Linux: zenity dialogs when available (form for key + token, entry for
//     the copyable address, question for auto-start, radiolist for language);
//     silently no-op otherwise.
//
// It returns a modified COPY of the settings; ok=false means the user
// cancelled (or no dialog backend exists) and the caller keeps the
// original values.
func openSettingsDialog(s *settings.Settings, publicURL, authToken string,
) (newSettings *settings.Settings, copyAddr bool, ok bool) {

        switch runtime.GOOS {
        case "darwin":
                return settingsDialogDarwin(s, publicURL, authToken)
        case "linux":
                return settingsDialogZenity(s, publicURL)
        }
        return s, false, false
}

// appleQuote escapes a Go string for embedding in a double-quoted
// AppleScript string literal.
func appleQuote(s string) string {
        r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
        return `"` + r.Replace(s) + `"`
}

// settingsDialogDarwin shows osascript dialogs on macOS. The flow is a
// sequence of small native dialogs, each matching one section of the
// Windows settings page:
//
//  1. Connection: Aitun.cc key + Access Token in one dialog
//  2. Copy Address (button) — copies URL+token to the clipboard
//  3. General: auto-start yes/no + language
//
// Cancel at any step keeps the original settings.
//
// Every dialog runs through runOsaActivated: dialogs shown by a background
// process (the tray app started by autostart/launchd or from a shell) do
// NOT get the key window on their own — the dialog renders but keystrokes
// keep going to whatever app was frontmost, so the text field looks dead
// ("cannot type the key"). runOsaActivated re-activates the dialog process
// by unix id right after the window appears, which reliably gives it key
// focus on macOS 10.14+.
func settingsDialogDarwin(s *settings.Settings, publicURL, authToken string,
) (*settings.Settings, bool, bool) {

        // ---- 1. Connection (key + token) ----
        connMsg := tr("dlg.key") + "\n\n" + tr("dlg.hint")
        out, err := runOsaActivated("display dialog " + appleQuote(connMsg) +
                " default answer " + appleQuote(s.AgentToken) +
                " with title " + appleQuote(tr("dlg.title")) +
                " buttons {" + appleQuote(tr("dlg.cancel")) + "," +
                appleQuote(tr("dlg.ok")) + "}" +
                " default button " + appleQuote(tr("dlg.ok")) +
                " cancel button " + appleQuote(tr("dlg.cancel")))
        if err != nil {
                // Exit 128 == user pressed the cancel button.
                return s, false, false
        }
        key, _ := parseOsaDialog(out)

        ns := *s
        ns.AgentToken = key

        // Access Token in its own dialog (osascript shows one text field per
        // dialog unless a full AppleScript app is built; a separate compact
        // dialog is clearer than losing the token entirely). The dialog
        // carries an explicit Regenerate button: pressing it creates a
        // fresh token, copies it to the clipboard and shows it once.
        tokenMsg := tr("dlg.token") + "\n(" + tr("dlg.tokenHint") + ")"
        tokenOut, tokenErr := runOsaActivated("display dialog " + appleQuote(tokenMsg) +
                " default answer " + appleQuote(s.AuthToken) +
                " with title " + appleQuote(tr("dlg.title")) +
                " buttons {" + appleQuote(tr("dlg.cancel")) + "," +
                appleQuote(tr("dlg.btnRegen")) + "," +
                appleQuote(tr("dlg.ok")) + "}" +
                " default button " + appleQuote(tr("dlg.ok")) +
                " cancel button " + appleQuote(tr("dlg.cancel")))
        if tokenErr == nil {
                token, tokenBtn := parseOsaDialog(tokenOut)
                if strings.Contains(tokenBtn, "Regenerate") || strings.Contains(tokenBtn, "重新生成") {
                        // Explicit Regenerate: new token + clipboard copy.
                        ns.AuthToken = settings.GenerateToken()
                        _, _ = runOsa("set the clipboard to " + appleQuote(ns.AuthToken))
                        _, _ = runOsaActivated("display dialog " +
                                appleQuote(tr("dlg.regenDone")+"\n"+ns.AuthToken) +
                                " with title " + appleQuote(tr("dlg.title")) +
                                " buttons {" + appleQuote(tr("dlg.ok")) + "}" +
                                " default button " + appleQuote(tr("dlg.ok")))
                } else {
                        ns.AuthToken = token
                }
        } else {
                // Cancelled the token step — cancel the whole settings flow.
                return s, false, false
        }
        // An emptied token field also means "rotate it" — same semantics as
        // the Windows dialog and the tray's applySettings().
        if ns.AuthToken == "" {
                ns.AuthToken = settings.GenerateToken()
        }

        // ---- 2. Copy Address (offer + actually copy) ----
        if publicURL != "" {
                fullURL := publicURL
                if ns.AuthToken != "" {
                        fullURL += "?token=" + ns.AuthToken
                }
                copyOut, copyErr := runOsaActivated("display dialog " +
                        appleQuote(tr("dlg.addrLabel")+":\n"+fullURL) +
                        " with title " + appleQuote(tr("dlg.title")) +
                        " buttons {" + appleQuote(tr("dlg.cancel")) + "," +
                        appleQuote(tr("dlg.btnCopy")) + "," +
                        appleQuote(tr("dlg.ok")) + "}" +
                        " default button " + appleQuote(tr("dlg.ok")) +
                        " cancel button " + appleQuote(tr("dlg.cancel")))
                if copyErr != nil {
                        return s, false, false
                }
                _, copyBtn := parseOsaDialog(copyOut)
                if strings.Contains(copyBtn, "Copy") || strings.Contains(copyBtn, "复制") {
                        // Real clipboard write via AppleScript (no window — plain
                        // runOsa, no activation needed).
                        _, _ = runOsa("set the clipboard to " + appleQuote(fullURL))
                        // Do not treat the copy click as "save" — fall through to
                        // the general dialog so language/autostart still apply.
                }
        }

        // ---- 3. General: auto-start + language ----
        genMsg := tr("dlg.autostartQ")
        genOut, genErr := runOsaActivated("display dialog " + appleQuote(genMsg) +
                " with title " + appleQuote(tr("dlg.title")) +
                " buttons {" + appleQuote(tr("dlg.language")) + ",\"" +
                "Yes\",\"No\"}" +
                " default button \"No\"" +
                " giving up after 120")
        if genErr == nil {
                _, genBtn := parseOsaDialog(genDialogButton(genOut))
                // The language chooser replaces the "Language" button path:
                // if the user pressed the Language button, show the picker.
                if strings.HasPrefix(genBtn, "Language") || strings.HasPrefix(genBtn, "语言") {
                        langOut, langErr := runOsaActivated("display dialog " + appleQuote(tr("dlg.language")) +
                                " with title " + appleQuote(tr("dlg.title")) +
                                " buttons {\"Auto\",\"中文\",\"English\"} default button \"Auto\"" +
                                " giving up after 120")
                        if langErr == nil {
                                if _, lbtn := parseOsaDialog(langOut); lbtn != "" {
                                        switch lbtn {
                                        case "中文":
                                                ns.Language = "zh"
                                        case "English":
                                                ns.Language = "en"
                                        default:
                                                ns.Language = ""
                                        }
                                }
                        }
                } else {
                        switch genBtn {
                        case "Yes":
                                ns.AutoStart = true
                        case "No":
                                ns.AutoStart = false
                        }
                }
        }

        return &ns, false, true
}

// genDialogButton extracts the raw "button returned:X" line from an
// osascript result (helper for the general dialog).
func genDialogButton(out string) string { return out }

// runOsaActivated runs `osascript -e script` (any Standard Additions
// script, typically display dialog) and makes sure the window it creates
// becomes the FRONTMOST one so its text field accepts keyboard input.
//
// Why: the tray app is a background agent (started by autostart, launchd
// or a shell) and macOS never hands the key window to a background
// process on its own — the dialog renders but typed keys keep going to
// whatever app was already frontmost. Fix (verified on macOS 15): after
// the dialog process exists, a second tiny osascript tells System Events
// to `activate` it BY UNIX ID. `set frontmost` alone does not work, and
// plain `activate` from inside the dialog script fires before the window
// is mapped and does not stick either.
//
// Two activation attempts (0.7s and 1.8s) cover slow machines: the first
// may run before the window exists, activation only sticks once it is
// mapped. Activating an already-closed pid just errors harmlessly — that
// error is ignored. First use triggers a one-time "wants to control
// System Events" permission prompt; approving it once is enough forever.
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

// runOsa shells out to osascript and returns stdout (no activation —
// used for non-dialog scripts like clipboard writes).
func runOsa(script string) (string, error) {
        cmd := exec.Command("osascript", "-e", script)
        out, err := cmd.Output()
        return strings.TrimSpace(string(out)), err
}

// parseOsaDialog extracts "text returned:X" and "button returned:Y"
// from an osascript display-dialog result.
func parseOsaDialog(out string) (text, button string) {
        for _, line := range strings.Split(out, ",") {
                line = strings.TrimSpace(line)
                if v, ok := strings.CutPrefix(line, "text returned:"); ok {
                        text = v
                }
                if v, ok := strings.CutPrefix(line, "button returned:"); ok {
                        button = v
                }
        }
        return
}

// runZenity runs a zenity dialog and returns trimmed stdout. Exit code 1
// means the user dismissed the dialog (Cancel/Close); exit code 5 is the
// --timeout expiry; anything else is an error worth logging.
func runZenity(zenity string, args ...string) (string, error) {
        cmd := exec.Command(zenity, args...)
        outRaw, err := cmd.Output()
        out := strings.TrimSpace(string(outRaw))
        if err != nil {
                if ee, ok := err.(*exec.ExitError); ok && (ee.ExitCode() == 1 || ee.ExitCode() == 5) {
                        return out, err // normal dismissals — callers decide semantics
                }
                log.Printf("zenity %v: %v", args[:1], err)
        }
        return out, err
}

// runZenityEntry shows a `zenity --entry` dialog with the field prefilled
// (unlike --forms, --entry supports prefill on every zenity version).
// extraButton (when non-empty) adds a custom button — pressing it exits 0
// with the button label printed to stdout or stderr depending on the
// zenity build; both streams are captured and reported via the returned
// button value. A plain OK returns the (possibly emptied) field text.
// Cancel or window-close returns an error (exit 1).
func runZenityEntry(zenity, text, prefill, extraButton string) (entry, button string, err error) {
        args := []string{"--entry",
                "--title", tr("dlg.title"),
                "--text", text,
        }
        if prefill != "" {
                args = append(args, "--entry-text", prefill)
        }
        if extraButton != "" {
                args = append(args, "--extra-button", extraButton)
        }
        cmd := exec.Command(zenity, args...)
        var outBuf, errBuf bytes.Buffer
        cmd.Stdout = &outBuf
        cmd.Stderr = &errBuf
        if err := cmd.Run(); err != nil {
                return "", "", err // exit 1 = cancelled, 5 = timeout
        }
        out := strings.TrimRight(outBuf.String(), "\n")
        errOut := strings.TrimSpace(strings.TrimRight(errBuf.String(), "\n"))
        if extraButton != "" {
                firstLine := out
                if i := strings.IndexByte(out, '\n'); i >= 0 {
                        firstLine = out[:i]
                }
                if errOut == extraButton || out == extraButton || strings.TrimSpace(firstLine) == extraButton {
                        return "", extraButton, nil
                }
        }
        return strings.TrimSpace(out), "", nil
}

// settingsDialogZenity shows zenity dialogs on Linux; no-op without zenity.
//
// NOTE ON ZENITY 4.x (GTK4, Ubuntu 26.04+): `--forms` no longer accepts
// `--add-checkbox` — it exits 255 with "option not available", which made
// the ENTIRE settings dialog silently fail on new distros (nothing at all
// appeared when the user clicked Settings).
//
// NOTE ON PREFILL: `--forms` also CANNOT prefill its entry fields, so
// every open showed EMPTY key and token fields — the auto-generated token
// looked missing, and clicking OK silently WIPED the configured key. The
// connection step therefore uses prefilled `--entry` dialogs (--entry-text
// works on every zenity version), one per field, with an extra
// Regenerate button on the token dialog (--extra-button).
//
// The copy-address button is a pre-filled --entry whose text is selectable
// (Ctrl+A, Ctrl+C) in every zenity version — stock GNOME Wayland has no
// clipboard tool, so a selectable field is the reliable way to copy.
//
// Flow, mirroring the Windows settings page sections:
//  1. Connection entries: Aitun.cc key, then Access Token (+ Regenerate)
//  2. Copy Address: selectable entry pre-filled with URL (+ auto-copy on OK)
//  3. Auto-start: question dialog — OK = enable, Cancel = disable
//  4. Language: radiolist (Auto / 中文 / English)
func settingsDialogZenity(s *settings.Settings, publicURL string,
) (*settings.Settings, bool, bool) {
        zenity, err := exec.LookPath("zenity")
        if err != nil {
                return s, false, false
        }

        ns := *s

        // ---- 1a. Aitun.cc key (prefilled; empty = free proxy mode) ----
        keyText := tr("dlg.key") + "\n\n" + tr("dlg.hint")
        if publicURL != "" {
                keyText += "\n\n" + tr("dlg.addrLabel") + ": " + publicURL
        }
        keyOut, _, keyErr := runZenityEntry(zenity, keyText, s.AgentToken, "")
        if keyErr != nil {
                return s, false, false // dismissed (exit 1) — keep originals
        }
        ns.AgentToken = keyOut

        // ---- 1b. Access token (prefilled, extra Regenerate button) ----
        for {
                tokOut, tokBtn, tokErr := runZenityEntry(zenity,
                        tr("dlg.token")+"\n("+tr("dlg.tokenHint")+")",
                        ns.AuthToken, tr("dlg.btnRegen"))
                if tokErr != nil {
                        return s, false, false // dismissed — keep originals
                }
                if tokBtn != "" {
                        // Regenerate pressed: fresh token, shown in the
                        // field again so the user can see/copy it.
                        ns.AuthToken = settings.GenerateToken()
                        continue
                }
                ns.AuthToken = tokOut
                break
        }
        // An emptied token field means "rotate it" — same semantics as the
        // Windows dialog and the tray's applySettings().
        if ns.AuthToken == "" {
                ns.AuthToken = settings.GenerateToken()
        }

        // ---- 2. Copy Address: selectable, pre-filled entry + auto-copy ----
        if publicURL != "" {
                fullURL := publicURL
                if ns.AuthToken != "" {
                        fullURL += "?token=" + ns.AuthToken
                }
                entryArgs := []string{"--entry", "--title", tr("dlg.title"),
                        "--text", tr("dlg.copyHint"),
                        "--entry-text", fullURL}
                if _, err := runZenity(zenity, entryArgs...); err == nil {
                        // OK pressed — try a real clipboard write. Even when no
                        // clipboard tool exists the user could still select + Ctrl+C
                        // the text inside the dialog above.
                        if cerr := copyToClipboard(fullURL); cerr != nil {
                                log.Printf("clipboard: %v", cerr)
                        }
                }
        }

        // ---- 3. Auto-start: OK = enable, Cancel = disable ----
        state := tr("dlg.stOff")
        if ns.AutoStart {
                state = tr("dlg.stOn")
        }
        qArgs := []string{"--question", "--title", tr("dlg.title"),
                "--text", tr("dlg.autostartQ") + "\n" +
                        tr("dlg.now") + " " + state + " · " + tr("dlg.okEnable")}
        if _, qerr := runZenity(zenity, qArgs...); qerr == nil {
                ns.AutoStart = true
        } else {
                ns.AutoStart = false
        }

        // ---- 4. Language (radiolist works on zenity 3 and 4) ----
        langRaw, err := runZenity(zenity, "--list",
                "--title", tr("dlg.title"),
                "--text", tr("dlg.language"),
                "--radiolist",
                "--column", "", "--column", "Language",
                "TRUE", "Auto", "FALSE", "中文", "FALSE", "English")
        if err != nil {
                // Dismissed — keep the current language, keep the form results.
                return &ns, false, true
        }
        switch strings.TrimSpace(langRaw) {
        case "中文":
                ns.Language = "zh"
        case "English":
                ns.Language = "en"
        case "Auto":
                ns.Language = ""
        }
        return &ns, false, true
}

// showCopyFallback is the last-resort clipboard helper for the tray
// "Copy Address" menu item: when no clipboard tool exists, open a dialog
// with the URL in a selectable text field so the user can copy it
// manually (Ctrl+A, Ctrl+C) instead of getting a silent failure.
func showCopyFallback(text string) {
        switch runtime.GOOS {
        case "linux":
                if zenity, err := exec.LookPath("zenity"); err == nil {
                        _, _ = runZenity(zenity, "--entry",
                                "--title", tr("dlg.title"),
                                "--text", tr("dlg.copyFallback"),
                                "--entry-text", text)
                        return
                }
        case "darwin":
                _, _ = runOsaActivated("display dialog " + appleQuote(tr("dlg.copyFallback")+"\n"+text) +
                        " with title " + appleQuote(tr("dlg.title")) +
                        " default answer " + appleQuote(text) +
                        " buttons {" + appleQuote(tr("dlg.ok")) + "}" +
                        " default button 1 giving up after 120")
        }
}
