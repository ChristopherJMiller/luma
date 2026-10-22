package attachments

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/webp"

	"github.com/ChristopherJMiller/luma/core/internal/protocol"
)

const (
	// maxPixels is the largest decoded image, 40,000,000 pixels.
	maxPixels = protocol.MaxImageMegapixels * 1_000_000
	// maxDecodeBytes bounds what one decode may allocate, estimated from the
	// header: a 40 MP JPEG or 8-bit PNG fits; a 16-bit PNG, an interlaced
	// PNG, a progressive JPEG or a lossless WebP fits up to about 25 MP.
	// Two decodes at a time plus the scaler stay near half of Core's 768Mi.
	maxDecodeBytes = 192 << 20
	// scratchPixels bounds the scratch buffer of one CatmullRom call, which
	// costs 32 bytes a pixel: 8 MiB.
	scratchPixels = 1 << 18
	jpegQuality   = 85
)

// decodeSlots is the process-wide decode semaphore: two decodes at a time.
var decodeSlots = make(chan struct{}, 2)

// acquireDecode takes a decode slot, waiting until one frees or ctx ends.
func acquireDecode(ctx context.Context) (release func(), err error) {
	select {
	case decodeSlots <- struct{}{}:
		return func() { <-decodeSlots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// normalizeImage checks the header against the pixel and decode-cost caps
// before anything is decoded, then decodes (one slot of two), turns the
// picture upright, downscales it to the long-edge limit and re-encodes it:
// PNG when it has transparency, otherwise JPEG at quality 85. Nothing of the
// original file but its pixels survives: no EXIF, GPS, ICC profile or text.
func normalizeImage(ctx context.Context, mt string, data []byte) (Normalized, error) {
	cfg, err := decodeConfig(mt, data)
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return Normalized{}, refused(protocol.CodeUnsupportedAttachment)
	}
	if int64(cfg.Width)*int64(cfg.Height) > maxPixels || decodeCost(mt, cfg, data) > maxDecodeBytes {
		return Normalized{}, refused(protocol.CodeImageTooLarge)
	}
	orientation := 1
	if mt == "image/jpeg" {
		orientation = jpegOrientation(data)
	}

	release, err := acquireDecode(ctx)
	if err != nil {
		return Normalized{}, err
	}
	defer release()
	src, err := decode(mt, data)
	if err != nil || src.Bounds().Empty() {
		return Normalized{}, refused(protocol.CodeUnsupportedAttachment)
	}
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	var m *image.RGBA
	if w, h := fit(sw, sh); w != sw || h != sh {
		m = scale(src, w, h)
	} else {
		m = toRGBA(src)
	}
	return encode(orient(m, orientation))
}

// decodeConfig reads only the header, with the decoder of the sniffed type.
func decodeConfig(mt string, data []byte) (cfg image.Config, err error) {
	defer recoverInto(&err)
	r := bytes.NewReader(data)
	switch mt {
	case "image/jpeg":
		return jpeg.DecodeConfig(r)
	case "image/png":
		return png.DecodeConfig(r)
	case "image/gif":
		return gif.DecodeConfig(r)
	case "image/webp":
		return webp.DecodeConfig(r)
	}
	return image.Config{}, fmt.Errorf("attachments: no decoder for %s", mt)
}

// decode decodes with the decoder of the sniffed type. A GIF yields its
// first frame only.
func decode(mt string, data []byte) (m image.Image, err error) {
	defer recoverInto(&err)
	r := bytes.NewReader(data)
	switch mt {
	case "image/jpeg":
		return jpeg.Decode(r)
	case "image/png":
		return png.Decode(r)
	case "image/gif":
		return gif.Decode(r)
	case "image/webp":
		return webp.Decode(r)
	}
	return nil, fmt.Errorf("attachments: no decoder for %s", mt)
}

// recoverInto turns a decoder panic into an error: a malformed file must
// never take Core down.
func recoverInto(err *error) {
	if r := recover(); r != nil {
		*err = fmt.Errorf("attachments: decoder panic: %v", r)
	}
}

// decodeCost estimates, from the header alone, the bytes the decoder will
// allocate: the bitmap at its native depth, an interlaced PNG's pass
// images, a progressive JPEG's coefficients (256 bytes per 8x8 block per
// component), a CMYK JPEG's conversion.
func decodeCost(mt string, cfg image.Config, data []byte) int64 {
	px := int64(cfg.Width) * int64(cfg.Height)
	switch mt {
	case "image/png":
		n := px * bytesPerPixel(cfg.ColorModel)
		if pngInterlaced(data) {
			n *= 2
		}
		return n
	case "image/jpeg":
		return jpegCost(cfg, data)
	case "image/gif":
		return px // the first frame, one byte a pixel, within the screen
	}
	// WebP: a lossy frame decodes to YCbCr; lossless (and lossless alpha)
	// to a 4-byte ARGB buffer and then an NRGBA image.
	if len(data) >= 16 && string(data[12:16]) == "VP8 " {
		return px * 2
	}
	return px * 8
}

// bytesPerPixel is the size of the bitmap a PNG decodes into.
func bytesPerPixel(m color.Model) int64 {
	if _, ok := m.(color.Palette); ok {
		return 1
	}
	switch m {
	case color.GrayModel:
		return 1
	case color.Gray16Model:
		return 2
	case color.RGBAModel, color.NRGBAModel:
		return 4
	}
	return 8 // 16-bit colour
}

// pngInterlaced reads IHDR's interlace byte (DecodeConfig has checked that
// IHDR comes first).
func pngInterlaced(data []byte) bool {
	return len(data) > 28 && string(data[12:16]) == "IHDR" && data[28] == 1
}

// jpegCost follows image/jpeg's allocations: a plane per component, padded
// to whole blocks; a progressive file keeps every block's coefficients; a
// four-component (CMYK) file is converted into a second, 4-byte image.
func jpegCost(cfg image.Config, data []byte) int64 {
	f, ok := readJPEGFrame(data)
	if !ok {
		return int64(cfg.Width) * int64(cfg.Height) * 16
	}
	hmax, vmax := 1, 1
	if len(f.comps) > 1 {
		for _, c := range f.comps {
			hmax, vmax = max(hmax, c[0]), max(vmax, c[1])
		}
	} else {
		f.comps = [][2]int{{1, 1}} // a grey image is decoded block by block
	}
	mxx := int64((cfg.Width + 8*hmax - 1) / (8 * hmax))
	myy := int64((cfg.Height + 8*vmax - 1) / (8 * vmax))
	var blocks int64
	for _, c := range f.comps {
		blocks += mxx * int64(c[0]) * myy * int64(c[1])
	}
	n := blocks * 64
	if f.progressive {
		n += blocks * 256
	}
	if len(f.comps) == 4 {
		n += int64(cfg.Width) * int64(cfg.Height) * 4
	}
	return n
}

// fit is the size an image is shown to the model at: the long edge at most
// MaxImageLongEdge, the aspect kept, never upscaled.
func fit(w, h int) (int, int) {
	const edge = protocol.MaxImageLongEdge
	switch {
	case w <= edge && h <= edge:
		return w, h
	case w >= h:
		return edge, max(1, (h*edge+w/2)/w)
	default:
		return max(1, (w*edge+h/2)/h), edge
	}
}

// scale resamples src to w×h with CatmullRom. One direct Scale call
// allocates a w×(source height) buffer of 32-byte pixels: 300 MB for a
// 40 MP photo. CatmullRom is separable, so this runs the horizontal pass in
// bands of rows (the vertical step an identity) into an 8-bit intermediate,
// then the vertical pass in strips of columns: the same result, to within
// the intermediate's rounding, with each scratch buffer at most 8 MiB.
func scale(src image.Image, w, h int) *image.RGBA {
	sb := src.Bounds()
	sh := sb.Dy()
	mid := image.NewRGBA(image.Rect(0, 0, w, sh))
	band := max(1, scratchPixels/w)
	for y := 0; y < sh; y += band {
		y1 := min(y+band, sh)
		xdraw.CatmullRom.Scale(mid, image.Rect(0, y, w, y1), src,
			image.Rect(sb.Min.X, sb.Min.Y+y, sb.Max.X, sb.Min.Y+y1), xdraw.Src, nil)
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	strip := max(1, scratchPixels/sh)
	for x := 0; x < w; x += strip {
		x1 := min(x+strip, w)
		xdraw.CatmullRom.Scale(dst, image.Rect(x, 0, x1, h), mid, image.Rect(x, 0, x1, sh), xdraw.Src, nil)
	}
	return dst
}

// toRGBA copies src into an RGBA image at the origin.
func toRGBA(src image.Image) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	xdraw.Draw(dst, dst.Bounds(), src, b.Min, xdraw.Src)
	return dst
}

// orient applies an EXIF orientation (TIFF tag 274), so that the stored
// pixels are the picture as it was taken: 2 mirrors left-right, 3 turns
// 180°, 4 mirrors top-bottom, 5 transposes, 6 turns 90° clockwise, 7
// transverses, 8 turns 90° counter-clockwise. Anything else leaves m as is.
func orient(m *image.RGBA, o int) *image.RGBA {
	if o < 2 || o > 8 {
		return m
	}
	w, h := m.Bounds().Dx(), m.Bounds().Dy()
	ow, oh := w, h
	if o >= 5 {
		ow, oh = h, w
	}
	out := image.NewRGBA(image.Rect(0, 0, ow, oh))
	for y := range oh {
		for x := range ow {
			// (sx, sy) is the stored pixel shown at (x, y).
			var sx, sy int
			switch o {
			case 2:
				sx, sy = w-1-x, y
			case 3:
				sx, sy = w-1-x, h-1-y
			case 4:
				sx, sy = x, h-1-y
			case 5:
				sx, sy = y, x
			case 6:
				sx, sy = y, h-1-x
			case 7:
				sx, sy = w-1-y, h-1-x
			case 8:
				sx, sy = w-1-y, x
			}
			i, j := out.PixOffset(x, y), m.PixOffset(sx, sy)
			copy(out.Pix[i:i+4], m.Pix[j:j+4])
		}
	}
	return out
}

// encode writes PNG when any pixel is transparent, otherwise JPEG at
// quality 85. The encoders write pixels only: no metadata segment or chunk.
func encode(m *image.RGBA) (Normalized, error) {
	var b bytes.Buffer
	mt := "image/jpeg"
	var err error
	if m.Opaque() {
		err = jpeg.Encode(&b, m, &jpeg.Options{Quality: jpegQuality})
	} else {
		mt = "image/png"
		err = png.Encode(&b, m)
	}
	if err != nil {
		return Normalized{}, fmt.Errorf("attachments: encode %s: %w", mt, err)
	}
	return Normalized{MediaType: mt, Data: b.Bytes(), Width: m.Bounds().Dx(), Height: m.Bounds().Dy()}, nil
}
