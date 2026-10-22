// Package tray provides a cross-platform system tray icon for SamVNC.
//
// On Windows it uses Shell_NotifyIconW via golang.org/x/sys/windows (no CGO).
// On other platforms a stub is used.
package tray

// MenuItem describes one entry in the tray context menu.
type MenuItem struct {
	ID         string // internal ID used in callbacks
	Label      string // display text
	Disabled   bool   // greyed out
	Checked    bool   // shows a checkmark
	Separator  bool   // menu separator bar (other fields ignored)
}

// Tray is the system tray interface.
type Tray interface {
	// Run starts the tray event loop. It blocks until Stop is called
	// (or the user quits via the tray menu, which calls the callback
	// with "exit").
	Run() error

	// SetMenu updates the context menu items.
	SetMenu(items []MenuItem)

	// SetTooltip sets the tray icon tooltip text.
	SetTooltip(text string)

	// SetCallback sets the function called when a menu item is clicked.
	SetCallback(cb func(itemID string))

	// Stop removes the tray icon and exits the event loop.
	Stop()
}

// New creates a platform-native Tray.
func New() Tray { return newTray() }
