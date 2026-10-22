package attachments

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherJMiller/luma/core/internal/protocol"
)

func TestPdfPageCountAndTierCap(t *testing.T) {
	n := normalizeOK(t, pdfBytes(t, 25, 1000), "report.pdf")
	if n.MediaType != "application/pdf" || n.Pages != 25 || n.Width != 0 || n.Height != 0 {
		t.Fatalf("a 25-page PDF = %s, %d pages, %dx%d", n.MediaType, n.Pages, n.Width, n.Height)
	}
	if !bytes.Equal(n.Data, pdfBytes(t, 25, 1000)) {
		t.Fatal("a PDF is stored as uploaded")
	}

	h := newResolverHarness(t)
	fast, smart := h.tree(chris, "ses_fast"), h.tree(chris, "ses_smart")
	id := h.upload(chris, "att_pdf", "application/pdf", n.Data, n.Pages)
	_, err := h.r.Bind(bg, chris, fast, []string{id}, "fast")
	info := refusal(t, err)
	if want := "That PDF has more than 20 pages, the most Luma reads in this mode."; info.Code != protocol.CodePDFTooLong || info.Message != want {
		t.Fatalf("25 pages on fast = %+v, want pdf_too_long %q", info, want)
	}
	if h.rootOf(chris, id) != "" {
		t.Fatal("the refused PDF was bound")
	}
	blocks, err := h.r.Bind(bg, chris, smart, []string{id}, "smart")
	if err != nil || len(blocks) != 1 || blocks[0].Type != "document" || blocks[0].Source.MediaType != "application/pdf" {
		t.Fatalf("25 pages on smart = %+v, %v", blocks, err)
	}
	// Exactly the tier's maximum is allowed.
	twenty := normalizeOK(t, pdfBytes(t, 20, 10), "twenty.pdf")
	id20 := h.upload(chris, "att_20", "application/pdf", twenty.Data, twenty.Pages)
	if _, err := h.r.Bind(bg, chris, fast, []string{id20}, "fast"); err != nil {
		t.Fatalf("20 pages on fast: %v", err)
	}
}

func TestPdfUnparseableIsUnsupported(t *testing.T) {
	good := pdfBytes(t, 3, 10)
	for name, data := range map[string][]byte{
		"a header and nothing else": []byte("%PDF-1.7\n"),
		"a header then garbage":     append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte{0x00, 0xff, '<', '('}, 300)...),
		"cut before the xref":       good[:len(good)/2],
		"no pages":                  []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n2 0 obj\n<< /Type /Pages /Kids [] /Count 0 >>\nendobj\nxref\n0 3\n0000000000 65535 f \n0000000009 00000 n \n0000000058 00000 n \ntrailer\n<< /Size 3 /Root 1 0 R >>\nstartxref\n110\n%%EOF\n"),
	} {
		t.Run(name, func(t *testing.T) { wantRefused(t, data, "x.pdf", protocol.CodeUnsupportedAttachment) })
	}
}

// pdfcpu's default configuration installs a config directory under the
// user's config dir; Core's root filesystem is read-only, and nothing may
// be written.
func TestPdfReadsNoConfigDir(t *testing.T) {
	home, tmp := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("TMPDIR", tmp) // pdfcpu's fallback when there is no config dir
	// In this process, so the directories are the ones set above.
	if n, err := countPages(t.Context(), pdfBytes(t, 2, 10)); err != nil || n != 2 {
		t.Fatalf("countPages = %d, %v", n, err)
	}
	for _, dir := range []string{home, tmp} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("counting pages wrote %s into %s", entries[0].Name(), dir)
		}
	}
}

// withChild points the page counter at one of TestMain's misbehaving
// children, with a short timeout.
func withChild(t *testing.T, mode string) {
	t.Helper()
	env, timeout := pdfChildEnv, pdfTimeout
	pdfChildEnv, pdfTimeout = testChildEnv+"="+mode, 2*time.Second
	t.Cleanup(func() { pdfChildEnv, pdfTimeout = env, timeout })
}

// The page counter is a child process (S57d): a child that hangs, crashes
// or answers nonsense is unsupported_attachment, and a hung one is killed.
func TestPdfChildFailuresAreUnsupported(t *testing.T) {
	good := pdfBytes(t, 2, 10)
	for _, mode := range []string{"hang", "crash", "noise", "zero"} {
		t.Run(mode, func(t *testing.T) {
			withChild(t, mode)
			start := time.Now()
			_, err := pageCount(t.Context(), good)
			if codeOf(err) != protocol.CodeUnsupportedAttachment {
				t.Fatalf("a %s child = %v, want unsupported_attachment", mode, err)
			}
			if d := time.Since(start); d > 6*time.Second {
				t.Fatalf("a %s child took %v; the 2 s timer should have killed it", mode, d)
			}
		})
	}
	// A client that leaves takes its count down with it.
	withChild(t, "hang")
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, err := pageCount(ctx, good); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a count whose client left = %v, want the context's error", err)
	}
}

// The real child runs under a 256 MiB committed-memory limit: a child that
// commits 320 MiB dies, and the parent's heap never sees it. The same
// allocation without the limit succeeds, so the limit is what kills it (the
// runtime dies on whichever mapping is refused first, with an OOM throw, a
// failed pthread_create in a cgo test binary, or a fault in the GC, so its
// message is not checked). A race build cannot run under such a limit (see
// race_on.go), so this runs without -race: make test-go runs it so.
func TestPdfChildMemoryLimit(t *testing.T) {
	if raceBuild {
		t.Skip("RLIMIT_DATA cannot apply under the race detector; `make test-go` runs this test without -race")
	}
	child := func(mode string) (string, error) {
		cmd := exec.CommandContext(t.Context(), os.Args[0], PDFPagesCommand)
		cmd.Env = []string{testChildEnv + "=" + mode}
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	if out, err := child("alloc-unlimited"); err != nil || out != "335544320" {
		t.Fatalf("committing 320 MiB without the limit = %q, %v; want it to succeed", out, err)
	}
	out, err := child("alloc")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() == 0 || out != "" {
		t.Fatalf("committing 320 MiB under the limit = %q, %v; want the child killed", out, err)
	}
	// The real counter still works under the limit.
	if n, err := pageCount(t.Context(), pdfBytes(t, 3, 1<<20)); err != nil || n != 3 {
		t.Fatalf("pageCount under the limit = %d, %v", n, err)
	}
}
