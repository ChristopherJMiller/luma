package agents

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ChristopherJMiller/luma/core/internal/protocol"
)

// Catalog is an immutable, validated set of agents plus models.yaml.
type Catalog struct {
	agents map[string]AgentFile
	models Models
}

// LoadCatalog reads dir, which holds either flat ConfigMap keys
// (<id>.agent.md) or one directory per agent (<id>/AGENT.md), and models.yaml.
// Dot entries (the kubelet's ..data and timestamp dirs) are ignored. The whole
// catalog is validated, down to compiling every agent for every tier it
// offers, so a bad file is rejected here and never reaches a consumer.
func LoadCatalog(dir string) (*Catalog, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read agents dir: %w", err)
	}
	c := &Catalog{agents: map[string]AgentFile{}}
	var haveModels bool
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(dir, name)
		var id, path string
		switch {
		case name == "models.yaml":
			b, err := os.ReadFile(full) //nolint:gosec // G304: dir is the operator-mounted config dir
			if err != nil {
				return nil, fmt.Errorf("read models.yaml: %w", err)
			}
			if c.models, err = ParseModels(b); err != nil {
				return nil, err
			}
			haveModels = true
			continue
		case strings.HasSuffix(name, ".agent.md"):
			id, path = strings.TrimSuffix(name, ".agent.md"), full
		default:
			// A directory (or a symlink to one) holding AGENT.md.
			if fi, err := os.Stat(full); err != nil || !fi.IsDir() {
				continue
			}
			p := filepath.Join(full, "AGENT.md")
			if _, err := os.Stat(p); err != nil {
				continue
			}
			id, path = name, p
		}
		b, err := os.ReadFile(path) //nolint:gosec // G304: dir is the operator-mounted config dir
		if err != nil {
			return nil, fmt.Errorf("read agent %s: %w", id, err)
		}
		a, err := ParseAgent(b)
		if err != nil {
			return nil, fmt.Errorf("agent %s: %w", id, err)
		}
		if a.Name != id {
			return nil, fmt.Errorf("agent file %s declares name %q", id, a.Name)
		}
		c.agents[id] = a
	}
	if !haveModels {
		return nil, fmt.Errorf("no models.yaml in %s", dir)
	}
	if len(c.agents) == 0 {
		return nil, errors.New("no agents found")
	}
	for _, a := range c.agents {
		for _, tier := range a.Tiers {
			if _, err := c.Compile(a.Name, tier); err != nil {
				return nil, err
			}
		}
	}
	return c, nil
}

// clone copies the slices, so a caller cannot mutate a shared snapshot.
func (a AgentFile) clone() AgentFile {
	a.Tiers = slices.Clone(a.Tiers)
	a.BuiltinTools = slices.Clone(a.BuiltinTools)
	a.MCP = slices.Clone(a.MCP)
	a.UntrustedInputs = slices.Clone(a.UntrustedInputs)
	a.Subagents = slices.Clone(a.Subagents)
	return a
}

// Agent returns a copy of one agent by id.
func (c *Catalog) Agent(id string) (AgentFile, bool) {
	a, ok := c.agents[id]
	return a.clone(), ok
}

// List returns a copy of every agent, sorted by name.
func (c *Catalog) List() []AgentFile {
	out := make([]AgentFile, 0, len(c.agents))
	for _, a := range c.agents {
		out = append(out, a.clone())
	}
	slices.SortFunc(out, func(x, y AgentFile) int { return strings.Compare(x.Name, y.Name) })
	return out
}

// Models returns a copy of the catalog's models.yaml.
func (c *Catalog) Models() Models {
	m := c.models
	m.Tiers = maps.Clone(m.Tiers)
	return m
}

// Compile compiles one agent of the catalog for a tier.
func (c *Catalog) Compile(id, tier string) (protocol.AgentSpec, error) {
	a, ok := c.agents[id]
	if !ok {
		return protocol.AgentSpec{}, fmt.Errorf("no agent %q", id)
	}
	subs := map[string]AgentFile{}
	for _, name := range a.Subagents {
		if s, ok := c.agents[name]; ok {
			subs[name] = s
		}
	}
	return Compile(a, subs, c.models, tier)
}
