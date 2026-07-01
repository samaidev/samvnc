// Package demo provides a synthetic, fully in-memory desktop backend used when
// no real display is available (e.g. a headless server sandbox).
//
// The demo renders an animated scene to an RGBA frame and echoes the client's
// mouse / keyboard / clipboard input back onto that scene, so a browser user
// sees a live, interactive desktop even though nothing is attached. A single
// shared State instance backs the Capturer, Injector and Manager so injected
// input is immediately visible in the next captured frame.
package demo

import (
	"image"
	"image/color"
	"math"
	"sync"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"

	"github.com/samaidev/samvnc/internal/protocol"
)

// Canvas dimensions of the synthetic desktop.
const (
	Width  = 1280
	Height = 800
)

// State holds the shared demo desktop state written by input/clipboard and read
// by the capturer's renderer.
type State struct {
	mu        sync.Mutex
	mouseX    int
	mouseY    int
	mouseMask uint16
	keys      []rune
	clip      string
	clicks    int // increments on left button press, for a visible ripple count
}

// defaultState is the single shared desktop for the process. SamVNC runs one
// session per server in demo mode, so a package-level singleton is sufficient.
var defaultState = &State{mouseX: Width / 2, mouseY: Height / 2}

// State snapshot taken under the mutex.
type snapshot struct {
	mouseX, mouseY int
	mouseMask      uint16
	keys           string
	clip           string
	clicks         int
}

func (s *State) snap() snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return snapshot{
		mouseX:    s.mouseX,
		mouseY:    s.mouseY,
		mouseMask: s.mouseMask,
		keys:      string(s.keys),
		clip:      s.clip,
		clicks:    s.clicks,
	}
}

// --- Capturer --------------------------------------------------------------

// Capturer renders the shared demo state to an RGBA frame.
type Capturer struct{ s *State; buf *image.RGBA }

// NewCapturer returns a capturer backed by the shared demo state.
func NewCapturer() *Capturer { return &Capturer{s: defaultState} }

func (*Capturer) Name() string          { return "demo" }
func (*Capturer) Size() (int, int)      { return Width, Height }
func (*Capturer) Close() error          { return nil }

// renderMu serializes all rendering. opentype font.Face is not safe for
// concurrent use, and the shared package-level faces (faceBody/faceBig/faceHud)
// would corrupt their internal sfnt.Buffer if two capture loops drew at once.
var renderMu sync.Mutex

func (c *Capturer) Capture() (*image.RGBA, error) {
	renderMu.Lock()
	defer renderMu.Unlock()
	if c.buf == nil {
		c.buf = image.NewRGBA(image.Rect(0, 0, Width, Height))
	}
	render(c.buf, c.s.snap())
	return c.buf, nil
}

// --- Injector --------------------------------------------------------------

// Injector records input into the shared demo state so it shows up on screen.
type Injector struct{ s *State }

// NewInjector returns an injector backed by the shared demo state.
func NewInjector() *Injector { return &Injector{s: defaultState} }

func (*Injector) Name() string { return "demo" }
func (*Injector) Close() error { return nil }

func (i *Injector) Key(ev protocol.KeyEvent) error {
	i.s.mu.Lock()
	defer i.s.mu.Unlock()
	// Echo printable key presses; also handle Backspace visually.
	if ev.Down {
		switch ev.Key {
		case "Backspace":
			if len(i.s.keys) > 0 {
				i.s.keys = i.s.keys[:len(i.s.keys)-1]
			}
		case "Enter":
			i.s.keys = append(i.s.keys, '\n')
		default:
			if len(ev.Key) == 1 {
				i.s.keys = append(i.s.keys, []rune(ev.Key)...)
			}
		}
		if len(i.s.keys) > 80 {
			i.s.keys = i.s.keys[len(i.s.keys)-80:]
		}
	}
	return nil
}

func (i *Injector) Mouse(ev protocol.MouseEvent) error {
	i.s.mu.Lock()
	defer i.s.mu.Unlock()
	// Count left-button presses for a visible "click" counter.
	if ev.Mask&protocol.ButtonLeft != 0 && i.s.mouseMask&protocol.ButtonLeft == 0 {
		i.s.clicks++
	}
	i.s.mouseX = ev.X
	i.s.mouseY = ev.Y
	i.s.mouseMask = ev.Mask
	return nil
}

