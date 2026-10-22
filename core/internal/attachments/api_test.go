package attachments

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ChristopherJMiller/luma/core/internal/agents"
	"github.com/ChristopherJMiller/luma/core/internal/auth"
	"github.com/ChristopherJMiller/luma/core/internal/auth/oidctest"
	lumametrics "github.com/ChristopherJMiller/luma/core/internal/metrics"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/sessions"
	"github.com/ChristopherJMiller/luma/core/internal/store"
	"github.com/ChristopherJMiller/luma/core/internal/store/storetest"
)

// ---- the web harness: the real BFF in front of the routes, as Task 15 wires them ----

const devCookie = "luma_dev" // the session cookie's name for an http public URL (S17)

const webRegistry = `people:
  pers-chris:
    display_name: Synthetic Chris
    tier: admin
    emails:
      mock: [chris@example.test]
  pers-erin:
    display_name: Synthetic Erin
    tier: family
    emails:
      mock: [erin@example.test]
  pers-sam:
    display_name: Synthetic Sam
    tier: family
    emails:
      mock: [sam@example.test]
`

var (
	sam      = auth.Person{ID: "pers-sam", DisplayName: "Synthetic Sam", Tier: "family"}
	webEmail = map[string]string{chris.ID: "chris@example.test", erin.ID: "erin@example.test", sam.ID: "sam@example.test"}
)

type web struct {
	t      *testing.T
	st     *store.Store
	srv    *httptest.Server
	origin string
	cond   *fakeConductor
	att    *Handler
}

// newWeb serves the attachment routes and the session routes (over the real
// agent catalog, with this package's Resolver) behind the BFF's
// RequirePerson, the whole mux inside UnsafeRequestGuard.
func newWeb(t *testing.T) *web {
	t.Helper()
	w := &web{t: t, st: storetest.New(t), srv: httptest.NewUnstartedServer(nil), cond: &fakeConductor{}}
	w.origin = "http://" + w.srv.Listener.Addr().String()
	regPath := filepath.Join(t.TempDir(), "registry.yaml")
	if err := os.WriteFile(regPath, []byte(webRegistry), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := auth.LoadRegistry(regPath)
	if err != nil {
		t.Fatal(err)
	}
	iss := oidctest.New(t, "luma", "synthetic-secret", w.origin+"/auth/callback")
	bff, err := auth.NewBFF(t.Context(), auth.BFFConfig{
		PublicURL: w.origin, Issuer: iss.URL, ClientID: "luma", ClientSecret: "synthetic-secret",
		SigningKey: bytes.Repeat([]byte{0x5a}, 32), AllowedOrigins: []string{w.origin},
	}, w.st, reg)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := agents.LoadCatalog("../../../agents")
	if err != nil {
		t.Fatal(err)
	}
	live := agents.NewLive(cat)
	svc := sessions.NewService(w.st, live, w.cond, emptyProfile{}, NewResolver(w.st, live), openGate{},
		sessions.NopLedger{}, lumametrics.NewCore(prometheus.NewRegistry()), time.Now, sessions.Config{})
	mux := http.NewServeMux()
	bff.Routes(mux)
	w.att = NewHandler(w.st)
	w.att.Routes(mux, bff.RequirePerson)
	svc.Routes(mux, bff.RequirePerson)
	w.srv.Config.Handler = auth.UnsafeRequestGuard([]string{w.origin})(mux)
	w.srv.Start()
	t.Cleanup(w.srv.Close)
	return w
}

type emptyProfile struct{}

func (emptyProfile) Snapshot(context.Context, string) (string, error) { return "", nil }

type openGate struct{}

func (openGate) CheckNewTurn(context.Context) *protocol.ErrorInfo { return nil }

// fakeConductor accepts every host and records the turns sent.
type fakeConductor struct {
	mu    sync.Mutex
	sends []protocol.TurnSend
}

func (c *fakeConductor) Open(context.Context, protocol.HostSpec) error { return nil }
func (c *fakeConductor) SendTurn(_ context.Context, ts protocol.TurnSend) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sends = append(c.sends, ts)
	return nil
}
func (c *fakeConductor) Interrupt(context.Context, string) error         { return nil }
func (c *fakeConductor) Close(context.Context, string, string) error     { return nil }
func (c *fakeConductor) PurgeTree(context.Context, string, string) error { return nil }
func (c *fakeConductor) Connected() bool                                 { return true }
func (c *fakeConductor) lastSend() (protocol.TurnSend, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sends) == 0 {
		return protocol.TurnSend{}, false
	}
	return c.sends[len(c.sends)-1], true
}

