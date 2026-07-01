//go:build windows

package capture

import (
	"fmt"
	"image"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// gdiCapturer grabs the desktop via GDI BitBlt into a DIB section.
type gdiCapturer struct {
	w, h int
}

// GDI entry points.
var (
	libGDI   = windows.NewLazySystemDLL("gdi32.dll")
	libUser  = windows.NewLazySystemDLL("user32.dll")

	getDC       = libUser.NewProc("GetDC")
	releaseDC   = libUser.NewProc("ReleaseDC")
	createDC    = libGDI.NewProc("CreateCompatibleDC")
	createBMP   = libGDI.NewProc("CreateCompatibleBitmap")
	selectObj   = libGDI.NewProc("SelectObject")
	deleteObj   = libGDI.NewProc("DeleteObject")
	deleteDC    = libGDI.NewProc("DeleteDC")
	bitBlt      = libGDI.NewProc("BitBlt")
	getDIBits   = libGDI.NewProc("GetDIBits")
	getSysMtrcs = libUser.NewProc("GetSystemMetrics")
)

const (
	cxScreen = 0
	cyScreen = 1
	srccopy  = 0x00cc0020
	dibRGB   = 0
	biRGB    = 0
)

type bitmapInfoHeader struct {
	Size        uint32
	Width       int32
	Height      int32
	Planes      uint16
	BitCount    uint16
	Compression uint32
	SizeImage   uint32
	XPPM        int32
	YPPM        int32
	ClrUsed     uint32
	ClrImp      uint32
}

type bitmapInfo struct {
	Header bitmapInfoHeader
	Colors [4]uint32
}

func newCapturer() (Capturer, error) {
	w := int(callInt(getSysMtrcs, cxScreen))
	h := int(callInt(getSysMtrcs, cyScreen))
	if w == 0 || h == 0 {
		return nil, fmt.Errorf("windows: cannot determine screen size")
	}
	return &gdiCapturer{w: w, h: h}, nil
}

func (c *gdiCapturer) Name() string { return "windows-gdi" }

func (c *gdiCapturer) Size() (int, int) {
	c.w = int(callInt(getSysMtrcs, cxScreen))
	c.h = int(callInt(getSysMtrcs, cyScreen))
	return c.w, c.h
}

func (c *gdiCapturer) Capture() (*image.RGBA, error) {
	w, h := c.Size()
	screenDC := callPtr(getDC, 0)
	if screenDC == 0 {
		return nil, fmt.Errorf("GetDC failed")
	}
	defer releaseDC.Call(0, screenDC)

	memDC := callPtr(createDC, screenDC, 0, 0, 0)
	if memDC == 0 {
		return nil, fmt.Errorf("CreateCompatibleDC failed")
	}
	defer deleteDC.Call(memDC)

	bmp := callPtr(createBMP, screenDC, uintptr(w), uintptr(h))
	if bmp == 0 {
		return nil, fmt.Errorf("CreateCompatibleBitmap failed")
	}
	defer deleteObj.Call(bmp)

	oldObj := callPtr(selectObj, memDC, bmp)
	defer selectObj.Call(memDC, oldObj)

	if r, _, _ := bitBlt.Call(memDC, 0, 0, uintptr(w), uintptr(h), screenDC, 0, 0, srccopy); r == 0 {
		return nil, fmt.Errorf("BitBlt failed")
	}

	bi := bitmapInfo{}
	bi.Header.Size = uint32(unsafe.Sizeof(bitmapInfoHeader{}))
	bi.Header.Width = int32(w)
	bi.Header.Height = int32(-h) // top-down
	bi.Header.Planes = 1
	bi.Header.BitCount = 32
	bi.Header.Compression = biRGB

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	getDIBits.Call(memDC, bmp, 0, uintptr(h),
		uintptr(unsafe.Pointer(&img.Pix[0])),
		uintptr(unsafe.Pointer(&bi)), dibRGB)
	// DIB is BGRA; swap to RGBA.
	for i := 0; i < w*h; i++ {
		o := i * 4
		img.Pix[o+0], img.Pix[o+2] = img.Pix[o+2], img.Pix[o+0] // swap B/R
		img.Pix[o+3] = 255
	}
	return img, nil
}

func (c *gdiCapturer) Close() error { return nil }

func callInt(p *windows.LazyProc, a ...uintptr) uintptr {
	r, _, _ := p.Call(a...)
	return r
}

func callPtr(p *windows.LazyProc, a ...uintptr) uintptr {
	r, _, _ := p.Call(a...)
	return r
}

// Ensure syscall is referenced (used by LazySystemDLL internally on some builds).
var _ = syscall.Handle(0)
