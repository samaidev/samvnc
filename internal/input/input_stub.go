//go:build !linux && !windows

package input

import (
	"errors"

	"github.com/samaidev/samvnc/internal/protocol"
)

type stubInjector struct{}

func newInjector() (Injector, error) { return &stubInjector{}, nil }

func (stubInjector) Name() string                              { return "stub" }
func (stubInjector) Key(protocol.KeyEvent) error               { return errors.New("input: unsupported platform") }
func (stubInjector) Type(string) error                         { return errors.New("input: unsupported platform") }
func (stubInjector) Mouse(protocol.MouseEvent) error           { return errors.New("input: unsupported platform") }
func (stubInjector) Close() error                              { return nil }
