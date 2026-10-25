//go:build !windows

package main

import (
        "os/exec"
        "runtime"
        "strings"

        "github.com/samaidev/samvnc/internal/settings"
)

// openSettingsDialog opens a native settings UI on non-Windows platforms:
//   - macOS: AppleScript dialogs via osascript (key + token input, language
//     choice, auto-start toggle, copy-address button that really copies)
//   - Linux: zenity dialogs when available (form for key + token +
//     auto-start, radiolist for language); silently no-op otherwise.
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
func settingsDialogDarwin(s *settings.Settings, publicURL, authToken string,
) (*settings.Settings, bool, bool) {

        // ---- 1. Connection (key + token) ----
        connMsg := tr("dlg.key") + "\n\n" + tr("dlg.hint")
        out, err := runOsa("display dialog " + appleQuote(connMsg) +
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
        // dialog is clearer than losing the token entirely).
        tokenMsg := tr("dlg.token") + "\n(" + tr("dlg.tokenHint") + ")"
        tokenOut, tokenErr := runOsa("display dialog " + appleQuote(tokenMsg) +
                " default answer " + appleQuote(s.AuthToken) +
                " with title " + appleQuote(tr("dlg.title")) +
                " buttons {" + appleQuote(tr("dlg.cancel")) + "," +
                appleQuote(tr("dlg.ok")) + "}" +
                " default button " + appleQuote(tr("dlg.ok")) +
                " cancel button " + appleQuote(tr("dlg.cancel")))
        if tokenErr == nil {
                token, _ := parseOsaDialog(tokenOut)
                ns.AuthToken = token
        } else {
                // Cancelled the token step — cancel the whole settings flow.
                return s, false, false
        }

        // ---- 2. Copy Address (offer + actually copy) ----
        if publicURL != "" {
                fullURL := publicURL
                if ns.AuthToken != "" {
                        fullURL += "?token=" + ns.AuthToken
                }
                copyOut, copyErr := runOsa("display dialog " +
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
                        // Real clipboard write via AppleScript.
                        _, _ = runOsa("set the clipboard to " + appleQuote(fullURL))
                        // Do not treat the copy click as "save" — fall through to
                        // the general dialog so language/autostart still apply.
                }
        }

        // ---- 3. General: auto-start + language ----
        genMsg := tr("dlg.autostartQ")
        genOut, genErr := runOsa("display dialog " + appleQuote(genMsg) +
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
                        langOut, langErr := runOsa("display dialog " + appleQuote(tr("dlg.language")) +
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

// runOsa shells out to osascript and returns stdout.
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

// settingsDialogZenity shows zenity dialogs on Linux; no-op without zenity.
// One form dialog collects key + token + auto-start, then a radiolist
// picks the language — mirroring the Windows settings page sections.
func settingsDialogZenity(s *settings.Settings, publicURL string,
) (*settings.Settings, bool, bool) {
        zenity, err := exec.LookPath("zenity")
        if err != nil {
                return s, false, false
        }

        ns := *s

        // Connection form: key + token + auto-start checkbox.
        args := []string{"--forms", "--title", tr("dlg.title"),
                "--text", tr("dlg.hint"),
                "--add-entry", tr("dlg.key"),
                "--add-entry", tr("dlg.token"),
                "--add-checkbox", strings.TrimSpace(tr("dlg.autostart")),
        }
        // Pre-fill: zenity --forms has no direct prefill for entries; values
        // start empty. Show the current URL as extra text when present.
        if publicURL != "" {
                args = append(args, "--text", tr("dlg.hint")+"\n\n"+tr("dlg.addrLabel")+": "+publicURL)
        }
        cmd := exec.Command(zenity, args...)
        outRaw, err := cmd.Output()
        if err != nil {
                if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
                        return s, false, false // user cancelled
                }
                return s, false, false
        }
        // Output is field-separated by "|" and lines by "\n".
        fields := strings.Split(strings.TrimSpace(string(outRaw)), "\n")
        if len(fields) > 0 {
                parts := strings.Split(fields[0], "|")
                if len(parts) > 0 {
                        ns.AgentToken = strings.TrimSpace(parts[0])
                }
                if len(parts) > 1 {
                        ns.AuthToken = strings.TrimSpace(parts[1])
                }
                if len(parts) > 2 {
                        ns.AutoStart = strings.TrimSpace(parts[2]) == "TRUE" ||
                                strings.TrimSpace(parts[2]) == "true"
                }
        }

        // Language chooser (radiolist).
        cmd = exec.Command(zenity, "--list",
                "--title", tr("dlg.title"),
                "--text", tr("dlg.language"),
                "--radiolist",
                "--column", "", "--column", "Language",
                "TRUE", "Auto", "FALSE", "中文", "FALSE", "English")
        langRaw, err := cmd.Output()
        if err != nil {
                if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
                        return &ns, false, true // keep form results, skip language
                }
                return &ns, false, true
        }
        switch strings.TrimSpace(string(langRaw)) {
        case "中文":
                ns.Language = "zh"
        case "English":
                ns.Language = "en"
        default:
                ns.Language = ""
        }
        return &ns, false, true
}
