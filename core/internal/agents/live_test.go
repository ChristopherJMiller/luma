package agents

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func withHelper(helper string) string {
	return strings.Replace(modelsHaikuHelper, "helper_model: claude-haiku-5-5", "helper_model: "+helper, 1)
}

// startWatch runs live.Watch and returns once the watcher has registered and
// done its first reload (the reload that follows registration). It returns a
// stop function that cancels and waits for Watch to return nil.
func startWatch(t *testing.T, live *Live, dir string) (stop func()) {
	t.Helper()
	var reloads atomic.Int32
	live.OnReload(func(error) { reloads.Add(1) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- live.Watch(ctx, dir) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Watch returned %v after cancel, want nil", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("Watch did not return after cancel")
			}
		})
	}
	t.Cleanup(stop)
	eventually(t, "the watcher's first reload", func() bool { return reloads.Load() >= 1 })
	return stop
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestApplyKeepsOldOnError(t *testing.T) {
	dir := t.TempDir()
	writeFlat(t, dir, modelsHaikuHelper)
	first, err := LoadCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	live := NewLive(first)

	write(t, dir, "models.yaml", "version: 1\nhelper_model: [broken")
	c, err := LoadCatalog(dir)
	if err == nil {
		t.Fatal("a broken models.yaml loaded")
	}
	live.apply(c, err)
	if live.Load() != first || live.Models().HelperModel != "claude-haiku-5-5" {
		t.Error("a bad reload replaced the catalog")
	}
	if live.LastReloadError() == nil {
		t.Error("LastReloadError is nil after a bad reload")
	}

	write(t, dir, "models.yaml", withHelper("claude-haiku-5-6"))
	c, err = LoadCatalog(dir)
	live.apply(c, err)
	if live.Load() == first || live.Models().HelperModel != "claude-haiku-5-6" {
		t.Errorf("a good reload did not apply: %q", live.Models().HelperModel)
	}
	if live.LastReloadError() != nil {
		t.Errorf("LastReloadError = %v after a good reload", live.LastReloadError())
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
	stop := startWatch(t, live, dir)
	// startWatch's observer only counts; swap in one that sorts by result.
	var errs, oks atomic.Int32
	live.OnReload(func(err error) {
		if err != nil {
			errs.Add(1)
		} else {
			oks.Add(1)
		}
	})

	write(t, dir, "models.yaml", withHelper("claude-haiku-9-9"))
	eventually(t, "helper_model 9-9", func() bool { return live.Models().HelperModel == "claude-haiku-9-9" })
	if oks.Load() < 1 || live.LastReloadError() != nil {
		t.Errorf("ok reloads = %d, last error = %v", oks.Load(), live.LastReloadError())
	}

	// A broken file leaves the old catalog in place and is reported.
	write(t, dir, "models.yaml", "helper_model: [broken")
	eventually(t, "a failed reload", func() bool { return errs.Load() >= 1 })
	if got := live.Models().HelperModel; got != "claude-haiku-9-9" {
		t.Errorf("helper_model = %q after a broken file, want the old claude-haiku-9-9", got)
	}
	if live.LastReloadError() == nil {
		t.Error("LastReloadError is nil after a broken file")
	}

	// And it recovers once the file is good again.
	write(t, dir, "models.yaml", withHelper("claude-haiku-7-7"))
	eventually(t, "helper_model 7-7", func() bool { return live.Models().HelperModel == "claude-haiku-7-7" })
	if live.LastReloadError() != nil {
		t.Errorf("LastReloadError = %v after recovery", live.LastReloadError())
	}
	stop()
}

func TestWatchReloadsOnceAfterRegistering(t *testing.T) {
	dir := t.TempDir()
	writeFlat(t, dir, modelsHaikuHelper)
	c, err := LoadCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	live := NewLive(c)
	// A change that lands between the caller's LoadCatalog and Watch.
	write(t, dir, "models.yaml", withHelper("claude-haiku-6-6"))
	startWatch(t, live, dir)
	if got := live.Models().HelperModel; got != "claude-haiku-6-6" {
		t.Errorf("helper_model = %q after Watch started, want the file's claude-haiku-6-6", got)
	}
}

func TestWatchFollowsKubeletDataSwap(t *testing.T) {
	// A ConfigMap mount is files -> ..data -> ..<timestamp>/; an update
	// creates a new timestamp dir and renames a fresh ..data symlink over the old.
	mount := filepath.Join(t.TempDir(), "mount")
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
	stop := startWatch(t, live, mount)

	publish("gen2", withHelper("claude-haiku-4-4"))
	eventually(t, "helper_model 4-4 after the ..data swap", func() bool { return live.Models().HelperModel == "claude-haiku-4-4" })
	stop()
}

func TestWatchSeesNewAgentSubdirectory(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "models.yaml", modelsHaikuHelper)
	for _, id := range []string{"luma", "deep-think"} {
		if err := os.Mkdir(filepath.Join(dir, id), 0o700); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(dir, id), "AGENT.md", string(readShipped(t, id+"/AGENT.md")))
	}
	c, err := LoadCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	live := NewLive(c)
	startWatch(t, live, dir)

	extra := strings.Replace(string(readShipped(t, "deep-think/AGENT.md")), "name: deep-think", "name: pondering", 1)
	if err := os.Mkdir(filepath.Join(dir, "pondering"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "pondering"), "AGENT.md", extra)
	eventually(t, "the new agent", func() bool { _, ok := live.Agent("pondering"); return ok })

	// An edit inside the new subdirectory is watched too.
	write(t, filepath.Join(dir, "pondering"), "AGENT.md", strings.Replace(extra, "Work through", "Think through", 1))
	eventually(t, "the edit in the new subdirectory", func() bool {
		a, ok := live.Agent("pondering")
		return ok && strings.Contains(a.Body, "Think through")
	})
}