// --- Clipboard -------------------------------------------------------------

// Manager stores shared clipboard text in the demo state.
type Manager struct{ s *State }

// NewManager returns a clipboard manager backed by the shared demo state.
func NewManager() *Manager { return &Manager{s: defaultState} }

func (*Manager) Name() string          { return "demo" }
func (*Manager) Close() error          { return nil }

func (m *Manager) Read() (string, error) {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	return m.s.clip, nil
}

func (m *Manager) Write(text string) error {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	m.s.clip = text
	return nil
}

// --- Rendering -------------------------------------------------------------

var (
	fontOnce sync.Once
	faceBody font.Face // 22px
	faceBig  font.Face // 44px
	faceHud  font.Face // 18px
)

func initFonts() {
	fontOnce.Do(func() {
		f, err := sfnt.Parse(goregular.TTF)
		if err != nil {
			return
		}
		faceBody, _ = opentype.NewFace(f, &opentype.FaceOptions{Size: 22, DPI: 96, Hinting: font.HintingFull})
		faceBig, _ = opentype.NewFace(f, &opentype.FaceOptions{Size: 44, DPI: 96, Hinting: font.HintingFull})
		faceHud, _ = opentype.NewFace(f, &opentype.FaceOptions{Size: 18, DPI: 96, Hinting: font.HintingFull})
	})
}

var (
	white  = color.RGBA{0xE6, 0xED, 0xF3, 255}
	cyan   = color.RGBA{0x58, 0xA6, 0xFF, 255}
	green  = color.RGBA{0x3F, 0xB9, 0x50, 255}
	yellow = color.RGBA{0xD2, 0x99, 0x22, 255}
	red    = color.RGBA{0xF8, 0x51, 0x4B, 255}
	gray   = color.RGBA{0x8B, 0x94, 0x9E, 255}
)

// render paints one animated frame of the demo desktop into dst.
func render(dst *image.RGBA, s snapshot) {
	initFonts()

	// Vertical gradient background (precomputed once per row).
	stride := dst.Stride
	row := make([]byte, Width*4)
	for x := 0; x < Width; x++ {
		t := float64(x) / float64(Width)
		row[x*4+0] = byte(13 + t*18)
		row[x*4+1] = byte(17 + t*26)
		row[x*4+2] = byte(34 + t*48)
		row[x*4+3] = 255
	}
	for y := 0; y < Height; y++ {
		copy(dst.Pix[y*stride:y*stride+Width*4], row)
	}

	now := time.Now()
	secs := float64(now.UnixNano()) / 1e9

	// Bouncing "windows" — three rounded rectangles drifting with sine.
	draws := []struct {
		cx, cy float64
		w, h   int
		col    color.RGBA
		phase  float64
	}{
		{300, 300, 260, 180, color.RGBA{0x21, 0x2b, 0x45, 255}, 0},
		{700, 260, 300, 220, color.RGBA{0x1f, 0x3a, 0x3a, 255}, 1.1},
		{980, 470, 240, 160, color.RGBA{0x3a, 0x21, 0x3f, 255}, 2.3},
	}
	for _, d := range draws {
		dx := math.Sin(secs*0.6+d.phase) * 40
		dy := math.Cos(secs*0.5+d.phase) * 25
		x0 := int(d.cx + dx - float64(d.w)/2)
		y0 := int(d.cy + dy - float64(d.h)/2)
		fillRect(dst, x0, y0, d.w, d.h, d.col)
		// title bar
		fillRect(dst, x0, y0, d.w, 22, color.RGBA{0x30, 0x3b, 0x55, 255})
	}

	// Pulsing accent circle whose color reacts to clicks.
	pulse := uint8(120 + 100*math.Sin(secs*2))
	clickCol := color.RGBA{pulse, 255, pulse, 255}
	if s.mouseMask&protocol.ButtonLeft != 0 {
		clickCol = red
	}
	fillCircle(dst, 640, 420, 60, clickCol)
	fillCircle(dst, 640, 420, 60+int(10*math.Sin(secs*4)), color.RGBA{0, 0, 0, 0})

	// Title + clock.
	drawText(dst, faceBig, 40, 80, white, "SamVNC — Live Demo Desktop")
	drawText(dst, faceHud, 40, 110, gray, "headless sandbox backend (no real display attached)")
	drawText(dst, faceBig, 40, 180, cyan, now.Format("15:04:05.000"))

	// HUD: input echo.
	drawText(dst, faceBody, 40, 240, green, "Move your mouse — the cursor below tracks it live.")
	drawText(dst, faceBody, 40, 272, green, "Click — the pulsing circle turns red & click counter increments.")
	drawText(dst, faceBody, 40, 304, green, "Type — your keystrokes echo in the box at the bottom.")
	drawText(dst, faceBody, 40, 336, green, "Use the Clipboard bar to send text — it appears here too.")

	// Stats panel (right).
	drawText(dst, faceHud, 980, 130, yellow, "Session")
	drawText(dst, faceHud, 980, 155, gray, "backend: demo")
	drawText(dst, faceHud, 980, 178, gray, "canvas: 1280x800")
	drawText(dst, faceHud, 980, 201, gray, "clicks: "+itoa(s.clicks))
	drawText(dst, faceHud, 980, 224, gray, "mouse: "+itoa(s.mouseX)+","+itoa(s.mouseY))
	maskStr := "none"
	if s.mouseMask != 0 {
		maskStr = "0x" + itoaHex(int(s.mouseMask))
	}
	drawText(dst, faceHud, 980, 247, gray, "buttons: "+maskStr)

	// Key echo panel.
	panelY := 640
	fillRect(dst, 40, panelY, Width-80, 70, color.RGBA{0x0d, 0x11, 0x17, 220})
	drawText(dst, faceHud, 56, panelY+24, gray, "Keystrokes:")
	echo := s.keys
	if echo == "" {
		echo = "(type anywhere on the canvas…)"
	}
	drawText(dst, faceBody, 56, panelY+56, white, truncate(echo, 90))

	// Clipboard echo.
	clipY := 720
	fillRect(dst, 40, clipY, Width-80, 40, color.RGBA{0x1a, 0x12, 0x08, 220})
	drawText(dst, faceHud, 56, clipY+26, yellow, "Clipboard: "+truncate(orEmpty(s.clip, "(empty)"), 110))

	// Mouse cursor echo (drawn last so it's on top).
	drawCursor(dst, s.mouseX, s.mouseY, s.mouseMask&protocol.ButtonLeft != 0)
}

