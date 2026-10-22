package attachments

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"

	"github.com/ChristopherJMiller/luma/core/internal/protocol"
)

// pdfTimeout bounds one page count.
const pdfTimeout = 30 * time.Second

// pageCount counts a PDF's pages with pdfcpu, which reads the file's
// structure and decodes only its cross-reference and object streams (not
// images or page content). It runs in a decode slot, with pdfcpu's
// resource limits set for one 20 MiB attachment, and inside recover():
// pdfcpu re-panics anything that is not its own. Anything it cannot read,
// or a PDF without pages, is unsupported_attachment.
func pageCount(ctx context.Context, data []byte) (int, error) {
	release, err := acquireDecode(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, pdfTimeout)
	defer cancel()
	n, err := countPages(ctx, data)
	if err != nil || n < 1 {
		if cerr := ctx.Err(); cerr != nil && !errors.Is(cerr, context.DeadlineExceeded) {
			return 0, cerr // the client left
		}
		return 0, refused(protocol.CodeUnsupportedAttachment)
	}
	return n, nil
}

func countPages(ctx context.Context, data []byte) (n int, err error) {
	defer recoverInto(&err)
	return api.PageCount(ctx, bytes.NewReader(data), pdfConfig())
}

// pdfConfig is pdfcpu's configuration for counting pages. The stateless
// configuration reads and writes no config directory (the default installs
// one under the user's config dir, and Core's filesystem is read-only), and
// Offline keeps validation from fetching anything.
func pdfConfig() *model.Configuration {
	c := model.NewStatelessConfiguration()
	c.ValidationMode = model.ValidationRelaxed
	c.Offline = true
	c.Limits = model.DefaultResourceLimits()
	c.Limits.MaxInputBytes = protocol.MaxAttachmentBytes
	c.Limits.MaxStreamBytes = protocol.MaxAttachmentBytes
	c.Limits.MaxObjectBytes = 16 << 20
	c.Limits.MaxDecodeBytes = 32 << 20
	c.Limits.MaxObjectCount = 1_000_000
	c.Limits.MaxObjectStreamCount = 100_000
	c.Limits.MaxXRefEntries = 1_000_000
	return c
}