func TestLiveCompileUsesOneSnapshot(t *testing.T) {
	dir := t.TempDir()
	writeFlat(t, dir, modelsHaikuHelper)
	c, err := LoadCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	live := NewLive(c)
	spec, err := live.Compile("luma", "fast")
	if err != nil {
		t.Fatal(err)
	}
	if spec.TierBinding.Model != "claude-haiku-5-5" || spec.Subagents["deep-think"].Model != "claude-sonnet-5-5" {
		t.Errorf("spec = %+v", spec.TierBinding)
	}
	if _, err := live.Compile("nope", "fast"); err == nil {
		t.Error("compiled an agent that is not there")
	}
	if _, err := live.Compile("luma", "bogus"); err == nil {
		t.Error("compiled an unknown tier")
	}
}

func TestCatalogReturnsCopies(t *testing.T) {
	c, err := LoadCatalog(shippedDir)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := c.Agent("luma")
	a.Tiers[0] = "mutated"
	a.MCP[0] = "mutated"
	a.Subagents[0] = "mutated"
	for _, l := range c.List() {
		l.Tiers[0] = "mutated"
	}
	m := c.Models()
	m.Tiers["fast"] = Tier{Model: "mutated"}
	delete(m.Tiers, "smart")

	b, _ := c.Agent("luma")
	if !reflect.DeepEqual(b.Tiers, []string{"fast", "smart", "deep"}) || b.MCP[0] != "memory.remember" || b.Subagents[0] != "deep-think" {
		t.Errorf("mutating a returned agent changed the catalog: %+v", b)
	}
	if got := c.Models(); got.Tiers["fast"].Model != "claude-haiku-5-5" || len(got.Tiers) != 3 {
		t.Errorf("mutating returned models changed the catalog: %+v", got.Tiers)
	}
	if spec, err := c.Compile("luma", "fast"); err != nil || spec.TierBinding.Model != "claude-haiku-5-5" {
		t.Errorf("compile after mutation: %v, %+v", err, spec.TierBinding)
	}
}

func TestAudienceTierValidated(t *testing.T) {
	ok := strings.Replace(minimalAgent, "memory: none", "memory: none\naudience: {tier: admin}", 1)
	if _, err := ParseAgent([]byte(ok)); err != nil {
		t.Errorf("admin audience rejected: %v", err)
	}
	bad := strings.Replace(minimalAgent, "memory: none", "memory: none\naudience: {tier: guest}", 1)
	if _, err := ParseAgent([]byte(bad)); err == nil || !strings.Contains(err.Error(), "guest") {
		t.Errorf("err = %v, want one naming guest", err)
	}
}

func TestWatchErrorsOnMissingDir(t *testing.T) {
	err := NewLive(nil).Watch(context.Background(), filepath.Join(t.TempDir(), "absent"))
	if err == nil || errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want a watch error", err)
	}
}
