//go:build !linux && !windows

package capture

import (
	"errors"
	"image"
)

type stubCapturer struct{}

func newCapturer() (Capturer, error) { return &stubCapturer{}, nil }

func (stubCapturer) Name() string                       { return "stub" }
func (stubCapturer) Size() (int, int)                   { return 1024, 768 }
func (stubCapturer) Capture() (*image.RGBA, error)      { return nil, errors.New("capture: unsupported platform") }
func (stubCapturer) Close() error                       { return nil }
