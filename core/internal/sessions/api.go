package sessions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ChristopherJMiller/luma/core/internal/auth"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/store"
)

const (
	// maxRequestBody bounds a JSON request body; attachments have their own
	// route (Task 12).
	maxRequestBody = 1 << 20
	// listPageSize is the number of roots in one page of GET /v1/sessions.
	listPageSize = 50
)

// Routes registers C1's session routes, each behind requirePerson (the BFF's
// RequirePerson). Task 11 adds /fork and /tree here (S12). It does not apply
// UnsafeRequestGuard: Task 15 wraps the whole public mux in it, outside every
// RequirePerson (S17). A handler's error body is ErrorInfo (C1, C3); the
// auth layer's 401/403/415 stay status-only (S41).
func (s *Service) Routes(mux *http.ServeMux, requirePerson func(http.Handler) http.Handler) {
	route := func(pattern string, h personHandler) { mux.Handle(pattern, requirePerson(h)) }
	route("POST /v1/sessions", s.handleCreate)
	route("GET /v1/sessions", s.handleList)
	route("GET /v1/sessions/{id}", s.handleGet)
	route("PATCH /v1/sessions/{id}", s.handleRename)
	route("DELETE /v1/sessions/{id}", s.handleDelete)
	route("POST /v1/sessions/{id}/archive", s.handleArchive(true))
	route("POST /v1/sessions/{id}/unarchive", s.handleArchive(false))
	route("POST /v1/sessions/{id}/events", s.handleEvent)
	route("GET /v1/sessions/{id}/stream", s.handleStream)
}

// personHandler is a handler for a request RequirePerson admitted.
type personHandler func(w http.ResponseWriter, r *http.Request, p auth.Person)

func (h personHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PersonFrom(r.Context())
	if !ok { // not behind RequirePerson: refuse as the auth layer would
		unauthenticated(w)
		return
	}
	h(w, r, p)
}

func (s *Service) handleCreate(w http.ResponseWriter, r *http.Request, p auth.Person) {
	var req protocol.CreateSession
	if !readJSON(w, r, &req) {
		return
	}
	if req.Agent == "" {
		writeError(w, r, fmt.Errorf("%w: agent is required", ErrInvalid))
		return
	}
	ses, err := s.Create(r.Context(), p, req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ses)
}

