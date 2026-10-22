package memory

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/ChristopherJMiller/luma/core/internal/auth"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/store"
)

// maxRequestBody bounds a JSON request body, as on the session routes.
const maxRequestBody = 1 << 20

// Routes registers C1's memory routes, each behind requirePerson (the BFF's
// RequirePerson): the profile editor's GET, PUT and DELETE
// /v1/memory/profile, and POST /v1/memory/writes/{id}/accept and /reject.
// Like the session routes, it leaves UnsafeRequestGuard to the mux's
// wrapper (Task 15). A handler's error body is ErrorInfo (C1, C3); the auth
// layer's 401/403/415 stay status-only (S41).
func (m *Memory) Routes(mux *http.ServeMux, requirePerson func(http.Handler) http.Handler) {
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
	route("GET /v1/memory/profile", m.handleGet)
	route("PUT /v1/memory/profile", m.handlePut)
	route("DELETE /v1/memory/profile", m.handleDelete)
	route("POST /v1/memory/writes/{id}/accept", m.handleDecide(true))
	route("POST /v1/memory/writes/{id}/reject", m.handleDecide(false))
}

func (m *Memory) handleGet(w http.ResponseWriter, r *http.Request, p auth.Person) {
	prof, err := m.st.Profile(r.Context(), p.ID)
	if err != nil {
		internalError(w, r, err)
		return
	}
	m.respondProfile(w, r, prof)
}

// handlePut saves the editor's profile if its version is still the one the
// editor read (409 otherwise). Both fields are required: a body without
// content must not clear the profile. Over MaxProfileBytes is 413.
func (m *Memory) handlePut(w http.ResponseWriter, r *http.Request, p auth.Person) {
	var body struct {
		Content *string `json:"content"`
		Version *int    `json:"version"`
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		writeErrorInfo(w, r, http.StatusRequestEntityTooLarge, protocol.CodeRequestTooLarge)
		return
	case err != nil:
		writeErrorInfo(w, r, http.StatusBadRequest, protocol.CodeInvalidRequest)
		return
	}
	if json.Unmarshal(b, &body) != nil || body.Content == nil || body.Version == nil {
		writeErrorInfo(w, r, http.StatusBadRequest, protocol.CodeInvalidRequest)
		return
	}
	if len(*body.Content) > protocol.MaxProfileBytes {
		writeErrorInfo(w, r, http.StatusRequestEntityTooLarge, protocol.CodeRequestTooLarge)
		return
	}
	var saved store.Profile
	err = m.st.InMemoryTx(r.Context(), p.ID, func(tx *store.MemoryTx) error {
		var err error
		saved, err = tx.SetProfile(r.Context(), *body.Content, *body.Version)
		return err
	})
	switch {
	case errors.Is(err, store.ErrProfileConflict):
		writeErrorInfo(w, r, http.StatusConflict, protocol.CodeInvalidRequest)
		return
	case err != nil:
		internalError(w, r, err)
		return
	}
	slog.Info("memory: profile saved", "person", p.ID, "version", saved.Version)
	m.respondProfile(w, r, saved)
}

// handleDelete empties the profile (the row stays, its version moves on, so
// an editor still holding the old version is refused). Pending writes stay
// for the person to decide.
func (m *Memory) handleDelete(w http.ResponseWriter, r *http.Request, p auth.Person) {
	var saved store.Profile
	err := m.st.InMemoryTx(r.Context(), p.ID, func(tx *store.MemoryTx) error {
		cur, err := tx.Profile(r.Context())
		if err != nil || cur.Version == 0 {
			saved = cur
			return err
		}
		saved, err = tx.SetProfile(r.Context(), "", cur.Version)
		return err
	})
	if err != nil {
		internalError(w, r, err)
		return
	}
	slog.Info("memory: profile cleared", "person", p.ID, "version", saved.Version)
	m.respondProfile(w, r, saved)
}

func (m *Memory) handleDecide(accept bool) func(http.ResponseWriter, *http.Request, auth.Person) {
	return func(w http.ResponseWriter, r *http.Request, p auth.Person) {
		mw, err := m.Decide(r.Context(), p, r.PathValue("id"), accept)
		switch {
		case errors.Is(err, ErrNotFound):
			writeErrorInfo(w, r, http.StatusNotFound, protocol.CodeNotFound)
		case errors.Is(err, ErrFull):
			writeErrorInfo(w, r, http.StatusConflict, protocol.CodeMemoryFull)
		case err != nil:
			internalError(w, r, err)
		default:
			writeJSON(w, http.StatusOK, mw)
		}
	}
}

// respondProfile answers the C1 Profile: prof with the person's pending
// writes, oldest first.
func (m *Memory) respondProfile(w http.ResponseWriter, r *http.Request, prof store.Profile) {
	ws, err := m.st.PendingMemoryWrites(r.Context(), prof.PersonID)
	if err != nil {
		internalError(w, r, err)
		return
	}
	out := protocol.Profile{Content: prof.Content, Version: prof.Version, UpdatedAt: prof.UpdatedAt,
		Pending: make([]protocol.MemoryWrite, 0, len(ws))}
	for _, mw := range ws {
		out.Pending = append(out.Pending, mw.Protocol())
	}
	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		slog.Error("memory: encoding a response failed", "err", err)
		status = http.StatusInternalServerError
		b, _ = json.Marshal(protocol.ErrorInfo{Code: protocol.CodeUpstreamError, Message: protocol.ErrorCopy[protocol.CodeUpstreamError]})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func writeErrorInfo(w http.ResponseWriter, r *http.Request, status int, code protocol.ErrorCode) {
	slog.Debug("memory: request refused", "route", r.Pattern, "status", status, "code", code)
	writeJSON(w, status, protocol.ErrorInfo{Code: code, Message: protocol.ErrorCopy[code]})
}

func internalError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() == nil { // else the client left
		slog.Error("memory: request failed", "route", r.Pattern, "err", err)
	}
	writeErrorInfo(w, r, http.StatusInternalServerError, protocol.CodeUpstreamError)
}
