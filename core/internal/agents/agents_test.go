package agents

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/ChristopherJMiller/luma/core/internal/protocol"
)

// The shipped agents and models.yaml, two levels up from this package.
const shippedDir = "../../../agents"

func readShipped(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(shippedDir, rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func shipped(t *testing.T) (luma, deepThink AgentFile, m Models) {
	t.Helper()
	var err error
	if luma, err = ParseAgent(readShipped(t, "luma/AGENT.md")); err != nil {
		t.Fatal(err)
	}
	if deepThink, err = ParseAgent(readShipped(t, "deep-think/AGENT.md")); err != nil {
		t.Fatal(err)
	}
	if m, err = ParseModels(readShipped(t, "models.yaml")); err != nil {
		t.Fatal(err)
	}
	return luma, deepThink, m
}

func TestCompileLumaFast(t *testing.T) {
	luma, dt, m := shipped(t)
	spec, err := Compile(luma, map[string]AgentFile{"deep-think": dt}, m, "fast")
	if err != nil {
		t.Fatal(err)
	}
	wantTools := []string{SubagentTool, "WebFetch", "WebSearch", "mcp__luma__forget", "mcp__luma__remember"}
	if !reflect.DeepEqual(spec.Tools, wantTools) {
		t.Errorf("tools = %q, want %q", spec.Tools, wantTools)
	}
	if SubagentTool != "Agent" {
		t.Errorf("SubagentTool = %q, want Agent (Spike S)", SubagentTool)
	}
	wantBinding := protocol.TierBinding{
		Model: "claude-haiku-5-5", Effort: "low", CacheTTL: "1h",
		AutoCompactWindow: 100000, AutoCompactPct: 85, PDFMaxPages: 20,
	}
	if spec.TierBinding != wantBinding {
		t.Errorf("tier_binding = %+v, want %+v", spec.TierBinding, wantBinding)
	}
	sub, ok := spec.Subagents["deep-think"]
	if !ok {
		t.Fatalf("subagents = %v, want deep-think", spec.Subagents)
	}
	if sub.Model != "claude-sonnet-5-5" || sub.Effort != "medium" {
		t.Errorf("deep-think = %s/%s, want claude-sonnet-5-5/medium", sub.Model, sub.Effort)
	}
	if want := (protocol.AgentLimits{MaxTurns: 30, MaxToolCalls: 60, WebSearchPerTurn: 3, WebSearchPerSession: 15}); spec.Limits != want {
		t.Errorf("limits = %+v, want %+v", spec.Limits, want)
	}
	if spec.ID != "luma" || spec.Memory != "person" {
		t.Errorf("id/memory = %q/%q, want luma/person", spec.ID, spec.Memory)
	}
	if !reflect.DeepEqual(spec.BuiltinTools, []string{"WebSearch", "WebFetch"}) {
		t.Errorf("builtin_tools = %q", spec.BuiltinTools)
	}
	if !reflect.DeepEqual(spec.MCP, []string{"memory.remember", "memory.forget"}) {
		t.Errorf("mcp = %q", spec.MCP)
	}
	if !reflect.DeepEqual(spec.UntrustedInputs, []string{"WebSearch", "WebFetch"}) {
		t.Errorf("untrusted_inputs = %q", spec.UntrustedInputs)
	}
}

func TestCompileLumaSmartAndDeepBindings(t *testing.T) {
	luma, dt, m := shipped(t)
	subs := map[string]AgentFile{"deep-think": dt}
	for tier, want := range map[string]protocol.TierBinding{
		"smart": {Model: "claude-sonnet-5-5", Effort: "medium", CacheTTL: "1h", AutoCompactWindow: 200000, AutoCompactPct: 85, PDFMaxPages: 100},
		"deep":  {Model: "claude-opus-5-5", Effort: "medium", CacheTTL: "1h", AutoCompactWindow: 200000, AutoCompactPct: 85, PDFMaxPages: 100},
	} {
		spec, err := Compile(luma, subs, m, tier)
		if err != nil {
			t.Fatalf("%s: %v", tier, err)
		}
		if spec.TierBinding != want {
			t.Errorf("%s: tier_binding = %+v, want %+v", tier, spec.TierBinding, want)
		}
	}
}

func TestCompileDeepThink(t *testing.T) {
	_, dt, m := shipped(t)
	spec, err := Compile(dt, nil, m, "smart")
	if err != nil {
		t.Fatal(err)
	}
	if spec.ID != "deep-think" || spec.Memory != "none" {
		t.Errorf("id/memory = %q/%q, want deep-think/none", spec.ID, spec.Memory)
	}
	if len(spec.Tools) != 0 || len(spec.Subagents) != 0 {
		t.Errorf("tools/subagents = %q/%v, want none", spec.Tools, spec.Subagents)
	}
	if spec.TierBinding.Model != "claude-sonnet-5-5" || spec.TierBinding.Effort != "medium" {
		t.Errorf("tier_binding = %+v", spec.TierBinding)
	}
	// A subagent offered only tier smart is not bindable to fast.
	if _, err := Compile(dt, nil, m, "fast"); err == nil {
		t.Error("deep-think compiled for tier fast, which it does not offer")
	}
}

func TestSubagentToolsAreCompiledAndNotLeaked(t *testing.T) {
	luma, dt, m := shipped(t)
	spec, err := Compile(luma, map[string]AgentFile{"deep-think": dt}, m, "fast")
	if err != nil {
		t.Fatal(err)
	}
	sub := spec.Subagents["deep-think"]
	if sub.Tools == nil || len(sub.Tools) != 0 {
		t.Errorf("deep-think tools = %#v, want empty", sub.Tools)
	}
	if sub.Prompt != dt.Body || sub.Description != dt.Description {
		t.Error("deep-think prompt/description are not the file's body/description")
	}
}

func TestMissingSubagentRejected(t *testing.T) {
	luma, _, m := shipped(t)
	if _, err := Compile(luma, nil, m, "fast"); err == nil || !strings.Contains(err.Error(), "deep-think") {
		t.Errorf("err = %v, want one naming deep-think", err)
	}
}

const minimalAgent = `---
name: tiny
description: A tiny agent.
kind: agent
tiers: [fast]
memory: none
---
Be tiny.
`

func TestUnknownFrontmatterKeyRejected(t *testing.T) {
	_, err := ParseAgent([]byte(strings.Replace(minimalAgent, "memory: none", "memory: none\nbogus_key: 1", 1)))
	if err == nil || !strings.Contains(err.Error(), "bogus_key") {
		t.Errorf("err = %v, want one naming bogus_key", err)
	}
	if _, err := ParseAgent([]byte(minimalAgent)); err != nil {
		t.Errorf("minimal agent rejected: %v", err)
	}
}

func TestParseAgentFraming(t *testing.T) {
	for name, in := range map[string]string{
		"no frontmatter": "just a body\n",
		"unterminated":   "---\nname: x\n",
		"empty":          "",
	} {
		if _, err := ParseAgent([]byte(in)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	for name, mut := range map[string]func(string) string{
		"no name":      func(s string) string { return strings.Replace(s, "name: tiny\n", "", 1) },
		"bad kind":     func(s string) string { return strings.Replace(s, "kind: agent", "kind: robot", 1) },
		"no tiers":     func(s string) string { return strings.Replace(s, "tiers: [fast]\n", "", 1) },
		"bad memory":   func(s string) string { return strings.Replace(s, "memory: none", "memory: elephant", 1) },
		"bad name":     func(s string) string { return strings.Replace(s, "name: tiny", "name: Ti/ny", 1) },
		"empty prompt": func(s string) string { return strings.Split(s, "Be tiny.")[0] },
	} {
		if _, err := ParseAgent([]byte(mut(minimalAgent))); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestTierNotInModelsRejected(t *testing.T) {
	luma, dt, _ := shipped(t)
	m, err := ParseModels([]byte(`version: 1
helper_model: claude-haiku-5-5
tiers:
  smart: {harness: claude-agent-sdk, model: claude-sonnet-5-5, effort: medium, cache_ttl: 1h, auto_compact_window: 200000, auto_compact_pct: 85, pdf_max_pages: 100, capabilities: {tools_max: 64, vision: true}}
`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Compile(luma, map[string]AgentFile{"deep-think": dt}, m, "fast")
	if err == nil || !strings.Contains(err.Error(), "fast") {
		t.Errorf("err = %v, want one naming tier fast", err)
	}
	if _, err := Compile(luma, map[string]AgentFile{"deep-think": dt}, m, "nonsense"); err == nil {
		t.Error("tier nonsense accepted")
	}
}

func TestBindingBelowRequiresRejected(t *testing.T) {
	luma, dt, _ := shipped(t)
	const tier = `  fast: {harness: claude-agent-sdk, model: claude-haiku-5-5, effort: low, cache_ttl: 1h, auto_compact_window: 100000, auto_compact_pct: 85, pdf_max_pages: 20, capabilities: {tools_max: %s, vision: %s}}
  smart: {harness: claude-agent-sdk, model: claude-sonnet-5-5, effort: medium, cache_ttl: 1h, auto_compact_window: 200000, auto_compact_pct: 85, pdf_max_pages: 100, capabilities: {tools_max: 64, vision: true}}
`
	for name, c := range map[string]struct{ tools, vision, want string }{
		"tools_max": {"4", "true", "tools_max"},
		"vision":    {"32", "false", "vision"},
	} {
		src := "version: 1\nhelper_model: claude-haiku-5-5\ntiers:\n" + strings.NewReplacer("%s", "\x00").Replace(tier)
		src = strings.Replace(src, "\x00", c.tools, 1)
		src = strings.Replace(src, "\x00", c.vision, 1)
		m, err := ParseModels([]byte(src))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		_, err = Compile(luma, map[string]AgentFile{"deep-think": dt}, m, "fast")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one naming %s", name, err, c.want)
		}
	}
}

func TestPhase1Limits(t *testing.T) {
	cases := map[string]string{
		"workspace":        "workspace: sandbox",
		"mcp":              "mcp: [memory.remember, calendar.read]",
		"memory":           "memory: person+household",
		"web search limit": "limits: {max_turns: 0, max_tool_calls: 1, web_search_per_turn: 1, web_search_per_session: 1}",
	}
	base := strings.Replace(minimalAgent, "memory: none", "memory: none\nworkspace: none", 1)
	for name, repl := range cases {
		key := strings.SplitN(repl, ":", 2)[0]
		var src string
		switch key {
		case "memory":
			src = strings.Replace(base, "memory: none", repl, 1)
		case "workspace":
			src = strings.Replace(base, "workspace: none", repl, 1)
		default:
			src = strings.Replace(base, "memory: none", "memory: none\n"+repl, 1)
		}
		_, err := ParseAgent([]byte(src))
		if name == "web search limit" {
			if err == nil {
				t.Errorf("%s: want error for non-positive max_turns", name)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "phase 1") {
			t.Errorf("%s: err = %v, want one naming phase 1", name, err)
		}
	}
	_, err := ParseModels([]byte(`version: 1
helper_model: claude-haiku-5-5
tiers:
  fast: {harness: openai-agents, model: claude-haiku-5-5, effort: low, cache_ttl: 1h, pdf_max_pages: 20, capabilities: {tools_max: 32, vision: true}}
`))
	if err == nil || !strings.Contains(err.Error(), "phase 1") || !strings.Contains(err.Error(), "claude-agent-sdk") {
		t.Errorf("harness: err = %v, want one naming phase 1 and claude-agent-sdk", err)
	}
}

func TestParseModelsStrict(t *testing.T) {
	for name, src := range map[string]string{
		"unknown key": "version: 1\nhelper_model: h\nwat: 1\ntiers: {}\n",
		"no helper":   "version: 1\ntiers: {fast: {harness: claude-agent-sdk, model: m, effort: low, cache_ttl: 1h, pdf_max_pages: 1}}\n",
		"bad version": "version: 2\nhelper_model: h\ntiers: {fast: {harness: claude-agent-sdk, model: m, effort: low, cache_ttl: 1h, pdf_max_pages: 1}}\n",
		"bad ttl":     "version: 1\nhelper_model: h\ntiers: {fast: {harness: claude-agent-sdk, model: m, effort: low, cache_ttl: 2h, pdf_max_pages: 1}}\n",
		"bad effort":  "version: 1\nhelper_model: h\ntiers: {fast: {harness: claude-agent-sdk, model: m, effort: wild, cache_ttl: 1h, pdf_max_pages: 1}}\n",
		"bad tier":    "version: 1\nhelper_model: h\ntiers: {huge: {harness: claude-agent-sdk, model: m, effort: low, cache_ttl: 1h, pdf_max_pages: 1}}\n",
	} {
		if _, err := ParseModels([]byte(src)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestVersionIsContentHash(t *testing.T) {
	b := readShipped(t, "luma/AGENT.md")
	sum := sha256.Sum256(b)
	want := hex.EncodeToString(sum[:])
	a, err := ParseAgent(b)
	if err != nil {
		t.Fatal(err)
	}
	if a.Version != want {
		t.Errorf("version = %s, want %s", a.Version, want)
	}
	changed := append(append([]byte{}, b...), '\n')
	a2, err := ParseAgent(changed)
	if err != nil {
		t.Fatal(err)
	}
	if a2.Version == a.Version {
		t.Error("a changed file kept its version")
	}
	luma, dt, m := shipped(t)
	spec, err := Compile(luma, map[string]AgentFile{"deep-think": dt}, m, "fast")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Version != want {
		t.Errorf("spec version = %s, want %s", spec.Version, want)
	}
}

func TestSystemPromptIsBodyVerbatim(t *testing.T) {
	const src = "---\nname: tiny\ndescription: d\nkind: agent\ntiers: [fast]\nmemory: none\n---\n  Leading spaces.\n\n\nTwo blank lines, trailing newline x2.\n\n"
	a, err := ParseAgent([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	const want = "  Leading spaces.\n\n\nTwo blank lines, trailing newline x2.\n\n"
	if a.Body != want {
		t.Errorf("body = %q, want %q", a.Body, want)
	}
	_, _, m := shipped(t)
	spec, err := Compile(a, nil, m, "fast")
	if err != nil {
		t.Fatal(err)
	}
	if spec.SystemPrompt != want {
		t.Errorf("system prompt = %q, want %q", spec.SystemPrompt, want)
	}
}

func TestLumaBodyStatic(t *testing.T) {
	luma, _, _ := shipped(t)
	if strings.Contains(luma.Body, "{{") {
		t.Error("luma body has a template placeholder")
	}
	for _, bad := range []string{"2026", "2027", "today's date"} {
		if strings.Contains(strings.ToLower(luma.Body), bad) {
			t.Errorf("luma body mentions %q: the prompt must be static", bad)
		}
	}
	for _, need := range []string{"Luma", "powered by Claude", "AI", "<luma-context>", "<luma-now>", "remember", "deep-think"} {
		if !strings.Contains(luma.Body, need) {
			t.Errorf("luma body lacks %q", need)
		}
	}
}

type goldenTask struct {
	ID    string `yaml:"id"`
	Agent string `yaml:"agent"`
	Tier  string `yaml:"tier"`
	Stub  []struct {
		Title   string `yaml:"title"`
		URL     string `yaml:"url"`
		Snippet string `yaml:"snippet"`
	} `yaml:"stub_web_search"`
	Turns []struct {
		User   string `yaml:"user"`
		Expect struct {
			TextMatches       string   `yaml:"text_matches"`
			ToolsCalled       []string `yaml:"tools_called"`
			MaxCostUSD        float64  `yaml:"max_cost_usd"`
			RememberedMatches *string  `yaml:"remembered_matches"`
		} `yaml:"expect"`
	} `yaml:"turns"`
}

func loadGolden(t *testing.T, agent string) []goldenTask {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(shippedDir, agent, "evals", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var out []goldenTask
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		dec := yaml.NewDecoder(strings.NewReader(string(b)))
		dec.KnownFields(true)
		var g goldenTask
		if err := dec.Decode(&g); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if strings.TrimSuffix(filepath.Base(f), ".yaml") != g.ID {
			t.Errorf("%s: id %q does not match the file name", f, g.ID)
		}
		if g.Agent != agent || len(g.Turns) == 0 {
			t.Errorf("%s: agent %q / %d turns", f, g.Agent, len(g.Turns))
		}
		out = append(out, g)
	}
	return out
}

func TestGoldenTaskCount(t *testing.T) {
	count := func(tasks []goldenTask, tier string) int {
		n := 0
		for _, g := range tasks {
			if g.Tier == tier {
				n++
			}
		}
		return n
	}
	luma := loadGolden(t, "luma")
	if n := count(luma, "fast"); n < 10 || n > 20 {
		t.Errorf("luma fast golden tasks = %d, want 10..20", n)
	}
	if n := count(luma, "smart"); n != 3 {
		t.Errorf("luma smart golden tasks = %d, want 3", n)
	}
	if n := len(loadGolden(t, "deep-think")); n != 2 {
		t.Errorf("deep-think golden tasks = %d, want 2", n)
	}
	// Coverage the plan asks for: search with citation, remember, forget,
	// deep-think and a refusal to save secrets.
	var search, remember, forget, deep, secret bool
	for _, g := range luma {
		e := g.Turns[0].Expect
		for _, tc := range e.ToolsCalled {
			switch tc {
			case "WebSearch":
				search = search || len(g.Stub) > 0
			case "mcp__luma__remember":
				remember = true
			case "mcp__luma__forget":
				forget = true
			case "deep-think":
				deep = true
			}
		}
		if strings.Contains(g.ID, "refuse-secret") && len(e.ToolsCalled) == 0 {
			secret = true
		}
	}
	for name, ok := range map[string]bool{"stubbed search": search, "remember": remember, "forget": forget, "deep-think": deep, "refuse secret": secret} {
		if !ok {
			t.Errorf("no luma golden task covers %s", name)
		}
	}
	for _, g := range luma {
		if strings.Contains(strings.ToLower(g.Turns[0].User), "image") {
			t.Errorf("%s: image golden tasks are out of phase 1 (S10)", g.ID)
		}
	}
}
