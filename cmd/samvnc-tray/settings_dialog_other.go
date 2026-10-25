//go:build !windows

package main

import (
        "os/exec"
        "runtime"
        "strings"

        "github.com/samaidev/samvnc/internal/settings"
)

// openSettingsDialog opens a native settings UI on non-Windows platforms:
//   - macOS: AppleScript dialogs via osascript (key input, language choice,
//     optional copy-address button)
//   - Linux: zenity dialogs when available; silently no-op otherwise.
//
// It returns a modified COPY of the settings; ok=false means the user
// cancelled (or no dialog backend exists) and the caller keeps the
// original values.
func openSettingsDialog(s *settings.Settings, publicURL, authToken string,
) (newSettings *settings.Settings, copyAddr bool, ok bool) {

        switch runtime.GOOS {
        case "darwin":
                return settingsDialogDarwin(s, publicURL)
        case "linux":
                return settingsDialogZenity(s)
        }
        return s, false, false
}

// appleQuote escapes a Go string for embedding in a double-quoted
// AppleScript string literal.
func appleQuote(s string) string {
        r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
        return `"` + r.Replace(s) + `"`
}

// settingsDialogDarwin shows osascript dialogs on macOS.
func settingsDialogDarwin(s *settings.Settings, publicURL string,
) (*settings.Settings, bool, bool) {

        out, err := runOsa("display dialog " + appleQuote(tr("dlg.key")) +
                " default answer " + appleQuote(s.AgentToken) +
                " with title " + appleQuote(tr("dlg.title")) +
                " buttons {" + appleQuote(tr("dlg.cancel")) + "," +
                appleQuote(tr("dlg.btnCopy")) + "," +
                appleQuote(tr("dlg.ok")) + "}" +
                " default button " + appleQuote(tr("dlg.ok")) +
                " cancel button " + appleQuote(tr("dlg.cancel")))
        if err != nil {
                // Exit 128 == user pressed the cancel button.
                return s, false, false
        }
        key, btn := parseOsaDialog(out)

        ns := *s
        ns.AgentToken = key

        // Language chooser: three buttons (Auto / 中文 / English).
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

        copyAddr := strings.Contains(btn, "Copy") || strings.Contains(btn, "复制")
        return &ns, copyAddr, true
}

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
func settingsDialogZenity(s *settings.Settings) (*settings.Settings, bool, bool) {
        zenity, err := exec.LookPath("zenity")
        if err != nil {
                return s, false, false
        }

        // Key input.
        cmd := exec.Command(zenity, "--entry",
                "--title", tr("dlg.title"),
                "--text", tr("dlg.key"),
                "--entry-text", s.AgentToken)
        keyRaw, err := cmd.Output()
        if err != nil {
                if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
                        return s, false, false // user cancelled
                }
                return s, false, false
        }
        ns := *s
        ns.AgentToken = strings.TrimSpace(string(keyRaw))

        // Language chooser (radiolist).
        cmd = exec.Command(zenity, "--list",
                "--title", tr("dlg.title"),
                "--text", tr("dlg.language"),
                "--radiolist",
                "--column", "", "--column", "Language",
                "TRUE", "Auto", "FALSE", "中文", "FALSE", "English")
        langRaw, err := cmd.Output()
        if err == nil {
                switch strings.TrimSpace(string(langRaw)) {
                case "中文":
                        ns.Language = "zh"
                case "English":
                        ns.Language = "en"
                default:
                        ns.Language = ""
                }
        }
        return &ns, false, true
}
