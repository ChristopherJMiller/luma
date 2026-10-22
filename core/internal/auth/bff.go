// Package auth is Core's browser authentication. Core is the
// backend-for-frontend: a confidential OIDC client of Dex that runs the PWA's
// sign-in (code flow, PKCE S256, state and nonce sealed in a login cookie),
// then holds the browser's session itself in an HttpOnly, host-only cookie
// whose sha256 keys a web_sessions row. No token ever reaches the browser.
// The people registry decides who may sign in, through which connector.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/store"
)

// scopes are requested at sign-in: federated:id makes Dex add
// federated_claims.connector_id. No offline_access: Core keeps no refresh
// token in phase 1; its own web session is the only session.
var scopes = []string{oidc.ScopeOpenID, "email", "profile", "federated:id"}

// returnTo is C1's allowlist for /auth/login?return_to=.
var returnTo = regexp.MustCompile(`^/(c/ses_[0-9A-HJKMNP-TV-Z]{26}|profile)?$`)

// minSigningKey is the shortest SESSION_SIGNING_KEY accepted (C6).
const minSigningKey = 32

// oidcTimeout bounds every request to the issuer.
const oidcTimeout = 10 * time.Second

// BFFConfig configures the BFF (C6). A zero AbsoluteTTL or IdleTTL takes the
// default; empty AllowedOrigins means the public URL's own origin.
type BFFConfig struct {
	PublicURL, Issuer, ClientID, ClientSecret string
	SigningKey                                []byte
	AbsoluteTTL, IdleTTL                      time.Duration
	AllowedOrigins                            []string
}

// BFF runs sign-in, sign-out and the web session check.
type BFF struct {
	ctx         context.Context
	cfg         BFFConfig
	st          *store.Store
	reg         atomic.Pointer[Registry] // S11: swapped whole by SetRegistry
	sealer      *sealer
	origins     originSet
	secure      bool // PublicURL is https: __Host- names and Secure cookies
	redirectURL string
	absoluteTTL time.Duration
	idleTTL     time.Duration
	httpClient  *http.Client
	now         func() time.Time

	sessionCookie, loginCookie string

	oidcMu sync.Mutex
	oidc   *oidcClient // nil until discovery succeeds

	endedMu sync.Mutex
	ended   []func([]byte, string)
}

type oidcClient struct {
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// NewBFF validates cfg and builds the BFF. It keeps reg behind an atomic
// pointer (S11). It tries OIDC discovery at once, but an unreachable issuer
// is not fatal: existing sessions need only the database, so discovery is
// retried at the next sign-in. ctx is the server's lifetime: discovery and
// SetRegistry's sweep run under it.
func NewBFF(ctx context.Context, cfg BFFConfig, st *store.Store, reg *Registry) (*BFF, error) {
	if st == nil || reg == nil {
		return nil, errors.New("auth: NewBFF needs a store and a registry")
	}
	pub, err := url.Parse(cfg.PublicURL)
	if err != nil || (pub.Scheme != "https" && pub.Scheme != "http") || pub.Host == "" ||
		(pub.Path != "" && pub.Path != "/") || pub.RawQuery != "" || pub.Fragment != "" || pub.User != nil {
		return nil, fmt.Errorf("auth: public URL %q: want http(s)://host[:port]", cfg.PublicURL)
	}
	switch {
	case cfg.Issuer == "":
		return nil, errors.New("auth: OIDC issuer is required")
	case cfg.ClientID == "":
		return nil, errors.New("auth: OIDC client id is required")
	case cfg.ClientSecret == "":
		return nil, errors.New("auth: OIDC client secret is required")
	case len(cfg.SigningKey) < minSigningKey:
		return nil, fmt.Errorf("auth: session signing key is %d bytes, want at least %d", len(cfg.SigningKey), minSigningKey)
	case cfg.AbsoluteTTL < 0 || cfg.IdleTTL < 0:
		return nil, errors.New("auth: session TTLs must not be negative")
	}
	origin := pub.Scheme + "://" + pub.Host
	origins := cfg.AllowedOrigins
	if len(origins) == 0 {
		origins = []string{origin}
	}
	for _, o := range origins {
		if err := validOrigin(o); err != nil {
			return nil, fmt.Errorf("auth: allowed origins: %w", err)
		}
	}
	sl, err := newSealer(cfg.SigningKey)
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	b := &BFF{
		ctx: ctx, cfg: cfg, st: st, sealer: sl,
		origins:     newOriginSet(origins),
		secure:      pub.Scheme == "https",
		redirectURL: origin + "/auth/callback",
		absoluteTTL: cfg.AbsoluteTTL, idleTTL: cfg.IdleTTL,
		httpClient: &http.Client{Timeout: oidcTimeout},
		now:        time.Now,
	}
	if b.absoluteTTL == 0 {
		b.absoluteTTL = DefaultAbsoluteTTL
	}
	if b.idleTTL == 0 {
		b.idleTTL = DefaultIdleTTL
	}
	// S17: a __Host- cookie cannot be set over http (dev, e2e), so the names
	// follow the public URL's scheme.
	if b.secure {
		b.sessionCookie, b.loginCookie = "__Host-luma", "__Host-luma-login"
	} else {
		b.sessionCookie, b.loginCookie = "luma_dev", "luma_login_dev"
	}
	b.reg.Store(reg)
	if _, err := b.client(); err != nil {
		slog.Warn("auth: OIDC discovery failed; retrying at the next sign-in", "err", err)
	}
	return b, nil
}

// client returns the OIDC client, running discovery if it has not yet
// succeeded.
func (b *BFF) client() (*oidcClient, error) {
	b.oidcMu.Lock()
	defer b.oidcMu.Unlock()
	if b.oidc != nil {
		return b.oidc, nil
	}
	p, err := oidc.NewProvider(oidc.ClientContext(b.ctx, b.httpClient), b.cfg.Issuer)
	if err != nil {
		return nil, err
	}
	ep := p.Endpoint()
	ep.AuthStyle = oauth2.AuthStyleInHeader // client_secret_basic
	b.oidc = &oidcClient{
		oauth: oauth2.Config{
			ClientID: b.cfg.ClientID, ClientSecret: b.cfg.ClientSecret,
			Endpoint: ep, RedirectURL: b.redirectURL, Scopes: scopes,
		},
		verifier: p.Verifier(&oidc.Config{ClientID: b.cfg.ClientID}),
	}
	return b.oidc, nil
}

// Routes registers GET /auth/login, GET /auth/callback and POST /auth/logout.
// Logout is also wrapped in UnsafeRequestGuard here, so it is guarded even if
// the caller's outer guard is missing (a second pass is harmless).
func (b *BFF) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", b.login)
	mux.HandleFunc("GET /auth/callback", b.callback)
	mux.Handle("POST /auth/logout", UnsafeRequestGuard(originList(b.origins))(http.HandlerFunc(b.logout)))
}

