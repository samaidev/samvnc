#!/usr/bin/env python3
# settings_dialog_linux_gtk.py — native GTK3 settings dialog for the Linux
# tray, executed as a python3 subprocess by settings_dialog_linux.go.
#
# WHY: zenity 4.x (GTK4) cannot build this UI at all:
#   - `--forms` cannot PREFILL fields (key/token showed empty, OK wiped them)
#   - `--entry --extra-button` renders no Regenerate button on zenity 4.2.1
#   - language + key + token would stay three separate dialogs
# PyGObject/GTK3 is part of every GNOME desktop, so this script gives Linux
# the SAME single-page settings window as Windows/macOS:
#
#   Connection:  Aitun.cc Key   [___________]
#                Access Token   [________] [Regenerate]  <- auto-copy + popup
#                [Copy Address]  https://...?token=...
#   General:     Language: [Auto | English | 中文]   <- relabels EVERYTHING
#                [x] Start SamVNC on system boot
#                                            [Cancel] [OK]
#
# I/O protocol (with the Go parent):
#   stdin  <- one JSON config object (see gtkConfig in settings_dialog_linux.go)
#   stdout -> one JSON result object
#   exit 0 = clean result (ok:true or ok:false for cancel)
#   exit 4 = --selftest assertion failure (result JSON still printed)
#   exit 10 = GUI stack unavailable (Go falls back to the zenity flow)
#   exit 1  = unexpected crash (Go falls back to the zenity flow)
#
# --selftest runs a scripted UI pass (clicks Regenerate / Copy / language
# combo / OK programmatically) so the REAL dialog can be verified over a
# remote-exec link without a human in front of the screen.

import json
import os
import secrets
import subprocess
import sys

try:
    import gi
    gi.require_version("Gtk", "3.0")
    from gi.repository import Gtk, Gdk, GLib
except Exception as e:  # noqa: BLE001 - any import failure means "no GUI"
    try:
        sys.stderr.write("gi unavailable: %s\n" % e)
    except Exception:
        pass
    sys.stdout.write(json.dumps({"ok": False, "error": "gi-unavailable"}) + "\n")
    sys.stdout.flush()
    sys.exit(10)


def detect_lang():
    """Mirror the Go detectSystemLanguage(): LC_ALL > LC_MESSAGES > LANG,
    zh* -> zh, everything else -> en."""
    for key in ("LC_ALL", "LC_MESSAGES", "LANG"):
        v = os.environ.get(key, "").strip()
        if not v or v in ("C", "POSIX"):
            continue
        l = v.split(".", 1)[0].lower()
        if l.startswith("zh"):
            return "zh"
        if l.startswith("en"):
            return "en"
    return "en"


def copy_clipboard(text):
    """Set the CLIPBOARD selection. GTK keeps ownership while this process
    lives and store() hands the text to any clipboard manager; a
    wl-copy/xclip/xsel subprocess is also attempted so the text survives
    this process exiting on Wayland even when no manager runs."""
    ok = False
    try:
        cb = Gtk.Clipboard.get_default(Gdk.Display.get_default())
        cb.set_text(text, -1)
        cb.store()
        ok = True
    except Exception:
        pass
    for tool, args in (("wl-copy", []),
                       ("xclip", ["-selection", "clipboard"]),
                       ("xsel", ["--clipboard", "--input"])):
        try:
            p = subprocess.Popen([tool] + args, stdin=subprocess.PIPE,
                                 stdout=subprocess.DEVNULL,
                                 stderr=subprocess.DEVNULL)
            p.communicate(text.encode("utf-8"), timeout=5)
            ok = True
            break
        except Exception:
            continue
    return ok


