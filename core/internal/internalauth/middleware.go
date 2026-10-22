package internalauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// maxCacheTTL bounds how long a positive review is trusted.
const maxCacheTTL = 60 * time.Second

const (
	// maxTokenBytes: projected tokens are ~1-2 KiB; anything bigger is junk.
	maxTokenBytes = 8 << 10
	// maxConcurrentReviews bounds TokenReviews in flight so a flood of
	// distinct garbage bearers cannot starve Core's own apiserver quota.
	maxConcurrentReviews = 8
)

// jwtShaped reports whether tok is three non-empty base64url segments.
// A cheap gate in front of the apiserver; the apiserver does the real check.
func jwtShaped(tok string) bool {
	if len(tok) > maxTokenBytes {
		return false
	}
	segs, n := 0, 0
	for i := 0; i <= len(tok); i++ {
		if i == len(tok) || tok[i] == '.' {
			if n == 0 {
				return false
			}
			segs++
			n = 0
			continue
		}
		c := tok[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '=' {
			return false
		}
		n++
	}
	return segs == 3
}

// ParsePrincipals parses "conductor=system:serviceaccount:ns:sa,evals=...".
// The result maps ServiceAccount username -> principal.
func ParsePrincipals(s string) (map[string]Principal, error) {
	out := map[string]Principal{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, user, ok := strings.Cut(part, "=")
		name, user = strings.TrimSpace(name), strings.TrimSpace(user)
		if !ok || name == "" || user == "" {
			return nil, fmt.Errorf("internalauth: bad principal entry %q (want name=username)", part)
		}
		switch Principal(name) {
		case PrincipalConductor, PrincipalEvals, PrincipalReconcile:
		default:
			return nil, fmt.Errorf("internalauth: unknown principal %q", name)
		}
		segs := strings.Split(user, ":")
		if len(segs) != 4 || segs[0] != "system" || segs[1] != "serviceaccount" || segs[2] == "" || segs[3] == "" {
			return nil, fmt.Errorf("internalauth: principal %q: %q is not a ServiceAccount username", name, user)
		}
		if _, dup := out[user]; dup {
			return nil, fmt.Errorf("internalauth: ServiceAccount %q mapped twice", user)
		}
		out[user] = Principal(name)
	}
	if len(out) == 0 {
		return nil, errors.New("internalauth: no principals configured")
	}
	return out, nil
}

type ctxKey struct{}
type ctxVal struct {
	p  Principal
	id Identity
}

// PrincipalFrom returns the authenticated caller stored by Middleware.
func PrincipalFrom(ctx context.Context) (Principal, Identity, bool) {
	v, ok := ctx.Value(ctxKey{}).(ctxVal)
	return v.p, v.id, ok
}

type cacheEntry struct {
	p       Principal
	id      Identity
	expires time.Time
}

// Middleware requires a bearer whose review is authenticated, carries
// audience, and whose username maps to one of the allowed principals.
// 401: no/invalid/wrong-audience token. 403: valid token, wrong caller.
// 503: the review could not be completed (fail closed).
func Middleware(r Reviewer, audience string, principals map[string]Principal, allow ...Principal) func(http.Handler) http.Handler {
	return middleware(r, audience, principals, allow, time.Now)
}

func middleware(r Reviewer, audience string, principals map[string]Principal, allow []Principal, now func() time.Time) func(http.Handler) http.Handler {
	var mu sync.Mutex
	sem := make(chan struct{}, maxConcurrentReviews)
	cache := map[[32]byte]cacheEntry{}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			// Bodies are status text only (S41/S43).
			deny := func(code int, reason string, detail ...string) {
				attrs := []any{"path", req.URL.Path, "status", code, "reason", reason}
				if len(detail) > 0 {
					attrs = append(attrs, "detail", detail[0])
				}
				slog.Warn("internal auth denied", attrs...)
				switch code {
				case http.StatusUnauthorized:
					w.Header().Set("WWW-Authenticate", `Bearer realm="luma-internal"`)
				case http.StatusServiceUnavailable:
					w.Header().Set("Retry-After", "2")
				}
				http.Error(w, http.StatusText(code), code)
			}
			hv := req.Header.Values("Authorization")
			if len(hv) != 1 {
				deny(http.StatusUnauthorized, "no_bearer")
				return
			}
			scheme, tok, ok := strings.Cut(hv[0], " ")
			tok = strings.TrimSpace(tok)
			if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" {
				deny(http.StatusUnauthorized, "no_bearer")
				return
			}
			if !jwtShaped(tok) {
				deny(http.StatusUnauthorized, "malformed_token")
				return
			}
			key := sha256.Sum256([]byte(tok))
			mu.Lock()
			ent, hit := cache[key]
			if hit && !now().Before(ent.expires) {
				delete(cache, key)
				hit = false
			}
			mu.Unlock()
			if !hit {
				select {
				case sem <- struct{}{}:
				default:
					deny(http.StatusServiceUnavailable, "review_saturated")
					return
				}
				id, err := r.Review(req.Context(), tok)
				<-sem
				switch {
				case errors.Is(err, ErrUnauthenticated):
					deny(http.StatusUnauthorized, "unauthenticated")
					return
				case err != nil:
					// Review's errors are fixed text and never carry the token.
					deny(http.StatusServiceUnavailable, "review_unavailable", err.Error())
					return
				}
				if !hasAudience(id.Audiences, audience) {
					deny(http.StatusUnauthorized, "wrong_audience")
					return
				}
				p, found := lookupPrincipal(principals, id.Username)
				if !found {
					deny(http.StatusForbidden, "unknown_service_account")
					return
				}
				ent = cacheEntry{p: p, id: id}
				ttl := maxCacheTTL
				if exp, ok := tokenExpiry(tok); ok {
					if left := exp.Sub(now()); left < ttl {
						ttl = left
					}
				}
				if ttl > 0 {
					ent.expires = now().Add(ttl)
					mu.Lock()
					if len(cache) > 256 {
						for k, e := range cache {
							if !now().Before(e.expires) {
								delete(cache, k)
							}
						}
					}
					cache[key] = ent
					mu.Unlock()
				}
			}
			if !allowed(allow, ent.p) {
				deny(http.StatusForbidden, "principal_not_allowed")
				return
			}
			slog.Debug("internal auth ok", "path", req.URL.Path, "principal", string(ent.p), "pod", ent.id.PodName)
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), ctxKey{}, ctxVal{ent.p, ent.id})))
		})
	}
}

func hasAudience(have []string, want string) bool {
	found := 0
	for _, a := range have {
		found |= subtle.ConstantTimeCompare([]byte(a), []byte(want))
	}
	return found == 1
}

// lookupPrincipal compares against every configured username without
// short-circuiting. The data is not secret (usernames, audiences); this is
// tidiness, not a security boundary.
func lookupPrincipal(m map[string]Principal, user string) (Principal, bool) {
	var res Principal
	found := 0
	for u, p := range m {
		if subtle.ConstantTimeCompare([]byte(u), []byte(user)) == 1 {
			res, found = p, 1
		}
	}
	return res, found == 1
}

func allowed(allow []Principal, p Principal) bool {
	ok := false
	for _, a := range allow {
		if a == p {
			ok = true
		}
	}
	return ok
}
