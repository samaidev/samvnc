// Package trayicon embeds the SamVNC tray icon artwork so every platform
// shows the SAME icon as the Windows tray (blue rounded square + white V).
//
// The files are generated from assets/tray_icon.png (the 1024px master used
// for packaging); tray_icon_256.png is the Lanczos-downscaled embedding copy,
// tray_icon_32.png is the pixel-exact small size, and tray_icon.ico is the
// Windows multi-resolution icon used as a fallback when no tray_icon.ico
// file ships next to the executable.
//
// Consumers:
//   - internal/tray (linux): decodes + scales to SNI IconPixmap sizes
//   - internal/tray (darwin): passes PNG bytes to Cocoa (NSImage initWithData)
//   - internal/tray (windows): writes the embedded .ico to a temp file when
//     no tray_icon.ico exists next to the exe / in ~/.samvnc
package trayicon

import (
        "bytes"
        "embed"
        "image"
        "image/draw"
        "image/png"

        // x/image draw: high-quality scalers (CatmullRom), aliased to xdraw
        // to avoid clashing with the stdlib image/draw package.
        xdraw "golang.org/x/image/draw"
)

//go:embed tray_icon_256.png tray_icon_32.png tray_icon.ico
var files embed.FS

// PNG256 returns the master embedded icon as PNG-encoded bytes (256x256).
// macOS uses this directly (NSImage initWithData handles PNG).
func PNG256() []byte {
        b, _ := files.ReadFile("tray_icon_256.png")
        return b
}

// ICO returns the embedded Windows multi-resolution .ico (used as a
// fallback when no tray_icon.ico file exists next to the executable).
func ICO() []byte {
        b, _ := files.ReadFile("tray_icon.ico")
        return b
}

// decode256 decodes the embedded master icon into an image.Image.
func decode256() (image.Image, error) {
        b := PNG256()
        return png.Decode(bytes.NewReader(b))
}

// decode32 decodes the pixel-exact 32px embedded icon.
func decode32() (image.Image, error) {
        b, _ := files.ReadFile("tray_icon_32.png")
        return png.Decode(bytes.NewReader(b))
}

// Sizes returns the tray pixmap sizes rendered on demand (in pixels).
// Shells pick the closest match from the SNI IconPixmap array; 32 is exact,
// the rest are Catmull-Rom downscales of the 256px master.
var Sizes = []int{16, 24, 32, 48, 64, 128}

// ScaleTo renders the SamVNC icon at the requested square size.
// Size 32 comes from the pixel-exact asset; every other size is a
// high-quality downscale of the 256px master.
func ScaleTo(size int) (image.Image, error) {
        var src image.Image
        var err error
        if size == 32 {
                src, err = decode32()
        } else {
                src, err = decode256()
        }
        if err != nil {
                return nil, err
        }
        dst := image.NewNRGBA(image.Rect(0, 0, size, size))
        // CatmullRom gives the best downscale quality for flat artwork.
        draw.Draw(dst, dst.Bounds(), image.Transparent, image.Point{}, draw.Src)
        xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Over, nil)
        return dst, nil
}

// ARGB converts an NRGBA image to SNI IconPixmap ARGB32 row-major bytes
// (big-endian byte per channel: A, R, G, B).
func ARGB(img image.Image) []byte {
        b := img.Bounds()
        out := make([]byte, 0, b.Dx()*b.Dy()*4)
        for y := b.Min.Y; y < b.Max.Y; y++ {
                for x := b.Min.X; x < b.Max.X; x++ {
                        r, g, bl, a := img.At(x, y).RGBA() // 16-bit premultiplied
                        // Convert premultiplied 16-bit back to straight 8-bit channels.
                        if a > 0 {
                                r = r * 0xFFFF / a
                                g = g * 0xFFFF / a
                                bl = bl * 0xFFFF / a
                        }
                        out = append(out, byte(a>>8), byte(r>>8), byte(g>>8), byte(bl>>8))
                }
        }
        return out
}
