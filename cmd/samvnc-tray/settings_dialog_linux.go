//go:build linux

package main

import (
    "bytes"
    _ "embed"
    "encoding/json"
    "fmt"
    "log"
    "os"
    "os/exec"
    "strings"

    "github.com/samaidev/samvnc/internal/settings"
)

// The GTK3 settings dialog itself, embedded into the binary and handed to
// python3 at runtime (see settingsDialogGTK). Kept as a separate file so
// editors and reviewers can read/tune the UI without touching the Go glue.
//
//go:embed settings_dialog_linux_gtk.py
var gtkSettingsScript string

// settingsSelftest is set by main() when the tray is started with
// --settings-selftest: the dialog runs a scripted UI pass (clicks the
// Regenerate / Copy / language controls itself) and reports PASS/FAIL on
// stderr. Other platforms ignore the flag.
var settingsSelftest bool

// openSettingsDialog opens the settings dialog on Linux.
//
// PRIMARY: a native GTK3 window (settings_dialog_linux_gtk.py, embedded and
// run via python3 + PyGObject) that mirrors the Windows/macOS settings page
// in ONE dialog: language + key + token + Regenerate + copy-address +
// auto-start. zenity 4.x cannot express this (no --forms prefill, no
// --extra-button on --entry in 4.2.1, no multi-field form), which is why
// the Regenerate button was invisible and language lived on its own page.
//
// FALLBACK: the old zenity flow, kept for rare desktops without PyGObject.
//
// It returns a modified COPY of the settings; ok=false means the user
// cancelled and the caller keeps the original values.
func openSettingsDialog(s *settings.Settings, publicURL, authToken string,
) (newSettings *settings.Settings, copyAddr bool, ok bool) {
    if ns, regenURL, ok, handled := settingsDialogGTK(s, publicURL); handled {
        if ok && regenURL != "" {
            // Re-copy from the long-lived tray process: the dialog's
            // GTK clipboard store() can be lost on Wayland once the
            // short-lived python process exits without a manager.
            if err := copyToClipboard(regenURL); err != nil {
                log.Printf("clipboard re-copy after regen: %v", err)
            }
        }
        return ns, false, ok
    }
    return settingsDialogZenity(s, publicURL)
}

// runSettingsSelftest drives the real GTK dialog with scripted clicks and
// reports the outcome. Used by `samvnc-tray --settings-selftest`; a fake
// public URL exercises the address/copy row without any server running.
func runSettingsSelftest(s *settings.Settings) int {
    // Surface the dialog's [SELFTEST] lines on the real stderr (the
    // default log target is ~/.samvnc/tray.log).
    log.SetOutput(os.Stderr)
    url := "https://demo-host.t.aitun.cc"
    ns, _, ok := openSettingsDialog(s, url, s.AuthToken)
    if !ok {
        log.Printf("[settings] selftest: dialog cancelled or failed")
        return 4
    }
    out, _ := json.Marshal(map[string]interface{}{
        "applied":   true,
        "key_len":   len(ns.AgentToken),
        "token":     ns.AuthToken,
        "language":  ns.Language,
        "autostart": ns.AutoStart,
    })
    fmt.Println(string(out))
    return 0
}

// ---------------------------------------------------------------------
// GTK3 dialog (python3 + PyGObject subprocess)
// ---------------------------------------------------------------------

// gtkConfig is the JSON piped to the embedded python dialog on stdin.
type gtkConfig struct {
    Lang      string                       `json:"lang"`     // resolved active UI language
    LangPref  string                       `json:"langPref"` // combo index source: ""|en|zh
    Key       string                       `json:"key"`
    Token     string                       `json:"token"`
    URL       string                       `json:"url"` // public URL without token
    AutoStart bool                         `json:"autostart"`
    Selftest  bool                         `json:"selftest"`
    I18N      map[string]map[string]string `json:"i18n"` // key -> {en,zh}
}

// gtkResult is the JSON the python dialog prints on stdout.
type gtkResult struct {
    OK        bool   `json:"ok"`
    Key       string `json:"key"`
    Token     string `json:"token"`
    Language  string `json:"language"`
    AutoStart bool   `json:"autostart"`
    RegenDone bool   `json:"regenDone"`
    Error     string `json:"error,omitempty"`
}

