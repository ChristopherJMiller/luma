package attachments

import (
	"image/color"
	"testing"
)

// The fuzz targets run their seed corpus in every `go test`. To search
// further, run one locally for as long as you like, for example:
//
//	nix develop -c go test -run '^$' -fuzz '^FuzzPDFPageCount$' -fuzztime 10m ./core/internal/attachments/
//	nix develop -c go test -run '^$' -fuzz '^FuzzImageHeaders$' -fuzztime 10m ./core/internal/attachments/
//
// A crasher lands in testdata/fuzz/<target>/; keep it only as a hand-checked
// regression input.

// FuzzPDFPageCount drives pdfcpu as the child does, without the child's
// recover(), so a panic, a hang or a runaway allocation in pdfcpu shows up
// here (Core is protected from all three by the child; this finds them so
// the limits can be tuned and pdfcpu bugs reported).
func FuzzPDFPageCount(f *testing.F) {
	for _, seed := range [][]byte{
		pdfBytes(f, 1, 0),
		pdfBytes(f, 3, 64),
		pdfBytes(f, 2, 10)[:200],
		[]byte("%PDF-1.7\n"),
		[]byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\ntrailer\n<< /Root 1 0 R >>\n%%EOF\n"),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		n, err := pdfcpuPageCount(t.Context(), data)
		if err == nil && n < 0 {
			t.Fatalf("a negative page count %d", n)
		}
	})
}

// FuzzImageHeaders drives Core's own header parsers (EXIF orientation,
// JPEG segments, scans and frame, PNG and WebP chunks, the decode-cost
// estimate) over arbitrary bytes: none may panic, and an orientation is
// always 1 to 8.
func FuzzImageHeaders(f *testing.F) {
	for _, seed := range [][]byte{
		cameraEXIF(6),
		withAPP1(f, encodeJPEG(f, quadrants()), cameraEXIF(8)),
		withPNGChunks(encodePNG(f, gradient(4, 4)), pngChunk("eXIf", cameraEXIF(3))),
		webpSolid(8, 8, color.NRGBA{1, 2, 3, 255}),
		jpegScanned(3),
		jpegSOF(0xc2, 100, 80, [][2]byte{{2, 2}, {1, 1}, {1, 1}}),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, o := range []int{tiffOrientation(data), imageOrientation("image/jpeg", data),
			imageOrientation("image/png", data), imageOrientation("image/webp", data)} {
			if o < 1 || o > 8 {
				t.Fatalf("orientation %d", o)
			}
		}
		if end := jpegEnd(data); end < 0 || end > len(data) {
			t.Fatalf("jpegEnd = %d of %d", end, len(data))
		}
		_ = jpegScans(data)
		if f, ok := readJPEGFrame(data); ok {
			_ = jpegMayBeRGB(data, f)
		}
		for _, mt := range []string{"image/jpeg", "image/png", "image/gif", "image/webp"} {
			if cfg, err := decodeConfig(mt, data); err == nil {
				_ = headerRefusal(mt, cfg, data)
			}
		}
	})
}
