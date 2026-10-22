package attachments

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"strconv"
	"testing"
)

// Every test input is generated here; there are no binary fixtures.

var (
	red   = color.RGBA{255, 0, 0, 255}
	green = color.RGBA{0, 255, 0, 255}
	blue  = color.RGBA{0, 0, 255, 255}
	white = color.RGBA{255, 255, 255, 255}
)

// quadrants is a 40x20 image (a landscape photo's stored pixels) whose
// quarters are top-left red, top-right green, bottom-left blue and
// bottom-right white.
func quadrants() *image.RGBA {
	const w, h = 40, 20
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			c := red
			switch {
			case x >= w/2 && y < h/2:
				c = green
			case x < w/2 && y >= h/2:
				c = blue
			case x >= w/2 && y >= h/2:
				c = white
			}
			m.SetRGBA(x, y, c)
		}
	}
	return m
}

// gradient is an opaque w×h image with smooth ramps in every channel.
func gradient(w, h int) *image.RGBA {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			m.SetRGBA(x, y, color.RGBA{uint8(x * 255 / w), uint8(y * 255 / h), uint8((x + y) * 255 / (w + h)), 255})
		}
	}
	return m
}

func encodePNG(t testing.TB, m image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&b, m); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func encodeJPEG(t testing.TB, m image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, m, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// withAPP1 inserts an APP1 segment carrying "Exif\0\0" + tiff right after a
// JPEG's SOI marker, where a camera writes it.
func withAPP1(t testing.TB, jpg, tiff []byte) []byte {
	t.Helper()
	payload := append([]byte("Exif\x00\x00"), tiff...)
	if len(payload)+2 > 0xffff {
		t.Fatal("APP1 too long")
	}
	seg := []byte{0xff, 0xe1}
	seg = binary.BigEndian.AppendUint16(seg, uint16(len(payload)+2))
	seg = append(seg, payload...)
	out := append([]byte{}, jpg[:2]...)
	out = append(out, seg...)
	return append(out, jpg[2:]...)
}

// tiffTag is one IFD entry; data is its value, typeSize×count bytes.
type tiffTag struct {
	id, typ uint16
	count   uint32
	data    []byte
}

func short(id uint16, v uint16) tiffTag {
	return tiffTag{id, 3, 1, binary.LittleEndian.AppendUint16(nil, v)}
}

func ascii(id uint16, s string) tiffTag {
	return tiffTag{id, 2, uint32(len(s) + 1), append([]byte(s), 0)}
}

func rationals(id uint16, vs ...uint32) tiffTag {
	var b []byte
	for _, v := range vs {
		b = binary.LittleEndian.AppendUint32(b, v)
	}
	return tiffTag{id, 5, uint32(len(vs) / 2), b}
}

// exifTIFF lays out a little-endian TIFF block: IFD0 (offset 8) with ifd0,
// and when gps is not empty a GPS IFD that IFD0's GPSInfo tag points at.
func exifTIFF(ifd0, gps []tiffTag) []byte {
	le := binary.LittleEndian
	if len(gps) > 0 {
		ifd0 = append(append([]tiffTag{}, ifd0...), tiffTag{0x8825, 4, 1, nil}) // patched below
	}
	dirSize := func(n int) int { return 2 + 12*n + 4 }
	dataSize := func(tags []tiffTag) int {
		n := 0
		for _, tg := range tags {
			if len(tg.data) > 4 {
				n += len(tg.data)
			}
		}
		return n
	}
	gpsOff := 8 + dirSize(len(ifd0)) + dataSize(ifd0)
	if len(gps) > 0 {
		ifd0[len(ifd0)-1].data = le.AppendUint32(nil, uint32(gpsOff))
	}
	b := []byte("II*\x00")
	b = le.AppendUint32(b, 8)
	writeDir := func(tags []tiffTag, at int) {
		dataAt := at + dirSize(len(tags))
		b = le.AppendUint16(b, uint16(len(tags)))
		var data []byte
		for _, tg := range tags {
			b = le.AppendUint16(b, tg.id)
			b = le.AppendUint16(b, tg.typ)
			b = le.AppendUint32(b, tg.count)
			if len(tg.data) > 4 {
				b = le.AppendUint32(b, uint32(dataAt+len(data)))
				data = append(data, tg.data...)
			} else {
				v := make([]byte, 4)
				copy(v, tg.data)
				b = append(b, v...)
			}
		}
		b = le.AppendUint32(b, 0) // no next IFD
		b = append(b, data...)
	}
	writeDir(ifd0, 8)
	if len(gps) > 0 {
		writeDir(gps, gpsOff)
	}
	return b
}

// gpsMarker is a synthetic place name carried in the GPS IFD and the
// image description, so a test can look for it in the output bytes.
const gpsMarker = "SYNTHETIC-GPS-AREA"

// cameraEXIF is what a phone writes: orientation, a description, and a GPS
// IFD with a latitude and a processing-method string.
func cameraEXIF(orientation uint16) []byte {
	return exifTIFF(
		[]tiffTag{ascii(0x010e, gpsMarker), short(0x0112, orientation)},
		[]tiffTag{ascii(0x0001, "N"), rationals(0x0002, 47, 1, 36, 1, 2201, 100), ascii(0x001d, gpsMarker)},
	)
}

func pngChunk(typ string, data []byte) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	b = append(b, typ...)
	b = append(b, data...)
	return binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(append([]byte(typ), data...)))
}