func drawText(dst *image.RGBA, face font.Face, x, y int, col color.RGBA, s string) {
	if face == nil {
		return
	}
	// Defense in depth: a bad glyph should never take down the server.
	defer func() { _ = recover() }()
	d := &font.Drawer{
		Dst:  dst,
		Src:  image.NewUniform(col),
		Face: face,
		Dot:  fixed.P(x, y),
	}
	d.DrawString(s)
}

func fillRect(dst *image.RGBA, x, y, w, h int, c color.RGBA) {
	b := dst.Bounds()
	for j := 0; j < h; j++ {
		yy := y + j
		if yy < b.Min.Y || yy >= b.Max.Y {
			continue
		}
		for i := 0; i < w; i++ {
			xx := x + i
			if xx < b.Min.X || xx >= b.Max.X {
				continue
			}
			dst.SetRGBA(xx, yy, c)
		}
	}
}

func fillCircle(dst *image.RGBA, cx, cy, r int, c color.RGBA) {
	if r <= 0 {
		return
	}
	for y := -r; y <= r; y++ {
		for x := -r; x <= r; x++ {
			if x*x+y*y <= r*r {
				dst.SetRGBA(cx+x, cy+y, c)
			}
		}
	}
}

// drawCursor paints a simple arrow cursor at (x,y), red while the left button
// is held.
func drawCursor(dst *image.RGBA, x, y int, down bool) {
	col := white
	if down {
		col = red
	}
	pts := []struct{ dx, dy int }{{0, 0}, {0, 20}, {5, 13}, {8, 20}, {11, 18}, {6, 12}, {12, 12}}
	for _, p := range pts {
		fillRect(dst, x+p.dx, y+p.dy, 3, 3, col)
	}
	// outline for visibility
	out := color.RGBA{0, 0, 0, 255}
	for _, p := range pts {
		fillRect(dst, x+p.dx-1, y+p.dy-1, 1, 1, out)
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func orEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func itoaHex(n int) string {
	const hex = "0123456789abcdef"
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = hex[n&0xf]
		n >>= 4
	}
	return string(b[i:])
}
