package main

// i18n.go — minimal UI language support for the tray app.
//
// Two languages are supported: English ("en") and Simplified Chinese
// ("zh"). The active language is resolved at startup from settings:
//
//      settings.Language == "en"  -> English
//      settings.Language == "zh"  -> Chinese
//      settings.Language == ""    -> auto-detect from the Windows UI language
//                                    (zh locales -> "zh", everything else "en")
//
// The language is switchable in the settings dialog (Language combo box)
// and persisted in settings.json. Menu items and the settings dialog are
// rebuilt after a change, so it takes effect immediately — no restart
// of the server or the tray is needed.

// uiLang is the ACTIVE UI language ("en" or "zh"). It is a package-level
// var rather than part of TrayApp because the settings dialog code (which
// also renders localized strings) runs before/without a TrayApp reference.
var uiLang = "en"

// ResolveLang maps a settings.Language value to a concrete language code.
func ResolveLang(pref string) string {
        switch pref {
        case "en", "zh":
                return pref
        }
        // Empty or unknown: follow the system UI language.
        return detectSystemLanguage()
}

// SetLanguage switches the active UI language ("en" / "zh"). Unknown
// values fall back to English.
func SetLanguage(lang string) {
        if lang == "zh" {
                uiLang = "zh"
        } else {
                uiLang = "en"
        }
}

// CurrentLang returns the active UI language code.
func CurrentLang() string { return uiLang }

// i18nPair holds the two translations of one UI string.
type i18nPair struct{ en, zh string }

// msgMap is the string table. Keys are stable identifiers; the tray menu
// and the settings dialog look up everything through tr().
var msgMap = map[string]i18nPair{
        // Tray menu items.
        "menu.start":      {"Start", "启动"},
        "menu.stop":       {"Stop", "停止"},
        "menu.autostart":  {"Auto-start on boot", "开机自动启动"},
        "menu.settings":   {"Settings...", "设置..."},
        "menu.copy":       {"Copy Address", "复制地址"},
        "menu.screenshot": {"Screenshot", "截图"},
        "menu.exit":       {"Exit", "退出"},

        // Tray tooltips (appended after "SamVNC — ").
        "tip.stopped":    {"Stopped", "已停止"},
        "tip.starting":   {"Starting…", "启动中…"},
        "tip.running":    {"Running", "运行中"},
        "tip.waitingURL": {"Running (waiting for URL…)", "运行中（等待公网地址…）"},
        "tip.noServer":   {"Error (server not found)", "错误（找不到服务端程序）"},
        "tip.noStart":    {"Error (could not start)", "错误（启动失败）"},
        "tip.noURL":      {"No URL yet (server not ready)", "暂无地址（服务端未就绪）"},
        "tip.copied":     {"Address copied!", "地址已复制！"},
        "tip.shotSaving": {"Taking screenshot…", "正在截图…"},
        "tip.shotSaved":  {"Screenshot saved: %s", "截图已保存：%s"},
        "tip.shotFail":   {"Screenshot failed", "截图失败"},

        // Settings dialog.
        "dlg.title":     {"SamVNC Settings", "SamVNC 设置"},
        "dlg.subtitle": {"Browser-accessible remote desktop",
                "浏览器即可访问的远程桌面"},
        "dlg.groupConn":    {"Connection", "连接"},
        "dlg.groupGeneral": {"General", "通用"},
        "dlg.addrLabel":    {"Current Address", "当前地址"},
        "dlg.key":       {"Aitun.cc Key:", "Aitun.cc 密钥："},
        "dlg.token":     {"Access Token:", "访问令牌："},
        "dlg.tokenHint": {"clear + OK rotates the token", "清空并确定后将生成新令牌"},
        "dlg.language":  {"Language:", "语言："},
        "dlg.autostart": {" Start SamVNC on system boot", " 开机自动启动 SamVNC"},
        "dlg.autostartQ": {"Start SamVNC automatically on system boot?",
                "是否开机自动启动 SamVNC？"},
        "dlg.hint": {"Enter your aitun.cc key to bind a permanent subdomain. " +
                "Leave it EMPTY to use the free aitun.cc proxy-address mode " +
                "(random https://aitun.cc/<code> URL, valid ~24h).\n" +
                "Clearing the Access Token rotates it: a new token is generated " +
                "and old share URLs stop working.",
                "填写 aitun.cc 密钥可绑定固定子域名；留空则使用 aitun.cc 免费代理地址模式" +
                        "（随机 https://aitun.cc/<code> 地址，约 24 小时有效）。\n" +
                        "清空访问令牌并确定后将生成新令牌，旧的分享链接随即失效。"},
        "dlg.addrNone": {"Address: (not available)", "地址：（暂无）"},
        "dlg.btnCopy":  {"Copy Address", "复制地址"},
        "dlg.copied":   {"✓ Copied: ", "✓ 已复制："},
        "dlg.ok":       {"OK", "确定"},
        "dlg.cancel":   {"Cancel", "取消"},

        // Linux zenity dialog extras.
        "dlg.copyHint": {"Current address — select the text and press Ctrl+C to copy, or click OK to copy automatically:",
                "当前地址 —— 可选中文字后按 Ctrl+C 复制，点“确定”自动复制："},
        "dlg.copyFallback": {"Automatic copy failed (no clipboard tool). Select the address below and press Ctrl+C:",
                "自动复制失败（系统缺少剪贴板工具）。请选中下面的地址后按 Ctrl+C 复制："},
        "dlg.now":      {"Auto-start on boot:", "开机自动启动："},
        "dlg.stOn":     {"enabled", "已启用"},
        "dlg.stOff":    {"disabled", "已停用"},
        "dlg.okEnable": {"OK = enable, Cancel = disable", "点“确定”= 启用 · 点“取消”= 停用"},

        // Server-only (headless) fallback messages.
        "msg.serverOnly": {"no GUI/tray environment detected — running in server-only mode (press Ctrl+C to stop).",
                "未检测到图形界面/托盘环境 —— 已进入无界面服务模式（按 Ctrl+C 停止）。"},
        "msg.serverOnlyURL": {"SamVNC public URL:", "SamVNC 公网访问地址："},

        // Language combo box. These intentionally stay bilingual in both UI
        // languages so the user can always find their language.
        "lang.auto": {"Auto (system)", "自动（跟随系统）"},
        "lang.en":   {"English", "English"},
        "lang.zh":   {"中文", "中文"},
}

// tr returns the translation of key in the active language. Unknown keys
// render as the key itself (loudly wrong instead of silently empty).
func tr(key string) string {
        m, ok := msgMap[key]
        if !ok {
                return key
        }
        if uiLang == "zh" {
                return m.zh
        }
        return m.en
}
