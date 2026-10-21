// Package agents loads agent definitions (AGENT.md) and the site's
// models.yaml, compiles an agent for a tier into the frozen
// protocol.AgentSpec, and keeps the current catalog behind an atomic
// pointer so a ConfigMap update reloads it live (C5, S11).
package agents

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Phase-1 vocabulary. Each limit says "phase 1" in its error, so a later
// phase relaxes one line rather than hunting for it.
var (
	validKinds    = []string{"agent", "subagent"}
	validTiers    = []string{"fast", "smart", "deep"}
	validEfforts  = []string{"low", "medium", "high"}
	phase1MCP     = []string{"memory.remember", "memory.forget"}
	phase1Builtin = []string{"WebSearch", "WebFetch"}
)

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// Requires is what an agent needs of a tier's capabilities.
type Requires struct {
	ToolsMax int  `yaml:"tools_max"`
	Vision   bool `yaml:"vision"`
}

// Limits are the per-agent runtime limits.
type Limits struct {
	MaxTurns            int `yaml:"max_turns"`
	MaxToolCalls        int `yaml:"max_tool_calls"`
	WebSearchPerTurn    int `yaml:"web_search_per_turn"`
	WebSearchPerSession int `yaml:"web_search_per_session"`
}

// DefaultLimits apply when an agent file has no limits key.
var DefaultLimits = Limits{MaxTurns: 30, MaxToolCalls: 60, WebSearchPerTurn: 3, WebSearchPerSession: 15}

// Audience says who may use an agent.
type Audience struct {
	Tier string `yaml:"tier"`
}

// frontmatter is the strict AGENT.md header (C5): an unknown key is an error.
type frontmatter struct {
	Name            string         `yaml:"name"`
	Description     string         `yaml:"description"`
	Kind            string         `yaml:"kind"`
	Tiers           []string       `yaml:"tiers"`
	Requires        Requires       `yaml:"requires"`
	Workspace       string         `yaml:"workspace"`
	BuiltinTools    []string       `yaml:"builtin_tools"`
	MCP             []string       `yaml:"mcp"`
	Approval        map[string]any `yaml:"approval"`
	UntrustedInputs []string       `yaml:"untrusted_inputs"`
	Skills          []string       `yaml:"skills"`
	Subagents       []string       `yaml:"subagents"`
	Limits          *Limits        `yaml:"limits"`
	Audience        Audience       `yaml:"audience"`
	Memory          string         `yaml:"memory"`
	Effort          string         `yaml:"effort"`
}

// AgentFile is one parsed AGENT.md.
type AgentFile struct {
	Name            string
	Description     string
	Kind            string // agent | subagent
	Tiers           []string
	Requires        Requires
	BuiltinTools    []string
	MCP             []string
	UntrustedInputs []string
	Subagents       []string
	Limits          Limits
	Audience        Audience
	Memory          string // person | none
	Effort          string // optional; a subagent's explicit effort
	// Body is the static system prompt, verbatim: every byte after the
	// closing frontmatter line.
	Body string
	// Version is hex(sha256(file bytes)).
	Version string
}