func originList(s originSet) []string {
	out := make([]string, 0, len(s))
	for o := range s {
		out = append(out, o)
	}
	return out
}

// cookie builds one of the BFF's cookies: host-only (no Domain), Path=/,
// HttpOnly, SameSite=Lax, Secure over https. maxAge < 0 deletes it.
func (b *BFF) cookie(name, value string, maxAge time.Duration) *http.Cookie {
	//nolint:gosec // G124: Secure follows the public URL's scheme (S17); an http dev/e2e origin cannot keep a Secure cookie, and https always sets it (TestCookieAttributes)
	c := &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: b.secure, SameSite: http.SameSiteLaxMode}
	if maxAge < 0 {
		c.MaxAge = -1
	} else {
		c.MaxAge = int(maxAge / time.Second)
	}
	return c
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// sanitizeReturnTo keeps a return_to that matches C1's allowlist, else "/".
func sanitizeReturnTo(s string) string {
	if returnTo.MatchString(s) {
		return s
	}
	return "/"
}

// login starts a sign-in: it seals a fresh state, nonce and PKCE verifier in
// the login cookie and redirects to the issuer.
func (b *BFF) login(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	oc, err := b.client()
	if err != nil {
		slog.Warn("auth: sign-in unavailable, OIDC discovery failed", "err", err)
		http.Error(w, "Sign-in is unavailable right now. Try again in a minute.", http.StatusServiceUnavailable)
		return
	}
	state, err1 := randomToken()
	nonce, err2 := randomToken()
	if err := errors.Join(err1, err2); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	st := loginState{
		State: state, Nonce: nonce, Verifier: oauth2.GenerateVerifier(),
		ReturnTo: sanitizeReturnTo(r.URL.Query().Get("return_to")),
		Expires:  b.now().Add(loginCookieTTL).Unix(),
	}
	sealed, err := b.sealer.seal(b.loginCookie, st)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, b.cookie(b.loginCookie, sealed, loginCookieTTL))
	http.Redirect(w, r, oc.oauth.AuthCodeURL(st.State, oidc.Nonce(st.Nonce), oauth2.S256ChallengeOption(st.Verifier)), http.StatusFound)
}

// idClaims are the ID token claims the BFF reads. Dex writes
// federated_claims for the federated:id scope.
type idClaims struct {
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Federated     *struct {
		ConnectorID string `json:"connector_id"`
	} `json:"federated_claims"`
}

