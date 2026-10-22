package auth_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherJMiller/luma/core/internal/auth"
)

func TestRegistryEmailCaseInsensitive(t *testing.T) {
	t.Parallel()
	reg := mustParseRegistry(t, `people:
  p1:
    display_name: Person One
    tier: admin
    household: true
    emails:
      google: [a@example.com]
      microsoft: [B@Example.NET]
  p2:
    display_name: Person Two
    tier: family
    emails:
      google: [k@example.com]
`)
	want := auth.Person{ID: "p1", DisplayName: "Person One", Tier: "admin", Household: true}
	for _, c := range []struct{ connector, email string }{
		{"google", "A@Example.com"},
		{"google", "a@example.com"},
		{"microsoft", "b@example.net"},
		{"microsoft", "B@EXAMPLE.NET"},
	} {
		if p, ok := reg.Lookup(c.connector, c.email); !ok || p != want {
			t.Errorf("Lookup(%q, %q) = %+v, %v; want p1", c.connector, c.email, p, ok)
		}
	}
	for _, c := range []struct{ connector, email string }{
		{"microsoft", "A@Example.com"}, // case-folding never crosses connectors
		{"google", "B@Example.NET"},
		{"Google", "a@example.com"}, // connector ids are exact
		{"", "a@example.com"},
		{"google", ""},
		{"google", " a@example.com"},
		{"google", "K@example.com"}, // KELVIN SIGN folds to k only under Unicode rules
	} {
		if p, ok := reg.Lookup(c.connector, c.email); ok {
			t.Errorf("Lookup(%q, %q) = %+v; want no match", c.connector, c.email, p)
		}
	}
	if p, ok := reg.Get("p2"); !ok || p != (auth.Person{ID: "p2", DisplayName: "Person Two", Tier: "family"}) {
		t.Errorf("Get(p2) = %+v, %v", p, ok)
	}
	if _, ok := reg.Get("nobody"); ok {
		t.Error("Get(nobody) found someone")
	}
}

func TestLoadRegistryRejectsBadFiles(t *testing.T) {
	t.Parallel()
	person := func(body string) string {
		return "people:\n  p1:\n    display_name: One\n    tier: admin\n" + body
	}
	bad := map[string]string{
		"empty file":              "",
		"no people":               "people: {}\n",
		"unknown top key":         "people:\n  p1: {display_name: One, tier: admin}\nextra: 1\n",
		"unknown person key":      person("    role: owner\n"),
		"bad tier":                "people:\n  p1: {display_name: One, tier: root}\n",
		"no tier":                 "people:\n  p1: {display_name: One}\n",
		"no display name":         "people:\n  p1: {tier: family}\n",
		"bad id":                  "people:\n  P One: {display_name: One, tier: family}\n",
		"empty connector":         person("    emails: {\"\": [a@example.com]}\n"),
		"not an address":          person("    emails: {google: [nobody]}\n"),
		"empty address":           person("    emails: {google: [\"\"]}\n"),
		"spaced address":          person("    emails: {google: [\"a @example.com\"]}\n"),
		"duplicate in one person": person("    emails: {google: [a@example.com, A@example.com]}\n"),
		"duplicate across people": "people:\n  p1: {display_name: One, tier: admin, emails: {google: [a@example.com]}}\n" +
			"  p2: {display_name: Two, tier: family, emails: {google: [A@Example.com]}}\n",
		"not yaml": "people: [\n",
	}
	for name, content := range bad {
		path := filepath.Join(t.TempDir(), "registry.yaml")
		writeFile(t, path, content)
		if _, err := auth.LoadRegistry(path); err == nil {
			t.Errorf("%s: LoadRegistry accepted it", name)
		}
	}
	if _, err := auth.LoadRegistry(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("LoadRegistry accepted a missing file")
	}
	// The same address under two connectors is two separate pairs: fine.
	mustParseRegistry(t, person("    emails: {google: [a@example.com], microsoft: [a@example.com]}\n"))
}

type reload struct {
	reg *auth.Registry
	err error
}

