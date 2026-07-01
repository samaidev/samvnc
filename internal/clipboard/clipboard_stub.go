//go:build !linux && !windows

package clipboard

import "errors"

type stubClipboard struct{}

func newManager() (Manager, error) { return &stubClipboard{}, nil }

func (*stubClipboard) Name() string          { return "stub" }
func (*stubClipboard) Read() (string, error) { return "", errors.New("clipboard: unsupported platform") }
func (*stubClipboard) Write(string) error    { return errors.New("clipboard: unsupported platform") }
func (*stubClipboard) Close() error          { return nil }