// pngHeader is a PNG holding only IHDR and IEND: enough for DecodeConfig,
// with no pixel data at all.
func pngHeader(w, h int, depth, colorType, interlace byte) []byte {
	ihdr := binary.BigEndian.AppendUint32(nil, uint32(w))
	ihdr = binary.BigEndian.AppendUint32(ihdr, uint32(h))
	ihdr = append(ihdr, depth, colorType, 0, 0, interlace)
	b := []byte("\x89PNG\r\n\x1a\n")
	b = append(b, pngChunk("IHDR", ihdr)...)
	return append(b, pngChunk("IEND", nil)...)
}

// withPNGChunks inserts chunks right after a PNG's IHDR.
func withPNGChunks(p []byte, chunks ...[]byte) []byte {
	const afterIHDR = 8 + 25
	out := append([]byte{}, p[:afterIHDR]...)
	for _, c := range chunks {
		out = append(out, c...)
	}
	return append(out, p[afterIHDR:]...)
}

// jpegSOF is a JPEG holding SOI and one SOF segment (marker sof, e.g. 0xc2
// for progressive) with comps components sampled h×v each, then an empty
// SOS and EOI: enough for DecodeConfig, with no scan data.
func jpegSOF(sof byte, w, h int, comps [][2]byte) []byte {
	b := []byte{0xff, 0xd8, 0xff, sof}
	b = binary.BigEndian.AppendUint16(b, uint16(8+3*len(comps)))
	b = append(b, 8)
	b = binary.BigEndian.AppendUint16(b, uint16(h))
	b = binary.BigEndian.AppendUint16(b, uint16(w))
	b = append(b, byte(len(comps)))
	for i, c := range comps {
		b = append(b, byte(i+1), c[0]<<4|c[1], 0)
	}
	// An empty SOS: without a JFIF segment, DecodeConfig reads on to the
	// first scan; decoding then fails on it.
	return append(b, 0xff, 0xda, 0x00, 0x02, 0xff, 0xd9)
}

// bitWriter writes VP8L's least-significant-bit-first bit stream.
type bitWriter struct {
	b    []byte
	acc  uint64
	nacc uint
}

func (w *bitWriter) write(v uint32, n uint) {
	w.acc |= uint64(v) << w.nacc
	w.nacc += n
	for w.nacc >= 8 {
		w.b = append(w.b, byte(w.acc))
		w.acc >>= 8
		w.nacc -= 8
	}
}

func (w *bitWriter) bytes() []byte {
	if w.nacc > 0 {
		w.b = append(w.b, byte(w.acc))
	}
	return w.b
}

// webpSolid is a lossless (VP8L) WebP of one colour. Every prefix code has
// one symbol, so the pixels take no bits and any size costs a few bytes.
func webpSolid(w, h int, c color.NRGBA) []byte {
	var bw bitWriter
	bw.write(0x2f, 8)
	bw.write(uint32(w-1), 14)
	bw.write(uint32(h-1), 14)
	alpha := uint32(0)
	if c.A != 255 {
		alpha = 1
	}
	bw.write(alpha, 1)
	bw.write(0, 3) // version
	bw.write(0, 1) // no transform
	bw.write(0, 1) // no colour cache
	bw.write(0, 1) // no meta prefix codes
	for _, sym := range []uint8{c.G, c.R, c.B, c.A} {
		bw.write(1, 1) // simple code
		bw.write(0, 1) // one symbol
		bw.write(1, 1) // an 8-bit symbol
		bw.write(uint32(sym), 8)
	}
	bw.write(1, 1) // distance: simple code,
	bw.write(0, 1) // one symbol,
	bw.write(0, 1) // a 1-bit symbol:
	bw.write(0, 1) // 0
	data := bw.bytes()
	chunk := append([]byte("VP8L"), binary.LittleEndian.AppendUint32(nil, uint32(len(data)))...)
	chunk = append(chunk, data...)
	if len(data)%2 == 1 {
		chunk = append(chunk, 0)
	}
	out := append([]byte("RIFF"), binary.LittleEndian.AppendUint32(nil, uint32(4+len(chunk)))...)
	out = append(out, "WEBP"...)
	return append(out, chunk...)
}