// settingsDialogGTK runs the embedded GTK3 settings window. handled=false
// means the GUI stack is unavailable and the caller must fall back to
// zenity. regenURL is non-empty when Regenerate was pressed in the dialog
// (the caller re-copies it from the long-lived tray process).
func settingsDialogGTK(s *settings.Settings, publicURL string,
) (ns *settings.Settings, regenURL string, ok bool, handled bool) {
    python, err := exec.LookPath("python3")
    if err != nil {
        return s, "", false, false
    }

    // Write the embedded script to a private temp file for this run.
    tmp, err := os.CreateTemp("", "samvnc-settings-*.py")
    if err != nil {
        log.Printf("settings gtk: temp script: %v", err)
        return s, "", false, false
    }
    scriptPath := tmp.Name()
    defer os.Remove(scriptPath)
    if _, werr := tmp.WriteString(gtkSettingsScript); werr != nil {
        tmp.Close()
        log.Printf("settings gtk: write script: %v", werr)
        return s, "", false, false
    }
    tmp.Close()
    if cherr := os.Chmod(scriptPath, 0o700); cherr != nil {
        log.Printf("settings gtk: chmod: %v", cherr)
    }

    cfg := gtkConfig{
        Lang:      CurrentLang(),
        LangPref:  s.Language,
        Key:       s.AgentToken,
        Token:     s.AuthToken,
        URL:       publicURL,
        AutoStart: s.AutoStart,
        Selftest:  settingsSelftest,
        I18N:      gtkI18N(),
    }
    cfgJSON, _ := json.Marshal(cfg)

    cmd := exec.Command(python, scriptPath)
    cmd.Stdin = bytes.NewReader(cfgJSON)
    var outBuf, errBuf bytes.Buffer
    cmd.Stdout = &outBuf
    cmd.Stderr = &errBuf
    runErr := cmd.Run()

    // In selftest mode a failure is an automated-check result: report
    // loudly, never silently fall back into a visible zenity flow.
    if runErr != nil && settingsSelftest {
        log.Printf("[settings] gtk selftest failed: %v", runErr)
        return s, "", false, true
    }

    if stderr := strings.TrimSpace(errBuf.String()); stderr != "" {
        for _, line := range strings.Split(stderr, "\n") {
            log.Printf("[settings-gtk] %s", line)
        }
    }
    if runErr != nil {
        log.Printf("settings gtk dialog: %v — falling back to zenity", runErr)
        return s, "", false, false
    }

    var res gtkResult
    if jerr := json.Unmarshal(bytes.TrimSpace(outBuf.Bytes()), &res); jerr != nil {
        log.Printf("settings gtk: unparseable result — falling back: %v", jerr)
        return s, "", false, false
    }
    if !res.OK {
        log.Printf("[settings] Cancel (gtk)")
        return s, "", false, true
    }

    log.Printf("[settings] OK (gtk): key_len=%d token_len=%d autostart=%v lang=%q regen=%v",
        len(res.Key), len(res.Token), res.AutoStart, res.Language, res.RegenDone)
    nsCopy := *s
    nsCopy.AgentToken = res.Key
    nsCopy.AuthToken = res.Token
    nsCopy.Language = res.Language
    nsCopy.AutoStart = res.AutoStart
    if res.RegenDone && publicURL != "" {
        full := publicURL
        if nsCopy.AuthToken != "" {
            sep := "?"
            if strings.Contains(full, "?") {
                sep = "&"
            }
            full += sep + "token=" + nsCopy.AuthToken
        }
        return &nsCopy, full, true, true
    }
    return &nsCopy, "", true, true
}

// gtkI18NKeys lists the string table entries the python dialog needs, in
// both languages so the dialog can relabel itself live on language change.
var gtkI18NKeys = []string{
    "dlg.title", "dlg.subtitle", "dlg.groupConn", "dlg.groupGeneral",
    "dlg.key", "dlg.keyPh", "dlg.token", "dlg.tokenHint",
    "dlg.btnRegen", "dlg.regenDone", "dlg.btnCopy", "dlg.copied",
    "dlg.language", "dlg.autostart", "dlg.ok", "dlg.cancel",
    "lang.auto", "lang.en", "lang.zh",
}

func gtkI18N() map[string]map[string]string {
    m := make(map[string]map[string]string, len(gtkI18NKeys))
    for _, k := range gtkI18NKeys {
        if pair, ok := msgMap[k]; ok {
            m[k] = map[string]string{"en": pair.en, "zh": pair.zh}
        }
    }
    return m
}

// ---------------------------------------------------------------------
// zenity fallback (kept for systems without PyGObject)
// ---------------------------------------------------------------------

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
// This is the FALLBACK path (PyGObject missing). It cannot offer one
// combined page — zenity has no multi-field form with prefill — so it
// walks the same settings as sequential dialogs: language first, then
// key, token (+Regenerate where the zenity build renders extra buttons),
// address, auto-start.
func settingsDialogZenity(s *settings.Settings, publicURL string,
) (*settings.Settings, bool, bool) {
    zenity, err := exec.LookPath("zenity")
    if err != nil {
        return s, false, false
    }

    ns := *s

    // ---- 0. Language (first — applies to every dialog below) ----
    langRaw, err := runZenity(zenity, "--list",
        "--title", tr("dlg.title"),
        "--text", tr("dlg.language"),
        "--radiolist",
        "--column", "", "--column", "Language",
        "TRUE", "Auto", "FALSE", "中文", "FALSE", "English")
    if err == nil {
        switch strings.TrimSpace(langRaw) {
        case "中文":
            ns.Language = "zh"
        case "English":
            ns.Language = "en"
        case "Auto":
            ns.Language = ""
        }
        // Switch the UI language right away: the key/token/address
        // dialogs below are rendered with the new strings.
        SetLanguage(ResolveLang(ns.Language))
    }
    // Dismissed picker: keep the current language, keep the flow going.

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

    return &ns, false, true
}

// showCopyFallback is the last-resort clipboard helper for the tray
// "Copy Address" menu item: when no clipboard tool exists, open a dialog
// with the URL in a selectable text field so the user can copy it
// manually (Ctrl+A, Ctrl+C) instead of getting a silent failure.
func showCopyFallback(text string) {
    if zenity, err := exec.LookPath("zenity"); err == nil {
        _, _ = runZenity(zenity, "--entry",
            "--title", tr("dlg.title"),
            "--text", tr("dlg.copyFallback"),
            "--entry-text", text)
    }
}