class SettingsDialog:
    def __init__(self, cfg):
        self.cfg = cfg
        self.i18n = cfg.get("i18n", {})
        self.lang = cfg.get("lang") or detect_lang()
        self.url = cfg.get("url", "")
        self.regen_done = False
        self.result = None
        self.selftest = bool(cfg.get("selftest"))
        self._popup = None
        self._exit_code = 0

        lang_pref = cfg.get("langPref", "")
        self._combo_init = {"": 0, "en": 1, "zh": 2}.get(lang_pref, 0)

        self._build()
        self._relocalize()

    # -- text --------------------------------------------------------------
    def tr(self, key):
        pair = self.i18n.get(key)
        if not pair:
            return key
        return pair.get(self.lang) or pair.get("en") or key

    # -- layout --------------------------------------------------------------
    def _build(self):
        self.win = Gtk.Window()
        self.win.set_resizable(True)
        self.win.set_position(Gtk.WindowPosition.CENTER)
        self.win.set_border_width(14)
        self.win.set_default_size(640, -1)
        self.win.connect("delete-event", self.on_cancel)

        outer = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=10)
        self.win.add(outer)

        # Header: title + subtitle.
        self.lbl_title = Gtk.Label()
        self.lbl_title.set_halign(Gtk.Align.START)
        outer.pack_start(self.lbl_title, False, False, 0)
        self.lbl_subtitle = Gtk.Label()
        self.lbl_subtitle.set_halign(Gtk.Align.START)
        outer.pack_start(self.lbl_subtitle, False, False, 0)

        # ---- Connection group ----
        self.frame_conn = Gtk.Frame()
        outer.pack_start(self.frame_conn, False, False, 0)
        conn = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=8)
        conn.set_border_width(10)
        self.frame_conn.add(conn)

        grid = Gtk.Grid(column_spacing=8, row_spacing=8)
        conn.pack_start(grid, False, False, 0)

        self.lbl_key = Gtk.Label()
        self.lbl_key.set_halign(Gtk.Align.START)
        grid.attach(self.lbl_key, 0, 0, 1, 1)
        self.ent_key = Gtk.Entry()
        self.ent_key.set_text(self.cfg.get("key", ""))
        self.ent_key.set_width_chars(34)
        self.ent_key.set_hexpand(True)
        grid.attach(self.ent_key, 1, 0, 1, 1)

        self.lbl_token = Gtk.Label()
        self.lbl_token.set_halign(Gtk.Align.START)
        grid.attach(self.lbl_token, 0, 1, 1, 1)
        tok_box = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=6)
        self.ent_token = Gtk.Entry()
        self.ent_token.set_text(self.cfg.get("token", ""))
        self.ent_token.set_width_chars(34)
        self.ent_token.set_hexpand(True)
        self.ent_token.connect("changed", self.on_token_changed)
        tok_box.pack_start(self.ent_token, True, True, 0)
        self.btn_regen = Gtk.Button()
        self.btn_regen.connect("clicked", self.on_regen)
        tok_box.pack_start(self.btn_regen, False, False, 0)
        grid.attach(tok_box, 1, 1, 1, 1)

        self.lbl_token_hint = Gtk.Label()
        self.lbl_token_hint.set_halign(Gtk.Align.START)
        self.lbl_token_hint.set_line_wrap(True)
        grid.attach(self.lbl_token_hint, 1, 2, 1, 1)

        copy_row = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=6)
        self.btn_copy = Gtk.Button()
        self.btn_copy.connect("clicked", self.on_copy)
        copy_row.pack_start(self.btn_copy, False, False, 0)
        self.lbl_addr = Gtk.Label()
        self.lbl_addr.set_halign(Gtk.Align.START)
        self.lbl_addr.set_ellipsize(3)  # PANGO_ELLIPSIZE_END
        self.lbl_addr.set_selectable(True)
        self.lbl_addr.set_hexpand(True)
        copy_row.pack_start(self.lbl_addr, True, True, 0)
        grid.attach(copy_row, 1, 3, 1, 1)
        self.copy_row = copy_row
        self.copy_row_visible = self.url != ""

        # ---- General group ----
        self.frame_gen = Gtk.Frame()
        outer.pack_start(self.frame_gen, False, False, 0)
        gen = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=8)
        gen.set_border_width(10)
        self.frame_gen.add(gen)

        lang_row = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=8)
        self.lbl_lang = Gtk.Label()
        self.lbl_lang.set_halign(Gtk.Align.START)
        lang_row.pack_start(self.lbl_lang, False, False, 0)
        self.cmb_lang = Gtk.ComboBoxText()
        # Index-mapped like Windows: 0=auto 1=en 2=zh — never matched by text.
        for key in ("lang.auto", "lang.en", "lang.zh"):
            self.cmb_lang.append_text(self.tr(key))
        self.cmb_lang.set_active(self._combo_init)
        self.cmb_lang.connect("changed", self.on_lang_changed)
        lang_row.pack_start(self.cmb_lang, False, False, 0)
        gen.pack_start(lang_row, False, False, 0)

        self.chk_autostart = Gtk.CheckButton()
        self.chk_autostart.set_active(bool(self.cfg.get("autostart")))
        gen.pack_start(self.chk_autostart, False, False, 0)

        # ---- action buttons ----
        btns = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=8)
        btns.set_halign(Gtk.Align.END)
        self.btn_cancel = Gtk.Button()
        self.btn_cancel.connect("clicked", self.on_cancel)
        btns.pack_start(self.btn_cancel, False, False, 0)
        self.btn_ok = Gtk.Button()
        self.btn_ok.connect("clicked", self.on_ok)
        self.btn_ok.set_can_default(True)
        btns.pack_start(self.btn_ok, False, False, 0)
        outer.pack_start(btns, False, False, 0)

        self.win.set_default(self.btn_ok)

    # -- live re-localization ------------------------------------------------
    def _relocalize(self):
        self.win.set_title(self.tr("dlg.title"))
        self.lbl_title.set_markup("<b><big>%s</big></b>" % self.tr("dlg.title"))
        self.lbl_subtitle.set_markup(
            "<span foreground='gray'>%s</span>" % self.tr("dlg.subtitle"))
        self.frame_conn.set_label(self.tr("dlg.groupConn"))
        self.frame_gen.set_label(self.tr("dlg.groupGeneral"))
        self.lbl_key.set_text(self.tr("dlg.key"))
        self.ent_key.set_placeholder_text(self.tr("dlg.keyPh"))
        self.lbl_token.set_text(self.tr("dlg.token"))
        self.lbl_token_hint.set_text(self.tr("dlg.tokenHint"))
        self.btn_regen.set_label(self.tr("dlg.btnRegen"))
        self.btn_copy.set_label(self.tr("dlg.btnCopy"))
        self.lbl_lang.set_text(self.tr("dlg.language"))
        self.chk_autostart.set_label(self.tr("dlg.autostart").strip())
        self.btn_ok.set_label(self.tr("dlg.ok"))
        self.btn_cancel.set_label(self.tr("dlg.cancel"))
        # Language combo entries stay bilingual by design; only the index
        # carries meaning (same as the Windows Language combo).
        if not self.copy_row_visible:
            self.btn_copy.hide()
            self.lbl_addr.hide()
        self.update_addr(prefix=None)

    # -- address row -----------------------------------------------------------
    def current_token(self):
        return self.ent_token.get_text().strip()

    def full_url(self):
        if not self.url:
            return ""
        tok = self.current_token()
        if not tok:
            return self.url
        sep = "&" if "?" in self.url else "?"
        return self.url + sep + "token=" + tok

    def update_addr(self, prefix=None):
        if not self.copy_row_visible:
            return
        full = self.full_url()
        self.lbl_addr.set_text((prefix + full) if prefix is not None else full)

    # -- callbacks ---------------------------------------------------------------
    def on_token_changed(self, _w):
        if self.copy_row_visible:
            # Keep the plain URL (feedback prefixes go stale on edits).
            self.update_addr(prefix=None)

    def on_lang_changed(self, combo):
        idx = combo.get_active()
        if idx == 2:
            self.lang = "zh"
        elif idx == 1:
            self.lang = "en"
        else:
            self.lang = detect_lang()
        self._relocalize()

    def on_regen(self, _btn):
        tok = secrets.token_hex(16)
        self.ent_token.set_text(tok)
        self.regen_done = True
        full = self.full_url()
        # Auto-copy the share URL with the NEW token (same as macOS).
        copy_clipboard(full if self.url else tok)
        if self.copy_row_visible:
            self.update_addr(prefix=self.tr("dlg.regenDone") + " ")
        # Popup note — same content as the macOS alert.
        dlg = Gtk.MessageDialog(
            transient_for=self.win, modal=True,
            message_type=Gtk.MessageType.INFO,
            buttons=Gtk.ButtonsType.OK,
            text=self.tr("dlg.regenDone") + "\n" + tok)
        self._popup = dlg
        dlg.connect("response", self.on_popup_done)
        dlg.show_all()

    def on_popup_done(self, dlg, _resp):
        self._popup = None
        dlg.destroy()

    def on_copy(self, _btn):
        full = self.full_url()
        if not full:
            return
        copy_clipboard(full)
        self.update_addr(prefix=self.tr("dlg.copied"))

    def on_ok(self, _btn=None):
        self.result = {
            "ok": True,
            "key": self.ent_key.get_text().strip(),
            "token": self.current_token(),
            "language": {0: "", 1: "en", 2: "zh"}.get(
                self.cmb_lang.get_active(), ""),
            "autostart": self.chk_autostart.get_active(),
            "regenDone": self.regen_done,
        }
        self.finish(0)

    def on_cancel(self, *a):
        self.result = {"ok": False}
        self.finish(0)

    def finish(self, code):
        res = self.result or {"ok": False}
        try:
            sys.stdout.write(json.dumps(res) + "\n")
            sys.stdout.flush()
        except Exception:
            pass
        self._exit_code = code
        try:
            Gtk.main_quit()
        except Exception:
            pass


