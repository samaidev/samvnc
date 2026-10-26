//go:build darwin

package main

import (
	"os/exec"
	"strings"
)

// detectSystemLanguage reads the macOS UI language so "Auto" behaves like
// on Windows: zh locales -> Chinese, everything else -> English.
//
// `defaults read -g AppleLocale` returns e.g. "zh_CN@rg=uszzzz" or
// "en_US@rg=uszzzz" for the active user session — one clean line, no
// parsing gymnastics. AppleLanguages is the fallback (first entry, e.g.
// "zh-Hans-CN").
func detectSystemLanguage() string {
	if out, err := exec.Command("defaults", "read", "-g", "AppleLocale").Output(); err == nil {
		loc := strings.ToLower(strings.TrimSpace(string(out)))
		if loc != "" {
			if strings.HasPrefix(loc, "zh") {
				return "zh"
			}
			if strings.HasPrefix(loc, "en") {
				return "en"
			}
		}
	}
	if out, err := exec.Command("defaults", "read", "-g", "AppleLanguages").Output(); err == nil {
		first := strings.ToLower(strings.SplitN(string(out), "\n", 2)[0])
		if strings.Contains(first, "zh") {
			return "zh"
		}
	}
	return "en"
}
