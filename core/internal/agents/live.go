package agents

import "sync/atomic"

// Live is the hot-reload handle every consumer holds (S11): an atomic pointer
// to the current catalog. A reload swaps the pointer, so a reader never sees a
// half-built catalog.
type Live struct {
	cur atomic.Pointer[Catalog]
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

func (l *Live) store(c *Catalog) { l.cur.Store(c) }

// Load returns the current catalog; never nil after NewLive.
func (l *Live) Load() *Catalog { return l.cur.Load() }

// Agent returns one agent from the current catalog.
func (l *Live) Agent(id string) (AgentFile, bool) { return l.Load().Agent(id) }

// List returns the current catalog's agents.
func (l *Live) List() []AgentFile { return l.Load().List() }

// Models returns the current catalog's models.yaml.
func (l *Live) Models() Models { return l.Load().Models() }
