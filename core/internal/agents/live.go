package agents

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/ChristopherJMiller/luma/core/internal/protocol"
)

// Live is the hot-reload handle every consumer holds (S11): an atomic pointer
// to the current catalog. A reload swaps the pointer, so a reader never sees a
// half-built catalog.
type Live struct {
	cur atomic.Pointer[Catalog]

	mu       sync.Mutex
	lastErr  error
	onReload func(err error)
}

// NewLive wraps c. A nil c becomes an empty catalog, so Load is never nil.
func NewLive(c *Catalog) *Live {
	if c == nil {
		c = &Catalog{agents: map[string]AgentFile{}}
	}
	l := &Live{}
	l.cur.Store(c)
	return l
}

// Load returns the current catalog; never nil after NewLive.
func (l *Live) Load() *Catalog { return l.cur.Load() }

// Agent returns one agent from the current catalog.
func (l *Live) Agent(id string) (AgentFile, bool) { return l.Load().Agent(id) }

// List returns the current catalog's agents.
func (l *Live) List() []AgentFile { return l.Load().List() }

// Models returns the current catalog's models.yaml.
func (l *Live) Models() Models { return l.Load().Models() }

// Compile compiles an agent, its subagents and the tier bindings from one
// snapshot of the catalog, so a reload cannot mix two versions.
func (l *Live) Compile(id, tier string) (protocol.AgentSpec, error) {
	return l.Load().Compile(id, tier)
}

// LastReloadError is the error of the most recent reload, nil if it was good
// (or none has run). The catalog in force is the last good one either way.
func (l *Live) LastReloadError() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastErr
}

// OnReload registers f to be called after every reload with its error (nil on
// success). Core's wiring uses it to count luma_agents_reload_total. Call it
// before Watch.
func (l *Live) OnReload(f func(err error)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onReload = f
}

// apply records one reload's outcome: a good catalog replaces the current
// one, a bad one is logged and leaves it in force.
func (l *Live) apply(c *Catalog, err error) {
	if err == nil {
		l.cur.Store(c)
	} else {
		slog.Warn("agents: reload failed, keeping the previous catalog", "err", err)
	}
	l.mu.Lock()
	l.lastErr = err
	f := l.onReload
	l.mu.Unlock()
	if f != nil {
		f(err)
	}
}

// Watch reloads dir into the live catalog: each good reload swaps the pointer;
// a bad one keeps the old catalog and is kept in LastReloadError.
func (l *Live) Watch(ctx context.Context, dir string) error {
	return watch(ctx, dir, l.apply)
}
