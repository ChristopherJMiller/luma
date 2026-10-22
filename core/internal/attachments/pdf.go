package attachments

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"

	"github.com/ChristopherJMiller/luma/core/internal/protocol"
)

// PDFPagesCommand is luma-core's hidden subcommand that counts a PDF's
// pages: the PDF on stdin, the count on stdout (S57d).
const PDFPagesCommand = "pdf-pages"

const (
	// childEnv marks the process Core starts to count pages. Any binary that
	// links this package (luma-core, and every test binary that imports it)
	// becomes the page counter when started with it, before its main runs,
	// so a test binary never runs its own tests as the child.
	childEnv = "LUMA_ATTACHMENTS_CHILD"
	// childDataLimit is the child's hard limit on committed memory
	// (RLIMIT_DATA): pdfcpu allocating without bound kills the child, never
	// Core. It is RLIMIT_DATA, not RLIMIT_AS: a Go process reserves 1.2 GB of
	// address space at start (VmSize 1,227,232 kB, measured) but commits
	// 44 MB (VmData), and only committed mappings count toward
	// RLIMIT_DATA, so this bounds the heap at about 210 MiB.
	childDataLimit = 256 << 20
	// childSoftLimit makes the child's GC work hard well before the hard
	// limit.
	childSoftLimit = 160 << 20
)

var (
	// pdfTimeout bounds one page count; the child is killed after it.
	pdfTimeout = 30 * time.Second
	// pdfChildEnv is the child's whole environment; a test points it at a
	// misbehaving child.
	pdfChildEnv = childEnv + "=" + PDFPagesCommand
)

func init() {
	if os.Getenv(childEnv) == PDFPagesCommand && len(os.Args) == 2 && os.Args[1] == PDFPagesCommand {
		os.Exit(RunPDFPages(os.Stdin, os.Stdout))
	}
}

// pageCount counts a PDF's pages in a child process: the running binary,
// re-executed as `pdf-pages` with nothing in its environment but childEnv
// and GOMAXPROCS, under a 256 MiB committed-memory limit and killed after 30 s. pdfcpu reads a
// file's whole structure, and a hostile file can make it allocate or loop
// far past what one upload is worth; the child takes that, not Core. The
// count runs in a decode slot. A child that fails, crashes, is killed or
// prints no count is unsupported_attachment (C3); one that cannot be
// started is a server error.
func pageCount(ctx context.Context, data []byte) (int, error) {
	release, err := acquireDecode(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	exe, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("attachments: the page counter: %w", err)
	}
	cctx, cancel := context.WithTimeout(ctx, pdfTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, exe, PDFPagesCommand) //nolint:gosec // G204: exe is this running binary (os.Executable), with a constant argument
	// Two Ps: pdfcpu is sequential, and every thread's stack counts toward
	// the child's memory limit.
	cmd.Env = []string{pdfChildEnv, "GOMAXPROCS=2"}
	cmd.Stdin = bytes.NewReader(data)
	out := &shortBuffer{}
	cmd.Stdout = out
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("attachments: start the page counter: %w", err)
	}
	werr := cmd.Wait()
	if err := ctx.Err(); err != nil {
		return 0, err // the client left
	}
	n, perr := strconv.Atoi(strings.TrimSpace(out.String()))
	if werr != nil || perr != nil || n < 1 {
		slog.Info("attachments: PDF refused by the page counter", "exit", cmd.ProcessState.String(),
			"timed_out", cctx.Err() != nil)
		return 0, refused(protocol.CodeUnsupportedAttachment)
	}
	return n, nil
}

// shortBuffer keeps the first 32 bytes written to it and drops the rest, so
// a child's output can never grow Core's memory.
type shortBuffer struct{ b []byte }

func (s *shortBuffer) Write(p []byte) (int, error) {
	s.b = append(s.b, p[:min(len(p), 32-len(s.b))]...)
	return len(p), nil
}

func (s *shortBuffer) String() string { return string(s.b) }

// RunPDFPages is the page counter's main: it limits its own memory, reads
// a PDF from in (at most MaxAttachmentBytes), and prints the page count to
// out. It returns the exit code: 0 with a count, 1 for a PDF it cannot
// read, 3 if the memory limit could not be set.
func RunPDFPages(in io.Reader, out io.Writer) int {
	if err := limitChildMemory(); err != nil {
		return 3
	}
	data, err := io.ReadAll(io.LimitReader(in, protocol.MaxAttachmentBytes+1))
	if err != nil || len(data) > protocol.MaxAttachmentBytes {
		return 1
	}
	n, err := countPages(context.Background(), data)
	if err != nil || n < 1 {
		return 1
	}
	if _, err := fmt.Fprintln(out, n); err != nil {
		return 1
	}
	return 0
}

// limitChildMemory sets the child's committed-memory limit, hard and soft,
// so it cannot be raised again. A race build (the tests under -race) cannot
// run under any such limit, since the race detector maps terabytes of
// shadow memory, so there it sets only the GC's soft limit.
func limitChildMemory() error {
	debug.SetMemoryLimit(childSoftLimit)
	if raceBuild {
		return nil
	}
	return syscall.Setrlimit(syscall.RLIMIT_DATA, &syscall.Rlimit{Cur: childDataLimit, Max: childDataLimit})
}

// countPages counts pages in this process, inside recover(): pdfcpu
// re-panics anything that is not its own.
func countPages(ctx context.Context, data []byte) (n int, err error) {
	defer recoverInto(&err)
	return pdfcpuPageCount(ctx, data)
}

// pdfcpuPageCount is pdfcpu's page count over data, with pdfConfig. It
// reads the file's structure and decodes only cross-reference and object
// streams, not images or page content.
func pdfcpuPageCount(ctx context.Context, data []byte) (int, error) {
	return api.PageCount(ctx, bytes.NewReader(data), pdfConfig())
}

// pdfConfig is pdfcpu's configuration for counting pages. The stateless
// configuration reads and writes no config directory (the default installs
// one under the user's config dir, and Core's filesystem is read-only), and
// Offline keeps validation from fetching anything. The limits fit one
// 20 MiB attachment; a decoded object stream over 4 MiB is refused.
func pdfConfig() *model.Configuration {
	c := model.NewStatelessConfiguration()
	c.ValidationMode = model.ValidationRelaxed
	c.Offline = true
	c.Limits = model.DefaultResourceLimits()
	c.Limits.MaxInputBytes = protocol.MaxAttachmentBytes
	c.Limits.MaxStreamBytes = protocol.MaxAttachmentBytes
	c.Limits.MaxObjectBytes = 16 << 20
	c.Limits.MaxDecodeBytes = 4 << 20
	c.Limits.MaxObjectCount = 1_000_000
	c.Limits.MaxObjectStreamCount = 100_000
	c.Limits.MaxXRefEntries = 1_000_000
	return c
}
