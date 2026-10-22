package attachments

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ChristopherJMiller/luma/core/internal/auth"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/sessions"
	"github.com/ChristopherJMiller/luma/core/internal/store"
)

const (
	// bodyRoom is what a multipart body may carry beyond the file: part
	// headers and boundaries (S18).
	bodyRoom = 64 << 10
	// maxNameBytes bounds a stored filename.
	maxNameBytes = 255
	// maxUploads and maxUploadsPerPerson bound the uploads in flight, each
	// holding up to 20 MiB while it is read, waits for a decode slot and is
	// stored. Past them an upload is refused before its body is read.
	maxUploads          = 4
	maxUploadsPerPerson = 2
	// uploadRetryAfter is the Retry-After of a refused upload, in seconds.
	uploadRetryAfter = "5"
)

// Handler serves C1's attachment routes.
type Handler struct {
	st    *store.Store
	clock func() time.Time

	mu        sync.Mutex
	uploads   int            // in flight
	perPerson map[string]int // in flight, by person id
}

// NewHandler returns the attachment routes' handler.
func NewHandler(st *store.Store) *Handler {
	return &Handler{st: st, clock: time.Now, perPerson: map[string]int{}}
}

// takeUpload claims an upload slot for p: one of four in all and two per
// person. It reports false when none is free.
func (h *Handler) takeUpload(personID string) (release func(), ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.uploads >= maxUploads || h.perPerson[personID] >= maxUploadsPerPerson {
		return nil, false
	}
	h.uploads++
	h.perPerson[personID]++
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.uploads--
		if h.perPerson[personID]--; h.perPerson[personID] == 0 {
			delete(h.perPerson, personID)
		}
	}, true
}

// Routes registers POST /v1/attachments, GET and DELETE
// /v1/attachments/{id}, each behind requirePerson (the BFF's
// RequirePerson). Like the session routes, it leaves UnsafeRequestGuard to
// the mux's wrapper (Task 15), which admits multipart here only.
func (h *Handler) Routes(mux *http.ServeMux, requirePerson func(http.Handler) http.Handler) {
	route := func(pattern string, f func(http.ResponseWriter, *http.Request, auth.Person)) {
		mux.Handle(pattern, requirePerson(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := auth.PersonFrom(r.Context())
			if !ok { // not behind RequirePerson: refuse as the auth layer would (S41)
				http.Error(w, "unauthenticated", http.StatusUnauthorized)
				return
			}
			f(w, r, p)
		})))
	}
	route("POST /v1/attachments", h.handleUpload)
	route("GET /v1/attachments/{id}", h.handleGet)
	route("DELETE /v1/attachments/{id}", h.handleDelete)
}

// handleUpload streams the multipart body: it never calls
// ParseMultipartForm, the body is capped at MaxAttachmentBytes plus framing
// room, and the "file" part itself at MaxAttachmentBytes exactly. Other
// parts are skipped. The file is normalised, then stored unbound. With the
// upload slots full it answers 503 rate_limited with Retry-After, before
// reading anything (C1).
func (h *Handler) handleUpload(w http.ResponseWriter, r *http.Request, p auth.Person) {
	release, ok := h.takeUpload(p.ID)
	if !ok {
		w.Header().Set("Retry-After", uploadRetryAfter)
		writeErrorInfo(w, r, http.StatusServiceUnavailable, protocol.CodeRateLimited)
		return
	}
	defer release()
	r.Body = http.MaxBytesReader(w, r.Body, protocol.MaxAttachmentBytes+bodyRoom)
	mr, err := r.MultipartReader()
	if err != nil {
		writeErrorInfo(w, r, http.StatusBadRequest, protocol.CodeInvalidRequest)
		return
	}
	for {
		part, err := mr.NextPart()
		if err != nil {
			uploadFailed(w, r, err)
			return
		}
		if part.FormName() != "file" {
			if _, err := io.Copy(io.Discard, part); err != nil {
				uploadFailed(w, r, err)
				return
			}
			continue
		}
		n, err := normalize(r.Context(), p.ID, part, r.ContentLength)
		if err != nil {
			uploadFailed(w, r, err)
			return
		}
		h.save(w, r, p, n, cleanName(part.FileName()))
		return
	}
}

// save stores a normalised upload, unbound, and answers with its Attachment.
func (h *Handler) save(w http.ResponseWriter, r *http.Request, p auth.Person, n Normalized, filename string) {
	sum := sha256.Sum256(n.Data)
	a := store.Attachment{
		ID: newID("att_", h.clock()), PersonID: p.ID, MediaType: n.MediaType, Filename: filename,
		SizeBytes: int64(len(n.Data)), Width: positive(n.Width), Height: positive(n.Height), Pages: positive(n.Pages),
		SHA256: hex.EncodeToString(sum[:]), Data: n.Data,
	}
	if err := h.st.InsertAttachment(r.Context(), a); err != nil {
		internalError(w, r, err)
		return
	}
	slog.Info("attachments: stored", "id", a.ID, "media_type", a.MediaType, "bytes", a.SizeBytes)
	writeJSON(w, http.StatusOK, protocol.Attachment{
		ID: a.ID, MediaType: a.MediaType, Filename: a.Filename, SizeBytes: a.SizeBytes,
		Width: n.Width, Height: n.Height, Pages: n.Pages,
	})
}

