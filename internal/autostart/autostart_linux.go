//go:build linux

package autostart

import (
	"fmt"
	"os"
	"path/filepath"
)

const desktopContent = `[Desktop Entry]
Type=Application
Name=SamVNC
Exec=%s
Terminal=false
X-GNOME-Autostart-enabled=true
`

func desktopPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "autostart", "samvnc.desktop"), nil
}

func enable(execPath string) error {
	p, err := desktopPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	content := fmt.Sprintf(desktopContent, execPath)
	return os.WriteFile(p, []byte(content), 0o644)
}

func disable() error {
	p, err := desktopPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func isEnabled() (bool, error) {
	p, err := desktopPath()
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
