// Package internalauth authenticates callers of Core's internal port (8081)
// with projected ServiceAccount tokens checked by the Kubernetes TokenReview
// API (P20). It uses plain net/http; there is no client-go.
package internalauth

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Principal names an internal caller class.
type Principal string

const (
	PrincipalConductor Principal = "conductor"
	PrincipalEvals     Principal = "evals"
	PrincipalReconcile Principal = "reconcile"
)

// Identity is what a successful TokenReview says about the token.
type Identity struct {
	Username  string
	Audiences []string
	PodName   string
}

// Reviewer checks a bearer token.
type Reviewer interface {
	Review(ctx context.Context, token string) (Identity, error)
}

// Sentinel errors. None ever carries the token.
var (
	// ErrUnauthenticated means the apiserver answered and rejected the token.
	ErrUnauthenticated = errors.New("internalauth: token not authenticated")
	// ErrReviewUnavailable means the review could not be completed (fail closed).
	ErrReviewUnavailable = errors.New("internalauth: token review unavailable")
)

// KubeReviewer calls POST /apis/authentication.k8s.io/v1/tokenreviews.
type KubeReviewer struct {
	apiServer    string
	audience     string
	ownTokenFile string
	client       *http.Client
	cfgErr       error // fixed-text configuration failure; every review fails closed
}

// Err reports a configuration failure found at construction (unusable CA,
// non-https API server). The constructor's signature is frozen, so the
// serve task treats a non-nil Err as a startup error.
func (k *KubeReviewer) Err() error { return k.cfgErr }

// NewKubeReviewer builds a reviewer. apiServer is a base URL such as
// https://10.96.0.1:443 (see APIServerFromEnv); caFile is the cluster CA
// bundle; ownTokenFile is Core's own API-audience token, re-read on every call
// because the kubelet rotates it. audience is the audience the reviewed token
// must carry.
func NewKubeReviewer(apiServer, caFile, ownTokenFile, audience string) *KubeReviewer {
	pool := x509.NewCertPool()
	var cfgErr error
	if pem, err := os.ReadFile(caFile); err != nil { //nolint:gosec // G304: operator-mounted CA path
		cfgErr = errors.New("ca file unreadable")
	} else if !pool.AppendCertsFromPEM(pem) {
		cfgErr = errors.New("ca file has no usable certificate")
	}
	if !strings.HasPrefix(apiServer, "https://") {
		cfgErr = errors.New("apiserver url is not https")
	}
	return &KubeReviewer{
		cfgErr:       cfgErr,
		apiServer:    strings.TrimRight(apiServer, "/"),
		audience:     audience,
		ownTokenFile: ownTokenFile,
		client: &http.Client{
			Timeout: 5 * time.Second,
			// A redirect would resend our bearer to wherever it points.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			},
		},
	}
}

// APIServerFromEnv builds the in-cluster API server URL from the kubelet's
// KUBERNETES_SERVICE_HOST / KUBERNETES_SERVICE_PORT values (passed in so tests
// need no process env).
func APIServerFromEnv(host, port string) (string, error) {
	if host == "" || port == "" {
		return "", errors.New("internalauth: KUBERNETES_SERVICE_HOST/PORT unset")
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return "https://" + host + ":" + port, nil
}

type reviewRequest struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Spec       reviewSpec `json:"spec"`
}
type reviewSpec struct {
	Token     string   `json:"token"`
	Audiences []string `json:"audiences"`
}
type reviewResponse struct {
	Status struct {
		Authenticated bool     `json:"authenticated"`
		Audiences     []string `json:"audiences"`
		User          struct {
			Username string              `json:"username"`
			Extra    map[string][]string `json:"extra"`
		} `json:"user"`
		Error string `json:"error"`
	} `json:"status"`
}

// Review implements Reviewer.
func (k *KubeReviewer) Review(ctx context.Context, token string) (Identity, error) {
	if k.cfgErr != nil {
		return Identity{}, fmt.Errorf("%w: %s", ErrReviewUnavailable, k.cfgErr.Error())
	}
	own, err := os.ReadFile(k.ownTokenFile) //nolint:gosec // G304: operator-mounted token path
	if err != nil || len(bytes.TrimSpace(own)) == 0 {
		return Identity{}, fmt.Errorf("%w: own token unreadable", ErrReviewUnavailable)
	}
	body, err := json.Marshal(reviewRequest{
		APIVersion: "authentication.k8s.io/v1",
		Kind:       "TokenReview",
		Spec:       reviewSpec{Token: token, Audiences: []string{k.audience}},
	})
	if err != nil {
		return Identity{}, fmt.Errorf("%w: encode", ErrReviewUnavailable)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, //nolint:gosec // G704: apiServer is operator config
		k.apiServer+"/apis/authentication.k8s.io/v1/tokenreviews", bytes.NewReader(body))
	if err != nil {
		return Identity{}, fmt.Errorf("%w: request", ErrReviewUnavailable)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(own)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := k.client.Do(req) //nolint:gosec // G704: apiServer is operator config (in-cluster env)
	if err != nil {
		// Deliberately not wrapped: keep transport detail out of errors.
		return Identity{}, fmt.Errorf("%w: apiserver unreachable", ErrReviewUnavailable)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Identity{}, fmt.Errorf("%w: read", ErrReviewUnavailable)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return Identity{}, fmt.Errorf("%w: apiserver status %d", ErrReviewUnavailable, resp.StatusCode)
	}
	var out reviewResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return Identity{}, fmt.Errorf("%w: decode", ErrReviewUnavailable)
	}
	if !out.Status.Authenticated {
		return Identity{}, ErrUnauthenticated
	}
	id := Identity{Username: out.Status.User.Username, Audiences: out.Status.Audiences}
	if v := out.Status.User.Extra["authentication.kubernetes.io/pod-name"]; len(v) > 0 {
		id.PodName = v[0]
	}
	return id, nil
}

// tokenExpiry reads the unverified `exp` claim of a JWT. The apiserver has
// already verified the token; this only bounds how long we may cache it.
func tokenExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var c struct {
		Exp float64 `json:"exp"`
	}
	if json.Unmarshal(b, &c) != nil || c.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(c.Exp), 0), true
}
