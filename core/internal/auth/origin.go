package auth

import (
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

// attachmentsPath is the one route whose body is multipart (C1).
const attachmentsPath = "/v1/attachments"

// originSet is an allowlist of serialised origins, lower-cased.
type originSet map[string]struct{}

// newOriginSet keeps each entry lower-cased with a trailing slash removed.
// "null" (an opaque origin) is never allowed.
func newOriginSet(origins []string) originSet {
	s := originSet{}
	for _, o := range origins {
		o = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(o), "/"))
		if o != "" && o != "null" {
			s[o] = struct{}{}
		}
	}
	return s
}

// allows reports whether a request's Origin header values are exactly one
// allowed origin. A browser serialises an origin as scheme://host[:port]
// with no path, so anything else (a trailing slash, a path) is foreign.
func (s originSet) allows(values []string) bool {
	if len(values) != 1 {
		return false
	}
	_, ok := s[strings.ToLower(values[0])]
	return ok
}

// validOrigin checks a configured origin: http(s), a host, nothing else.
func validOrigin(o string) error {
	u, err := url.Parse(strings.TrimSuffix(o, "/"))
	if err != nil {
		return err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" ||
		u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("origin %q: want scheme://host[:port]", o)
	}
	return nil
}

// safeMethod is a method that changes nothing (RFC 9110 §9.2.1) and so needs
// no Origin. Every other method, including unknown ones, is unsafe.
func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

var (
	errForeignOrigin = errors.New("forbidden: origin not allowed")
	errMediaType     = errors.New("unsupported media type")
)

// UnsafeRequestGuard refuses every unsafe method (POST, PUT, PATCH, DELETE,
// and anything else that is not GET, HEAD or OPTIONS) unless it carries
// exactly one allowed Origin (403 otherwise) and a JSON body type, or
// multipart/form-data on POST /v1/attachments (415 otherwise). It sits
// outside RequirePerson (S17), so a cross-origin request is refused before
// authentication is looked at: 403, never 401. Task 15 wraps the whole
// public mux in it once.
func UnsafeRequestGuard(allowedOrigins []string) func(http.Handler) http.Handler {
	allowed := newOriginSet(allowedOrigins)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if safeMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			if !allowed.allows(r.Header.Values("Origin")) {
				http.Error(w, errForeignOrigin.Error(), http.StatusForbidden)
				return
			}
			if !bodyTypeAllowed(r) {
				http.Error(w, errMediaType.Error(), http.StatusUnsupportedMediaType)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// bodyTypeAllowed: application/json everywhere, multipart/form-data on POST
// /v1/attachments only. A cross-site page cannot send JSON without a CORS
// preflight, which Core never answers. A form can send multipart, so it is
// accepted only on the one route that needs it, behind the Origin check.
func bodyTypeAllowed(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return false
	}
	if r.Method == http.MethodPost && r.URL.Path == attachmentsPath {
		return mt == "multipart/form-data"
	}
	return mt == "application/json"
}
