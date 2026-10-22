package attachments

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"io"
	"math/rand/v2"
	"runtime"
	"strings"
	"testing"
	"time"

	xdraw "golang.org/x/image/draw"

	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/sessions"
)

// codeOf is the C3 code of a refusal, or "" for nil or another error.
func codeOf(err error) protocol.ErrorCode {
	var r *sessions.RefusedError
	if errors.As(err, &r) {
		return r.Info.Code
	}
	return ""
}

func normalizeOK(t *testing.T, data []byte, name string) Normalized {
	t.Helper()
	n, err := Normalize(bytes.NewReader(data), name)
	if err != nil {
		t.Fatalf("Normalize(%s): %v", name, err)
	}
	return n
}

func wantRefused(t *testing.T, data []byte, name string, code protocol.ErrorCode) {
	t.Helper()
	_, err := Normalize(bytes.NewReader(data), name)
	if got := codeOf(err); got != code {
		t.Fatalf("Normalize(%s) = %v, want %s", name, err, code)
	}
	var r *sessions.RefusedError
	errors.As(err, &r)
	if r.Info.Message != protocol.ErrorCopy[code] {
		t.Fatalf("message %q, want the C3 copy %q", r.Info.Message, protocol.ErrorCopy[code])
	}
}

// bytesAllocated is the heap allocated while f runs.
func bytesAllocated(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func TestSniffIgnoresTheName(t *testing.T) {
	alpha := encodePNG(t, withAlpha(gradient(8, 8)))
	if n := normalizeOK(t, alpha, "scan.pdf"); n.MediaType != "image/png" {
		t.Fatalf("a PNG named .pdf = %s, want image/png", n.MediaType)
	}
	if n := normalizeOK(t, pdfBytes(t, 2, 10), "photo.png"); n.MediaType != "application/pdf" || n.Pages != 2 {
		t.Fatalf("a PDF named .png = %s, %d pages", n.MediaType, n.Pages)
	}
	if n := normalizeOK(t, []byte("milk, eggs\n"), "list.jpg"); n.MediaType != "text/plain" || string(n.Data) != "milk, eggs\n" {
		t.Fatalf("text named .jpg = %s %q", n.MediaType, n.Data)
	}
	elf := append([]byte("\x7fELF\x02\x01\x01\x00"), make([]byte, 120)...)
	for name, data := range map[string][]byte{
		"an ELF named .txt": elf,
		"HTML":              []byte("<!DOCTYPE html><html><body>hi</body></html>"),
		"UTF-16 text":       {0xfe, 0xff, 0x00, 'h', 0x00, 'i'},
		"a BMP":             append([]byte("BM"), make([]byte, 60)...),
	} {
		t.Run(name, func(t *testing.T) { wantRefused(t, data, "notes.txt", protocol.CodeUnsupportedAttachment) })
	}
}

func TestTextMustBeUTF8AndAtMost1MiB(t *testing.T) {
	exact := bytes.Repeat([]byte("a"), 1<<20)
	if n := normalizeOK(t, exact, "a.txt"); len(n.Data) != 1<<20 {
		t.Fatalf("1 MiB of text: %d bytes", len(n.Data))
	}
	wantRefused(t, append(exact, 'a'), "a.txt", protocol.CodeUnsupportedAttachment)
	// Invalid UTF-8 past the 512 bytes the sniffer looks at.
	bad := append(bytes.Repeat([]byte("a"), 600), 0xc3, 0x28)
	wantRefused(t, bad, "a.txt", protocol.CodeUnsupportedAttachment)
	// C0 controls other than tab, LF and CR, anywhere in the file.
	for _, c := range []byte{0x00, 0x01, 0x07, 0x08, 0x0b, 0x0c, 0x1b, 0x1f} {
		ctl := append(bytes.Repeat([]byte("a"), 600), c)
		wantRefused(t, ctl, "a.txt", protocol.CodeUnsupportedAttachment)
	}
	if n := normalizeOK(t, []byte("a\tb\r\nc\n\x7f é"), "a.txt"); n.MediaType != "text/plain" {
		t.Fatalf("tab, CR, LF, DEL and UTF-8 text = %s", n.MediaType)
	}
}

func TestFileOverMaxRefusedWithoutReadingOn(t *testing.T) {
	// An endless body: Normalize must stop one byte past the limit.
	endless := io.MultiReader(strings.NewReader("%PDF-1.7\n"), zeros{})
	_, err := Normalize(endless, "big.pdf")
	if codeOf(err) != protocol.CodeAttachmentTooLarge {
		t.Fatalf("an endless upload = %v, want attachment_too_large", err)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestPixelCapBeforeDecode(t *testing.T) {
	huge := pngHeader(20000, 20000, 8, 6, 0) // 400 MP, 1.6 GB as RGBA
	var err error
	n := bytesAllocated(func() { _, err = Normalize(bytes.NewReader(huge), "huge.png") })
	if codeOf(err) != protocol.CodeImageTooLarge {
		t.Fatalf("a 20000x20000 PNG = %v, want image_too_large", err)
	}
	if n > 1<<20 {
		t.Fatalf("refusing the header allocated %d bytes; the bitmap must never be allocated", n)
	}
	// 40,000,000 pixels is the most allowed; one more is refused.
	wantRefused(t, pngHeader(6326, 6324, 8, 2, 0), "big.png", protocol.CodeImageTooLarge) // 40,005,624 px
	if _, err := Normalize(bytes.NewReader(pngHeader(8000, 5000, 8, 2, 0)), "ok.png"); codeOf(err) != protocol.CodeUnsupportedAttachment {
		t.Fatalf("an 8000x5000 header with no pixel data = %v; want it past the cap, then unsupported", err)
	}
	// Each side is at most 20,000 px (S57b): a 40,000,000x1 PNG is a few KB
	// and within 40 MP, but scaling it would build weights for a 25,000:1
	// reduction, gigabytes.
	for _, wh := range [][2]int{{40_000_000, 1}, {1, 40_000_000}, {20_001, 1}, {1, 20_001}} {
		hdr := pngHeader(wh[0], wh[1], 8, 2, 0)
		n := bytesAllocated(func() { _, err = Normalize(bytes.NewReader(hdr), "thin.png") })
		if codeOf(err) != protocol.CodeImageTooLarge || n > 1<<20 {
			t.Fatalf("a %dx%d PNG = %v after %d bytes allocated, want image_too_large under 1 MiB", wh[0], wh[1], err, n)
		}
	}
	// With its pixels, too; and 20,000 px is allowed.
	thin := func(w, h int) []byte { return encodePNG(t, gradient(w, h)) }
	wantRefused(t, thin(20_001, 1), "thin.png", protocol.CodeImageTooLarge)
	wantRefused(t, thin(1, 20_001), "thin.png", protocol.CodeImageTooLarge)
	for _, wh := range [][2]int{{20_000, 1}, {1, 20_000}} {
		var got Normalized
		data := thin(wh[0], wh[1])
		n := bytesAllocated(func() { got, err = Normalize(bytes.NewReader(data), "thin.png") })
		if err != nil || max(got.Width, got.Height) != 1568 || min(got.Width, got.Height) != 1 || n > 16<<20 {
			t.Fatalf("a %dx%d PNG = %dx%d, %v after %d bytes", wh[0], wh[1], got.Width, got.Height, err, n)
		}
	}
	// The same check for each format, from its own header.
	wantRefused(t, jpegSOF(0xc0, 9000, 5000, [][2]byte{{2, 2}, {1, 1}, {1, 1}}), "huge.jpg", protocol.CodeImageTooLarge)
	wantRefused(t, webpSolid(9000, 5000, color.NRGBA{1, 2, 3, 255}), "huge.webp", protocol.CodeImageTooLarge)
	var g bytes.Buffer
	if err := gif.Encode(&g, image.NewPaletted(image.Rect(0, 0, 1, 1), color.Palette{red}), nil); err != nil {
		t.Fatal(err)
	}
	gifHuge := g.Bytes()
	gifHuge[6], gifHuge[7], gifHuge[8], gifHuge[9] = 0x60, 0xea, 0x60, 0xea // logical screen 60000x60000
	wantRefused(t, gifHuge, "huge.gif", protocol.CodeImageTooLarge)
}

// Under the pixel cap a decoder can still need far more than 4 bytes a
// pixel: 16-bit PNG channels, an interlaced PNG's pass images, a
// progressive JPEG's coefficients (256 bytes per 8x8 block per component).
// Those are refused from the header too.
func TestDecodeCostCapBeforeDecode(t *testing.T) {
	for _, c := range []struct {
		name string
		data []byte
	}{
		{"36 MP 16-bit RGBA PNG (288 MB)", pngHeader(6000, 6000, 16, 6, 0)},
		{"30 MP interlaced RGBA PNG (240 MB)", pngHeader(6000, 5000, 8, 6, 1)},
		{"20 MP progressive 4:4:4 JPEG (300 MB)", jpegSOF(0xc2, 5000, 4000, [][2]byte{{1, 1}, {1, 1}, {1, 1}})},
		{"30 MP lossless WebP (240 MB)", webpSolid(6000, 5000, color.NRGBA{1, 2, 3, 255})},
	} {
		t.Run(c.name, func(t *testing.T) {
			var err error
			n := bytesAllocated(func() { _, err = Normalize(bytes.NewReader(c.data), "x") })
			if codeOf(err) != protocol.CodeImageTooLarge {
				t.Fatalf("= %v, want image_too_large", err)
			}
			if n > 1<<20 {
				t.Fatalf("allocated %d bytes before refusing", n)
			}
		})
	}
	// Cheaper layouts of the same sizes pass the cost check (and then fail to
	// decode, having no pixel data).
	for name, data := range map[string][]byte{
		"36 MP 8-bit RGBA PNG (144 MB)":           pngHeader(6000, 6000, 8, 6, 0),
		"20 MP baseline 4:4:4 JPEG (60 MB)":       jpegSOF(0xc0, 5000, 4000, [][2]byte{{1, 1}, {1, 1}, {1, 1}}),
		"20 MP progressive 4:2:0 JPEG (150 MB)":   jpegSOF(0xc2, 5000, 4000, [][2]byte{{2, 2}, {1, 1}, {1, 1}}),
		"40 MP baseline 4:2:0 JPEG (60 MB)":       jpegSOF(0xc0, 8000, 5000, [][2]byte{{2, 2}, {1, 1}, {1, 1}}),
		"20 MP lossless WebP header (160 MB)":     webpSolid(5000, 4000, color.NRGBA{1, 2, 3, 255})[:30],
		"36 MP 16-bit grey PNG (72 MB, 2 B/px)":   pngHeader(6000, 6000, 16, 0, 0),
		"40 MP paletted PNG (40 MB, 1 B/px)":      withPNGChunks(pngHeader(8000, 5000, 8, 3, 0), pngChunk("PLTE", []byte{1, 2, 3})),
		"25 MP interlaced grey PNG (50 MB, 2x1B)": pngHeader(5000, 5000, 8, 0, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Normalize(bytes.NewReader(data), "x"); codeOf(err) != protocol.CodeUnsupportedAttachment {
				t.Fatalf("= %v, want it past the cost check, then unsupported", err)
			}
		})
	}
}

func TestDownscaleLongEdge1568(t *testing.T) {
	for _, c := range []struct{ w, h, ww, wh int }{
		{3000, 2000, 1568, 1045},
		{1200, 2400, 784, 1568},
		{1569, 1000, 1568, 999},
	} {
		n := normalizeOK(t, encodePNG(t, gradient(c.w, c.h)), "big.png")
		m := decodeOut(t, n)
		if n.MediaType != "image/jpeg" || n.Width != c.ww || n.Height != c.wh || m.Bounds().Dx() != c.ww || m.Bounds().Dy() != c.wh {
			t.Fatalf("%dx%d → %s %dx%d (decoded %v), want image/jpeg %dx%d", c.w, c.h, n.MediaType, n.Width, n.Height, m.Bounds(), c.ww, c.wh)
		}
	}
}

func TestNoUpscale(t *testing.T) {
	for _, c := range []struct{ w, h int }{{800, 600}, {1568, 1000}, {10, 1568}, {1, 1}} {
		n := normalizeOK(t, encodePNG(t, gradient(c.w, c.h)), "small.png")
		if m := decodeOut(t, n); n.Width != c.w || n.Height != c.h || m.Bounds().Dx() != c.w || m.Bounds().Dy() != c.h {
			t.Fatalf("%dx%d → %dx%d (decoded %v)", c.w, c.h, n.Width, n.Height, m.Bounds())
		}
	}
}

// The scaler runs CatmullRom in bands to keep its scratch buffer small; on a
// smooth image it must agree with one direct CatmullRom call (within the
// rounding of its 8-bit intermediate), with no seams at band edges.
func TestBandedScaleMatchesDirect(t *testing.T) {
	src := gradient(2000, 1700)
	got := scale(src, 1568, 1333)
	want := image.NewRGBA(image.Rect(0, 0, 1568, 1333))
	xdraw.CatmullRom.Scale(want, want.Bounds(), src, src.Bounds(), xdraw.Src, nil)
	worst := 0
	for i := range want.Pix {
		d := int(got.Pix[i]) - int(want.Pix[i])
		worst = max(worst, d, -d)
	}
	if worst > 2 {
		t.Fatalf("banded and direct CatmullRom differ by up to %d levels", worst)
	}
}

func TestAlphaKeepsPNG(t *testing.T) {
	n := normalizeOK(t, encodePNG(t, withAlpha(gradient(40, 30))), "a.png")
	m := decodeOut(t, n)
	if n.MediaType != "image/png" {
		t.Fatalf("an image with alpha → %s, want image/png", n.MediaType)
	}
	if a := color.NRGBAModel.Convert(m.At(0, 0)).(color.NRGBA).A; a < 120 || a > 136 {
		t.Fatalf("the half-transparent pixel's alpha = %d", a)
	}
}

func TestGIFKeepsItsFirstFrame(t *testing.T) {
	frame := func(c color.RGBA) *image.Paletted {
		p := image.NewPaletted(image.Rect(0, 0, 30, 20), color.Palette{c})
		return p
	}
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, &gif.GIF{Image: []*image.Paletted{frame(red), frame(green)}, Delay: []int{10, 10}}); err != nil {
		t.Fatal(err)
	}
	n := normalizeOK(t, b.Bytes(), "anim.gif")
	m := decodeOut(t, n)
	if n.MediaType != "image/jpeg" || n.Width != 30 || n.Height != 20 || !near(rgbaAt(m, 15, 10), red, 40) {
		t.Fatalf("an animated GIF → %s %dx%d, centre %v; want its red first frame as JPEG", n.MediaType, n.Width, n.Height, rgbaAt(m, 15, 10))
	}
}

func TestWebPDecodes(t *testing.T) {
	n := normalizeOK(t, webpSolid(64, 48, color.NRGBA{200, 100, 50, 128}), "a.webp")
	m := decodeOut(t, n)
	got := color.NRGBAModel.Convert(m.At(10, 10)).(color.NRGBA)
	if n.MediaType != "image/png" || n.Width != 64 || n.Height != 48 || got.A < 120 || got.A > 136 || got.R < 190 || got.G < 90 || got.G > 110 {
		t.Fatalf("a half-transparent WebP → %s %dx%d, pixel %v", n.MediaType, n.Width, n.Height, got)
	}
}

// Orientation 6 is a phone held upright: the sensor's bottom-left is the
// picture's top-left, and the 40x20 stored image is 20x40 upright.
func TestExifOrientation6Rotates(t *testing.T) {
	for _, c := range []struct {
		o    uint16
		tl   color.RGBA
		name string
	}{
		{6, blue, "orientation 6: the stored bottom-left (blue) is the top-left"},
		{8, green, "orientation 8: the stored top-right (green) is the top-left"},
	} {
		t.Run(c.name, func(t *testing.T) {
			photo := withAPP1(t, encodeJPEG(t, quadrants()), cameraEXIF(c.o))
			n := normalizeOK(t, photo, "IMG_0001.jpg")
			m := decodeOut(t, n)
			if n.Width != 20 || n.Height != 40 || m.Bounds().Dx() != 20 || m.Bounds().Dy() != 40 {
				t.Fatalf("%dx%d, want 20x40", n.Width, n.Height)
			}
			if got := rgbaAt(m, 0, 0); !near(got, c.tl, 60) {
				t.Fatalf("top-left pixel %v, want about %v", got, c.tl)
			}
		})
	}
}

// Every EXIF orientation, by the colours that land in the four corners.
// The stored 40x20 image is red, green / blue, white.
func TestExifOrientations(t *testing.T) {
	R, G, B, W := red, green, blue, white
	for _, c := range []struct {
		o              uint16
		w, h           int
		tl, tr, bl, br color.RGBA
	}{
		{1, 40, 20, R, G, B, W},
		{2, 40, 20, G, R, W, B}, // mirrored left-right
		{3, 40, 20, W, B, G, R}, // rotated 180
		{4, 40, 20, B, W, R, G}, // mirrored top-bottom
		{5, 20, 40, R, B, G, W}, // transposed
		{6, 20, 40, B, R, W, G}, // rotated 90 clockwise
		{7, 20, 40, W, G, B, R}, // transversed
		{8, 20, 40, G, W, R, B}, // rotated 90 counter-clockwise
	} {
		photo := withAPP1(t, encodeJPEG(t, quadrants()), cameraEXIF(c.o))
		n := normalizeOK(t, photo, "o.jpg")
		m := decodeOut(t, n)
		if n.Width != c.w || n.Height != c.h {
			t.Fatalf("orientation %d: %dx%d, want %dx%d", c.o, n.Width, n.Height, c.w, c.h)
		}
		corners := []struct {
			x, y int
			want color.RGBA
		}{{0, 0, c.tl}, {c.w - 1, 0, c.tr}, {0, c.h - 1, c.bl}, {c.w - 1, c.h - 1, c.br}}
		for _, k := range corners {
			if got := rgbaAt(m, k.x, k.y); !near(got, k.want, 60) {
				t.Fatalf("orientation %d: pixel (%d,%d) = %v, want about %v", c.o, k.x, k.y, got, k.want)
			}
		}
	}
}

// All metadata is dropped: EXIF (with GPS) from a JPEG, text and eXIf
// chunks from a PNG.
func TestStripsExif(t *testing.T) {
	photo := withAPP1(t, encodeJPEG(t, quadrants()), cameraEXIF(1))
	if jpegEXIF(photo) == nil || !bytes.Contains(photo, []byte(gpsMarker)) {
		t.Fatal("the test photo carries no EXIF GPS block to strip")
	}
	n := normalizeOK(t, photo, "IMG_0002.jpg")
	assertNoMetadata(t, n.Data)

	p := encodePNG(t, gradient(20, 10))
	tagged := withPNGChunks(p,
		pngChunk("tEXt", []byte("Comment\x00"+gpsMarker)),
		pngChunk("eXIf", cameraEXIF(1)),
	)
	n = normalizeOK(t, tagged, "shot.png")
	assertNoMetadata(t, n.Data)
}

func assertNoMetadata(t *testing.T, out []byte) {
	t.Helper()
	if bytes.Contains(out, []byte(gpsMarker)) || bytes.Contains(out, []byte("Exif")) || bytes.Contains(out, []byte("II*\x00")) {
		t.Fatal("metadata survived normalisation")
	}
	if jpegEXIF(out) != nil || pngEXIF(out) != nil {
		t.Fatal("the output still carries an EXIF block")
	}
	// The JPEG has no APPn segment at all (no JFIF, EXIF, XMP or ICC).
	for _, seg := range jpegMarkers(out) {
		if seg >= 0xe0 && seg <= 0xef {
			t.Fatalf("the output has an APP%d segment", seg-0xe0)
		}
	}
}

// jpegMarkers lists a JPEG's segment markers up to the first scan.
func jpegMarkers(b []byte) []byte {
	var ms []byte
	jpegSegments(b, func(m byte, _ []byte) bool { ms = append(ms, m); return true })
	return ms
}

// le encodes its arguments little-endian, in order.
func le(t *testing.T, v ...any) []byte {
	t.Helper()
	var b bytes.Buffer
	for _, x := range v {
		switch x := x.(type) {
		case []byte:
			b.Write(x)
		case string:
			b.WriteString(x)
		case uint16:
			b.Write([]byte{byte(x), byte(x >> 8)})
		case uint32:
			b.Write([]byte{byte(x), byte(x >> 8), byte(x >> 16), byte(x >> 24)})
		default:
			t.Fatalf("le: %T", x)
		}
	}
	return b.Bytes()
}

// aliasingIFD0 is a TIFF block whose IFD0 holds orientation 6 and then n
// RATIONAL tags of 4,000 values each, every one pointing at the same 32 KB
// value area. It passed the structural check that guarded goexif (each
// value lies inside the block), and goexif built a 4,000-element array per
// tag: 828 MiB at 1,200 tags (S57a).
func aliasingIFD0(t *testing.T, n int) []byte {
	t.Helper()
	const values = 4000
	area := uint32(8 + 2 + 12*(n+1) + 4)
	b := le(t, "II*\x00", uint32(8), uint16(n+1), uint16(0x0112), uint16(3), uint32(1), uint32(6))
	for i := range n {
		b = append(b, le(t, uint16(0x1000+i), uint16(5), uint32(values), area)...)
	}
	b = append(b, le(t, uint32(0))...)
	return append(b, make([]byte, 8*values)...)
}

// Orientation is read in-house from IFD0 alone (S57a): no IFD chain,
// sub-IFD or value array. A malformed block means orientation 1; none of
// these may stop the photo, hang, or cost memory.
func TestMalformedEXIFIgnored(t *testing.T) {
	plain := encodeJPEG(t, quadrants())
	// sized normalises photo within a deadline, under a heap budget, and
	// checks its size: 40x20 unrotated, 20x40 with orientation 6.
	sized := func(t *testing.T, photo []byte, w, h int) {
		t.Helper()
		type res struct {
			n   Normalized
			err error
		}
		done := make(chan res, 1)
		var heap uint64
		go func() {
			var r res
			heap = bytesAllocated(func() { r.n, r.err = Normalize(bytes.NewReader(photo), "x.jpg") })
			done <- r
		}()
		select {
		case r := <-done:
			if r.err != nil || r.n.Width != w || r.n.Height != h {
				t.Fatalf("got %dx%d (%v), want %dx%d", r.n.Width, r.n.Height, r.err, w, h)
			}
			if heap > 8<<20 {
				t.Fatalf("normalising a 40x20 photo allocated %d bytes, want < 8 MiB", heap)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("normalising hung")
		}
	}

	orientation6 := le(t, uint16(0x0112), uint16(3), uint32(1), uint32(6))
	gpsAt := func(off uint32) []byte { return le(t, uint16(0x8825), uint16(4), uint32(1), off) }
	for _, c := range []struct {
		name string
		tiff []byte
		w, h int
	}{
		{"1,200 tags aliasing one value area (more than 512 entries)", aliasingIFD0(t, 1200), 40, 20},
		{"500 tags aliasing one value area", aliasingIFD0(t, 500), 20, 40},
		// IFD0 (at 8) names IFD1 (at 26) as next, and IFD1 names IFD0: the
		// chain is never followed.
		{"an IFD cycle", le(t, "II*\x00", uint32(8), uint16(1), orientation6, uint32(26), uint16(1), orientation6, uint32(8)), 20, 40},
		{"an overflowing count on the orientation", le(t, "II*\x00", uint32(8), uint16(1), uint16(0x0112), uint16(3), uint32(0x80000001), uint32(6), uint32(0)), 40, 20},
		{"an overflowing count in the GPS IFD", le(t, "II*\x00", uint32(8), uint16(2), orientation6, gpsAt(38), uint32(0),
			uint16(1), uint16(0x0002), uint16(5), uint32(0x20000001), uint32(0), uint32(0)), 20, 40},
		{"a GPS pointer past the end", le(t, "II*\x00", uint32(8), uint16(2), orientation6, gpsAt(9999), uint32(0)), 20, 40},
		{"an orientation of 4,000 LONGs", le(t, "II*\x00", uint32(8), uint16(1), uint16(0x0112), uint16(4), uint32(4000), uint32(30), uint32(0)), 40, 20},
		{"orientation as a LONG", le(t, "II*\x00", uint32(8), uint16(1), uint16(0x0112), uint16(4), uint32(1), uint32(6), uint32(0)), 20, 40},
		{"big-endian", []byte("MM\x00*\x00\x00\x00\x08\x00\x01\x01\x12\x00\x03\x00\x00\x00\x01\x00\x06\x00\x00\x00\x00\x00\x00"), 20, 40},
		{"a truncated IFD", le(t, "II*\x00", uint32(8), uint16(5), orientation6), 40, 20},
		{"an IFD past the end", le(t, "II*\x00", uint32(4000)), 40, 20},
		{"an IFD offset near 4 GiB", le(t, "II*\x00", uint32(0xfffffff0)), 40, 20},
		{"orientation as text", le(t, "II*\x00", uint32(8), uint16(1), uint16(0x0112), uint16(2), uint32(2), "6\x00\x00\x00", uint32(0)), 40, 20},
		{"orientation 9", le(t, "II*\x00", uint32(8), uint16(1), uint16(0x0112), uint16(3), uint32(1), uint32(9), uint32(0)), 40, 20},
		{"orientation 0", le(t, "II*\x00", uint32(8), uint16(1), uint16(0x0112), uint16(3), uint32(1), uint32(0), uint32(0)), 40, 20},
		{"no TIFF header", []byte("not a tiff at all"), 40, 20},
		{"an empty block", nil, 40, 20},
	} {
		t.Run(c.name, func(t *testing.T) { sized(t, withAPP1(t, plain, c.tiff), c.w, c.h) })
	}
	// A truncated APP1 segment (its length runs past the file) is skipped.
	t.Run("an APP1 longer than the file", func(t *testing.T) {
		bad := append([]byte{0xff, 0xd8, 0xff, 0xe1, 0xff, 0xff}, "Exif\x00\x00II*\x00"...)
		if _, err := Normalize(bytes.NewReader(bad), "x.jpg"); codeOf(err) != protocol.CodeUnsupportedAttachment {
			t.Fatalf("= %v, want unsupported_attachment", err)
		}
	})
}

// webpWithEXIF is a VP8X WebP holding webpSolid's lossless frame and an
// EXIF chunk.
func webpWithEXIF(t *testing.T, w, h int, c color.NRGBA, tiff []byte) []byte {
	t.Helper()
	frame := webpSolid(w, h, c)[12:] // the VP8L chunk
	vp8x := []byte{0x08, 0, 0, 0}    // flags: EXIF
	vp8x = append(vp8x, byte(w-1), byte((w-1)>>8), byte((w-1)>>16), byte(h-1), byte((h-1)>>8), byte((h-1)>>16))
	chunks := append(le(t, "VP8X", uint32(10)), vp8x...)
	chunks = append(chunks, frame...)
	chunks = append(chunks, le(t, "EXIF", uint32(len(tiff)))...)
	chunks = append(chunks, tiff...)
	if len(tiff)%2 == 1 {
		chunks = append(chunks, 0)
	}
	return append(le(t, "RIFF", uint32(4+len(chunks)), "WEBP"), chunks...)
}

// A PNG's eXIf chunk and a WebP's EXIF chunk are read the same way.
func TestExifOrientationInPNGAndWebP(t *testing.T) {
	p := withPNGChunks(encodePNG(t, quadrants()), pngChunk("eXIf", cameraEXIF(6)))
	n := normalizeOK(t, p, "screenshot.png")
	if m := decodeOut(t, n); n.Width != 20 || n.Height != 40 || !near(rgbaAt(m, 0, 0), blue, 60) {
		t.Fatalf("a PNG with eXIf orientation 6 = %dx%d, top-left %v", n.Width, n.Height, rgbaAt(m, 0, 0))
	}
	for name, tiff := range map[string][]byte{"raw TIFF": cameraEXIF(8), `"Exif\0\0" first`: append([]byte("Exif\x00\x00"), cameraEXIF(8)...)} {
		n := normalizeOK(t, webpWithEXIF(t, 40, 20, color.NRGBA{10, 20, 30, 255}, tiff), "a.webp")
		if n.Width != 20 || n.Height != 40 {
			t.Fatalf("a WebP with an EXIF chunk (%s) of orientation 8 = %dx%d, want 20x40", name, n.Width, n.Height)
		}
	}
}

// jpegScanned is a 16x16 progressive JPEG header followed by n scans of one
// entropy byte each: what a scan bomb looks like to the scan counter.
func jpegScanned(n int) []byte {
	b := jpegSOF(0xc2, 16, 16, [][2]byte{{1, 1}})
	b = b[:len(b)-6] // drop the empty SOS and EOI
	for range n {
		b = append(b, 0xff, 0xda, 0x00, 0x08, 0x01, 0x01, 0x00, 0x00, 0x3f, 0x00, 0x55)
	}
	return append(b, 0xff, 0xd9)
}

// A progressive JPEG's scans each revisit every coefficient; thousands of
// tiny scans would hold a decode slot for an hour (S57c). Past 100 the file
// is refused before decoding.
func TestJPEGScanCap(t *testing.T) {
	var err error
	bomb := jpegScanned(5000)
	start := time.Now()
	n := bytesAllocated(func() { _, err = Normalize(bytes.NewReader(bomb), "bomb.jpg") })
	if codeOf(err) != protocol.CodeImageTooLarge || n > 1<<20 || time.Since(start) > 5*time.Second {
		t.Fatalf("5,000 scans = %v after %d bytes and %v, want image_too_large at once", err, n, time.Since(start))
	}
	wantRefused(t, jpegScanned(101), "x.jpg", protocol.CodeImageTooLarge)
	if _, err := Normalize(bytes.NewReader(jpegScanned(100)), "x.jpg"); codeOf(err) != protocol.CodeUnsupportedAttachment {
		t.Fatalf("100 scans = %v, want past the scan cap (then undecodable)", err)
	}

	// The count is exact: one scan in an encoder's JPEG; 0xFF 0xDA inside an
	// APP1 is not a scan; stuffed bytes and RST markers stay in the scan.
	if got := jpegScans(encodeJPEG(t, quadrants())); got != 1 {
		t.Fatalf("an encoded baseline JPEG has %d scans, want 1", got)
	}
	inAPP1 := withAPP1(t, encodeJPEG(t, quadrants()), exifTIFF([]tiffTag{ascii(0x010e, "\xff\xda\xff\xda")}, nil))
	if got := jpegScans(inAPP1); got != 1 {
		t.Fatalf("0xFFDA inside an APP1 counted: %d scans, want 1", got)
	}
	sos := []byte{0xff, 0xda, 0x00, 0x08, 0x01, 0x01, 0x00, 0x00, 0x3f, 0x00}
	entropy := []byte{0x01, 0xff, 0x00, 0x02, 0xff, 0xd0, 0x03, 0xff, 0xff}
	two := append(append(append(append([]byte{0xff, 0xd8}, sos...), entropy...), sos[1:]...), 0x04, 0xff, 0xd9)
	if got := jpegScans(two); got != 2 {
		t.Fatalf("scans with stuffing, RST and fill bytes = %d, want 2", got)
	}
}

// Decoding runs under a process-wide semaphore of two.
func TestDecodeSemaphoreLimitsConcurrency(t *testing.T) {
	if cap(decodeSlots) != 2 {
		t.Fatalf("decode slots = %d, want 2", cap(decodeSlots))
	}
	ctx := context.Background()
	rel1, err := acquireDecode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rel2, err := acquireDecode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := acquireDecode(short); err == nil {
		t.Fatal("a third decode got a slot")
	}

	// With both slots taken an image waits; text needs no slot.
	if _, err := Normalize(strings.NewReader("plain text"), "a.txt"); err != nil {
		t.Fatal(err)
	}
	small, larger := encodePNG(t, gradient(30, 20)), encodePNG(t, gradient(300, 200))
	done := make(chan error, 1)
	go func() {
		_, err := Normalize(bytes.NewReader(small), "a.png")
		done <- err
	}()
	select {
	case err := <-done:
		rel1()
		rel2()
		t.Fatalf("an image decoded while both slots were held (err %v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	rel1()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the image never got the freed slot")
	}
	rel2()

	// Many at once still pass through two at a time, and all finish.
	errs := make(chan error, 8)
	for range 8 {
		go func() {
			_, err := Normalize(bytes.NewReader(larger), "b.png")
			errs <- err
		}()
	}
	for range 8 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if len(decodeSlots) != 0 {
		t.Fatalf("%d slots still held", len(decodeSlots))
	}
}

// Malformed and truncated files of every kind come back refused, never as
// a panic.
func TestMalformedInputsDoNotPanic(t *testing.T) {
	var g bytes.Buffer
	if err := gif.Encode(&g, image.NewPaletted(image.Rect(0, 0, 20, 10), color.Palette{red, blue}), nil); err != nil {
		t.Fatal(err)
	}
	valid := map[string][]byte{
		"png":  encodePNG(t, gradient(40, 30)),
		"jpeg": withAPP1(t, encodeJPEG(t, gradient(40, 30)), cameraEXIF(6)),
		"gif":  g.Bytes(),
		"webp": webpSolid(40, 30, color.NRGBA{1, 2, 3, 255}),
		"pdf":  pdfBytes(t, 3, 100),
	}
	rng := rand.New(rand.NewPCG(12, 1))
	for kind, data := range valid {
		cuts := []int{1, 4, 8, 12, 16, 24, 33, 64, len(data) / 3, len(data) / 2, len(data) - 1}
		for _, cut := range cuts {
			if cut <= 0 || cut >= len(data) {
				continue
			}
			_, _ = Normalize(bytes.NewReader(data[:cut]), kind) // any answer but a panic
		}
		for range 50 {
			bad := bytes.Clone(data)
			for range 1 + rng.IntN(8) {
				bad[25+rng.IntN(len(bad)-25)] = byte(rng.Uint32())
			}
			_, _ = Normalize(bytes.NewReader(bad), kind)
		}
		garbage := make([]byte, 4096)
		for i := range garbage {
			garbage[i] = byte(rng.Uint32())
		}
		_, _ = Normalize(bytes.NewReader(append(bytes.Clone(data[:min(16, len(data))]), garbage...)), kind)
	}
}