// Reload errors are logged, so they never quote an address from the file.
func TestRegistryErrorsNeverQuoteAddresses(t *testing.T) {
	t.Parallel()
	// yaml.v3 quotes a value of up to 10 bytes whole, and the first 7 bytes
	// of a longer one.
	// Unknown keys and duplicate keys are named unquoted or %q-quoted.
	for name, content := range map[string]string{
		"short address where a list goes":   "people:\n  p1: {display_name: One, tier: admin, emails: {google: me@ex.io}}\n",
		"long address where a list goes":    "people:\n  p1: {display_name: One, tier: admin, emails: {google: secret.person@example.org}}\n",
		"duplicate address":                 "people:\n  p1: {display_name: One, tier: admin, emails: {google: [me@ex.io, ME@ex.io]}}\n",
		"malformed address":                 "people:\n  p1: {display_name: One, tier: admin, emails: {google: [\"me@ex.io x\"]}}\n",
		"address as a person id":            "people:\n  me@ex.io: {display_name: One, tier: admin}\n",
		"reversed emails map":               "people:\n  p1: {display_name: One, tier: admin, emails: {me@ex.io: [google]}}\n",
		"unknown key named like an address": "people:\n  p1: {display_name: One, tier: admin, me@ex.io: 1}\n",
		"unknown top key like an address":   "people:\n  p1: {display_name: One, tier: admin}\nme@ex.io: 1\n",
		"duplicate person key, an address":  "people:\n  me@ex.io: {display_name: One, tier: admin}\n  me@ex.io: {display_name: Two, tier: admin}\n",
		"address as the tier":               "people:\n  p1: {display_name: One, tier: me@ex.io}\n",
		"address as a connector, mapped":    "people:\n  p1: {display_name: One, tier: admin, emails: {me@ex.io: {a: b}}}\n",
	} {
		path := filepath.Join(t.TempDir(), "registry.yaml")
		writeFile(t, path, content)
		_, err := auth.LoadRegistry(path)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		msg := strings.TrimPrefix(err.Error(), "registry "+path+": ")
		if strings.Contains(msg, "@") || strings.Contains(msg, "secret") || strings.Contains(msg, "ex.io") {
			t.Errorf("%s: error quotes the address: %v", name, err)
		}
	}
	// What is left still says where and what.
	path := filepath.Join(t.TempDir(), "registry.yaml")
	writeFile(t, path, "people:\n  p1: {display_name: One, tier: admin, me@ex.io: 1}\n")
	if _, err := auth.LoadRegistry(path); err == nil || !strings.HasSuffix(err.Error(), ": yaml: line 2: unknown key") {
		t.Errorf("unknown key error = %v, want it to end \": yaml: line 2: unknown key\"", err)
	}
}

// A bad file is a failed reload (WatchRegistry keeps the old registry and
// does not call onChange), and the next good file is delivered.
func TestWatchRegistryKeepsOldOnBadFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "registry.yaml")
	writeFile(t, path, registryYAML)
	got := make(chan reload, 8)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- auth.WatchRegistryReloads(ctx, path, func(r *auth.Registry, err error) { got <- reload{r, err} })
	}()
	next := func() reload {
		t.Helper()
		select {
		case r := <-got:
			return r
		case <-time.After(5 * time.Second):
			t.Fatal("no reload")
			return reload{}
		}
	}

	// The reload right after the watch is registered.
	if r := next(); r.err != nil {
		t.Fatalf("initial reload: %v", r.err)
	} else if _, ok := r.reg.Lookup("google", "a@example.com"); !ok {
		t.Fatal("the initial reload lost p1")
	}

	writeFile(t, path, "people: {p1: {display_name: One, tier: wizard}}\n")
	if r := next(); r.err == nil || r.reg != nil {
		t.Fatalf("a bad file reloaded as %+v, %v; want an error and no registry", r.reg, r.err)
	}

	// One write can debounce into two reloads on a slow runner (the tmp-file
	// create and the rename land either side of the 20ms window), so a second
	// failed reload of the bad file may still be in flight. Skip failures
	// until the good file arrives; next() fails the test at its deadline.
	writeFile(t, path, registryWithoutP1)
	r := next()
	for r.err != nil {
		r = next()
	}
	if _, ok := r.reg.Lookup("google", "c@example.com"); !ok {
		t.Fatal("the good file after the bad one was not delivered")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("watch returned %v after cancel, want nil", err)
	}

	// The public wrapper calls onChange only for good loads.
	writeFile(t, path, "not: [valid\n")
	called := false
	wctx, wcancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer wcancel()
	if err := auth.WatchRegistry(wctx, path, func(*auth.Registry) { called = true }); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("WatchRegistry called onChange for a bad file")
	}
}

func TestWatchRegistryMissingDir(t *testing.T) {
	t.Parallel()
	err := auth.WatchRegistry(t.Context(), filepath.Join(t.TempDir(), "nope", "registry.yaml"), func(*auth.Registry) {})
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("WatchRegistry on a missing dir = %v, want an error naming it", err)
	}
}