// login stores a web session for p, as a sign-in would, and returns its cookie.
func (w *web) login(p auth.Person) *http.Cookie {
	w.t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		w.t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if err := w.st.CreateWebSession(bg, store.WebSession{
		IDHash: sum[:], PersonID: p.ID, ConnectorID: "mock", Email: webEmail[p.ID], ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		w.t.Fatal(err)
	}
	return &http.Cookie{Name: devCookie, Value: base64.RawURLEncoding.EncodeToString(raw)}
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (w *web) send(req *http.Request, c *http.Cookie) response {
	w.t.Helper()
	req.Header.Set("Origin", w.origin)
	if c != nil {
		req.AddCookie(c)
	}
	resp, err := w.srv.Client().Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		w.t.Fatal(err)
	}
	return response{resp.StatusCode, resp.Header, b}
}

func (w *web) do(method, path string, c *http.Cookie, body string) response {
	w.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, w.srv.URL+path, rd)
	if err != nil {
		w.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return w.send(req, c)
}

// multipartBody frames content as the one "file" part of a form, with a
// declared part Content-Type, and returns the body's reader, its length
// and the request Content-Type. The content streams through; it is never
// copied into the body.
func multipartBody(filename, partType string, content io.Reader, size int64) (io.Reader, int64, string) {
	return multipartPart("file", filename, partType, content, size)
}

// multipartPart is multipartBody for a part of any name. The filename is
// quoted as a browser does: raw UTF-8, with only `"` and `\` escaped.
func multipartPart(field, filename, partType string, content io.Reader, size int64) (io.Reader, int64, string) {
	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	hdr := textproto.MIMEHeader{}
	quoted := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(filename)
	hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename="%s"`, field, quoted))
	hdr.Set("Content-Type", partType)
	_, _ = mw.CreatePart(hdr)
	prefix := bytes.Clone(head.Bytes())
	head.Reset()
	_ = mw.Close()
	trailer := head.Bytes()
	return io.MultiReader(bytes.NewReader(prefix), content, bytes.NewReader(trailer)),
		int64(len(prefix)) + size + int64(len(trailer)), mw.FormDataContentType()
}

// upload POSTs data as /v1/attachments' file part.
func (w *web) upload(c *http.Cookie, filename, partType string, data []byte) response {
	w.t.Helper()
	body, n, ct := multipartBody(filename, partType, bytes.NewReader(data), int64(len(data)))
	return w.uploadBody(c, body, n, ct)
}

func (w *web) uploadBody(c *http.Cookie, body io.Reader, n int64, contentType string) response {
	w.t.Helper()
	req, err := http.NewRequest(http.MethodPost, w.srv.URL+"/v1/attachments", body)
	if err != nil {
		w.t.Fatal(err)
	}
	req.ContentLength = n
	req.Header.Set("Content-Type", contentType)
	return w.send(req, c)
}

func (w *web) uploaded(c *http.Cookie, filename string, data []byte) protocol.Attachment {
	w.t.Helper()
	r := w.upload(c, filename, "application/octet-stream", data)
	if r.status != http.StatusOK {
		w.t.Fatalf("upload %s = %d %s", filename, r.status, r.body)
	}
	var a protocol.Attachment
	if err := json.Unmarshal(r.body, &a); err != nil {
		w.t.Fatal(err)
	}
	return a
}

func errorInfo(t *testing.T, r response) protocol.ErrorInfo {
	t.Helper()
	var e protocol.ErrorInfo
	if err := json.Unmarshal(r.body, &e); err != nil {
		t.Fatalf("body %q is not an ErrorInfo: %v", r.body, err)
	}
	return e
}

func wantError(t *testing.T, r response, status int, code protocol.ErrorCode) {
	t.Helper()
	if r.status != status {
		t.Fatalf("status = %d (%s), want %d", r.status, r.body, status)
	}
	if e := errorInfo(t, r); e.Code != code || e.Message != protocol.ErrorCopy[code] {
		t.Fatalf("error = %+v, want %s with its C3 copy", e, code)
	}
}

// ---- upload ----

func TestRejectsOver20MB(t *testing.T) {
	w := newWeb(t)
	c := w.login(chris)
	over := append([]byte("%PDF-1.7\n"), make([]byte, protocol.MaxAttachmentBytes+1-9)...)
	wantError(t, w.upload(c, "big.pdf", "application/pdf", over), http.StatusRequestEntityTooLarge, protocol.CodeAttachmentTooLarge)

	// Exactly the limit is accepted, multipart framing included.
	exact := pdfBytes(t, 3, fillerFor(t, 3, protocol.MaxAttachmentBytes))
	if len(exact) != protocol.MaxAttachmentBytes {
		t.Fatalf("test PDF is %d bytes", len(exact))
	}
	r := w.upload(c, "exact.pdf", "application/pdf", exact)
	var a protocol.Attachment
	if r.status != http.StatusOK || json.Unmarshal(r.body, &a) != nil {
		t.Fatalf("a %d-byte PDF = %d %s", len(exact), r.status, r.body)
	}
	if a.MediaType != "application/pdf" || a.SizeBytes != protocol.MaxAttachmentBytes || a.Pages != 3 || a.Filename != "exact.pdf" ||
		!strings.HasPrefix(a.ID, "att_") || len(a.ID) != 30 {
		t.Fatalf("attachment = %+v", a)
	}
	// A body past the limit plus framing room is cut off by the body cap,
	// even when the bytes are in a part that is not the file.
	junk := strings.NewReader(strings.Repeat("x", protocol.MaxAttachmentBytes+70<<10))
	body, n, ct := multipartPart("junk", "junk.bin", "application/octet-stream", junk, int64(junk.Len()))
	wantError(t, w.uploadBody(c, body, n, ct), http.StatusRequestEntityTooLarge, protocol.CodeAttachmentTooLarge)
}

// A 19 MiB PDF goes through with the heap's live peak under 60 MiB: the
// body streams (no ParseMultipartForm), the bytes sit in memory once,
// plus pdfcpu's read of its stream and one copy on the wire to Postgres.
func TestUploadStreamsMultipart(t *testing.T) {
	w := newWeb(t)
	c := w.login(chris)
	const size = 19 << 20
	filler := fillerFor(t, 2, size)
	pr, pw := io.Pipe()
	go func() { _, err := writePDF(pw, 2, filler); pw.CloseWithError(err) }()
	body, n, ct := multipartBody("scan.pdf", "application/pdf", pr, size)

	// GOMEMLIMIT-style accounting: collect continuously and record the live
	// heap each collection leaves, so garbage awaiting a cycle never counts.
	sample := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	runtime.GC()
	metrics.Read(sample)
	baseline := sample[0].Value.Uint64()
	var peak atomic.Uint64
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			select {
			case <-stop:
				return
			default:
			}
			runtime.GC()
			metrics.Read(sample)
			if v := sample[0].Value.Uint64(); v > peak.Load() {
				peak.Store(v)
			}
		}
	}()
	r := w.uploadBody(c, body, n, ct)
	close(stop)
	<-stopped
	if r.status != http.StatusOK {
		t.Fatalf("a 19 MiB PDF = %d %s", r.status, r.body)
	}
	var a protocol.Attachment
	if err := json.Unmarshal(r.body, &a); err != nil || a.SizeBytes != size || a.Pages != 2 {
		t.Fatalf("attachment = %+v, %v", a, err)
	}
	t.Logf("live heap: baseline %.1f MiB, peak %.1f MiB", float64(baseline)/(1<<20), float64(peak.Load())/(1<<20))
	if p := peak.Load(); p >= 60<<20 {
		t.Fatalf("the live heap peaked at %.1f MiB, want < 60 MiB", float64(p)/(1<<20))
	}
}

func TestSniffNotDeclaredType(t *testing.T) {
	w := newWeb(t)
	c := w.login(chris)
	png := encodePNG(t, withAlpha(gradient(64, 48)))
	r := w.upload(c, "statement.pdf", "application/pdf", png)
	var a protocol.Attachment
	if r.status != http.StatusOK || json.Unmarshal(r.body, &a) != nil {
		t.Fatalf("a PNG named .pdf = %d %s", r.status, r.body)
	}
	if a.MediaType != "image/png" || a.Width != 64 || a.Height != 48 || a.Pages != 0 {
		t.Fatalf("a PNG named .pdf = %+v, want image/png 64x48", a)
	}
	elf := append([]byte("\x7fELF\x02\x01\x01\x00"), make([]byte, 4096)...)
	wantError(t, w.upload(c, "notes.txt", "text/plain", elf), http.StatusUnsupportedMediaType, protocol.CodeUnsupportedAttachment)
	wantError(t, w.upload(c, "huge.png", "image/png", pngHeader(20000, 20000, 8, 6, 0)), http.StatusRequestEntityTooLarge, protocol.CodeImageTooLarge)
}

// slowUpload starts an upload whose file arrives only when finish is
// called; the handler holds its slot while it waits.
func (w *web) slowUpload(c *http.Cookie) (finish func(), status <-chan int) {
	pr, pw := io.Pipe()
	// A failed test still ends the body, so the server's Close can return.
	w.t.Cleanup(func() { _ = pw.CloseWithError(io.ErrUnexpectedEOF) })
	body, n, ct := multipartBody("slow.txt", "text/plain", pr, 5)
	req, err := http.NewRequest(http.MethodPost, w.srv.URL+"/v1/attachments", body)
	if err != nil {
		w.t.Fatal(err)
	}
	req.ContentLength = n
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Origin", w.origin)
	req.AddCookie(c)
	ch := make(chan int, 1)
	go func() {
		resp, err := w.srv.Client().Do(req)
		if err != nil {
			ch <- -1
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		ch <- resp.StatusCode
	}()
	return func() { _, _ = pw.Write([]byte("hello")); _ = pw.Close() }, ch
}

func (w *web) waitUploads(n int) {
	w.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		w.att.mu.Lock()
		got := w.att.uploads
		w.att.mu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("uploads in flight = %d, want %d", got, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// At most four uploads are in flight, two per person, each holding up to
// 20 MiB; past that an upload is refused before its body is read: 503
// rate_limited with Retry-After.
func TestUploadSlots(t *testing.T) {
	w := newWeb(t)
	a, b, c := w.login(chris), w.login(erin), w.login(sam)
	busy := func(r response) {
		t.Helper()
		wantError(t, r, http.StatusServiceUnavailable, protocol.CodeRateLimited)
		if r.header.Get("Retry-After") != "5" {
			t.Fatalf("Retry-After = %q, want 5", r.header.Get("Retry-After"))
		}
	}
	finishA1, a1 := w.slowUpload(a)
	finishA2, a2 := w.slowUpload(a)
	w.waitUploads(2)
	busy(w.upload(a, "third.txt", "text/plain", []byte("hi"))) // a third for one person
	finishB1, b1 := w.slowUpload(b)
	finishB2, b2 := w.slowUpload(b)
	w.waitUploads(4)
	busy(w.upload(c, "fifth.txt", "text/plain", []byte("hi"))) // a fifth in all
	finishA1()
	if s := <-a1; s != http.StatusOK {
		t.Fatalf("the first slow upload = %d", s)
	}
	w.waitUploads(3)
	if r := w.upload(c, "now.txt", "text/plain", []byte("hi")); r.status != http.StatusOK {
		t.Fatalf("an upload once a slot freed = %d %s", r.status, r.body)
	}
	for _, f := range []func(){finishA2, finishB1, finishB2} {
		f()
	}
	for _, ch := range []<-chan int{a2, b1, b2} {
		if s := <-ch; s != http.StatusOK {
			t.Fatalf("a slow upload = %d", s)
		}
	}
	w.waitUploads(0)
	w.att.mu.Lock()
	defer w.att.mu.Unlock()
	if len(w.att.perPerson) != 0 {
		t.Fatalf("per-person slots left: %v", w.att.perPerson)
	}
}

func TestUploadNeedsAFilePart(t *testing.T) {
	w := newWeb(t)
	c := w.login(chris)
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	_ = mw.WriteField("note", "no file here")
	_ = mw.Close()
	wantError(t, w.uploadBody(c, &b, int64(b.Len()), mw.FormDataContentType()), http.StatusBadRequest, protocol.CodeInvalidRequest)
	// A multipart Content-Type without a boundary.
	wantError(t, w.uploadBody(c, strings.NewReader("x"), 1, "multipart/form-data"), http.StatusBadRequest, protocol.CodeInvalidRequest)
	// Without a cookie the auth layer answers first.
	if r := w.upload(nil, "a.txt", "text/plain", []byte("hi")); r.status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated upload = %d", r.status)
	}
}

func TestUploadedFilenameIsCleaned(t *testing.T) {
	w := newWeb(t)
	c := w.login(chris)
	for _, tc := range []struct{ in, want string }{
		{"../../etc/passwd.txt", "passwd.txt"},
		{"evil" + string(rune(0x202e)) + "gnp.exe.txt", "evilgnp.exe.txt"}, // a right-to-left override
		{"tab\there.txt", "tabhere.txt"},
		{`say "cheese".txt`, `say "cheese".txt`},
		{"", "attachment"},
		{strings.Repeat("é", 200) + ".txt", strings.Repeat("é", 127)},
	} {
		a := w.uploaded(c, tc.in, []byte("notes"))
		if a.Filename != tc.want {
			t.Fatalf("filename %q stored as %q, want %q", tc.in, a.Filename, tc.want)
		}
	}
}

// ---- serving ----

func TestOwnerOnlyGet(t *testing.T) {
	w := newWeb(t)
	mine, theirs := w.login(chris), w.login(erin)
	img := w.uploaded(mine, "kite.png", encodePNG(t, gradient(30, 20)))
	doc := w.uploaded(mine, "bill.pdf", pdfBytes(t, 1, 10))
	txt := w.uploaded(mine, "list.txt", []byte("milk\n"))

	r := w.do(http.MethodGet, "/v1/attachments/"+img.ID, mine, "")
	if r.status != http.StatusOK || r.header.Get("Content-Type") != "image/jpeg" ||
		r.header.Get("X-Content-Type-Options") != "nosniff" || r.header.Get("Content-Security-Policy") != "sandbox" ||
		r.header.Get("Cache-Control") != "private, no-store" ||
		r.header.Get("Content-Disposition") != `inline; filename=kite.png` {
		t.Fatalf("GET image = %d %v", r.status, r.header)
	}
	if m, err := jpeg.Decode(bytes.NewReader(r.body)); err != nil || m.Bounds().Dx() != 30 || int64(len(r.body)) != img.SizeBytes {
		t.Fatalf("GET image body: %v, %d bytes (stored %d)", err, len(r.body), img.SizeBytes)
	}
	r = w.do(http.MethodGet, "/v1/attachments/"+doc.ID, mine, "")
	if r.status != http.StatusOK || r.header.Get("Content-Type") != "application/pdf" || r.header.Get("Content-Security-Policy") != "sandbox" ||
		r.header.Get("Content-Disposition") != `attachment; filename=bill.pdf` || !bytes.Equal(r.body, pdfBytes(t, 1, 10)) {
		t.Fatalf("GET pdf = %d %v", r.status, r.header)
	}
	r = w.do(http.MethodGet, "/v1/attachments/"+txt.ID, mine, "")
	if r.status != http.StatusOK || r.header.Get("Content-Type") != "text/plain; charset=utf-8" ||
		r.header.Get("Content-Disposition") != `attachment; filename=list.txt` || string(r.body) != "milk\n" {
		t.Fatalf("GET text = %d %v %q", r.status, r.header, r.body)
	}

	// Someone else's attachment, or a missing one, is 404 not_found.
	wantError(t, w.do(http.MethodGet, "/v1/attachments/"+img.ID, theirs, ""), http.StatusNotFound, protocol.CodeNotFound)
	wantError(t, w.do(http.MethodGet, "/v1/attachments/att_nope", mine, ""), http.StatusNotFound, protocol.CodeNotFound)
	wantError(t, w.do(http.MethodDelete, "/v1/attachments/"+img.ID, theirs, ""), http.StatusNotFound, protocol.CodeNotFound)
	if r := w.do(http.MethodGet, "/v1/attachments/"+img.ID, nil, ""); r.status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET = %d", r.status)
	}
	// A non-ASCII name is carried RFC 2231-encoded.
	u := w.uploaded(mine, "café menu.pdf", pdfBytes(t, 1, 10))
	if r := w.do(http.MethodGet, "/v1/attachments/"+u.ID, mine, ""); r.header.Get("Content-Disposition") != `attachment; filename*=utf-8''caf%C3%A9%20menu.pdf` {
		t.Fatalf("Content-Disposition = %q", r.header.Get("Content-Disposition"))
	}

	// The owner deletes an unbound upload; afterwards it is gone.
	if r := w.do(http.MethodDelete, "/v1/attachments/"+txt.ID, mine, ""); r.status != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s", r.status, r.body)
	}
	wantError(t, w.do(http.MethodGet, "/v1/attachments/"+txt.ID, mine, ""), http.StatusNotFound, protocol.CodeNotFound)
}

// A sent attachment is part of the conversation (its bytes are in every
// later request), so only deleting the conversation removes it: deleting
// it alone would free tree bytes the transcript still carries.
func TestDeleteRefusesABoundAttachment(t *testing.T) {
	w := newWeb(t)
	c := w.login(erin)
	ses := w.createSession(c, "fast")
	a := w.uploaded(c, "list.txt", []byte("milk\n"))
	if r := w.sendMessage(c, ses.ID, a.ID); r.status != http.StatusOK {
		t.Fatalf("send = %d %s", r.status, r.body)
	}
	wantError(t, w.do(http.MethodDelete, "/v1/attachments/"+a.ID, c, ""), http.StatusBadRequest, protocol.CodeInvalidRequest)
	if r := w.do(http.MethodGet, "/v1/attachments/"+a.ID, c, ""); r.status != http.StatusOK {
		t.Fatalf("the bound attachment after a refused delete: %d", r.status)
	}
}

// ---- through the session API: the C3 copy reaches the client ----

func (w *web) createSession(c *http.Cookie, tier string) protocol.Session {
	w.t.Helper()
	r := w.do(http.MethodPost, "/v1/sessions", c, fmt.Sprintf(`{"agent":"luma","params":{"tier":%q}}`, tier))
	var s protocol.Session
	if r.status != http.StatusOK || json.Unmarshal(r.body, &s) != nil {
		w.t.Fatalf("create session = %d %s", r.status, r.body)
	}
	return s
}

func (w *web) sendMessage(c *http.Cookie, sessionID string, ids ...string) response {
	w.t.Helper()
	b, _ := json.Marshal(ids)
	u := make([]byte, 16)
	_, _ = rand.Read(u)
	u[6], u[8] = u[6]&0x0f|0x40, u[8]&0x3f|0x80 // a version 4 UUID
	clientID := fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
	body := fmt.Sprintf(`{"type":"user.message","client_id":%q,"content":[{"type":"text","text":"look"}],"attachment_ids":%s}`, clientID, b)
	return w.do(http.MethodPost, "/v1/sessions/"+sessionID+"/events", c, body)
}

func TestRefusalCopyReachesTheClient(t *testing.T) {
	w := newWeb(t)
	c := w.login(erin)

	// pdf_too_long carries the tier's maximum in its copy.
	fast := w.createSession(c, "fast")
	long := w.uploaded(c, "manual.pdf", pdfBytes(t, 25, 10))
	r := w.sendMessage(c, fast.ID, long.ID)
	if e := errorInfo(t, r); r.status != http.StatusBadRequest || e.Code != protocol.CodePDFTooLong ||
		e.Message != "That PDF has more than 20 pages, the most Luma reads in this mode." {
		t.Fatalf("25 pages on fast = %d %+v", r.status, e)
	}

	// Two 11 MiB files are more than one conversation can carry.
	ses := w.createSession(c, "smart")
	x := w.uploaded(c, "a.pdf", pdfBytes(t, 1, 11<<20))
	y := w.uploaded(c, "b.pdf", pdfBytes(t, 1, 11<<20))
	r = w.sendMessage(c, ses.ID, x.ID, y.ID)
	if e := errorInfo(t, r); r.status != http.StatusBadRequest || e.Code != protocol.CodeConversationAttachmentsFull || e.Message != treeCp {
		t.Fatalf("22 MiB in one conversation = %d %+v", r.status, e)
	}
	if _, ok := w.cond.lastSend(); ok {
		t.Fatal("a refused message reached the conductor")
	}

	// An accepted photo reaches the conductor as an image block of the
	// normalised bytes, after the message text.
	photo := w.uploaded(c, "IMG_0003.jpg", withAPP1(t, encodeJPEG(t, quadrants()), cameraEXIF(6)))
	if photo.MediaType != "image/jpeg" || photo.Width != 20 || photo.Height != 40 {
		t.Fatalf("photo = %+v", photo)
	}
	if r := w.sendMessage(c, ses.ID, photo.ID); r.status != http.StatusOK {
		t.Fatalf("send photo = %d %s", r.status, r.body)
	}
	ts, ok := w.cond.lastSend()
	if !ok || len(ts.Content) < 2 || ts.Content[0].Text != "look" || ts.Content[1].Type != "image" ||
		ts.Content[1].Source.MediaType != "image/jpeg" || !ts.Untrusted {
		t.Fatalf("turn.send = %+v", ts)
	}
	got, err := base64.StdEncoding.DecodeString(ts.Content[1].Source.Data)
	if err != nil {
		t.Fatal(err)
	}
	stored := w.do(http.MethodGet, "/v1/attachments/"+photo.ID, c, "")
	if !bytes.Equal(got, stored.body) {
		t.Fatal("the image block is not the stored, normalised photo")
	}
}