// ParseAgent parses one AGENT.md: strict YAML frontmatter, then the body.
func ParseAgent(b []byte) (AgentFile, error) {
	fm, body, err := splitFrontmatter(b)
	if err != nil {
		return AgentFile{}, err
	}
	var f frontmatter
	if err := decodeStrict(fm, &f); err != nil {
		return AgentFile{}, fmt.Errorf("agent frontmatter: %w", err)
	}
	if !nameRE.MatchString(f.Name) {
		return AgentFile{}, fmt.Errorf("agent name %q must match %s", f.Name, nameRE)
	}
	if f.Description == "" {
		return AgentFile{}, fmt.Errorf("agent %s: description is required", f.Name)
	}
	if !slices.Contains(validKinds, f.Kind) {
		return AgentFile{}, fmt.Errorf("agent %s: kind %q must be agent or subagent", f.Name, f.Kind)
	}
	if len(f.Tiers) == 0 {
		return AgentFile{}, fmt.Errorf("agent %s: tiers is required", f.Name)
	}
	for _, t := range f.Tiers {
		if !slices.Contains(validTiers, t) {
			return AgentFile{}, fmt.Errorf("agent %s: tier %q must be fast, smart or deep", f.Name, t)
		}
	}
	if f.Workspace != "" && f.Workspace != "none" {
		return AgentFile{}, fmt.Errorf("agent %s: workspace %q is not available in phase 1 (only none)", f.Name, f.Workspace)
	}
	for _, m := range f.MCP {
		if !slices.Contains(phase1MCP, m) {
			return AgentFile{}, fmt.Errorf("agent %s: mcp %q is not available in phase 1 (only %s)", f.Name, m, strings.Join(phase1MCP, ", "))
		}
	}
	for _, t := range f.BuiltinTools {
		if !slices.Contains(phase1Builtin, t) {
			return AgentFile{}, fmt.Errorf("agent %s: builtin tool %q is not available in phase 1 (only %s)", f.Name, t, strings.Join(phase1Builtin, ", "))
		}
	}
	if len(f.Skills) > 0 {
		return AgentFile{}, fmt.Errorf("agent %s: skills are not available in phase 1", f.Name)
	}
	if len(f.Approval) > 0 {
		return AgentFile{}, fmt.Errorf("agent %s: approval rules are not available in phase 1", f.Name)
	}
	memory := f.Memory
	if memory == "" {
		memory = "none"
	}
	switch memory {
	case "person", "none":
	case "person+household":
		return AgentFile{}, fmt.Errorf("agent %s: memory %q is not available in phase 1 (only person or none)", f.Name, memory)
	default:
		return AgentFile{}, fmt.Errorf("agent %s: memory %q must be person or none", f.Name, memory)
	}
	if f.Effort != "" && !slices.Contains(validEfforts, f.Effort) {
		return AgentFile{}, fmt.Errorf("agent %s: effort %q must be low, medium or high", f.Name, f.Effort)
	}
	limits := DefaultLimits
	if f.Limits != nil {
		limits = *f.Limits
		if limits.MaxTurns <= 0 || limits.MaxToolCalls <= 0 || limits.WebSearchPerTurn <= 0 || limits.WebSearchPerSession <= 0 {
			return AgentFile{}, fmt.Errorf("agent %s: every limit must be positive", f.Name)
		}
	}
	if strings.TrimSpace(body) == "" {
		return AgentFile{}, fmt.Errorf("agent %s: the body (the system prompt) is empty", f.Name)
	}
	sum := sha256.Sum256(b)
	return AgentFile{
		Name:            f.Name,
		Description:     f.Description,
		Kind:            f.Kind,
		Tiers:           slices.Clone(f.Tiers),
		Requires:        f.Requires,
		BuiltinTools:    slices.Clone(f.BuiltinTools),
		MCP:             slices.Clone(f.MCP),
		UntrustedInputs: slices.Clone(f.UntrustedInputs),
		Subagents:       slices.Clone(f.Subagents),
		Limits:          limits,
		Audience:        f.Audience,
		Memory:          memory,
		Effort:          f.Effort,
		Body:            body,
		Version:         hex.EncodeToString(sum[:]),
	}, nil
}

// splitFrontmatter returns the YAML between the opening "---" line and the
// next line that is exactly "---", and every byte after that second line.
func splitFrontmatter(b []byte) (fm []byte, body string, err error) {
	const open = "---\n"
	if !bytes.HasPrefix(b, []byte(open)) {
		return nil, "", errors.New("agent file must start with a --- frontmatter line")
	}
	rest := b[len(open):]
	for off := 0; off <= len(rest); {
		end := bytes.IndexByte(rest[off:], '\n')
		line := rest[off:]
		if end >= 0 {
			line = rest[off : off+end]
		}
		if string(line) == "---" {
			next := len(rest)
			if end >= 0 {
				next = off + end + 1
			}
			return rest[:off], string(rest[next:]), nil
		}
		if end < 0 {
			break
		}
		off += end + 1
	}
	return nil, "", errors.New("agent file frontmatter is not closed by a --- line")
}

func decodeStrict(b []byte, into any) error {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(into); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("empty document")
		}
		return err
	}
	return nil
}