// handleList is GET /v1/sessions?roots=true&archived=false&after=<cursor>:
// the person's roots, newest updated_at first, a page at a time. archived=true
// lists archived roots too (each carries archived_at). Only roots are listed
// in phase 1, so roots, if given, must be true.
func (s *Service) handleList(w http.ResponseWriter, r *http.Request, p auth.Person) {
	q := r.URL.Query()
	if v := q.Get("roots"); v != "" && v != "true" {
		writeError(w, r, fmt.Errorf("%w: only roots are listed", ErrInvalid))
		return
	}
	var archived bool
	switch q.Get("archived") {
	case "", "false":
	case "true":
		archived = true
	default:
		writeError(w, r, fmt.Errorf("%w: archived must be true or false", ErrInvalid))
		return
	}
	var after *store.RootCursor
	if c := q.Get("after"); c != "" {
		cur, err := decodeCursor(c)
		if err != nil {
			writeError(w, r, err)
			return
		}
		after = &cur
	}
	rows, err := s.st.ListRoots(r.Context(), p.ID, archived, after, listPageSize+1)
	if err != nil {
		writeError(w, r, err)
		return
	}
	var out protocol.SessionList
	if len(rows) > listPageSize {
		rows = rows[:listPageSize]
		out.Next = encodeCursor(rows[len(rows)-1])
	}
	out.Sessions = make([]protocol.Session, 0, len(rows))
	for _, row := range rows {
		out.Sessions = append(out.Sessions, view(row))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Service) handleGet(w http.ResponseWriter, r *http.Request, p auth.Person) {
	row, err := s.owned(r.Context(), p, r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view(row))
}

// handleRename is PATCH {title}: the title is normalised as a first message's
// is (whitespace collapsed, at most 60 runes; P7) and must not be empty.
func (s *Service) handleRename(w http.ResponseWriter, r *http.Request, p auth.Person) {
	var patch protocol.SessionPatch
	if !readJSON(w, r, &patch) {
		return
	}
	title := titleFrom(patch.Title)
	if title == "" {
		writeError(w, r, fmt.Errorf("%w: the title is empty", ErrInvalid))
		return
	}
	s.respondUpdated(w, r, p, func(ctx context.Context, id string) error { return s.st.SetSessionTitle(ctx, id, title) })
}

func (s *Service) handleArchive(archived bool) personHandler {
	return func(w http.ResponseWriter, r *http.Request, p auth.Person) {
		s.respondUpdated(w, r, p, func(ctx context.Context, id string) error { return s.st.ArchiveSession(ctx, id, archived) })
	}
}

// respondUpdated applies a row update through the session's actor, which
// also sets the title from a first message, so a rename and a first message
// cannot overwrite each other, and answers with the updated Session.
func (s *Service) respondUpdated(w http.ResponseWriter, r *http.Request, p auth.Person, update func(ctx context.Context, id string) error) {
	var row store.Session
	err := s.call(r.Context(), r.PathValue("id"), func(ctx context.Context, a *actor) error {
		if err := a.reload(ctx); err != nil {
			return err
		}
		if a.sess.PersonID != p.ID {
			return ErrNotFound
		}
		if err := update(ctx, a.id); err != nil {
			return err
		}
		if err := a.reload(ctx); err != nil {
			return err
		}
		row = *a.sess
		return nil
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view(row))
}

// handleDelete deletes the tree of the named session, whichever session of
// the tree it is (P3).
func (s *Service) handleDelete(w http.ResponseWriter, r *http.Request, p auth.Person) {
	row, err := s.owned(r.Context(), p, r.PathValue("id"))
	if err == nil {
		err = s.DeleteTree(r.Context(), p.ID, row.RootID)
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleEvent is POST /events: a ClientEvent → SendResult.
func (s *Service) handleEvent(w http.ResponseWriter, r *http.Request, p auth.Person) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	ev, err := protocol.DecodeClientEvent(body)
	if err != nil {
		writeError(w, r, fmt.Errorf("%w: %w", ErrInvalid, err))
		return
	}
	m, err := s.Send(r.Context(), p, r.PathValue("id"), ev)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protocol.SendResult{Message: m})
}

// owned reads a session the person owns; anyone else's is ErrNotFound.
func (s *Service) owned(ctx context.Context, p auth.Person, sessionID string) (store.Session, error) {
	row, err := s.st.Session(ctx, sessionID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && row.PersonID != p.ID) {
		return store.Session{}, ErrNotFound
	}
	return row, err
}

// ---- tree delete (S39) ----

// DeleteTree deletes a person's conversation tree: the one tree-delete path,
// used by DELETE and by the Task 15 janitor (S39). In order:
//  1. it marks the root deleting, so Send (and Task 11's Fork) on any session
//     of the tree answers 404 from here on, and waits until every send of the
//     tree already in an actor has gone out, so no host opens after the purge;
//  2. Conductor.PurgeTree(root, the owner's person ref), which returns on the
//     conductor's tree.purged ack, link loss, or after 30 s (Task 10). A
//     failure or timeout is logged and the delete goes on: the janitor's
//     orphan sweep catches a late mirror write;
//  3. store.DeleteTree.
//
// rootID must be a root the person owns, otherwise ErrNotFound. The delete
// runs to the end even if ctx is cancelled (a client that leaves mid-delete
// must not leave a purged tree's rows behind).
func (s *Service) DeleteTree(ctx context.Context, personID, rootID string) error {
	ctx = context.WithoutCancel(ctx)
	root, err := s.st.Session(ctx, rootID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (root.PersonID != personID || root.RootID != root.ID)) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	ref, err := s.st.PersonRef(ctx, personID)
	if err != nil {
		return err
	}
	s.deletions.mark(rootID)
	defer s.deletions.unmark(rootID)
	tree, err := s.st.TreeSessions(ctx, rootID)
	if err != nil {
		return err
	}
	for _, ses := range tree { // a barrier: each actor finishes the work it already has
		if err := s.call(ctx, ses.ID, func(context.Context, *actor) error { return nil }); err != nil {
			return err
		}
	}
	started := s.clock()
	if err := s.cond.PurgeTree(ctx, rootID, ref); err != nil {
		slog.Warn("sessions: tree purge not acknowledged; deleting the rows anyway", "root", rootID, "err", err)
	}
	if err := s.st.DeleteTree(ctx, personID, rootID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound // a concurrent delete got there first
		}
		return err
	}
	slog.Info("sessions: tree deleted", "root", rootID, "sessions", len(tree), "purge_seconds", s.clock().Sub(started).Seconds())
	return nil
}

// deletions counts the deletes in progress per root.
type deletions struct {
	mu    sync.Mutex
	roots map[string]int
}

func (d *deletions) mark(rootID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.roots == nil {
		d.roots = map[string]int{}
	}
	d.roots[rootID]++
}

func (d *deletions) unmark(rootID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.roots[rootID]--; d.roots[rootID] <= 0 {
		delete(d.roots, rootID)
	}
}

// has reports whether the tree of rootID is being deleted.
func (d *deletions) has(rootID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.roots[rootID] > 0
}

// ---- list cursor ----

// The cursor of GET /v1/sessions is the last listed root's updated_at (Unix
// microseconds, Postgres's precision) and id, base64url: opaque to clients.
func encodeCursor(s store.Session) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(s.UpdatedAt.UnixMicro(), 10) + "." + s.ID))
}

