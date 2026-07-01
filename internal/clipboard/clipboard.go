// Package clipboard abstracts desktop clipboard read/write for sharing.
//
// Implementations exist for Linux (X11 selection via xsel/xclip) and Windows
// (OpenClipboard API). A no-op stub is used on unsupported platforms.
package clipboard

// Selection identifies which desktop selection to read/write. On X11 there are
// two: CLIPBOARD (Ctrl+C/V) and PRIMARY (mouse-select / middle-click paste).
// Other platforms have only CLIPBOARD; PRIMARY reads/writes are mapped to it.
type Selection int

const (
	// SelClipboard is the Ctrl+C/Ctrl+V selection.
	SelClipboard Selection = iota
	// SelPrimary is the mouse-select selection (X11 PRIMARY).
	SelPrimary
)

func (s Selection) String() string {
	if s == SelPrimary {
		return "primary"
	}
	return "clipboard"
}

// Manager provides bi-directional clipboard sharing between the remote desktop
// and the browser client.
type Manager interface {
	// Read returns the current text of the given selection.
	Read(sel Selection) (string, error)

	// Write sets the text of the given selection.
	Write(text string, sel Selection) error

	// Close releases resources.
	Close() error

	// Name returns a short backend identifier.
	Name() string
}

// New returns the platform-native clipboard Manager, or a stub.
func New() (Manager, error) { return newManager() }
