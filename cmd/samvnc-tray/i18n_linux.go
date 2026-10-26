//go:build linux

package main

import (
	"os"
	"strings"
)

// detectSystemLanguage resolves the desktop session language from the
// standard locale environment (LC_ALL beats LC_MESSAGES beats LANG).
// Values like "zh_CN.UTF-8" -> Chinese, "en_US.UTF-8" -> English;
// "C"/"POSIX"/unset fall through to the next variable, finally English.
func detectSystemLanguage() string {
	for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" || v == "C" || v == "POSIX" {
			continue
		}
		l := strings.ToLower(strings.SplitN(v, ".", 2)[0])
		if strings.HasPrefix(l, "zh") {
			return "zh"
		}
		if strings.HasPrefix(l, "en") {
			return "en"
		}
	}
	return "en"
}
