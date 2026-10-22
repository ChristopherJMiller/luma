package auth

import (
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const (
	// loginCookieTTL is how long a sign-in may take, from /auth/login to the
	// callback.
	loginCookieTTL = 10 * time.Minute
	// loginCookieInfo is the HKDF info that derives the login cookie's key
	// from the session signing key.
	loginCookieInfo = "luma login cookie v1"
)

// loginState is what /auth/login hands the callback, sealed in the login
// cookie: the OAuth state, the OIDC nonce, the PKCE verifier and where to go
// after. It never reaches the browser in the clear.
type loginState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	ReturnTo string `json:"r"`
	Expires  int64  `json:"e"` // unix seconds
}

// sealer seals and opens login cookies with XChaCha20-Poly1305, keyed by
// HKDF-SHA256(signing key, info "luma login cookie v1"). The cookie name is
// the additional data, so a value cannot move between cookie names.
type sealer struct{ aead cipher.AEAD }

func newSealer(signingKey []byte) (*sealer, error) {
	key := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(hkdf.New(sha256.New, signingKey, nil, []byte(loginCookieInfo)), key); err != nil {
		return nil, fmt.Errorf("login cookie key: %w", err)
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("login cookie key: %w", err)
	}
	return &sealer{aead: aead}, nil
}

// seal returns base64url(nonce || ciphertext).
func (s *sealer) seal(name string, st loginState) (string, error) {
	plain, err := json.Marshal(st)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, s.aead.NonceSize(), s.aead.NonceSize()+len(plain)+s.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(s.aead.Seal(nonce, nonce, plain, []byte(name))), nil
}

var errLoginCookie = errors.New("login cookie invalid or expired")

// open authenticates and decrypts a login cookie and checks its expiry.
func (s *sealer) open(name, value string, now time.Time) (loginState, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) < s.aead.NonceSize()+s.aead.Overhead() {
		return loginState{}, errLoginCookie
	}
	n := s.aead.NonceSize()
	plain, err := s.aead.Open(nil, raw[:n], raw[n:], []byte(name))
	if err != nil {
		return loginState{}, errLoginCookie
	}
	var st loginState
	if err := json.Unmarshal(plain, &st); err != nil || st.State == "" || st.Nonce == "" || st.Verifier == "" {
		return loginState{}, errLoginCookie
	}
	if now.Unix() >= st.Expires {
		return loginState{}, errLoginCookie
	}
	return st, nil
}
