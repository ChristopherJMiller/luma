package attachments

import (
	"bytes"
	"encoding/binary"

	"github.com/rwcarlsen/goexif/exif"
)

// decodeExif is goexif's decoder; a test replaces it with one that panics.
var decodeExif = exif.Decode

// jpegOrientation is the EXIF orientation of a JPEG, 1 to 8, or 1 when it
// has none or its EXIF cannot be read safely.
func jpegOrientation(data []byte) int {
	var app1 []byte
	jpegSegments(data, func(m byte, p []byte) bool {
		if m == 0xe1 && bytes.HasPrefix(p, []byte("Exif\x00\x00")) {
			app1 = p
			return false
		}
		return true
	})
	if app1 == nil || !tiffSane(app1[6:]) {
		return 1
	}
	return exifOrientation(app1)
}

// exifOrientation parses an APP1 payload ("Exif\0\0" + TIFF) with goexif,
// inside recover(): a panic means no orientation.
func exifOrientation(app1 []byte) (o int) {
	defer func() {
		if recover() != nil {
			o = 1
		}
	}()
	x, err := decodeExif(bytes.NewReader(app1))
	// A failed sub-IFD (Exif, GPS, Interop) is not critical: IFD0, which
	// holds the orientation, was read.
	if x == nil || (err != nil && exif.IsCriticalError(err)) {
		return 1
	}
	tag, err := x.Get(exif.Orientation)
	if err != nil {
		return 1
	}
	v, err := tag.Int(0)
	if err != nil || v < 1 || v > 8 {
		return 1
	}
	return v
}

// jpegSegments walks a JPEG's marker segments up to its first scan (SOS),
// calling fn with each marker and payload until fn returns false. It stops
// quietly at anything malformed; a caller treats what it did not see as
// absent.
func jpegSegments(data []byte, fn func(marker byte, payload []byte) bool) {
	if len(data) < 2 || data[0] != 0xff || data[1] != 0xd8 {
		return
	}
	for i := 2; i+4 <= len(data); {
		if data[i] != 0xff {
			return
		}
		m := data[i+1]
		switch {
		case m == 0xff: // a fill byte
			i++
			continue
		case m == 0x01 || m == 0xd8 || (m >= 0xd0 && m <= 0xd7): // no payload
			i += 2
			continue
		case m == 0xda || m == 0xd9: // the scan, or the end
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

const (
	maxIFDs    = 8 // IFD0 and its next-IFD chain (a photo has two)
	maxSubIFDs = 8 // the Exif, GPS and Interoperability IFDs
)

// tiffTypeSize is the byte size of each TIFF field type, 1 to 12.
var tiffTypeSize = [...]uint64{0, 1, 1, 2, 4, 8, 1, 1, 2, 4, 8, 4, 8}

// tiffSane reports whether goexif can parse an EXIF block in bounded time
// and memory. goexif follows the next-IFD chain until an offset repeats the
// previous one, so an A→B→A cycle never ends; and it sizes each tag's value
// array by the declared count, after a uint32 product that can overflow,
// so a 12-byte tag can ask for gigabytes. Neither is a panic, so recover()
// cannot help. This checks every IFD goexif reads (IFD0's chain, and the
// IFDs that the Exif, GPS and Interoperability pointers name): no repeated
// IFD, and every value inside the block. A block that fails is read as no
// orientation, the same as one goexif rejects.
func tiffSane(t []byte) bool {
	if len(t) < 8 {
		return false
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return false
	}
	if bo.Uint16(t[2:]) != 42 {
		return false
	}
	seen := map[int64]bool{}
	var subs []int64
	off := int64(int32(bo.Uint32(t[4:])))
	for n := 0; off != 0; n++ {
		if n == maxIFDs || seen[off] {
			return false
		}
		seen[off] = true
		next, ptrs, ok := saneIFD(t, bo, off)
		if !ok {
			return false
		}
		subs = append(subs, ptrs...)
		off = next
	}
	checked := 0
	for i := 0; i < len(subs); i++ {
		o := subs[i]
		// goexif reads one IFD at a pointer, never its chain. One outside
		// the block fails to read and goexif goes on without it.
		if o < 0 || o >= int64(len(t)) || seen[o] {
			continue
		}
		if checked == maxSubIFDs {
			return false
		}
		checked++
		seen[o] = true
		_, ptrs, ok := saneIFD(t, bo, o)
		if !ok {
			return false
		}
		subs = append(subs, ptrs...)
	}
	return true
}

// saneIFD checks the IFD at off: its entries lie inside t, each of a known
// type with its value inside t. It returns the next-IFD offset and the
// values of any sub-IFD pointer tags.
func saneIFD(t []byte, bo binary.ByteOrder, off int64) (next int64, subs []int64, ok bool) {
	size := int64(len(t))
	if off < 0 || off+2 > size {
		return 0, nil, false
	}
	n := int64(int16(bo.Uint16(t[off:]))) // goexif reads an int16, and loops zero times when it is negative
	n = max(n, 0)
	end := off + 2 + 12*n
	if end+4 > size {
		return 0, nil, false
	}
	for e := off + 2; e < end; e += 12 {
		id, typ, count := bo.Uint16(t[e:]), bo.Uint16(t[e+2:]), bo.Uint32(t[e+4:])
		if typ == 0 || int(typ) >= len(tiffTypeSize) {
			return 0, nil, false
		}
		total := tiffTypeSize[typ] * uint64(count)
		if total == 0 || total > uint64(size) {
			return 0, nil, false
		}
		at := e + 8
		if total > 4 {
			at = int64(bo.Uint32(t[e+8:]))
			if uint64(at)+total > uint64(size) {
				return 0, nil, false
			}
		}
		if id == 0x8769 || id == 0x8825 || id == 0xa005 { // Exif, GPS, Interoperability
			if v, ok := firstInt(t[at:], typ, bo); ok {
				subs = append(subs, v)
			}
		}
	}
	return int64(int32(bo.Uint32(t[end:]))), subs, true
}

// firstInt is a tag's first value as goexif's Int64(0) reads it, for the
// integer types; other types make goexif skip the pointer.
func firstInt(v []byte, typ uint16, bo binary.ByteOrder) (int64, bool) {
	switch typ {
	case 1: // BYTE
		return int64(v[0]), true
	case 3: // SHORT
		return int64(bo.Uint16(v)), true
	case 4: // LONG
		return int64(bo.Uint32(v)), true
	case 6: // SBYTE
		return int64(int8(v[0])), true
	case 8: // SSHORT
		return int64(int16(bo.Uint16(v))), true
	case 9: // SLONG
		return int64(int32(bo.Uint32(v))), true
	}
	return 0, false
}