// uploadFailed answers an upload that was not normalised: a refusal with
// its status, a body over the cap 413. Anything else came from reading the
// body (no file part, a malformed or cut-off body, a client that left), so
// it is the request's fault: 400.
func uploadFailed(w http.ResponseWriter, r *http.Request, err error) {
	var refusal *sessions.RefusedError
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &refusal):
		writeErrorInfo(w, r, uploadStatus(refusal.Info.Code), refusal.Info.Code)
	case errors.As(err, &tooBig):
		writeErrorInfo(w, r, http.StatusRequestEntityTooLarge, protocol.CodeAttachmentTooLarge)
	default:
		writeErrorInfo(w, r, http.StatusBadRequest, protocol.CodeInvalidRequest)
	}
}

// uploadStatus is the HTTP status of an upload refusal: 413 for a file or
// an image that is too large, 415 for a type Luma does not read.
func uploadStatus(code protocol.ErrorCode) int {
	if code == protocol.CodeAttachmentTooLarge || code == protocol.CodeImageTooLarge {
		return http.StatusRequestEntityTooLarge
	}
	if code == protocol.CodeUnsupportedAttachment {
		return http.StatusUnsupportedMediaType
	}
	return http.StatusBadRequest
}

// handleGet serves an attachment to its owner only (anyone else gets 404,
// S49). The bytes are served as stored, never sniffed, in a sandbox: images
// inline, everything else as a download.
func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request, p auth.Person) {
	id := r.PathValue("id")
	if _, err := h.st.AttachmentMeta(r.Context(), p.ID, id); err != nil {
		h.lookupFailed(w, r, err)
		return
	}
	a, err := h.st.Attachment(r.Context(), id)
	if err != nil {
		h.lookupFailed(w, r, err)
		return
	}
	ct, disposition := a.MediaType, "attachment"
	if strings.HasPrefix(ct, "image/") {
		disposition = "inline"
	}
	if ct == "text/plain" {
		ct += "; charset=utf-8"
	}
	hd := w.Header()
	hd.Set("Content-Type", ct)
	hd.Set("Content-Length", strconv.Itoa(len(a.Data)))
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Content-Security-Policy", "sandbox")
	hd.Set("Cache-Control", "private, no-store")
	if v := mime.FormatMediaType(disposition, map[string]string{"filename": a.Filename}); v != "" {
		hd.Set("Content-Disposition", v)
	} else {
		hd.Set("Content-Disposition", disposition)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(a.Data)
}

// handleDelete deletes the owner's unbound upload (a file taken off the
// composer before sending). A sent attachment is part of its conversation,
// in every later request, and is deleted only with the tree: deleting it
// alone would free tree bytes the transcript still carries. That is 400.
func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request, p auth.Person) {
	switch err := h.st.DeleteUnboundAttachment(r.Context(), p.ID, r.PathValue("id")); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, store.ErrBound):
		writeErrorInfo(w, r, http.StatusBadRequest, protocol.CodeInvalidRequest)
	default:
		h.lookupFailed(w, r, err)
	}
}

func (h *Handler) lookupFailed(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeErrorInfo(w, r, http.StatusNotFound, protocol.CodeNotFound)
		return
	}
	internalError(w, r, err)
}

// cleanName is the stored filename: the multipart FileName (a base name),
// valid UTF-8, without control or format characters (a right-to-left
// override can disguise an extension), at most 255 bytes.
func cleanName(name string) string {
	name = strings.ToValidUTF8(name, string(utf8.RuneError))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	for len(name) > maxNameBytes {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	if name == "" || name == "." {
		return "attachment"
	}
	return name
}

func positive(n int) *int {
	if n <= 0 {
		return nil
	}
	return &n
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		slog.Error("attachments: encoding a response failed", "err", err)
		status = http.StatusInternalServerError
		b, _ = json.Marshal(protocol.ErrorInfo{Code: protocol.CodeUpstreamError, Message: protocol.ErrorCopy[protocol.CodeUpstreamError]})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func writeErrorInfo(w http.ResponseWriter, r *http.Request, status int, code protocol.ErrorCode) {
	slog.Debug("attachments: request refused", "route", r.Pattern, "status", status, "code", code)
	writeJSON(w, status, protocol.ErrorInfo{Code: code, Message: protocol.ErrorCopy[code]})
}

func internalError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("attachments: request failed", "route", r.Pattern, "err", err)
	writeErrorInfo(w, r, http.StatusInternalServerError, protocol.CodeUpstreamError)
}

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newID returns prefix plus a ULID (48 bits of Unix milliseconds, 80 random
// bits, Crockford base32): the shape of every C4 id, as sessions makes them.
func newID(prefix string, t time.Time) string {
	var b [16]byte
	ms := uint64(t.UnixMilli())
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	_, _ = rand.Read(b[6:]) // crypto/rand.Read never fails (Go 1.24+)
	hi, lo := binary.BigEndian.Uint64(b[:8]), binary.BigEndian.Uint64(b[8:])
	var out [26]byte
	for i := 25; i >= 0; i-- {
		out[i] = crockford[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return prefix + string(out[:])
}
