package maintenance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"time"
)

// AdminPath is the admin API's one route.
const AdminPath = "/maintenance"

const maxAdminBody = 4 << 10

// ErrNotLoopback: the admin listener must be on a loopback IP (S12).
var ErrNotLoopback = errors.New("maintenance: the admin listener must be a loopback IP")

// CheckAdminAddr accepts host:port only when host is a loopback IP literal
// (127.0.0.0/8 or ::1); a hostname, an empty host or a wildcard is refused.
func CheckAdminAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%w: %q: %w", ErrNotLoopback, addr, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("%w: %q", ErrNotLoopback, addr)
	}
	return nil
}

// ListenAdmin listens on addr (LUMA_ADMIN_LISTEN) after CheckAdminAddr.
func ListenAdmin(addr string) (net.Listener, error) {
	if err := CheckAdminAddr(addr); err != nil {
		return nil, err
	}
	return (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
}

// adminRequest is POST /maintenance's body. DrainTimeout is a Go duration;
// empty is DefaultDrainTimeout.
type adminRequest struct {
	Enabled      *bool  `json:"enabled"`
	Reason       string `json:"reason"`
	DrainTimeout string `json:"drain_timeout"`
}

// AdminHandler serves GET /maintenance (the status) and POST /maintenance
// ({enabled, reason?, drain_timeout?}; it answers once the drain is done).
// It is served only on the loopback admin listener; it has no auth of its
// own, so it must never be mounted on another port.
func (mt *Maintenance) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+AdminPath, func(w http.ResponseWriter, _ *http.Request) {
		writeStatus(w, mt.Status())
	})
	mux.HandleFunc("POST "+AdminPath, mt.serveChange)
	return mux
}

func (mt *Maintenance) serveChange(w http.ResponseWriter, r *http.Request) {
	if ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || ct != "application/json" {
		http.Error(w, http.StatusText(http.StatusUnsupportedMediaType), http.StatusUnsupportedMediaType)
		return
	}
	var req adminRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAdminBody))
	if err := dec.Decode(&req); err != nil || req.Enabled == nil {
		http.Error(w, "body must be {enabled, reason?, drain_timeout?}", http.StatusBadRequest)
		return
	}
	drain := DefaultDrainTimeout
	if req.DrainTimeout != "" {
		d, err := time.ParseDuration(req.DrainTimeout)
		if err != nil || d < 0 {
			http.Error(w, "drain_timeout must be a non-negative duration such as 120s", http.StatusBadRequest)
			return
		}
		drain = d
	}
	// The drain runs to its end even if the caller goes away.
	ctx := context.WithoutCancel(r.Context())
	var s Status
	var err error
	if *req.Enabled {
		s, err = mt.On(ctx, req.Reason, drain)
	} else {
		s, err = mt.Off(ctx)
	}
	if err != nil {
		slog.Error("maintenance: change failed", "enabled", *req.Enabled, "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeStatus(w, s)
}

func writeStatus(w http.ResponseWriter, s Status) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(s); err != nil {
		slog.Warn("maintenance: status not written", "err", err)
	}
}
