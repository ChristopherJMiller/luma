package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func reconcileEnv(t *testing.T) (env map[string]string, days *[]string) {
	t.Helper()
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/workspaces") {
			_, _ = w.Write([]byte(`{"data":[{"id":"wrkspc_i","name":"luma-interactive"},{"id":"wrkspc_c","name":"luma-ci"}],"has_more":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[],"has_more":false}`))
	}))
	t.Cleanup(admin.Close)
	days = new([]string)
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Day string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		*days = append(*days, b.Day)
		_, _ = w.Write([]byte(`{"workspaces":[]}`))
	}))
	t.Cleanup(core.Close)
	dir := t.TempDir()
	key, tok := filepath.Join(dir, "key"), filepath.Join(dir, "tok")
	for _, p := range []string{key, tok} {
		if err := os.WriteFile(p, []byte("fake"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return map[string]string{
		"LUMA_ANTHROPIC_ADMIN_KEY_FILE": key, "LUMA_CORE_HTTP_URL": core.URL, "LUMA_CORE_TOKEN_FILE": tok,
		"LUMA_RECONCILE_WORKSPACES": "luma-interactive, luma-ci", "LUMA_ANTHROPIC_API_URL": admin.URL,
	}, days
}

func TestUsageReconcileDay(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 10, 15, 7, 30, 0, 0, time.UTC) }
	for name, c := range map[string]struct {
		args []string
		want string
	}{"default is yesterday UTC": {nil, "2026-10-14"}, "--day": {[]string{"--day", "2026-09-30"}, "2026-09-30"}} {
		t.Run(name, func(t *testing.T) {
			e, days := reconcileEnv(t)
			if err := runUsageReconcile(bg, c.args, func(k string) string { return e[k] }, now); err != nil {
				t.Fatal(err)
			}
			if len(*days) != 1 || (*days)[0] != c.want {
				t.Errorf("posted days = %v, want %s", *days, c.want)
			}
		})
	}
}

func TestUsageReconcileUsageErrors(t *testing.T) {
	now := time.Now
	e, _ := reconcileEnv(t)
	get := func(k string) string { return e[k] }
	if err := runUsageReconcile(bg, []string{"--day", "yesterday"}, get, now); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("bad --day: %v", err)
	}
	if err := runUsageReconcile(bg, nil, func(string) string { return "" }, now); err == nil {
		t.Error("an empty environment must fail")
	}
	var out, errb bytes.Buffer
	if code := run(bg, []string{"usage-reconcile", "--bogus"}, get, &out, &errb); code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}
