package main

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/ChristopherJMiller/luma/test/fakeanthropic"
)

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// start runs the command as a harness would: it waits for the listen line
// on stdout and parses the address out of it. Cleanup stops it and checks
// that it shut down cleanly.
func start(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := run(ctx, args, pw, io.Discard)
		pw.Close()
		done <- err
	}()
	line, err := bufio.NewReader(pr).ReadString('\n')
	if err != nil {
		cancel()
		t.Fatalf("no listen line (%v); run: %v", err, <-done)
	}
	if !regexp.MustCompile(`^fakeanthropic listening on 127\.0\.0\.1:[0-9]+\n$`).MatchString(line) {
		t.Fatalf("listen line %q", line)
	}
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run after shutdown: %v", err)
		}
	})
	return "http://" + strings.TrimSuffix(strings.TrimPrefix(line, "fakeanthropic listening on "), "\n")
}

func ask(t *testing.T, base, body string) map[string]any {
	t.Helper()
	resp, err := http.Post(base+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRunServesScriptsSignsWithTheHexKeyAndRecords(t *testing.T) {
	scripts := writeFile(t, "scripts.json", `{"hi": [{"text": "scripted"}]}`)
	rec := filepath.Join(t.TempDir(), "rec")
	base := start(t, "--listen", "127.0.0.1:0", "--scripts", scripts, "--record-dir", rec, "--binding-key-hex", "00ff10")

	m := ask(t, base, `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"[[fake:hi]]"}]}`)
	mac := hmac.New(sha256.New, []byte{0x00, 0xff, 0x10})
	mac.Write([]byte(`{"messages":[{"content":"[[fake:hi]]","role":"user"}]}`))
	want := []any{
		map[string]any{"type": "thinking", "thinking": "", "signature": base64.StdEncoding.EncodeToString(mac.Sum(nil))},
		map[string]any{"type": "text", "text": "scripted"},
	}
	if !reflect.DeepEqual(m["content"], want) {
		t.Errorf("content\n got %v\nwant %v", m["content"], want)
	}

	m = ask(t, base, `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"plain"}]}`)
	if got := m["content"].([]any)[1].(map[string]any)["text"]; got != defaultText {
		t.Errorf("unmarked reply %q", got)
	}
	for _, f := range []string{"000001.body", "000001.meta.json", "000002.body", "000002.meta.json"} {
		if _, err := os.Stat(filepath.Join(rec, f)); err != nil {
			t.Errorf("record dir: %v", err)
		}
	}
}

func TestRunRejectsBadArguments(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"bad hex key":     {[]string{"--binding-key-hex", "zz"}, "--binding-key-hex"},
		"odd hex key":     {[]string{"--binding-key-hex", "abc"}, "--binding-key-hex"},
		"unknown flag":    {[]string{"--nope"}, "flag provided but not defined"},
		"positional args": {[]string{"--listen", "127.0.0.1:0", "extra"}, "unexpected arguments"},
		"missing scripts": {[]string{"--scripts", filepath.Join(t.TempDir(), "absent.json")}, "no such file"},
	} {
		err := run(context.Background(), tc.args, io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want it to mention %q", name, err, tc.want)
		}
	}
}

func TestLoadScripts(t *testing.T) {
	got, err := loadScripts(writeFile(t, "ok.json", `{"a": [{"text": "x", "no_thinking": true}, {"status": 529}]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]fakeanthropic.Step{"a": {{Text: "x", NoThinking: true}, {Status: 529}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("loaded %+v, want %+v", got, want)
	}
	if got, err := loadScripts(""); got != nil || err != nil {
		t.Errorf("no path: %v, %v", got, err)
	}
	for name, tc := range map[string]struct{ content, want string }{
		"unknown field":         {`{"a": [{"txet": "typo"}]}`, "unknown field"},
		"second object":         {`{"a": []} {"b": []}`, "trailing data"},
		"stray closing brace":   {`{"a": []}}`, "trailing data"},
		"not an object":         {`[]`, "cannot unmarshal"},
		"truncated":             {`{"a": [`, "unexpected EOF"},
		"wrong type for a step": {`{"a": [{"status": "429"}]}`, "cannot unmarshal"},
	} {
		_, err := loadScripts(writeFile(t, "bad.json", tc.content))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want it to mention %q", name, err, tc.want)
		}
	}
}
