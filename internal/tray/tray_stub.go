//go:build !windows

package tray

// stubTray is a no-op tray for unsupported platforms.
type stubTray struct{}

func newTray() Tray { return &stubTray{} }

func (*stubTray) Run() error                  { select {} } // block forever
func (*stubTray) SetMenu([]MenuItem)          {}
func (*stubTray) SetTooltip(string)            {}
func (*stubTray) SetCallback(func(string))    {}
func (*stubTray) Stop()                       {}