# ---------------------------------------------------------------------------
# scripted selftest
# ---------------------------------------------------------------------------

def report(name, ok, detail=""):
    sys.stderr.write("[SELFTEST] %-22s %s %s\n"
                     % (name, "PASS" if ok else "FAIL", detail))
    sys.stderr.flush()


def attach_selftest(d):
    """Programmatic pass over the real dialog: same-page layout, Regenerate
    (new token + address update + auto-copy + popup), live relabel in both
    languages, Copy feedback, and the OK round-trip."""
    st = {"fail": 0}

    def rep(name, ok, detail=""):
        if not ok:
            st["fail"] += 1
        report(name, ok, detail)

    def later(ms, fn):
        GLib.timeout_add(ms, fn)

    # -- 1. layout: everything on ONE page of ONE window ---------------------
    def step_layout():
        vis = d.btn_regen.get_visible() and d.btn_regen.get_mapped()
        rep("layout.regenerate", bool(vis), "visible+mapped=%s" % bool(vis))
        tops = {w.get_toplevel() for w in
                (d.cmb_lang, d.ent_key, d.ent_token, d.btn_regen,
                 d.chk_autostart)}
        rep("layout.same-page", len(tops) == 1,
            "lang+key+token+regen+autostart share one window")
        return False

    # -- 2. Regenerate ---------------------------------------------------------
    def step_regen_click():
        d._tok0 = d.current_token()
        d.btn_regen.clicked()
        later(300, step_regen_check)
        return False

    def step_regen_check():
        t1 = d.current_token()
        ok = (t1 != d._tok0 and len(t1) == 32
              and all(c in "0123456789abcdef" for c in t1))
        rep("regen.new-token", ok, "len=%d" % len(t1))
        d._tok1 = t1
        rep("regen.addr-updated", t1 in d.lbl_addr.get_text(),
            d.lbl_addr.get_text()[:70])
        rep("regen.popup", d._popup is not None)
        later(200, step_popup_dismiss)
        return False

    def step_popup_dismiss():
        if d._popup is not None:
            d._popup.response(Gtk.ResponseType.OK)
        later(200, step_clip_check)
        return False

    def step_clip_check():
        want = (d.url + "?token=" + d._tok1) if d.url else d._tok1

        def poll(tries):
            got = Gtk.Clipboard.get_default(
                Gdk.Display.get_default()).wait_for_text() or ""
            if got == want:
                rep("regen.auto-copy", True, got[:60])
                later(100, step_lang_zh)
            elif tries >= 25:
                rep("regen.auto-copy", False,
                    "got=%r want=%r" % (got[:60], want[:60]))
                later(100, step_lang_zh)
            else:
                later(100, lambda: poll(tries + 1))
        poll(0)
        return False

    # -- 3. live relabel ---------------------------------------------------------
    def step_lang_zh():
        d.cmb_lang.set_active(2)
        rep("lang.zh-relabel",
            d.lbl_key.get_text() == d.i18n["dlg.key"]["zh"],
            d.lbl_key.get_text())
        later(200, step_lang_en)
        return False

    def step_lang_en():
        d.cmb_lang.set_active(1)
        rep("lang.en-relabel",
            d.lbl_key.get_text() == d.i18n["dlg.key"]["en"],
            d.lbl_key.get_text())
        later(200, step_copy_click)
        return False

    # -- 4. Copy Address --------------------------------------------------------
    def step_copy_click():
        d.btn_copy.clicked()
        later(300, step_copy_check)
        return False

    def step_copy_check():
        want = d.full_url()
        got = Gtk.Clipboard.get_default(
            Gdk.Display.get_default()).wait_for_text() or ""
        rep("copy.clipboard", got == want, got[:60])
        rep("copy.feedback",
            d.lbl_addr.get_text().startswith(d.tr("dlg.copied")),
            d.lbl_addr.get_text()[:50])
        later(200, step_finish)
        return False

    # -- 5. OK round-trip ---------------------------------------------------------
    def step_finish():
        d.chk_autostart.set_active(True)
        d.btn_ok.clicked()
        return False

    later(500, step_layout)
    later(700, step_regen_click)

    # Validate the OK round-trip inside finish() before quitting.
    orig_finish = d.finish

    def checked_finish(code):
        res = d.result or {}
        want_tok = getattr(d, "_tok1", "")
        okd = res.get("ok") is True
        rep("ok.roundtrip", okd, json.dumps(res, ensure_ascii=False)[:90])
        tok_ok = bool(want_tok) and res.get("token") == want_tok
        rep("ok.token-preserved", tok_ok, str(res.get("token", ""))[:12])
        lang_ok = res.get("language") == "en"
        rep("ok.language", lang_ok, str(res.get("language")))
        auto_ok = res.get("autostart") is True
        rep("ok.autostart", auto_ok)
        reg_ok = res.get("regenDone") is True
        rep("ok.regenDone", reg_ok)
        ok_all = (okd and tok_ok and lang_ok and auto_ok and reg_ok
                  and st["fail"] == 0)
        sys.stderr.write("SELFTEST-%s\n" % ("PASS" if ok_all else "FAIL"))
        sys.stderr.flush()
        orig_finish(0 if ok_all else 4)

    d.finish = checked_finish


def main():
    if len(sys.argv) >= 2:
        raw = open(sys.argv[1], "rb").read()
    else:
        raw = sys.stdin.buffer.read()
    cfg = json.loads(raw.decode("utf-8"))
    d = SettingsDialog(cfg)
    d.win.show_all()
    if not d.copy_row_visible:
        d.copy_row.hide()
    if cfg.get("selftest"):
        attach_selftest(d)
    Gtk.main()
    sys.exit(d._exit_code)


if __name__ == "__main__":
    try:
        main()
    except SystemExit:
        raise
    except Exception as e:  # noqa: BLE001
        try:
            sys.stderr.write("dialog error: %r\n" % (e,))
        except Exception:
            pass
        sys.stdout.write(json.dumps({"ok": False, "error": str(e)}) + "\n")
        sys.stdout.flush()
        sys.exit(1)
