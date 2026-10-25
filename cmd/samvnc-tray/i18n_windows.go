//go:build windows

package main

import (
	"runtime"

	"golang.org/x/sys/windows"
)

var modKernel32 = windows.NewLazySystemDLL("kernel32.dll")
var procGetUserDefaultUILanguage = modKernel32.NewProc("GetUserDefaultUILanguage")

// detectSystemLanguage maps the Windows UI language to "en" or "zh".
// PRIMARYLANGID(langid) == 0x04 covers zh-CN/zh-TW/zh-HK/zh-SG.
func detectSystemLanguage() string {
	if runtime.GOOS != "windows" {
		return "en"
	}
	res, _, _ := procGetUserDefaultUILanguage.Call()
	if res == 0 {
		return "en"
	}
	langid := uint16(res & 0xFFFF)
	if langid&0x3FF == 0x04 { // PRIMARYLANGID == LANG_CHINESE
		return "zh"
	}
	return "en"
}
