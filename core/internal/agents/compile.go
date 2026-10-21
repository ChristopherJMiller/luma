package agents

import (
	"fmt"
	"slices"
	"strings"

	"github.com/ChristopherJMiller/luma/core/internal/protocol"
)

// SubagentTool is the SDK's tool name for launching a subagent. Spike S found
// it is "Agent" in the pinned SDK, not "Task". hack/check-versions.sh compares
// this string with SUBAGENT_TOOL in conductor/src/spike-results.ts. The
// compiled tools list carries it (the SDK needs it allowed); the UI and
// events name the call by the subagent (`deep-think`), which the conductor
// translates (S13), so the web never learns this constant.
const SubagentTool = "Agent"

// mcpTool maps a C5 mcp entry ("memory.remember") to the SDK tool name of the
// conductor's single MCP server, `luma` ("mcp__luma__remember").
func mcpTool(entry string) string {
	_, name, _ := strings.Cut(entry, ".")
	return "mcp__luma__" + name
}

// Compile binds an agent to a tier and produces the frozen protocol.AgentSpec.
// subs holds the agents named in a.Subagents.
func Compile(a AgentFile, subs map[string]AgentFile, m Models, tier string) (protocol.AgentSpec, error) {
	binding, err := bind(a, m, tier)
	if err != nil {
		return protocol.AgentSpec{}, err
	}

	tools := slices.Clone(a.BuiltinTools)
	for _, e := range a.MCP {
		tools = append(tools, mcpTool(e))
	}
	if len(a.Subagents) > 0 {
		tools = append(tools, SubagentTool)
	}
	// Bytewise: the frozen tool order is part of the cache prefix.
	slices.Sort(tools)
	tools = slices.Compact(tools)

	specs := map[string]protocol.SubagentSpec{}
	for _, name := range a.Subagents {
		sub, ok := subs[name]
		if !ok {
			return protocol.AgentSpec{}, fmt.Errorf("agent %s: subagent %q is not defined", a.Name, name)
		}
		if sub.Kind != "subagent" {
			return protocol.AgentSpec{}, fmt.Errorf("agent %s: %q is not kind: subagent", a.Name, name)
		}
		if len(sub.Subagents) > 0 {
			return protocol.AgentSpec{}, fmt.Errorf("subagent %s: nested subagents are not available in phase 1", name)
		}
		// The parent's tier if the subagent offers it, else its first.
		subTier := sub.Tiers[0]
		if slices.Contains(sub.Tiers, tier) {
			subTier = tier
		}
		sb, err := bind(sub, m, subTier)
		if err != nil {
			return protocol.AgentSpec{}, err
		}
		effort := sub.Effort
		if effort == "" {
			effort = sb.Effort
		}
		subTools := slices.Clone(sub.BuiltinTools)
		for _, e := range sub.MCP {
			subTools = append(subTools, mcpTool(e))
		}
		slices.Sort(subTools)
		specs[name] = protocol.SubagentSpec{
			Description: sub.Description,
			Prompt:      sub.Body,
			Model:       sb.Model,
			Effort:      effort,
			Tools:       nonNil(subTools),
		}
	}

	return protocol.AgentSpec{
		ID:              a.Name,
		Version:         a.Version,
		SystemPrompt:    a.Body,
		Tools:           nonNil(tools),
		BuiltinTools:    nonNil(slices.Clone(a.BuiltinTools)),
		MCP:             nonNil(slices.Clone(a.MCP)),
		UntrustedInputs: nonNil(slices.Clone(a.UntrustedInputs)),
		Subagents:       specs,
		Limits: protocol.AgentLimits{
			MaxTurns:            a.Limits.MaxTurns,
			MaxToolCalls:        a.Limits.MaxToolCalls,
			WebSearchPerTurn:    a.Limits.WebSearchPerTurn,
			WebSearchPerSession: a.Limits.WebSearchPerSession,
		},
		Memory:      a.Memory,
		TierBinding: binding,
	}, nil
}

// bind checks that the agent offers the tier, models.yaml defines it, and its
// capabilities meet the agent's requires.
func bind(a AgentFile, m Models, tier string) (protocol.TierBinding, error) {
	if !slices.Contains(a.Tiers, tier) {
		return protocol.TierBinding{}, fmt.Errorf("agent %s does not offer tier %q (offers %s)", a.Name, tier, strings.Join(a.Tiers, ", "))
	}
	t, ok := m.Tiers[tier]
	if !ok {
		return protocol.TierBinding{}, fmt.Errorf("agent %s: tier %q is not in models.yaml", a.Name, tier)
	}
	if t.Capabilities.ToolsMax < a.Requires.ToolsMax {
		return protocol.TierBinding{}, fmt.Errorf("agent %s: tier %s has tools_max %d, below the required %d", a.Name, tier, t.Capabilities.ToolsMax, a.Requires.ToolsMax)
	}
	if a.Requires.Vision && !t.Capabilities.Vision {
		return protocol.TierBinding{}, fmt.Errorf("agent %s: tier %s lacks vision, which the agent requires", a.Name, tier)
	}
	return protocol.TierBinding{
		Model:             t.Model,
		Effort:            t.Effort,
		CacheTTL:          t.CacheTTL,
		AutoCompactWindow: t.AutoCompactWindow,
		AutoCompactPct:    t.AutoCompactPct,
		PDFMaxPages:       t.PDFMaxPages,
	}, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
