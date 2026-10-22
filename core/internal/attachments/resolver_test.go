package attachments

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/ChristopherJMiller/luma/core/internal/agents"
	"github.com/ChristopherJMiller/luma/core/internal/auth"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/sessions"
	"github.com/ChristopherJMiller/luma/core/internal/store"
	"github.com/ChristopherJMiller/luma/core/internal/store/storetest"
)

const mib = 1 << 20

var (
	bg     = context.Background()
	chris  = auth.Person{ID: "pers-chris", DisplayName: "Synthetic Chris", Tier: "admin"}
	erin   = auth.Person{ID: "pers-erin", DisplayName: "Synthetic Erin", Tier: "family"}
	tiers  = fakeTiers{"fast": 20, "smart": 100, "deep": 100} // the plan's pdf_max_pages
	treeCp = "This conversation already holds 20 MB of files. Start a new chat to add more."
)

// fakeTiers stands in for agents.Live: tier → pdf_max_pages.
type fakeTiers map[string]int

func (f fakeTiers) Models() agents.Models {
	m := agents.Models{Version: 1, HelperModel: "claude-haiku-5-5", Tiers: map[string]agents.Tier{}}
	for name, pages := range f {
		m.Tiers[name] = agents.Tier{PDFMaxPages: pages}
	}
	return m
}

type resolverHarness struct {
	t  *testing.T
	st *store.Store
	r  *Resolver
}

func newResolverHarness(t *testing.T) *resolverHarness {
	st := storetest.New(t)
	return &resolverHarness{t: t, st: st, r: NewResolver(st, tiers)}
}

// tree creates a root session of p's.
func (h *resolverHarness) tree(p auth.Person, id string) string {
	h.t.Helper()
	if _, err := h.st.CreateSession(bg, store.Session{
		ID: id, PersonID: p.ID, AgentID: "luma", AgentVersion: "abc123",
		Tier: "fast", Model: "claude-haiku-5-5", HelperModel: "claude-haiku-5-5",
		Effort: "low", CacheTTL: "1h", APIWorkspace: "interactive", Source: "web",
	}); err != nil {
		h.t.Fatal(err)
	}
	return id
}

// upload stores an unbound attachment as the upload route would.
func (h *resolverHarness) upload(p auth.Person, id, mediaType string, data []byte, pages int) string {
	h.t.Helper()
	a := store.Attachment{ID: id, PersonID: p.ID, MediaType: mediaType, Filename: id, SizeBytes: int64(len(data)), SHA256: "00", Data: data}
	if pages > 0 {
		a.Pages = &pages
	}
	if err := h.st.InsertAttachment(bg, a); err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *resolverHarness) rootOf(p auth.Person, id string) string {
	h.t.Helper()
	a, err := h.st.AttachmentMeta(bg, p.ID, id)
	if err != nil {
		h.t.Fatal(err)
	}
	if a.RootID == nil {
		return ""
	}
	return *a.RootID
}

func refusal(t *testing.T, err error) protocol.ErrorInfo {
	t.Helper()
	var r *sessions.RefusedError
	if !errors.As(err, &r) {
		t.Fatalf("err = %v, want a *sessions.RefusedError", err)
	}
	return r.Info
}

func pdfOf(size int) []byte { return append([]byte("%PDF-"), make([]byte, size-5)...) }

// A conversation's attachments are resent with every later request, so a
// tree holds at most 20 MiB of them, and the refusal says so plainly.
func TestCumulativeTreeCap(t *testing.T) {
	h := newResolverHarness(t)
	root := h.tree(chris, "ses_root")
	a := h.upload(chris, "att_12", "application/pdf", pdfOf(12*mib), 1)
	b := h.upload(chris, "att_8", "application/pdf", pdfOf(8*mib), 1)
	c := h.upload(chris, "att_c", "text/plain", []byte("x"), 0)

	if _, err := h.r.Bind(bg, chris, root, []string{a}, "fast"); err != nil {
		t.Fatal(err)
	}
	// A later message takes the tree to exactly 20 MiB: allowed.
	if blocks, err := h.r.Bind(bg, chris, root, []string{b}, "fast"); err != nil || len(blocks) != 1 {
		t.Fatalf("binding up to exactly 20 MiB = %d blocks, %v", len(blocks), err)
	}
	// One more byte is refused with the C3 copy, and binds nothing.
	_, err := h.r.Bind(bg, chris, root, []string{c}, "fast")
	if info := refusal(t, err); info.Code != protocol.CodeConversationAttachmentsFull || info.Message != treeCp {
		t.Fatalf("one byte over = %+v, want conversation_attachments_full %q", info, treeCp)
	}
	if got := h.rootOf(chris, c); got != "" {
		t.Fatalf("the refused attachment was bound to %s", got)
	}
	// The same upload goes into a new chat.
	fresh := h.tree(chris, "ses_fresh")
	if _, err := h.r.Bind(bg, chris, fresh, []string{c}, "fast"); err != nil {
		t.Fatalf("the refused attachment in a new chat: %v", err)
	}
	// Sending the tree's own attachments again (an edit of their message
	// forks the tree) counts nothing twice.
	if _, err := h.r.Bind(bg, chris, root, []string{a, b}, "fast"); err != nil {
		t.Fatalf("re-sending the tree's attachments: %v", err)
	}
	// Two files that fit alone but not together, in one message.
	other := h.tree(chris, "ses_other")
	x := h.upload(chris, "att_x", "application/pdf", pdfOf(11*mib), 1)
	y := h.upload(chris, "att_y", "application/pdf", pdfOf(11*mib), 1)
	_, err = h.r.Bind(bg, chris, other, []string{x, y}, "fast")
	if info := refusal(t, err); info.Code != protocol.CodeConversationAttachmentsFull {
		t.Fatalf("22 MiB in one message = %+v", info)
	}
	if h.rootOf(chris, x) != "" || h.rootOf(chris, y) != "" {
		t.Fatal("part of a refused message was bound")
	}
}

