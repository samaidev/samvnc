//go:build linux

package capture

import (
	"fmt"
	"image"

	x "github.com/BurntSushi/xgb"
	"github.com/BurntSushi/xgb/xproto"
)

// x11Capturer grabs the root window via the core X11 GetImage request.
type x11Capturer struct {
	conn *x.Conn
	root xproto.Window
	w, h int
}

func newCapturer() (Capturer, error) {
	conn, err := x.NewConn()
	if err != nil {
		return nil, fmt.Errorf("x11 connect: %w (is DISPLAY set?)", err)
	}
	setup := xproto.Setup(conn)
	root := setup.DefaultScreen(conn).Root
	w := int(setup.DefaultScreen(conn).WidthInPixels)
	h := int(setup.DefaultScreen(conn).HeightInPixels)
	return &x11Capturer{conn: conn, root: root, w: w, h: h}, nil
}

func (c *x11Capturer) Name() string { return "linux-x11" }

func (c *x11Capturer) Size() (int, int) {
	// Refresh size in case the display was resized.
	s := xproto.Setup(c.conn).DefaultScreen(c.conn)
	c.w = int(s.WidthInPixels)
	c.h = int(s.HeightInPixels)
	return c.w, c.h
}

func (c *x11Capturer) Capture() (*image.RGBA, error) {
	w, h := c.Size()
	reply, err := xproto.GetImage(c.conn, xproto.ImageFormatZPixmap, xproto.Drawable(c.root),
		0, 0, uint16(w), uint16(h), 0xffffffff).Reply()
	if err != nil {
		return nil, fmt.Errorf("getimage: %w", err)
	}
	// Assume 32bpp ZPixmap (the common case). Byte order is handled below.
	const bpp = 4
	raw := reply.Data
	if len(raw) < w*h*bpp {
		return nil, fmt.Errorf("getimage: short data %d < %d", len(raw), w*h*bpp)
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	// X11 ZPixmap on little-endian (LSBFirst) 32bpp packs as B,G,R,X.
	for i := 0; i < w*h; i++ {
		off := i * bpp
		img.Pix[i*4+0] = raw[off+2] // R
		img.Pix[i*4+1] = raw[off+1] // G
		img.Pix[i*4+2] = raw[off+0] // B
		img.Pix[i*4+3] = 255        // A
	}
	return img, nil
}

func (c *x11Capturer) Close() error {
	if c.conn != nil {
		c.conn.Close()
	}
	return nil
}