func decodeCursor(c string) (store.RootCursor, error) {
	bad := fmt.Errorf("%w: after is not a cursor this server issued", ErrInvalid)
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return store.RootCursor{}, bad
	}
	micros, id, ok := strings.Cut(string(raw), ".")
	us, err := strconv.ParseInt(micros, 10, 64)
	if !ok || err != nil || !strings.HasPrefix(id, "ses_") {
		return store.RootCursor{}, bad
	}
	return store.RootCursor{UpdatedAt: time.UnixMicro(us).UTC(), ID: id}, nil
}

// ---- bodies and errors ----

// readBody reads a request body of at most maxRequestBody bytes; a larger one
// is answered 413.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErrorInfo(w, r, http.StatusRequestEntityTooLarge, protocol.CodeUpstreamError)
			return nil, false
		}
		writeError(w, r, fmt.Errorf("%w: unreadable body", ErrInvalid))
		return nil, false
	}
	return b, true
}

// readJSON decodes a JSON body into v. Unknown keys are ignored (S37), so
// a client-sent source is dropped, never read (S1).
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	b, ok := readBody(w, r)
	if !ok {
		return false
	}
	if err := json.Unmarshal(b, v); err != nil {
		writeError(w, r, fmt.Errorf("%w: malformed JSON", ErrInvalid))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		slog.Error("sessions: encoding a response failed", "err", err)
		status, b = http.StatusInternalServerError, mustErrorInfo(protocol.CodeUpstreamError)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// writeError answers a handler error with its status and an ErrorInfo body
// (C1, C3). C3 has no code for a malformed request or a missing session, so
// those carry upstream_error, C3's code for anything else; clients act on the
// status. A Gate refusal carries the Gate's ErrorInfo, already rendered.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	var refused *RefusedError
	switch {
	case errors.As(err, &refused):
		writeJSON(w, refusalStatus(refused.Info.Code), refused.Info)
		return
	case errors.Is(err, ErrBusy):
		writeErrorInfo(w, r, http.StatusConflict, protocol.CodeBusy)
		return
	case errors.Is(err, ErrNotFound):
		writeErrorInfo(w, r, http.StatusNotFound, protocol.CodeUpstreamError)
		return
	case errors.Is(err, ErrInvalid):
		writeErrorInfo(w, r, http.StatusBadRequest, protocol.CodeUpstreamError)
		return
	}
	if r.Context().Err() == nil { // else the client left; the work it started runs on
		slog.Error("sessions: request failed", "route", r.Pattern, "err", err)
	}
	writeErrorInfo(w, r, http.StatusInternalServerError, protocol.CodeUpstreamError)
}

// refusalStatus is the HTTP status of a refusal carrying a C3 code:
// maintenance 503 and budget_exhausted 402 (S1), busy 409, and 400 for a cap
// on the message's attachments (Task 12's Bind). No request is refused with
// the other codes, so one that is is a bug: 500.
func refusalStatus(code protocol.ErrorCode) int {
	switch code {
	case protocol.CodeMaintenance:
		return http.StatusServiceUnavailable
	case protocol.CodeBudgetExhausted:
		return http.StatusPaymentRequired
	case protocol.CodeBusy:
		return http.StatusConflict
	case protocol.CodeAttachmentTooLarge, protocol.CodeImageTooLarge, protocol.CodePDFTooLong,
		protocol.CodeConversationAttachmentsFull, protocol.CodeUnsupportedAttachment:
		return http.StatusBadRequest
	case protocol.CodeRateLimited, protocol.CodeOverloaded, protocol.CodeConductorRestarted, protocol.CodeCapacity,
		protocol.CodeUpstreamError: // turn errors: they reach the client as error events
	case protocol.CodeBudgetWarning, protocol.CodeNotInvited: // a banner, and the sign-in page
	case protocol.CodeMemoryFull, protocol.CodeMemoryDisabled, protocol.CodeMemoryPendingLimit,
		protocol.CodeForgetTooShort: // tool result text
	}
	return http.StatusInternalServerError
}

func writeErrorInfo(w http.ResponseWriter, r *http.Request, status int, code protocol.ErrorCode) {
	if status < http.StatusInternalServerError {
		slog.Debug("sessions: request refused", "route", r.Pattern, "status", status, "code", code)
	}
	writeJSON(w, status, protocol.ErrorInfo{Code: code, Message: protocol.ErrorCopy[code]})
}

func mustErrorInfo(code protocol.ErrorCode) []byte {
	b, _ := json.Marshal(protocol.ErrorInfo{Code: code, Message: protocol.ErrorCopy[code]})
	return b
}

// unauthenticated is the auth layer's status-only 401 (S41).
func unauthenticated(w http.ResponseWriter) {
	http.Error(w, "unauthenticated", http.StatusUnauthorized)
}