func TestBindRejectsOtherPersonsAttachment(t *testing.T) {
	h := newResolverHarness(t)
	root := h.tree(chris, "ses_root")
	theirs := h.upload(erin, "att_erin", "text/plain", []byte("erin's notes"), 0)
	mine := h.upload(chris, "att_mine", "text/plain", []byte("mine"), 0)
	if _, err := h.r.Bind(bg, chris, root, []string{mine, theirs}, "fast"); !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("another person's attachment: %v, want sessions.ErrNotFound (404)", err)
	}
	if h.rootOf(erin, theirs) != "" || h.rootOf(chris, mine) != "" {
		t.Fatal("a refused message bound something")
	}
	if _, err := h.r.Bind(bg, chris, root, []string{"att_missing"}, "fast"); !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("a missing attachment: %v, want sessions.ErrNotFound", err)
	}
}

func TestBindRejectsAttachmentOfAnotherTree(t *testing.T) {
	h := newResolverHarness(t)
	first, second := h.tree(chris, "ses_first"), h.tree(chris, "ses_second")
	id := h.upload(chris, "att_1", "text/plain", []byte("notes"), 0)
	if _, err := h.r.Bind(bg, chris, first, []string{id}, "fast"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.r.Bind(bg, chris, second, []string{id}, "fast"); !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("another tree's attachment: %v, want sessions.ErrNotFound", err)
	}
	if got := h.rootOf(chris, id); got != first {
		t.Fatalf("the attachment moved to %q", got)
	}
}

func TestBindLimitsPerMessage(t *testing.T) {
	h := newResolverHarness(t)
	root := h.tree(chris, "ses_root")
	var ids []string
	for i := range 11 {
		ids = append(ids, h.upload(chris, "att_"+strconv.Itoa(i), "text/plain", []byte("x"), 0))
	}
	if _, err := h.r.Bind(bg, chris, root, ids, "fast"); !errors.Is(err, sessions.ErrInvalid) {
		t.Fatalf("11 attachments: %v, want sessions.ErrInvalid (400)", err)
	}
	if _, err := h.r.Bind(bg, chris, root, []string{ids[0], ids[0]}, "fast"); !errors.Is(err, sessions.ErrInvalid) {
		t.Fatalf("one attachment twice: %v, want sessions.ErrInvalid", err)
	}
	if blocks, err := h.r.Bind(bg, chris, root, ids[:10], "fast"); err != nil || len(blocks) != 10 {
		t.Fatalf("10 attachments = %d blocks, %v", len(blocks), err)
	}
	if _, err := h.r.Bind(bg, chris, root, ids[10:], "no-such-tier"); err == nil || codeOf(err) != "" {
		t.Fatalf("an unknown tier = %v, want a server error", err)
	}
}

func TestBindReturnsBlocksInOrder(t *testing.T) {
	h := newResolverHarness(t)
	root := h.tree(chris, "ses_root")
	txt := h.upload(chris, "att_t", "text/plain", []byte("shopping"), 0)
	img := h.upload(chris, "att_i", "image/jpeg", []byte{1, 2, 3}, 0)
	blocks, err := h.r.Bind(bg, chris, root, []string{txt, img}, "smart")
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 2 || blocks[0].Type != "text" || blocks[0].Text != "<luma-attachment name=\"att_t\">\nshopping\n</luma-attachment>" ||
		blocks[1].Type != "image" || blocks[1].Source.MediaType != "image/jpeg" || blocks[1].Source.Data != "AQID" {
		t.Fatalf("blocks = %+v", blocks)
	}
}
