package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const modelsHaikuHelper = `version: 1
helper_model: claude-haiku-5-5
tiers:
  fast:  {harness: claude-agent-sdk, model: claude-haiku-5-5,  effort: low,    cache_ttl: 1h, auto_compact_window: 100000, auto_compact_pct: 85, pdf_max_pages: 20,  capabilities: {tools_max: 32, vision: true}}
  smart: {harness: claude-agent-sdk, model: claude-sonnet-5-5, effort: medium, cache_ttl: 1h, auto_compact_window: 200000, auto_compact_pct: 85, pdf_max_pages: 100, capabilities: {tools_max: 64, vision: true}}
  deep:  {harness: claude-agent-sdk, model: claude-opus-5-5,   effort: medium, cache_ttl: 1h, auto_compact_window: 200000, auto_compact_pct: 85, pdf_max_pages: 100, capabilities: {tools_max: 64, vision: true}}
`

// writeFlat writes the shipped agents as flat ConfigMap keys.
func writeFlat(t *testing.T, dir, models string) {
	t.Helper()
	write(t, dir, "luma.agent.md", string(readShipped(t, "luma/AGENT.md")))
	write(t, dir, "deep-think.agent.md", string(readShipped(t, "deep-think/AGENT.md")))
	write(t, dir, "models.yaml", models)
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadCatalogFlatAndNested(t *testing.T) {
	flat := t.TempDir()
	writeFlat(t, flat, modelsHaikuHelper)
	// kubelet debris must be ignored.
	if err := os.Mkdir(filepath.Join(flat, "..2026_10_21"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, flat, ".hidden.agent.md", "garbage")

	for name, dir := range map[string]string{"flat": flat, "nested": shippedDir} {
		c, err := LoadCatalog(dir)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, ok := c.Agent("luma"); !ok {
			t.Errorf("%s: no luma", name)
		}
		if a, ok := c.Agent("deep-think"); !ok || a.Kind != "subagent" {
			t.Errorf("%s: deep-think = %+v, %v", name, a, ok)
		}
		if _, ok := c.Agent("nope"); ok {
			t.Errorf("%s: found an agent that is not there", name)
		}
		if got := ids(c.List()); got != "deep-think,luma" {
			t.Errorf("%s: list = %s, want deep-think,luma (sorted)", name, got)
		}
		if c.Models().HelperModel != "claude-haiku-5-5" {
			t.Errorf("%s: helper_model = %q", name, c.Models().HelperModel)
		}
		spec, err := c.Compile("luma", "smart")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if spec.TierBinding.Model != "claude-sonnet-5-5" || spec.Subagents["deep-think"].Model != "claude-sonnet-5-5" {
			t.Errorf("%s: compiled smart = %+v", name, spec.TierBinding)
		}
	}
}

func ids(as []AgentFile) string {
	var s []string
	for _, a := range as {
		s = append(s, a.Name)
	}
	return strings.Join(s, ",")
}

func TestLoadCatalogRejects(t *testing.T) {
	for name, mut := range map[string]func(dir string){
		"no models.yaml": func(dir string) { _ = os.Remove(filepath.Join(dir, "models.yaml")) },
		"name vs file":   func(dir string) { write(t, dir, "other.agent.md", string(readShipped(t, "deep-think/AGENT.md"))) },
		"missing subagent": func(dir string) {
			_ = os.Remove(filepath.Join(dir, "deep-think.agent.md"))
		},
		"agent used as subagent": func(dir string) {
			write(t, dir, "deep-think.agent.md", strings.Replace(string(readShipped(t, "deep-think/AGENT.md")), "kind: subagent", "kind: agent", 1))
		},
		"tier not in models": func(dir string) {
			write(t, dir, "models.yaml", strings.Replace(modelsHaikuHelper, "  deep: ", "  #deep:", 1))
		},
		"broken agent": func(dir string) { write(t, dir, "luma.agent.md", "not an agent") },
	} {
		dir := t.TempDir()
		writeFlat(t, dir, modelsHaikuHelper)
		mut(dir)
		if _, err := LoadCatalog(dir); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if _, err := LoadCatalog(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("absent dir: want error")
	}
}
