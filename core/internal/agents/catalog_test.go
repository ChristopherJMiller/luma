package agents

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestCatalogReloadKeepsOldOnError(t *testing.T) {
	dir := t.TempDir()
	writeFlat(t, dir, modelsHaikuHelper)
	first, err := LoadCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	write(t, dir, "models.yaml", "version: 1\nhelper_model: [broken")

	got := first
	reload(dir, func(c *Catalog) { got = c })
	if got != first {
		t.Error("a bad reload replaced the catalog")
	}
	if got.Models().HelperModel != "claude-haiku-5-5" {
		t.Errorf("helper_model = %q after a bad reload", got.Models().HelperModel)
	}

	write(t, dir, "models.yaml", strings.Replace(modelsHaikuHelper, "helper_model: claude-haiku-5-5", "helper_model: claude-haiku-5-6", 1))
	reload(dir, func(c *Catalog) { got = c })
	if got == first || got.Models().HelperModel != "claude-haiku-5-6" {
		t.Errorf("a good reload did not apply: %q", got.Models().HelperModel)
	}
}

func TestLiveSwapsOnReload(t *testing.T) {
	dir := t.TempDir()
	writeFlat(t, dir, modelsHaikuHelper)
	c, err := LoadCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	live := NewLive(c)
	if live.Load() != c || live.Models().HelperModel != "claude-haiku-5-5" {
		t.Fatal("NewLive does not serve the catalog it was given")
	}
	if _, ok := live.Agent("luma"); !ok || len(live.List()) != 2 {
		t.Fatal("Live does not delegate Agent/List")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- live.Watch(ctx, dir) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Watch did not return after cancel")
		}
	})
	time.Sleep(200 * time.Millisecond) // let the watcher register

	waitFor := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if live.Models().HelperModel == want {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("helper_model = %q after 5 s, want %q", live.Models().HelperModel, want)
	}

	write(t, dir, "models.yaml", strings.Replace(modelsHaikuHelper, "helper_model: claude-haiku-5-5", "helper_model: claude-haiku-9-9", 1))
	waitFor("claude-haiku-9-9")

	// A broken file leaves the old catalog in place.
	write(t, dir, "models.yaml", "helper_model: [broken")
	time.Sleep(3 * time.Second) // longer than the debounce
	if got := live.Models().HelperModel; got != "claude-haiku-9-9" {
		t.Errorf("helper_model = %q after a broken file, want the old claude-haiku-9-9", got)
	}

	// And it recovers once the file is good again.
	write(t, dir, "models.yaml", strings.Replace(modelsHaikuHelper, "helper_model: claude-haiku-5-5", "helper_model: claude-haiku-7-7", 1))
	waitFor("claude-haiku-7-7")
}

func TestWatchFollowsKubeletDataSwap(t *testing.T) {
	// A ConfigMap mount is files -> ..data -> ..<timestamp>/; an update
	// creates a new timestamp dir and renames a fresh ..data symlink over the old.
	root := t.TempDir()
	mount := filepath.Join(root, "mount")
	if err := os.Mkdir(mount, 0o700); err != nil {
		t.Fatal(err)
	}
	publish := func(stamp, models string) {
		t.Helper()
		gen := filepath.Join(mount, ".."+stamp)
		if err := os.Mkdir(gen, 0o700); err != nil {
			t.Fatal(err)
		}
		writeFlat(t, gen, models)
		tmp := filepath.Join(mount, "..data_tmp")
		if err := os.Symlink(".."+stamp, tmp); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(mount, "..data")); err != nil {
			t.Fatal(err)
		}
	}
	publish("gen1", modelsHaikuHelper)
	for _, f := range []string{"luma.agent.md", "deep-think.agent.md", "models.yaml"} {
		if err := os.Symlink(filepath.Join("..data", f), filepath.Join(mount, f)); err != nil {
			t.Fatal(err)
		}
	}
	c, err := LoadCatalog(mount)
	if err != nil {
		t.Fatal(err)
	}
	live := NewLive(c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = live.Watch(ctx, mount) }()
	time.Sleep(200 * time.Millisecond)

	publish("gen2", strings.Replace(modelsHaikuHelper, "helper_model: claude-haiku-5-5", "helper_model: claude-haiku-4-4", 1))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if live.Models().HelperModel == "claude-haiku-4-4" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("helper_model = %q after a ..data swap", live.Models().HelperModel)
}
