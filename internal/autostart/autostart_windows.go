//go:build windows

package autostart

import (
	"fmt"

	"golang.org/x/sys/windows/registry"
)

// The registry key for per-user startup programs.
const (
	regPath  = `Software\Microsoft\Windows\CurrentVersion\Run`
	regName  = "SamVNC"
)

func enable(execPath string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, regPath, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return fmt.Errorf("open startup registry key: %w", err)
	}
	defer k.Close()
	if err := k.SetStringValue(regName, execPath); err != nil {
		return fmt.Errorf("set startup value: %w", err)
	}
	return nil
}

func disable() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, regPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("open startup registry key: %w", err)
	}
	defer k.Close()
	if err := k.DeleteValue(regName); err != nil {
		// If the value doesn't exist, that's fine.
		if err == registry.ErrNotExist {
			return nil
		}
		return fmt.Errorf("delete startup value: %w", err)
	}
	return nil
}

func isEnabled() (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, regPath, registry.QUERY_VALUE)
	if err != nil {
		return false, nil // key doesn't exist = not enabled
	}
	defer k.Close()
	_, _, err = k.GetStringValue(regName)
	if err != nil {
		if err == registry.ErrNotExist {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
