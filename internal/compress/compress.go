// Package compress encodes captured RGBA frames into compact image payloads
// according to the requested quality preset (low / medium / high).
//
// It is the bandwidth-control heart of SamVNC: low quality aggressively
// downscales and JPEG-encodes, high quality keeps native resolution with a
// light JPEG or PNG pass.
package compress

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"

	"github.com/samaidev/samvnc/internal/protocol"
)

// Encoder turns an RGBA frame into an encoded payload plus metadata.
type Encoder struct {
	q protocol.Quality
}

// New returns an Encoder at the given quality.
func New(q protocol.Quality) *Encoder {
	return &Encoder{q: q}
}

// SetQuality changes the active quality preset.
func (e *Encoder) SetQuality(q protocol.Quality) { e.q = q }

// Quality returns the current preset.
func (e *Encoder) Quality() protocol.Quality { return e.q }

// Options describes how a frame was encoded.
type Options struct {
	Format protocol.ImageFormat
	Quality protocol.Quality
	Scale   int // downscale factor applied (1 = native)
}

// Encode encodes src. It may downscale and choose format based on quality.
// Returns the payload bytes, options used, and any error.
func (e *Encoder) Encode(src *image.RGBA) ([]byte, Options, error) {
	q := e.q
	scale := 1
	jpegQual := 70
	format := protocol.FormatJPEG

	// Quality presets are tuned for aitun free tunnels where each poll round-
	// trip can take several seconds: smaller frames matter far more than
	// sharpness. Low aggressively downscales to 1/4 resolution + heavy JPEG;
	// Medium keeps half resolution; High keeps native with light JPEG.
	switch q {
	case protocol.QualityLow:
		scale = 4
		jpegQual = 25
	case protocol.QualityMedium:
		scale = 2
		jpegQual = 45
	case protocol.QualityHigh:
		scale = 1
		jpegQual = 70
		// High quality uses PNG for small frames (sharp text), JPEG otherwise.
		if src.Bounds().Dx()*src.Bounds().Dy() < 256*256 {
			format = protocol.FormatPNG
		}
	default:
		scale = 2
		jpegQual = 45
	}

	frame := src
	if scale > 1 {
		frame = downscale(src, scale)
	}

	var buf bytes.Buffer
	switch format {
	case protocol.FormatPNG:
		if err := png.Encode(&buf, frame); err != nil {
			return nil, Options{}, err
		}
	case protocol.FormatJPEG:
		if err := jpeg.Encode(&buf, frame, &jpeg.Options{Quality: jpegQual}); err != nil {
			return nil, Options{}, err
		}
	default:
		if err := jpeg.Encode(&buf, frame, &jpeg.Options{Quality: jpegQual}); err != nil {
			return nil, Options{}, err
		}
	}

	return buf.Bytes(), Options{Format: format, Quality: q, Scale: scale}, nil
}

// downscale averages src by an integer factor using a fast box filter.
func downscale(src *image.RGBA, factor int) *image.RGBA {
	if factor <= 1 {
		return src
	}
	b := src.Bounds()
	w := b.Dx() / factor
	h := b.Dy() / factor
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	area := factor * factor
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var r, g, b2, a uint32
			for dy := 0; dy < factor; dy++ {
				for dx := 0; dx < factor; dx++ {
					sx := x*factor + dx
					sy := y*factor + dy
					if sx >= b.Dx() || sy >= b.Dy() {
						continue
					}
					c := src.RGBAAt(b.Min.X+sx, b.Min.Y+sy)
					r += uint32(c.R)
					g += uint32(c.G)
					b2 += uint32(c.B)
					a += uint32(c.A)
				}
			}
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8(r / uint32(area)),
				G: uint8(g / uint32(area)),
				B: uint8(b2 / uint32(area)),
				A: uint8(a / uint32(area)),
			})
		}
	}
	return dst
}