// countingWriter counts what passes through it.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// writePDF streams a valid PDF with the given number of pages, plus a filler
// stream object of filler bytes (the bulk of a large PDF, like an embedded
// scan) written in chunks, so the whole file never sits in memory. It
// returns the bytes written.
func writePDF(w io.Writer, pages int, filler int64) (int64, error) {
	cw := &countingWriter{w: w}
	var offsets []int64
	obj := func(s string) error {
		offsets = append(offsets, cw.n)
		_, err := io.WriteString(cw, s)
		return err
	}
	if _, err := io.WriteString(cw, "%PDF-1.7\n%\xe2\xe3\xcf\xd3\n"); err != nil {
		return cw.n, err
	}
	// 1: catalog, 2: page tree, 3: the filler, then one object per page.
	if err := obj("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n"); err != nil {
		return cw.n, err
	}
	kids := ""
	for i := range pages {
		kids += strconv.Itoa(4+i) + " 0 R "
	}
	if err := obj(fmt.Sprintf("2 0 obj\n<< /Type /Pages /Kids [%s] /Count %d >>\nendobj\n", kids, pages)); err != nil {
		return cw.n, err
	}
	if err := obj(fmt.Sprintf("3 0 obj\n<< /Length %d >>\nstream\n", filler)); err != nil {
		return cw.n, err
	}
	chunk := bytes.Repeat([]byte("0123456789abcdef"), 4096)
	for left := filler; left > 0; {
		n := min(left, int64(len(chunk)))
		if _, err := cw.Write(chunk[:n]); err != nil {
			return cw.n, err
		}
		left -= n
	}
	if _, err := io.WriteString(cw, "\nendstream\nendobj\n"); err != nil {
		return cw.n, err
	}
	for range pages {
		if err := obj(fmt.Sprintf("%d 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>\nendobj\n", len(offsets)+1)); err != nil {
			return cw.n, err
		}
	}
	xref := cw.n
	s := fmt.Sprintf("xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, o := range offsets {
		s += fmt.Sprintf("%010d 00000 n \n", o)
	}
	s += fmt.Sprintf("trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	_, err := io.WriteString(cw, s)
	return cw.n, err
}

// pdfBytes is writePDF into memory.
func pdfBytes(t testing.TB, pages int, filler int64) []byte {
	t.Helper()
	var b bytes.Buffer
	if _, err := writePDF(&b, pages, filler); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// fillerFor returns the filler that makes writePDF's output exactly size
// bytes: the size grows by one per filler byte, plus the digits of the
// filler's /Length and of startxref.
func fillerFor(t testing.TB, pages int, size int64) int64 {
	t.Helper()
	f := size - 1000
	for range 10 {
		n, err := writePDF(io.Discard, pages, f)
		if err != nil {
			t.Fatal(err)
		}
		if n == size {
			return f
		}
		f += size - n
	}
	t.Fatalf("no filler makes a %d-byte PDF", size)
	return 0
}

// rgbaAt returns m's colour at (x, y) as 8-bit RGBA.
func rgbaAt(m image.Image, x, y int) color.RGBA {
	return color.RGBAModel.Convert(m.At(m.Bounds().Min.X+x, m.Bounds().Min.Y+y)).(color.RGBA)
}

// near reports whether two colours agree within tol per channel.
func near(a, b color.RGBA, tol int) bool {
	d := func(x, y uint8) bool { return int(x)-int(y) <= tol && int(y)-int(x) <= tol }
	return d(a.R, b.R) && d(a.G, b.G) && d(a.B, b.B) && d(a.A, b.A)
}

// decodeOut decodes a Normalize result's bytes with the decoder its media
// type names.
func decodeOut(t testing.TB, n Normalized) image.Image {
	t.Helper()
	var (
		m   image.Image
		err error
	)
	switch n.MediaType {
	case "image/jpeg":
		m, err = jpeg.Decode(bytes.NewReader(n.Data))
	case "image/png":
		m, err = png.Decode(bytes.NewReader(n.Data))
	default:
		t.Fatalf("not an image: %s", n.MediaType)
	}
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// withAlpha returns m with its top-left pixel half transparent, so it
// normalises to PNG.
func withAlpha(m *image.RGBA) *image.NRGBA {
	out := image.NewNRGBA(m.Bounds())
	draw.Draw(out, out.Bounds(), m, m.Bounds().Min, draw.Src)
	out.SetNRGBA(0, 0, color.NRGBA{255, 0, 0, 128})
	return out
}
