package attachments

import (
	"bytes"
	"encoding/binary"
)

const (
	// maxIFDEntries bounds the entries read from IFD0 (a camera writes a few
	// dozen); a larger IFD is treated as having no orientation.
	maxIFDEntries = 512
	// maxEXIFBytes bounds an EXIF block (a JPEG's APP1 holds at most 64 KiB).
	maxEXIFBytes = 1 << 20
	// tagOrientation is TIFF tag 274.
	tagOrientation = 0x0112
)

// imageOrientation is the EXIF orientation of an image, 1 to 8: from a
// JPEG's APP1, a PNG's eXIf chunk or a WebP's EXIF chunk. It is 1 when
// there is none, or when the EXIF cannot be read.
func imageOrientation(mt string, data []byte) int {
	switch mt {
	case "image/jpeg":
		return tiffOrientation(jpegEXIF(data))
	case "image/png":
		return tiffOrientation(pngEXIF(data))
	case "image/webp":
		return tiffOrientation(webpEXIF(data))
	}
	return 1
}

// tiffOrientation reads the Orientation tag from IFD0 of a TIFF block, and
// nothing else: no next-IFD chain, sub-IFD, thumbnail or maker note, and no
// value array (Orientation is one SHORT, held in the entry itself). It
// allocates nothing and reads at most 512 entries, so no block, however
// built, costs more than a few microseconds. goexif, which this replaces
// (S57a), parsed every tag of every IFD it could reach: a cycle of IFDs
// never ended, and many tags aliasing one value area cost gigabytes.
func tiffOrientation(t []byte) int {
	if len(t) < 8 || len(t) > maxEXIFBytes {
		return 1
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	if bo.Uint16(t[2:]) != 42 {
		return 1
	}
	size := uint64(len(t))
	off := uint64(bo.Uint32(t[4:]))
	if off+2 > size {
		return 1
	}
	n := uint64(bo.Uint16(t[off:]))
	if n > maxIFDEntries || off+2+12*n > size {
		return 1
	}
	for e := off + 2; e < off+2+12*n; e += 12 {
		if bo.Uint16(t[e:]) != tagOrientation {
			continue
		}
		if bo.Uint32(t[e+4:]) != 1 { // the count
			return 1
		}
		var v uint32
		switch bo.Uint16(t[e+2:]) { // the type
		case 3: // SHORT, as the standard says
			v = uint32(bo.Uint16(t[e+8:]))
		case 4: // LONG, from a lenient writer
			v = bo.Uint32(t[e+8:])
		default:
			return 1
		}
		if v < 1 || v > 8 {
			return 1
		}
		return int(v)
	}
	return 1
}

// jpegEXIF is the TIFF block of a JPEG's first APP1 "Exif\0\0" segment, or
// nil.
func jpegEXIF(data []byte) []byte {
	var t []byte
	jpegSegments(data, func(m byte, p []byte) bool {
		if m == 0xe1 && bytes.HasPrefix(p, []byte("Exif\x00\x00")) {
			t = p[6:]
			return false
		}
		return true
	})
	return t
}

// pngEXIF is the payload of a PNG's eXIf chunk (a TIFF block), or nil.
func pngEXIF(data []byte) []byte {
	if !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		return nil
	}
	for i := 8; i+12 <= len(data); {
		n := int(binary.BigEndian.Uint32(data[i:]))
		if n < 0 || n > len(data)-i-12 {
			return nil
		}
		switch string(data[i+4 : i+8]) {
		case "eXIf":
			return data[i+8 : i+8+n]
		case "IEND":
			return nil
		}
		i += 12 + n
	}
	return nil
}

// webpEXIF is the payload of a WebP's EXIF chunk, a TIFF block (some
// writers prefix "Exif\0\0"), or nil.
func webpEXIF(data []byte) []byte {
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return nil
	}
	for i := 12; i+8 <= len(data); {
		n := int(binary.LittleEndian.Uint32(data[i+4:]))
		if n < 0 || n > len(data)-i-8 {
			return nil
		}
		if string(data[i:i+4]) == "EXIF" {
			return bytes.TrimPrefix(data[i+8:i+8+n], []byte("Exif\x00\x00"))
		}
		i += 8 + n + n%2 // chunks are padded to an even size
	}
	return nil
}

// jpegSegments walks a JPEG's marker segments up to its first scan (SOS),
// calling fn with each marker and payload until fn returns false. It finds
// markers the way image/jpeg does (reader.go's marker loop), so it sees the
// segments the decoder acts on: a byte that is not 0xFF, or an FF 00 pair,
// is extraneous data the decoder slides past one byte at a time; 0xFF fill
// bytes are skipped; an RST marker has no payload; every other marker has a
// 16-bit length. It stops at the first scan, at EOI, or at a segment that
// runs past the data (where the decoder fails too).
func jpegSegments(data []byte, fn func(marker byte, payload []byte) bool) {
	if len(data) < 2 || data[0] != 0xff || data[1] != 0xd8 {
		return
	}
	for i := 2; i+1 < len(data); {
		if data[i] != 0xff || data[i+1] == 0x00 { // extraneous data
			i++
			continue
		}
		m := data[i+1]
		switch {
		case m == 0xff: // a fill byte
			i++
			continue
		case m >= 0xd0 && m <= 0xd7: // RST: no payload
			i += 2
			continue
		case m == 0xda || m == 0xd9: // the first scan, or the end
			return
		}
		if i+4 > len(data) {
			return
		}
		n := int(binary.BigEndian.Uint16(data[i+2:]))
		if n < 2 || i+2+n > len(data) {
			return
		}
		if !fn(m, data[i+4:i+2+n]) {
			return
		}
		i += 2 + n
	}
}

// jpegScans counts the FF DA pairs anywhere in a JPEG: an upper bound on
// its scans. image/jpeg starts a scan only at an FF DA marker, and inside
// entropy-coded data an 0xFF is followed only by 0x00 (stuffing) or an RST
// marker, so no scan is missed however the segments around it are built
// (S57c re-review: a walk that stopped at a junk byte saw none). An FF DA
// inside a metadata segment (an EXIF thumbnail, an ICC profile) is counted
// too: the safe direction.
func jpegScans(data []byte) int {
	return bytes.Count(data, []byte{0xff, 0xda})
}

// jpegFrame is what a JPEG's SOF segment says about its decoding.
type jpegFrame struct {
	progressive bool
	comps       [][2]int // each component's horizontal and vertical sampling
}

// readJPEGFrame finds the frame header (SOF0–SOF15 but DHT, JPG and DAC).
func readJPEGFrame(data []byte) (f jpegFrame, ok bool) {
	jpegSegments(data, func(m byte, p []byte) bool {
		if m < 0xc0 || m > 0xcf || m == 0xc4 || m == 0xc8 || m == 0xcc {
			return true
		}
		if len(p) < 6 || p[5] == 0 || len(p) < 6+3*int(p[5]) {
			return false
		}
		for i := range int(p[5]) {
			hv := p[6+3*i+1]
			f.comps = append(f.comps, [2]int{int(hv >> 4), int(hv & 0x0f)})
		}
		f.progressive = m == 0xc2 || m == 0xc6 || m == 0xca || m == 0xce
		ok = true
		return false
	})
	return f, ok
}
