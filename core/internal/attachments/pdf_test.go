package attachments

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

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
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	normalizeOK(t, pdfBytes(t, 2, 10), "a.pdf")
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("counting pages wrote %v into the home directory", entries[0].Name())
	}
}