func badLogin(w http.ResponseWriter, reason string) {
	slog.Info("auth: sign-in refused", "reason", reason)
	http.Error(w, "Sign-in failed. Go back to Luma and try again.", http.StatusBadRequest)
}

// callback finishes a sign-in. The login cookie is single use: it is cleared
// whatever happens. On success it always mints a new session id, and deletes
// the row of any session cookie presented (no session fixation).
func (b *BFF) callback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	lc, err := r.Cookie(b.loginCookie)
	http.SetCookie(w, b.cookie(b.loginCookie, "", -1))
	if err != nil {
		badLogin(w, "no_login_cookie")
		return
	}
	st, err := b.sealer.open(b.loginCookie, lc.Value, b.now())
	if err != nil {
		badLogin(w, "login_cookie")
		return
	}
	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(st.State)) != 1 {
		badLogin(w, "state_mismatch")
		return
	}
	if q.Get("error") != "" || q.Get("code") == "" {
		badLogin(w, "issuer_error")
		return
	}
	oc, err := b.client()
	if err != nil {
		slog.Warn("auth: sign-in unavailable, OIDC discovery failed", "err", err)
		http.Error(w, "Sign-in is unavailable right now. Try again in a minute.", http.StatusServiceUnavailable)
		return
	}
	ctx := context.WithValue(r.Context(), oauth2.HTTPClient, b.httpClient)
	tok, err := oc.oauth.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(st.Verifier))
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) {
			badLogin(w, "code_exchange")
			return
		}
		slog.Warn("auth: code exchange failed", "err", err)
		http.Error(w, "Sign-in is unavailable right now. Try again in a minute.", http.StatusBadGateway)
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		badLogin(w, "no_id_token")
		return
	}
	idt, err := oc.verifier.Verify(ctx, raw)
	if err != nil {
		badLogin(w, "id_token")
		return
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(st.Nonce)) != 1 {
		badLogin(w, "nonce_mismatch")
		return
	}
	var cl idClaims
	if err := idt.Claims(&cl); err != nil {
		badLogin(w, "claims")
		return
	}
	connector := ""
	if cl.Federated != nil {
		connector = cl.Federated.ConnectorID
	}
	p, invited := b.registry().Lookup(connector, cl.Email)
	if !cl.EmailVerified || !invited {
		reason := "not_invited"
		if !cl.EmailVerified {
			reason = "email_unverified"
		}
		slog.Info("auth: sign-in refused", "reason", reason, "connector", connector)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprintln(w, protocol.ErrorCopy[protocol.CodeNotInvited])
		return
	}

	value, hash, err := newSessionID()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	now := b.now()
	if err := b.st.CreateWebSession(r.Context(), store.WebSession{
		IDHash: hash, PersonID: p.ID, ConnectorID: connector, Email: cl.Email, ExpiresAt: now.Add(b.absoluteTTL),
	}); err != nil {
		slog.Error("auth: creating web session failed", "person", p.ID, "err", err)
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if old, ok := b.presentedSession(r); ok {
		b.replaced(r.Context(), old, now)
	}
	slog.Info("auth: signed in", "person", p.ID, "connector", connector)
	http.SetCookie(w, b.cookie(b.sessionCookie, value, b.absoluteTTL))
	w.Header().Set("Location", sanitizeReturnTo(st.ReturnTo))
	w.WriteHeader(http.StatusFound)
}

// replaced ends the session a new login replaces, if it was live, and
// deletes its row either way.
func (b *BFF) replaced(ctx context.Context, hash []byte, now time.Time) {
	ws, err := b.st.WebSession(ctx, hash, now)
	if err == nil {
		_ = b.endSession(ctx, hash, ws.PersonID, "replaced")
		return
	}
	if err := b.st.DeleteWebSession(ctx, hash); err != nil {
		slog.Warn("auth: deleting a replaced web session failed", "err", err)
	}
}

// logout deletes the web session (OnSessionEnded closes its WebSockets) and
// clears the cookie. Without a live session it still answers 204.
func (b *BFF) logout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if hash, ok := b.presentedSession(r); ok {
		ws, err := b.st.WebSession(r.Context(), hash, b.now())
		switch {
		case err == nil:
			if err := b.endSession(r.Context(), hash, ws.PersonID, "logout"); err != nil {
				http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
				return
			}
		case errors.Is(err, store.ErrNotFound):
			_ = b.st.DeleteWebSession(r.Context(), hash) // an expired row, if any
		default:
			slog.Error("auth: reading web session failed", "err", err)
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	http.SetCookie(w, b.cookie(b.sessionCookie, "", -1))
	w.WriteHeader(http.StatusNoContent)
}
