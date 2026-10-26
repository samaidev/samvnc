//go:build linux

package main

import (
	"bytes"
	"log"
	"os/exec"
	"strings"

	"github.com/samaidev/samvnc/internal/settings"
)

// openSettingsDialog opens the zenity settings flow on Linux. Windows has
// its own native dialog (settings_dialog_windows.go) and macOS a native
// Cocoa window (settings_dialog_darwin.go).
//
// It returns a modified COPY of the settings; ok=false means the user
// cancelled and the caller keeps the original values.
func openSettingsDialog(s *settings.Settings, publicURL, authToken string,
) (newSettings *settings.Settings, copyAddr bool, ok bool) {
	return settingsDialogZenity(s, publicURL)
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
// LANGUAGE FIRST: the language picker runs BEFORE every other step and the
// choice applies IMMEDIATELY (like the Language combo in the Windows
// settings page) — all following dialogs render in the selected language
// instead of the previous one. Cancelling the picker keeps the current
// language and continues with the rest of the flow.
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
//  0. Language (applies immediately to the steps below)
//  1. Connection entries: Aitun.cc key, then Access Token (+ Regenerate)
//  2. Copy Address: selectable entry pre-filled with URL (+ auto-copy on OK)
//  3. Auto-start: question dialog — OK = enable, Cancel = disable
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
