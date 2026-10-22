// Package oidctest is an in-process fake OpenID Connect issuer, shaped like
// Dex, for tests of the BFF: discovery, JWKS, an /auth endpoint that signs in
// the configured identity at once (no page), and a /token endpoint that checks
// client_secret_basic, the redirect URI and PKCE S256. It needs no network
// beyond the loopback httptest server. Not for use outside tests.
package oidctest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// keyID is the kid of the issuer's one signing key.
const keyID = "oidctest-1"

// Identity is who the next /auth request signs in as.
type Identity struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
	// ConnectorID becomes federated_claims.connector_id, as Dex writes it for
	// the federated:id scope. Empty leaves federated_claims out of the token.
	ConnectorID string
}

// TokenRequest is one call to /token, as the issuer received it.
type TokenRequest struct {
	Form      url.Values
	BasicUser string // url-decoded, as Dex decodes client_secret_basic
	BasicPass string
	HasBasic  bool
}

// Issuer is the fake. Its exported fields are read-only after New.
type Issuer struct {
	URL          string
	ClientID     string
	ClientSecret string
	RedirectURI  string

	srv *httptest.Server
	key *rsa.PrivateKey

	mu            sync.Mutex
	identity      Identity
	nonceOverride *string
	codes         map[string]grant
	authRequests  []url.Values
	tokenRequests []TokenRequest
}

type grant struct {
	identity    Identity
	nonce       string
	challenge   string
	redirectURI string
}

// New starts an issuer for one confidential client and stops it when the
// test ends.
func New(t testing.TB, clientID, clientSecret, redirectURI string) *Issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	i := &Issuer{
		ClientID: clientID, ClientSecret: clientSecret, RedirectURI: redirectURI,
		key: key, codes: map[string]grant{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", i.discovery)
	mux.HandleFunc("GET /keys", i.keys)
	mux.HandleFunc("GET /auth", i.auth)
	mux.HandleFunc("POST /token", i.token)
	i.srv = httptest.NewServer(mux)
	i.URL = i.srv.URL
	t.Cleanup(i.srv.Close)
	return i
}

// SetIdentity sets who the next /auth request signs in as.
func (i *Issuer) SetIdentity(id Identity) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.identity = id
}

// OverrideNonce makes every later ID token carry nonce n instead of the one
// the /auth request sent.
func (i *Issuer) OverrideNonce(n string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.nonceOverride = &n
}

// AuthRequests returns the query of every /auth request so far.
func (i *Issuer) AuthRequests() []url.Values {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]url.Values(nil), i.authRequests...)
}

// TokenRequests returns every /token request so far.
func (i *Issuer) TokenRequests() []TokenRequest {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]TokenRequest(nil), i.tokenRequests...)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (i *Issuer) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                i.URL,
		"authorization_endpoint":                i.URL + "/auth",
		"token_endpoint":                        i.URL + "/token",
		"jwks_uri":                              i.URL + "/keys",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
		"scopes_supported":                      []string{"openid", "email", "profile", "groups", "offline_access", "federated:id"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post"},
	})
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (i *Issuer) keys(w http.ResponseWriter, _ *http.Request) {
	pub := i.key.PublicKey
	writeJSON(w, http.StatusOK, map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "alg": "RS256", "use": "sig", "kid": keyID,
		"n": b64(pub.N.Bytes()), "e": b64(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

// auth signs the current identity in at once and redirects back with a code.
// A malformed request is a 400, so a BFF bug fails its test loudly.
func (i *Issuer) auth(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	i.mu.Lock()
	i.authRequests = append(i.authRequests, q)
	id := i.identity
	i.mu.Unlock()
	switch {
	case q.Get("response_type") != "code",
		q.Get("client_id") != i.ClientID,
		q.Get("redirect_uri") != i.RedirectURI,
		q.Get("code_challenge_method") != "S256",
		q.Get("code_challenge") == "",
		q.Get("state") == "":
		http.Error(w, "oidctest: bad authorization request", http.StatusBadRequest)
		return
	}
	code := b64(random(16))
	i.mu.Lock()
	i.codes[code] = grant{identity: id, nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirectURI: q.Get("redirect_uri")}
	i.mu.Unlock()
	back, _ := url.Parse(i.RedirectURI)
	bq := back.Query()
	bq.Set("code", code)
	bq.Set("state", q.Get("state"))
	back.RawQuery = bq.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

func (i *Issuer) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	tr := TokenRequest{Form: r.PostForm}
	if u, p, ok := r.BasicAuth(); ok {
		tr.HasBasic = true
		tr.BasicUser, _ = url.QueryUnescape(u)
		tr.BasicPass, _ = url.QueryUnescape(p)
	}
	i.mu.Lock()
	i.tokenRequests = append(i.tokenRequests, tr)
	g, found := i.codes[r.PostForm.Get("code")]
	delete(i.codes, r.PostForm.Get("code")) // single use
	nonceOverride := i.nonceOverride
	i.mu.Unlock()

	if !tr.HasBasic || tr.BasicUser != i.ClientID ||
		subtle.ConstantTimeCompare([]byte(tr.BasicPass), []byte(i.ClientSecret)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
		return
	}
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if r.PostForm.Get("grant_type") != "authorization_code" || !found ||
		r.PostForm.Get("redirect_uri") != g.redirectURI || b64(sum[:]) != g.challenge {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		return
	}
	nonce := g.nonce
	if nonceOverride != nil {
		nonce = *nonceOverride
	}
	now := time.Now()
	claims := map[string]any{
		"iss": i.URL, "sub": g.identity.Subject, "aud": i.ClientID,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		"nonce": nonce, "email": g.identity.Email, "email_verified": g.identity.EmailVerified,
		"name": g.identity.Name,
	}
	if g.identity.ConnectorID != "" {
		claims["federated_claims"] = map[string]string{"connector_id": g.identity.ConnectorID, "user_id": g.identity.Subject}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": b64(random(16)), "token_type": "bearer", "expires_in": 3600,
		"id_token": i.sign(claims),
	})
}

// sign makes an RS256 JWS compact serialisation.
func (i *Issuer) sign(claims map[string]any) string {
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": keyID, "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	input := b64(header) + "." + b64(payload)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, i.key, crypto.SHA256, sum[:])
	if err != nil {
		panic(err)
	}
	return input + "." + b64(sig)
}

func random(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}
