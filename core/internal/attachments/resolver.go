package attachments

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ChristopherJMiller/luma/core/internal/agents"
	"github.com/ChristopherJMiller/luma/core/internal/auth"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/sessions"
	"github.com/ChristopherJMiller/luma/core/internal/store"
)

// maxPerMessage is the most attachments one user message may carry.
const maxPerMessage = 10

// Tiers is the live models.yaml; *agents.Live satisfies it.
type Tiers interface {
	Models() agents.Models
}

// Resolver binds a message's attachments to its conversation tree and
// renders them (sessions.AttachmentResolver).
type Resolver struct {
	st    *store.Store
	tiers Tiers
}

var _ sessions.AttachmentResolver = (*Resolver)(nil)

// NewResolver returns a Resolver over the store, reading each tier's
// pdf_max_pages from tiers.
func NewResolver(st *store.Store, tiers Tiers) *Resolver {
	return &Resolver{st: st, tiers: tiers}
}

// Bind checks a message's attachments and binds them to the tree rootID,
// then returns their content blocks, in order. Every id must be the
// person's and unbound, or already in this tree (a retried or edited
// message sends it again); otherwise nothing is bound and the error is
// sessions.ErrNotFound (404). More than 10, or an id twice, is
// sessions.ErrInvalid (400). A PDF past the tier's pdf_max_pages is
// pdf_too_long, and a tree past 20 MiB of attachments
// conversation_attachments_full: every later request resends them, so a
// fuller tree could never be answered again. Refusals are
// *sessions.RefusedError, so their C3 copy reaches the client; neither
// binds anything.
func (r *Resolver) Bind(ctx context.Context, p auth.Person, rootID string, ids []string, tier string) ([]protocol.ContentBlock, error) {
	if len(ids) > maxPerMessage {
		return nil, fmt.Errorf("%w: at most %d attachments per message", sessions.ErrInvalid, maxPerMessage)
	}
	if len(slices.Compact(slices.Sorted(slices.Values(ids)))) != len(ids) {
		return nil, fmt.Errorf("%w: an attachment is listed twice", sessions.ErrInvalid)
	}
	t, ok := r.tiers.Models().Tiers[tier]
	if !ok {
		return nil, fmt.Errorf("attachments: tier %q is not in models.yaml", tier)
	}
	for _, id := range ids {
		a, err := r.st.AttachmentMeta(ctx, p.ID, id)
		if errors.Is(err, store.ErrNotFound) || (err == nil && a.RootID != nil && *a.RootID != rootID) {
			return nil, sessions.ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		if a.Pages != nil && *a.Pages > t.PDFMaxPages {
			return nil, pdfTooLong(t.PDFMaxPages)
		}
	}
	switch err := r.st.BindAttachments(ctx, p.ID, rootID, ids, protocol.MaxTreeAttachmentBytes); {
	case errors.Is(err, store.ErrTreeFull):
		return nil, refused(protocol.CodeConversationAttachmentsFull)
	case errors.Is(err, store.ErrNotFound):
		return nil, sessions.ErrNotFound
	case err != nil:
		return nil, err
	}
	atts := make([]store.Attachment, 0, len(ids))
	for _, id := range ids {
		a, err := r.st.Attachment(ctx, id)
		if err != nil {
			return nil, err
		}
		atts = append(atts, a)
	}
	return Blocks(atts)
}

// pdfTooLong is pdf_too_long with the tier's maximum in its copy.
func pdfTooLong(maxPages int) error {
	msg := strings.ReplaceAll(protocol.ErrorCopy[protocol.CodePDFTooLong], "{max}", strconv.Itoa(maxPages))
	return &sessions.RefusedError{Info: protocol.ErrorInfo{Code: protocol.CodePDFTooLong, Message: msg}}
}
