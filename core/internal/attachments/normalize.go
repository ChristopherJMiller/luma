// Package attachments turns an upload into something the model may read
// (Task 12): the type is sniffed, never taken from the client; files are
// capped at 20 MiB, images at 40 MP and at what their decoder would
// allocate, both checked from the header before anything is decoded;
// photos are turned upright from their EXIF orientation, downscaled to a
// 1568 px long edge and re-encoded without a byte of their metadata; PDFs
// have their pages counted. Uploads are served back to their owner only,
// bound to a conversation under its 20 MiB cap, and rendered as content
// blocks for turn.send.
package attachments

import (
	"bytes"
	"context"
	"io"
	"mime"
	"net/http"
	"unicode/utf8"

	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/sessions"
)

// maxTextBytes bounds a text attachment, which must also be valid UTF-8.
const maxTextBytes = 1 << 20

// Normalized is an upload ready to store: its sniffed media type, the bytes
// the model will read (an image re-encoded, a PDF or text as sent), and an
// image's size or a PDF's page count.
type Normalized struct {
	MediaType     string
	Data          []byte
	Width, Height int
	Pages         int
}

// Normalize reads one upload (at most protocol.MaxAttachmentBytes) and
// normalises it. The type comes from the bytes alone; filename, like any
// type the client declared, decides nothing. A refusal is a
// *sessions.RefusedError carrying attachment_too_large, image_too_large or
// unsupported_attachment.
func Normalize(r io.Reader, filename string) (Normalized, error) {
	return normalize(context.Background(), r, 0)
}

// normalize is Normalize with the request's context (a decode waits for a
// slot until the client leaves) and a size hint for the read buffer.
func normalize(ctx context.Context, r io.Reader, sizeHint int64) (Normalized, error) {
	data, err := readFile(r, sizeHint)
	if err != nil {
		return Normalized{}, err
	}
	switch mt := sniff(data); mt {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return normalizeImage(ctx, mt, data)
	case "application/pdf":
		pages, err := pageCount(ctx, data)
		if err != nil {
			return Normalized{}, err
		}
		return Normalized{MediaType: mt, Data: data, Pages: pages}, nil
	case "text/plain":
		if !plainText(data) {
			return Normalized{}, refused(protocol.CodeUnsupportedAttachment)
		}
		return Normalized{MediaType: mt, Data: data}, nil
	}
	return Normalized{}, refused(protocol.CodeUnsupportedAttachment)
}

// plainText reports a text attachment Luma reads: at most 1 MiB of valid
// UTF-8 with no C0 control character but tab, line feed and carriage
// return, anywhere (the sniffer looks only at the first 512 bytes).
func plainText(b []byte) bool {
	if len(b) > maxTextBytes || !utf8.Valid(b) {
		return false
	}
	for _, c := range b {
		if c < 0x20 && c != '\t' && c != '\n' && c != '\r' {
			return false
		}
	}
	return true
}

// maxPresize bounds the read buffer allocated before the bytes arrive: a
// Content-Length is the client's claim, so a larger file grows the buffer
// as it comes in.
const maxPresize = 1 << 20

// readFile reads at most MaxAttachmentBytes, and refuses one byte more
// without reading on. sizeHint (the request's Content-Length, which frames
// the file) presizes the buffer up to 1 MiB; past that it doubles as bytes
// arrive, never beyond the limit.
func readFile(r io.Reader, sizeHint int64) ([]byte, error) {
	const limit = protocol.MaxAttachmentBytes
	size := int64(64 << 10)
	if sizeHint > 0 {
		size = min(sizeHint, maxPresize)
	}
	buf := make([]byte, 0, size)
	lr := io.LimitReader(r, limit+1)
	for {
		if len(buf) > limit {
			return nil, refused(protocol.CodeAttachmentTooLarge)
		}
		if len(buf) == cap(buf) {
			grown := make([]byte, len(buf), min(2*cap(buf), limit+1))
			copy(grown, buf)
			buf = grown
		}
		n, err := lr.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err == io.EOF && len(buf) <= limit { // a last read may bring bytes and EOF together
			return buf, nil
		}
		if err != nil && err != io.EOF {
			return nil, err
		}
	}
}

// sniff is the media type the bytes say they are: a PDF by its %PDF- magic,
// anything else by http.DetectContentType, without parameters.
func sniff(data []byte) string {
	if bytes.HasPrefix(data, []byte("%PDF-")) {
		return "application/pdf"
	}
	mt, _, err := mime.ParseMediaType(http.DetectContentType(data))
	if err != nil {
		return ""
	}
	return mt
}

// refused is a C3 refusal with its copy. It is a *sessions.RefusedError, so
// a refusal from Bind reaches the client through the session API's error
// path unchanged (C3).
func refused(code protocol.ErrorCode) error {
	return &sessions.RefusedError{Info: protocol.ErrorInfo{Code: code, Message: protocol.ErrorCopy[code]}}
}
