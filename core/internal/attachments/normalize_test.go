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

	"github.com/rwcarlsen/goexif/exif"
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
	wantRefused(t, pngHeader(40_000_001, 1, 8, 2, 0), "wide.png", protocol.CodeImageTooLarge)
	if _, err := Normalize(bytes.NewReader(pngHeader(8000, 5000, 8, 2, 0)), "ok.png"); codeOf(err) != protocol.CodeUnsupportedAttachment {
		t.Fatalf("an 8000x5000 header with no pixel data = %v; want it past the cap, then unsupported", err)
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
	x, err := exif.Decode(bytes.NewReader(photo))
	if err != nil {
		t.Fatalf("the test photo's own EXIF does not parse: %v", err)
	}
	if _, err := x.Get(exif.GPSLatitude); err != nil {
		t.Fatalf("the test photo has no latitude to strip: %v", err)
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
	if bytes.Contains(out, []byte(gpsMarker)) || bytes.Contains(out, []byte("Exif")) {
		t.Fatal("metadata survived normalisation")
	}
	if _, err := exif.Decode(bytes.NewReader(out)); err == nil {
		t.Fatal("the output still parses as EXIF")
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

// goexif runs inside recover(), and before it runs the EXIF block is
// checked for what goexif cannot survive: none of these may stop the photo,
// hang, or rotate it.
func TestGoexifPanicRecovered(t *testing.T) {
	plain := encodeJPEG(t, quadrants())
	// sized normalises photo within a deadline and checks its size: 40x20
	// unrotated, 20x40 when orientation 6 still applied.
	sized := func(t *testing.T, photo []byte, w, h int) {
		t.Helper()
		type res struct {
			n   Normalized
			err error
		}
		done := make(chan res, 1)
		go func() { n, err := Normalize(bytes.NewReader(photo), "x.jpg"); done <- res{n, err} }()
		select {
		case r := <-done:
			if r.err != nil || r.n.Width != w || r.n.Height != h {
				t.Fatalf("got %dx%d (%v), want %dx%d", r.n.Width, r.n.Height, r.err, w, h)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("normalising hung")
		}
	}

	t.Run("a panic inside goexif", func(t *testing.T) {
		defer func(f func(io.Reader) (*exif.Exif, error)) { decodeExif = f }(decodeExif)
		decodeExif = func(io.Reader) (*exif.Exif, error) { panic("goexif: index out of range") }
		sized(t, withAPP1(t, plain, cameraEXIF(6)), 40, 20)
	})

	le := func(v ...any) []byte {
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
	orientation6 := le(uint16(0x0112), uint16(3), uint32(1), uint32(6))
	gpsAt := func(off uint32) []byte { return le(uint16(0x8825), uint16(4), uint32(1), off) }
	for _, c := range []struct {
		name string
		tiff []byte
		w, h int
	}{
		// IFD0 (at 8) names IFD1 (at 26) as next, and IFD1 names IFD0:
		// goexif only stops on next == previous, so it would loop forever.
		{"an IFD cycle", le("II*\x00", uint32(8), uint16(1), orientation6, uint32(26), uint16(1), orientation6, uint32(8)), 40, 20},
		// 2 bytes x 0x80000001 overflows uint32 to 2: goexif reads one value
		// and sizes the array for two billion (16 GiB).
		{"a count that overflows", le("II*\x00", uint32(8), uint16(1), uint16(0x0112), uint16(3), uint32(0x80000001), uint32(6), uint32(0)), 40, 20},
		// The same inside the GPS IFD, which goexif also reads.
		{"an overflowing count in the GPS IFD", le("II*\x00", uint32(8), uint16(2), orientation6, gpsAt(38), uint32(0),
			uint16(1), uint16(0x0002), uint16(5), uint32(0x20000001), uint32(0), uint32(0)), 40, 20},
		{"a count past the block", le("II*\x00", uint32(8), uint16(1), uint16(0x0112), uint16(4), uint32(4000), uint32(30), uint32(0)), 40, 20},
		{"a truncated IFD", le("II*\x00", uint32(8), uint16(5), orientation6), 40, 20},
		{"an IFD past the end", le("II*\x00", uint32(4000)), 40, 20},
		{"a negative IFD offset", le("II*\x00", uint32(0xfffffff0)), 40, 20},
		{"orientation as text", le("II*\x00", uint32(8), uint16(1), uint16(0x0112), uint16(2), uint32(2), "6\x00\x00\x00", uint32(0)), 40, 20},
		{"orientation 9", le("II*\x00", uint32(8), uint16(1), uint16(0x0112), uint16(3), uint32(1), uint32(9), uint32(0)), 40, 20},
		{"no TIFF header", []byte("not a tiff at all"), 40, 20},
		{"an empty block", nil, 40, 20},
		// goexif fails to read a GPS IFD that is not there and still reports
		// IFD0, so the orientation is kept.
		{"a GPS pointer past the end", le("II*\x00", uint32(8), uint16(2), orientation6, gpsAt(9999), uint32(0)), 20, 40},
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
			if _, err := Normalize(bytes.NewReader(data[:cut]), kind); err == nil && kind != "pdf" {
				// A cut PDF may still parse; a cut image header never decodes.
				t.Logf("%s cut at %d decoded", kind, cut)
			}
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
