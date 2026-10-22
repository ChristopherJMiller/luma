package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ChristopherJMiller/luma/core/internal/store"
)

const (
	// DefaultAbsoluteTTL is how long a web session lives however active it is.
	DefaultAbsoluteTTL = 30 * 24 * time.Hour
	// DefaultIdleTTL ends a web session that has seen no request for this long.
	DefaultIdleTTL = 14 * 24 * time.Hour
	// touchEvery bounds how often last_seen_at is written.
	touchEvery = 5 * time.Minute
	// sessionIDBytes is the size of a session id before base64url.
	sessionIDBytes = 32
)

type ctxKey int

const (
	personKey ctxKey = iota
	webSessionKey
)

// PersonFrom returns the person RequirePerson authenticated.
func PersonFrom(ctx context.Context) (Person, bool) {
	p, ok := ctx.Value(personKey).(Person)
	return p, ok
}

// WebSessionIDHashFrom returns the id hash of the web session RequirePerson
// authenticated: the value OnSessionEnded reports, so a WebSocket (Task 9b)
// can tell when its own session ends.
func WebSessionIDHashFrom(ctx context.Context) ([]byte, bool) {
	h, ok := ctx.Value(webSessionKey).([]byte)
	return h, ok
}

// newSessionID returns a cookie value (32 random bytes, base64url) and the
// sha256 the database keys it by. The value itself is never stored.
func newSessionID() (value string, hash []byte, err error) {
	raw := make([]byte, sessionIDBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(raw), sum[:], nil
}

// sessionHash returns the database key of a cookie value, or false if the
// value is not a session id at all.
func sessionHash(value string) ([]byte, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) != sessionIDBytes {
		return nil, false
	}
	sum := sha256.Sum256(raw)
	return sum[:], true
}

// presentedSession returns the hash of the request's session cookie, if it
// has a well-formed one.
func (b *BFF) presentedSession(r *http.Request) ([]byte, bool) {
	c, err := r.Cookie(b.sessionCookie)
	if err != nil {
		return nil, false
	}
	return sessionHash(c.Value)
}

// OnSessionEnded registers f to be called after a web session ends: logout,
// idle or absolute expiry, a new login replacing it, or a registry change
// that no longer admits it. Task 9b closes that session's WebSockets. f runs
// on the goroutine that ended the session and may be called more than once
// for one session (a sweep racing a logout), so it must be idempotent.
func (b *BFF) OnSessionEnded(f func(webSessionIDHash []byte, personID string)) {
	b.endedMu.Lock()
	defer b.endedMu.Unlock()
	b.ended = append(b.ended, f)
}

// endSession deletes a web session's row and, once it is gone, tells every
// OnSessionEnded subscriber.
func (b *BFF) endSession(ctx context.Context, hash []byte, personID, reason string) error {
	if err := b.st.DeleteWebSession(ctx, hash); err != nil {
		slog.Error("auth: ending web session failed", "person", personID, "reason", reason, "err", err)
		return err
	}
	slog.Info("auth: web session ended", "person", personID, "reason", reason)
	b.endedMu.Lock()
	subs := slices.Clone(b.ended)
	b.endedMu.Unlock()
	for _, f := range subs {
		f(slices.Clone(hash), personID)
	}
	return nil
}

// isWebSocket reports a WebSocket handshake.
func isWebSocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// RequirePerson admits a request with a live web session whose connector
// and address the current registry still maps to its person, and puts that
// person (as the registry now describes them) and the session's id hash in
// the context. Otherwise it answers 401.
//
// Before the cookie it checks the Origin: a request carrying a foreign
// Origin (a cross-origin fetch, a cross-site WebSocket handshake), or a
// WebSocket handshake carrying none, is 403. UnsafeRequestGuard has already
// done this for unsafe methods; this covers the GETs, the WebSocket
// handshake above all.
func (b *BFF) RequirePerson(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origins := r.Header.Values("Origin")
		if (len(origins) > 0 || isWebSocket(r)) && !b.origins.allows(origins) {
			http.Error(w, errForeignOrigin.Error(), http.StatusForbidden)
			return
		}
		hash, ok := b.presentedSession(r)
		if !ok {
			unauthorized(w)
			return
		}
		ctx := r.Context()
		now := b.now()
		ws, err := b.st.WebSession(ctx, hash, now)
		if errors.Is(err, store.ErrNotFound) {
			unauthorized(w)
			return
		}
		if err != nil {
			slog.Error("auth: reading web session failed", "err", err)
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		p, live := b.live(ws, now)
		if !live {
			_ = b.endSession(ctx, hash, ws.PersonID, b.endReason(ws, now))
			unauthorized(w)
			return
		}
		if now.Sub(ws.LastSeenAt) >= touchEvery {
			if err := b.st.TouchWebSession(ctx, hash, now); err != nil {
				slog.Warn("auth: recording web session activity failed", "person", p.ID, "err", err)
			}
		}
		ctx = context.WithValue(ctx, personKey, p)
		ctx = context.WithValue(ctx, webSessionKey, hash)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func unauthorized(w http.ResponseWriter) {
	http.Error(w, "unauthenticated", http.StatusUnauthorized)
}

// live reports whether a web session is still good at now: unexpired, used
// within IdleTTL, and admitted by the current registry.
func (b *BFF) live(ws store.WebSession, now time.Time) (Person, bool) {
	if !now.Before(ws.ExpiresAt) || now.Sub(ws.LastSeenAt) > b.idleTTL {
		return Person{}, false
	}
	return b.registry().admits(ws)
}

// endReason names why live refused a session, for the log.
func (b *BFF) endReason(ws store.WebSession, now time.Time) string {
	switch {
	case !now.Before(ws.ExpiresAt):
		return "expired"
	case now.Sub(ws.LastSeenAt) > b.idleTTL:
		return "idle"
	default:
		return "registry"
	}
}

// registry is the registry in force.
func (b *BFF) registry() *Registry { return b.reg.Load() }

// SetRegistry swaps in a new registry atomically and ends every web session
// it no longer admits: a removed person, or an address removed from a person
// (OnSessionEnded fires for each). Wire it as WatchRegistry(ctx, path,
// b.SetRegistry). A nil registry is ignored.
func (b *BFF) SetRegistry(r *Registry) {
	if r == nil {
		return
	}
	b.reg.Store(r)
	if _, err := b.Sweep(b.ctx); err != nil {
		slog.Error("auth: ending sessions after a registry change failed", "err", err)
	}
}

// Sweep ends every web session that is expired, idle past IdleTTL, or no
// longer admitted by the registry, firing OnSessionEnded for each, and
// returns how many it ended. SetRegistry runs it; the janitor (Task 15)
// should run it too, so an expired session's WebSockets close even if its
// browser never calls again.
func (b *BFF) Sweep(ctx context.Context) (int, error) {
	rows, err := b.st.ListWebSessions(ctx)
	if err != nil {
		return 0, err
	}
	now := b.now()
	n := 0
	var firstErr error
	for _, ws := range rows {
		if _, ok := b.live(ws, now); ok {
			continue
		}
		if err := b.endSession(ctx, ws.IDHash, ws.PersonID, b.endReason(ws, now)); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		n++
	}
	return n, firstErr
}
