//go:build gateway

// Spike G runs the pinned agentgateway image (AGENTGATEWAY_IMAGE, set by the
// Makefile) with the canonical agentgateway.yaml in front of fakeanthropic,
// and checks that Claude Code CLI traffic passes through it faithfully. Run
// it with `nix develop -c make gateway-spike`. See README.md.
package gateway

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ChristopherJMiller/luma/test/fakeanthropic"
)

// The gateway's addresses, from the canonical config.
const (
	gatewayURL = "http://127.0.0.1:8080"
	metricsURL = "http://127.0.0.1:15020/metrics"
	readyURL   = "http://127.0.0.1:15021/healthz/ready"
)

const (
	canonicalFile = "agentgateway.yaml"
	containerName = "luma-gateway-spike"
	// stepDelay is fakeanthropic's pause before each SSE event after
	// message_start.
	stepDelay = 2 * time.Second
	// upstreamHeader picks one of the harness's own SSE upstreams instead of
	// the fake: longStreamUpstream or splitUsageUpstream.
	upstreamHeader = "x-spike-upstream"
	// betas is a CLI-like anthropic-beta list.
	betas = "claude-code-20250219,interleaved-thinking-2025-05-14,thinking-binding-controls-2026-08-01"
)

// route is one of the canonical config's routes, with its gateway bearer and
// the workspace key behind it. All of them are fake.
type route struct{ name, bearer, key string }

var (
	interactive = route{"interactive", "gw-bearer-interactive-not-a-secret", "fake-workspace-key-interactive"}
	ci          = route{"ci", "gw-bearer-ci-not-a-secret", "fake-workspace-key-ci"}
	routes      = []route{interactive, ci}
)

var bindingKey = []byte("gateway-spike-binding-key-not-a-secret")

var client = &http.Client{Transport: &http.Transport{DisableCompression: true}}

// h is the harness: the upstream and the gateway container.
var h *harness

type harness struct {
	image string
	dir   string // config.yaml and keys/, readable by the image's user
	up    *upstream
	// running describes the running gateway's route type and TLS fill, ""
	// when none runs.
	running string
}

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	image := os.Getenv("AGENTGATEWAY_IMAGE")
	if image == "" {
		fmt.Fprintln(os.Stderr, "AGENTGATEWAY_IMAGE is unset; run `nix develop -c make gateway-spike`")
		return 2
	}
	if out, err := exec.Command("docker", "pull", "--quiet", image).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "docker pull %s: %v\n%s", image, err, out)
		return 2
	}
	dir, err := os.MkdirTemp("", "gateway-spike-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer os.RemoveAll(dir)
	if err := writeKeys(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	h = &harness{image: image, dir: dir, up: newUpstream()}
	defer h.up.srv.Close()
	defer h.stop()
	return m.Run()
}

// writeKeys writes the workspace key files the config reads, and makes the
// directory readable by the image's non-root user.
func writeKeys(dir string) error {
	keys := filepath.Join(dir, "keys")
	if err := os.Mkdir(keys, 0o755); err != nil {
		return err
	}
	for _, r := range routes {
		if err := os.WriteFile(filepath.Join(keys, r.name), []byte(r.key), 0o644); err != nil {
			return err
		}
	}
	return os.Chmod(dir, 0o755)
}

// dockerArgs runs the image with the config at cfg, both key files and both
// bearers, on the host network (the fake listens on 127.0.0.1).
func (hs *harness) dockerArgs(cfg string, extra ...string) []string {
	args := []string{
		"--network", "host",
		"-v", cfg + ":/etc/agentgateway/config.yaml:ro",
		"-v", filepath.Join(hs.dir, "keys") + ":/run/secrets/anthropic:ro",
		"-e", "GATEWAY_TOKEN_INTERACTIVE=" + interactive.bearer,
		"-e", "GATEWAY_TOKEN_CI=" + ci.bearer,
		hs.image, "-f", "/etc/agentgateway/config.yaml",
	}
	return append(args, extra...)
}

// gateway makes sure the gateway runs the canonical config with routeType
// for /v1/messages and plain http upstream, restarting it if it runs
// another.
func gateway(t *testing.T, routeType string) {
	t.Helper()
	gatewayWith(t, routeType, "null")
}

// gatewayWith is gateway with __UPSTREAM_TLS__ filled with tls.
func gatewayWith(t *testing.T, routeType, tls string) {
	t.Helper()
	want := routeType + ", __UPSTREAM_TLS__ " + tls
	if h.running == want {
		return
	}
	h.stop()
	cfg := filepath.Join(h.dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte(render(t, routeType, tls)), 0o644); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"run", "-d", "--name", containerName}, h.dockerArgs(cfg)...)
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		resp, err := client.Get(readyURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			out, _ := exec.Command("docker", "logs", containerName).CombinedOutput()
			h.stop()
			t.Fatalf("the gateway was not ready within 60s:\n%s", out)
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.running = want
	t.Logf("gateway running with /v1/messages: %s", want)
}

func (hs *harness) stop() {
	_ = exec.Command("docker", "rm", "-f", containerName).Run()
	hs.running = ""
}

// canonical is the committed config, unedited.
func canonical(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(canonicalFile)
	if err != nil {
		t.Fatalf("the canonical config: %v", err)
	}
	return string(b)
}

// upstreamHost names the fake by hostname, as the live config names
// api.anthropic.com: the gateway sets Host to a hostname target, but passes
// the client's Host through to an IP target.
const upstreamHost = "localhost"

// render fills the canonical config's upstream tokens with the fake's
// address and tls, and gives /v1/messages routeType, in a copy.
func render(t *testing.T, routeType, tls string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(h.up.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	cfg := strings.NewReplacer("__UPSTREAM_HOST__", upstreamHost, "__UPSTREAM_PORT__", port, "__UPSTREAM_TLS__", tls).Replace(canonical(t))
	from := "/v1/messages: " + RouteType + "\n"
	if n := strings.Count(cfg, from); n != len(routes) {
		t.Fatalf("the canonical config has %q %d times, want once per route", strings.TrimSpace(from), n)
	}
	return strings.ReplaceAll(cfg, from, "/v1/messages: "+routeType+"\n")
}

// upstream is what the gateway forwards to: the current test's fake, or one
// of the harness's own SSE upstreams. It records every exchange as the
// upstream wrote it.
type upstream struct {
	srv  *httptest.Server
	mu   sync.Mutex
	fake *fakeanthropic.Server
	seen []*exchange
}

// exchange is one response as the upstream wrote it.
type exchange struct {
	status int
	header http.Header // the headers the handler set
	body   []byte      // every byte the handler wrote
}

func newUpstream() *upstream {
	u := &upstream{fake: fakeanthropic.New(fakeanthropic.Options{BindingKey: bindingKey, StepDelay: stepDelay})}
	u.srv = httptest.NewServer(u)
	return u
}

// use gives the test a fresh fake (StepDelay 2 s, the spike's binding key)
// and forgets earlier exchanges.
func (u *upstream) use(o fakeanthropic.Options) *fakeanthropic.Server {
	o.BindingKey, o.StepDelay = bindingKey, stepDelay
	f := fakeanthropic.New(o)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.fake, u.seen = f, nil
	return f
}

func (u *upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ex := &exchange{}
	tw := &teeWriter{ResponseWriter: w, ex: ex}
	u.mu.Lock()
	fake := u.fake
	u.mu.Unlock()
	switch r.Header.Get(upstreamHeader) {
	case "long-stream":
		longStreamUpstream(tw, r)
	case "split-usage":
		splitUsageUpstream(tw, r)
	default:
		fake.ServeHTTP(tw, r)
	}
	u.mu.Lock()
	u.seen = append(u.seen, ex)
	u.mu.Unlock()
}

// only waits for the exchange since use to complete, and fails unless there
// is exactly one: the gateway sent the request once, without retrying.
func (u *upstream) only(t *testing.T) *exchange {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		u.mu.Lock()
		seen := slices.Clone(u.seen)
		u.mu.Unlock()
		if len(seen) > 0 || time.Now().After(deadline) {
			if len(seen) != 1 {
				t.Fatalf("the upstream saw %d exchanges, want 1", len(seen))
			}
			return seen[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// teeWriter records what the upstream handler writes.
type teeWriter struct {
	http.ResponseWriter
	ex *exchange
}

func (w *teeWriter) WriteHeader(code int) {
	if w.ex.status == 0 {
		w.ex.status, w.ex.header = code, w.Header().Clone()
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *teeWriter) Write(p []byte) (int, error) {
	if w.ex.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	w.ex.body = append(w.ex.body, p...)
	return w.ResponseWriter.Write(p)
}

func (w *teeWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// sse writes and flushes one event.
func sse(w http.ResponseWriter, name, data string) bool {
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data); err != nil {
		return false
	}
	return http.NewResponseController(w).Flush() == nil
}

func startSSE(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.WriteHeader(http.StatusOK)
}

// Long stream: message_start, a ping every 10 s for 90 s, then a short text
// reply.
const (
	longStreamPings    = 9
	longStreamInterval = 10 * time.Second
)

func longStreamUpstream(w http.ResponseWriter, r *http.Request) {
	startSSE(w, r)
	if !sse(w, "message_start", `{"type":"message_start","message":{"id":"msg_spike_long","type":"message","role":"assistant","model":"claude-haiku-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}`) {
		return
	}
	for range longStreamPings {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(longStreamInterval):
		}
		if !sse(w, "ping", `{"type": "ping"}`) {
			return
		}
	}
	for _, e := range [][2]string{
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Done."}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}`},
		{"message_stop", `{"type":"message_stop"}`},
	} {
		if !sse(w, e[0], e[1]) {
			return
		}
	}
}

// splitUsageUpstream streams the input and cache counts in message_start
// only, and the output count in message_delta only.
func splitUsageUpstream(w http.ResponseWriter, r *http.Request) {
	startSSE(w, r)
	for _, e := range [][2]string{
		{"message_start", `{"type":"message_start","message":{"id":"msg_spike_split","type":"message","role":"assistant","model":"claude-haiku-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1000,"cache_creation_input_tokens":0,"cache_read_input_tokens":800,"output_tokens":1}}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Split."}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":50}}`},
		{"message_stop", `{"type":"message_stop"}`},
	} {
		if !sse(w, e[0], e[1]) {
			return
		}
	}
}

// cliHeaders are the headers the CLI sends through Luma, with the route's
// bearer ("" for none).
func cliHeaders(bearer, person string) http.Header {
	hd := http.Header{}
	hd.Set("Content-Type", "application/json")
	hd.Set("Anthropic-Version", "2023-06-01")
	hd.Set("Anthropic-Beta", betas)
	hd.Set("X-Claude-Code-Request-Class", "main")
	hd.Set("X-Luma-Person", person)
	hd.Set("X-Luma-Agent", "luma")
	hd.Set("X-Luma-Tier", "fast")
	hd.Set("X-Luma-Source", "web")
	if bearer != "" {
		hd.Set("Authorization", "Bearer "+bearer)
	}
	return hd
}

// post sends body to the gateway at path. The caller closes the body.
func post(t *testing.T, path string, hd http.Header, body []byte) *http.Response {
	t.Helper()
	return do(t, http.MethodPost, path, hd, body)
}

func do(t *testing.T, method, path string, hd http.Header, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, gatewayURL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = hd
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// readAll reads and closes resp's body.
func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// simpleBody is a one-message request.
func simpleBody(text string, stream bool) []byte {
	msg, _ := json.Marshal(text)
	return fmt.Appendf(nil, `{"model":"claude-haiku-5-5","max_tokens":64,"stream":%t,"messages":[{"role":"user","content":[{"type":"text","text":%s}]}]}`, stream, msg)
}

// The CLI-shaped body: tools, a 1h cache_control, two signed thinking
// blocks, non-ASCII text and escapes, and numbers spelled 1.0, 1e3, -0,
// 2.50 and 1E-7. {{SIG1}} and {{SIG2}} are the thinking signatures.
const cliHead = `{"model":"claude-haiku-5-5","messages":[`

var cliMessages = []string{
	`{"role":"user","content":[{"type":"text","text":"Grüße aus Köln: 日本語のテキスト, emoji 👋🏽, escapes \u00e9 \/ \"q\" \\ \t, a control \u0001."}]}`,
	`{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"{{SIG1}}"},{"type":"tool_use","id":"toolu_spike_01","name":"remember","input":{"fact":"drinks tea ☕","weight":1.0,"limit":1e3,"nested":{"z":1,"a":[2.50,-0,1E-7]}}}]}`,
	`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_spike_01","content":[{"type":"text","text":"Saved."}],"cache_control":{"type":"ephemeral","ttl":"1h"}}]}`,
	`{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"{{SIG2}}"},{"type":"text","text":"Noted: tea it is."}]}`,
	`{"role":"user","content":[{"type":"text","text":"And the weather tomorrow?\u2028Thanks.","cache_control":{"type":"ephemeral","ttl":"1h"}}]}`,
}

const cliTail = `],"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.303; cc_entrypoint=sdk-ts;"},{"type":"text","text":"You are Luma, powered by Claude. Today is a synthetic day.","cache_control":{"type":"ephemeral","ttl":"1h"}}],` +
	`"tools":[{"name":"remember","description":"Save one fact about the person.","input_schema":{"type":"object","properties":{"fact":{"type":"string"},"weight":{"type":"number","default":1.0,"maximum":1e3}},"required":["fact"],"additionalProperties":false}},` +
	`{"name":"WebSearch","description":"Search the web.","input_schema":{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"query":{"type":"string","minLength":2}},"required":["query"]},"cache_control":{"type":"ephemeral","ttl":"1h"}}],` +
	`"metadata":{"user_id":"user_spike_account__session_00000000-0000-0000-0000-000000000000"},"max_tokens":32000,"thinking":{"type":"enabled","budget_tokens":1e3},"temperature":1.0,"stream":false}`

func cliBody(n int, sig1, sig2 string) []byte {
	s := cliHead + strings.Join(cliMessages[:n], ",") + cliTail
	return []byte(strings.NewReplacer("{{SIG1}}", sig1, "{{SIG2}}", sig2).Replace(s))
}

// signedCLIBody builds the CLI-shaped body as the CLI would: each thinking
// signature is the one the fake returned, through the gateway on r, for the
// conversation so far.
func signedCLIBody(t *testing.T, r route) []byte {
	t.Helper()
	sig := func(body []byte) string {
		t.Helper()
		resp := post(t, "/"+r.name+"/v1/messages?beta=true", cliHeaders(r.bearer, "u_5197000000"), body)
		b := readAll(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("a signing request: %d %s", resp.StatusCode, b)
		}
		var reply struct {
			Content []struct{ Type, Signature string }
		}
		if err := json.Unmarshal(b, &reply); err != nil || len(reply.Content) == 0 || reply.Content[0].Type != "thinking" {
			t.Fatalf("a signing request: no thinking block in %s", b)
		}
		return reply.Content[0].Signature
	}
	sig1 := sig(cliBody(1, "", ""))
	sig2 := sig(cliBody(3, sig1, ""))
	return cliBody(len(cliMessages), sig1, sig2)
}

// unboundSig is a well-formed signature bound to no prefix (32 zero bytes),
// which the fake answers with the binding error.
var unboundSig = base64.StdEncoding.EncodeToString(make([]byte, 32))

// semanticDiff reports the first way got differs from want as JSON: a
// different value (numbers compare as float64), an object member out of
// order, or a string that is not byte-equal once decoded. nil means
// semantically exact.
func semanticDiff(want, got []byte) error {
	a, b := json.NewDecoder(bytes.NewReader(want)), json.NewDecoder(bytes.NewReader(got))
	a.UseNumber()
	b.UseNumber()
	if err := cmpValue(a, b, "$"); err != nil {
		return err
	}
	if _, err := b.Token(); !errors.Is(err, io.EOF) {
		return errors.New("got has trailing data")
	}
	return nil
}

func cmpValue(a, b *json.Decoder, path string) error {
	ta, err := a.Token()
	if err != nil {
		return fmt.Errorf("%s: want: %w", path, err)
	}
	tb, err := b.Token()
	if err != nil {
		return fmt.Errorf("%s: got: %w", path, err)
	}
	switch x := ta.(type) {
	case json.Delim:
		if y, ok := tb.(json.Delim); !ok || x != y {
			return fmt.Errorf("%s: got %v, want %v", path, clip(tb), x)
		}
		for i := 0; ; i++ {
			if a.More() != b.More() {
				return fmt.Errorf("%s: got a different number of members", path)
			}
			if !a.More() {
				break
			}
			elem := fmt.Sprintf("%s[%d]", path, i)
			if x == '{' {
				ka, _ := a.Token()
				kb, err := b.Token()
				if err != nil || ka != kb {
					return fmt.Errorf("%s: member %d is %v, want %q", path, i, clip(kb), ka)
				}
				elem = fmt.Sprintf("%s.%s", path, ka)
			}
			if err := cmpValue(a, b, elem); err != nil {
				return err
			}
		}
		_, _ = a.Token()
		_, _ = b.Token()
		return nil
	case json.Number:
		y, ok := tb.(json.Number)
		if !ok {
			return fmt.Errorf("%s: got %v, want the number %s", path, clip(tb), x)
		}
		fx, errX := strconv.ParseFloat(string(x), 64)
		fy, errY := strconv.ParseFloat(string(y), 64)
		if errX != nil || errY != nil || fx != fy {
			return fmt.Errorf("%s: got %s, want %s", path, y, x)
		}
		return nil
	default: // string, bool, nil
		if ta != tb {
			return fmt.Errorf("%s: got %v, want %v", path, clip(tb), clip(ta))
		}
		return nil
	}
}

func clip(v any) string {
	s := fmt.Sprintf("%q", fmt.Sprint(v))
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

// sample is one Prometheus sample from the gateway's /metrics.
type sample struct {
	name   string
	labels map[string]string
	value  float64
}

func scrape(t *testing.T) []sample {
	t.Helper()
	resp, err := client.Get(metricsURL)
	if err != nil {
		t.Fatal(err)
	}
	var out []sample
	sc := bufio.NewScanner(bytes.NewReader(readAll(t, resp)))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		s, err := parseSample(line)
		if err != nil {
			t.Fatalf("metrics line %q: %v", line, err)
		}
		out = append(out, s)
	}
	return out
}

func parseSample(line string) (sample, error) {
	s := sample{labels: map[string]string{}}
	i := strings.IndexAny(line, "{ ")
	if i < 0 {
		return s, errors.New("no value")
	}
	s.name, line = line[:i], line[i:]
	if line[0] == '{' {
		line = line[1:]
		for line != "" && line[0] != '}' {
			eq := strings.Index(line, `="`)
			if eq < 0 {
				return s, errors.New("bad label")
			}
			key := line[:eq]
			line = line[eq+2:]
			var v strings.Builder
			for {
				if line == "" {
					return s, errors.New("unterminated label value")
				}
				c := line[0]
				line = line[1:]
				if c == '"' {
					break
				}
				if c == '\\' && line != "" {
					c, line = line[0], line[1:]
					if c == 'n' {
						c = '\n'
					}
				}
				v.WriteByte(c)
			}
			s.labels[key] = v.String()
			line = strings.TrimPrefix(line, ",")
		}
		if line == "" {
			return s, errors.New("unterminated labels")
		}
		line = line[1:]
	}
	f := strings.Fields(line)
	if len(f) == 0 {
		return s, errors.New("no value")
	}
	v, err := strconv.ParseFloat(f[0], 64)
	s.value = v
	return s, err
}

// metric returns the samples of name whose labels include want, waiting up
// to 5 s for the first to appear.
func metric(t *testing.T, name string, want map[string]string) []sample {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var out []sample
		for _, s := range scrape(t) {
			if s.name == name && hasLabels(s.labels, want) {
				out = append(out, s)
			}
		}
		if len(out) > 0 || time.Now().After(deadline) {
			return out
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func hasLabels(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// tokenUsage is the token usage sum per gen_ai_token_type recorded for the
// person, without the zero entries.
func tokenUsage(t *testing.T, person string) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, s := range metric(t, "agentgateway_gen_ai_client_token_usage_sum", map[string]string{"person": person}) {
		if s.value != 0 {
			out[s.labels["gen_ai_token_type"]] += s.value
		}
	}
	return out
}

// expectNothingUpstream fails if the upstream saw a request since use.
func expectNothingUpstream(t *testing.T, fake *fakeanthropic.Server) {
	t.Helper()
	if n := len(fake.Requests()); n != 0 {
		t.Errorf("the fake recorded %d requests, want none", n)
	}
	h.up.mu.Lock()
	defer h.up.mu.Unlock()
	if n := len(h.up.seen); n != 0 {
		t.Errorf("the upstream saw %d requests, want none", n)
	}
}

func TestCanonicalConfigHasNoUpstream(t *testing.T) {
	raw := canonical(t)
	for _, tok := range []string{"__UPSTREAM_HOST__", "__UPSTREAM_PORT__", "__UPSTREAM_TLS__"} {
		if !strings.Contains(raw, tok) {
			t.Errorf("the canonical config lacks %s", tok)
		}
	}
	for i, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "adminAddr: 127.0.0.1:15000" { // the admin listener, not an upstream
			continue
		}
		for _, addr := range []string{"127.0.0.1", "localhost", "api.anthropic.com"} {
			if strings.Contains(line, addr) {
				t.Errorf("line %d names %s: %q", i+1, addr, line)
			}
		}
	}

	var cfg struct {
		Routes []struct {
			Name     string
			Policies struct {
				AI struct{ Routes map[string]string }
			}
			Backends []struct {
				AI struct {
					HostOverride string `yaml:"hostOverride"`
					Policies     struct {
						BackendTLS string `yaml:"backendTLS"`
					}
				}
			}
		}
	}
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range cfg.Routes {
		names = append(names, r.Name)
		if got := r.Policies.AI.Routes["/v1/messages"]; got != RouteType {
			t.Errorf("route %q gives /v1/messages %q, want RouteType %q", r.Name, got, RouteType)
		}
		for _, b := range r.Backends {
			if b.AI.HostOverride != "__UPSTREAM_HOST__:__UPSTREAM_PORT__" || b.AI.Policies.BackendTLS != "__UPSTREAM_TLS__" {
				t.Errorf("route %q has upstream %q, TLS %q: want the tokens", r.Name, b.AI.HostOverride, b.AI.Policies.BackendTLS)
			}
		}
	}
	if !slices.Equal(names, []string{"interactive", "ci"}) {
		t.Errorf("route names are %q, want [interactive ci]", names)
	}

	// Both TLS fills load.
	for _, tls := range []string{"{}", "null"} {
		cfg := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(cfg, []byte(render(t, RouteType, tls)), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Dir(cfg), 0o755); err != nil {
			t.Fatal(err)
		}
		args := append([]string{"run", "--rm"}, h.dockerArgs(cfg, "--validate-only")...)
		if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
			t.Errorf("__UPSTREAM_TLS__=%s does not load: %v\n%s", tls, err, out)
		}
	}
}

// checkBody sends the signed CLI-shaped body on r and reports whether the
// upstream received it semantically exact and byte-equal.
func checkBody(t *testing.T, r route) (semantic error, byteEqual bool) {
	t.Helper()
	fake := h.up.use(fakeanthropic.Options{})
	body := signedCLIBody(t, r)
	n := len(fake.Requests())
	resp := post(t, "/"+r.name+"/v1/messages?beta=true", cliHeaders(r.bearer, "u_5197000001"), body)
	if b := readAll(t, resp); resp.StatusCode != http.StatusOK {
		t.Errorf("status %d: %s", resp.StatusCode, b)
	}
	recs := fake.Requests()[n:]
	if len(recs) != 1 {
		t.Fatalf("the fake recorded %d requests, want 1", len(recs))
	}
	if !recs[0].BindingOK {
		t.Error("the thinking signatures failed the binding check upstream")
	}
	if !bytes.Equal(body, recs[0].Body) {
		t.Logf("client body:\n%s\nupstream body:\n%s", body, recs[0].Body)
	}
	return semanticDiff(body, recs[0].Body), bytes.Equal(body, recs[0].Body)
}

func TestBodyExact(t *testing.T) {
	gateway(t, RouteType)
	for _, r := range routes {
		t.Run(r.name, func(t *testing.T) {
			semantic, byteEqual := checkBody(t, r)
			if semantic != nil {
				t.Errorf("the body upstream is not semantically exact: %v", semantic)
			}
			if byteEqual != BodyByteEqual {
				t.Errorf("bytes.Equal(client body, upstream body) = %t, want BodyByteEqual = %t", byteEqual, BodyByteEqual)
			}
		})
	}
}

func TestHeadersForwarded(t *testing.T) {
	gateway(t, RouteType)
	keys := map[string]string{}
	for _, r := range routes {
		t.Run(r.name, func(t *testing.T) {
			fake := h.up.use(fakeanthropic.Options{})
			sent := cliHeaders(r.bearer, "u_5197000002")
			resp := post(t, "/"+r.name+"/v1/messages?beta=true", sent, simpleBody("Headers.", false))
			if b := readAll(t, resp); resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d: %s", resp.StatusCode, b)
			}
			recs := fake.Requests()
			if len(recs) != 1 {
				t.Fatalf("the fake recorded %d requests, want 1", len(recs))
			}
			got := recs[0].Header
			for _, k := range []string{"Anthropic-Beta", "X-Claude-Code-Request-Class", "X-Luma-Person", "X-Luma-Agent", "X-Luma-Tier", "X-Luma-Source"} {
				if !slices.Equal(got.Values(k), sent.Values(k)) {
					t.Errorf("%s arrived as %q, want %q", k, got.Values(k), sent.Values(k))
				}
			}
			if v := got.Values("Anthropic-Version"); !slices.Equal(v, []string{"2023-06-01"}) {
				t.Errorf("anthropic-version arrived as %q", v)
			}
			if v := got.Values("Authorization"); v != nil {
				t.Errorf("authorization reached the upstream: %q", v)
			}
			if v := got.Values("X-Api-Key"); !slices.Equal(v, []string{r.key}) {
				t.Errorf("x-api-key arrived as %q, want the %s key", v, r.name)
			}
			keys[r.name] = got.Get("X-Api-Key")
		})
	}
	if keys[interactive.name] == keys[ci.name] {
		t.Errorf("both routes sent the key %q", keys[ci.name])
	}
}

func TestRouteBearersAreDistinct(t *testing.T) {
	gateway(t, RouteType)
	for _, c := range []struct{ path, bearer string }{
		{"/ci/v1/messages", interactive.bearer},
		{"/interactive/v1/messages", ci.bearer},
		{"/ci/v1/messages/count_tokens", interactive.bearer},
		{"/interactive/v1/messages/count_tokens", ci.bearer},
	} {
		fake := h.up.use(fakeanthropic.Options{})
		resp := post(t, c.path, cliHeaders(c.bearer, "u_5197000003"), simpleBody("Wrong route.", false))
		if b := readAll(t, resp); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s with the other route's bearer: %d %s, want 401", c.path, resp.StatusCode, b)
		}
		expectNothingUpstream(t, fake)
	}
}

func TestBearerRequired(t *testing.T) {
	gateway(t, RouteType)
	for _, r := range routes {
		for _, bearer := range []string{"", "gw-bearer-wrong-not-a-secret", r.key} {
			fake := h.up.use(fakeanthropic.Options{})
			resp := post(t, "/"+r.name+"/v1/messages", cliHeaders(bearer, "u_5197000004"), simpleBody("No bearer.", false))
			if b := readAll(t, resp); resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s with bearer %q: %d %s, want 401", r.name, bearer, resp.StatusCode, b)
			}
			expectNothingUpstream(t, fake)
		}
	}
}

func TestUnlistedPathRejected(t *testing.T) {
	gateway(t, RouteType)
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/interactive/v1/models"},
		{http.MethodPost, "/interactive/v1/chat/completions"},
		{http.MethodPost, "/interactive/v1/messages/batches"},
		{http.MethodPost, "/v1/messages"},
	} {
		fake := h.up.use(fakeanthropic.Options{})
		resp := do(t, c.method, c.path, cliHeaders(interactive.bearer, "u_5197000005"), simpleBody("Unlisted.", false))
		if b := readAll(t, resp); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s: %d %s, want 404", c.method, c.path, resp.StatusCode, b)
		}
		expectNothingUpstream(t, fake)
	}
}

func TestUpstreamPathRewritten(t *testing.T) {
	gateway(t, RouteType)
	for _, c := range []struct{ route, path, want string }{
		{ci.name, "/ci/v1/messages?beta=true", "/v1/messages?beta=true"},
		{ci.name, "/ci/v1/messages/count_tokens?beta=true", "/v1/messages/count_tokens?beta=true"},
		{interactive.name, "/interactive/v1/messages?beta=true", "/v1/messages?beta=true"},
		{interactive.name, "/interactive/v1/messages/count_tokens?beta=true", "/v1/messages/count_tokens?beta=true"},
	} {
		r := interactive
		if c.route == ci.name {
			r = ci
		}
		fake := h.up.use(fakeanthropic.Options{})
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, gatewayURL+c.path, bytes.NewReader(simpleBody("Path.", false)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header = cliHeaders(r.bearer, "u_5197000006")
		req.Host = "luma-gateway:8080" // as in the cluster
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if b := readAll(t, resp); resp.StatusCode != http.StatusOK {
			t.Errorf("%s: %d %s", c.path, resp.StatusCode, b)
		}
		recs := fake.Requests()
		if len(recs) != 1 {
			t.Fatalf("%s: the fake recorded %d requests, want 1", c.path, len(recs))
		}
		if recs[0].Path != c.want {
			t.Errorf("%s reached the fake as %s, want %s", c.path, recs[0].Path, c.want)
		}
		if recs[0].Host != upstreamHost {
			t.Errorf("%s reached the fake with Host %q, want %q", c.path, recs[0].Host, upstreamHost)
		}
	}
}

func TestLargeBody(t *testing.T) {
	gateway(t, RouteType)
	fake := h.up.use(fakeanthropic.Options{})
	const size = 28 << 20
	line := `A large body, ünïcödé \u00e9 and an escaped newline\n. `
	var b bytes.Buffer
	b.WriteString(`{"model":"claude-haiku-5-5","max_tokens":64,"stream":false,"messages":[{"role":"user","content":[{"type":"text","text":"`)
	for b.Len() < size-200 {
		b.WriteString(line)
	}
	b.WriteString(`"}]}]}`)
	body := b.Bytes()
	resp := post(t, "/interactive/v1/messages?beta=true", cliHeaders(interactive.bearer, "u_5197000007"), body)
	if out := readAll(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("a %d MiB body: %d %s", len(body)>>20, resp.StatusCode, out)
	}
	recs := fake.Requests()
	if len(recs) != 1 {
		t.Fatalf("the fake recorded %d requests, want 1", len(recs))
	}
	if err := semanticDiff(body, recs[0].Body); err != nil {
		t.Errorf("a %d-byte body is not semantically exact upstream: %v", len(body), err)
	}
}

// checkSSE streams a fake reply on r and reports when the first event
// arrived and whether the client's bytes equal the upstream's.
func checkSSE(t *testing.T, r route) (first time.Duration, equal bool) {
	t.Helper()
	h.up.use(fakeanthropic.Options{DefaultText: "Streamed."})
	start := time.Now()
	resp := post(t, "/"+r.name+"/v1/messages?beta=true", cliHeaders(r.bearer, "u_5197000008"), simpleBody("Stream, please.", true))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, readAll(t, resp))
	}
	br := bufio.NewReader(resp.Body)
	var got []byte
	for !bytes.HasSuffix(got, []byte("\n\n")) {
		c, err := br.ReadByte()
		if err != nil {
			t.Fatalf("before the first event ended: %v (read %q)", err, got)
		}
		got = append(got, c)
	}
	first = time.Since(start)
	if !bytes.HasPrefix(got, []byte("event: message_start\n")) {
		t.Errorf("the first event is %q, want message_start", got)
	}
	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, rest...)
	want := h.up.only(t).body
	if !bytes.Contains(got, []byte("event: ping\n")) {
		t.Errorf("the client stream has no ping event")
	}
	equal = bytes.Equal(got, want)
	if !equal {
		t.Logf("client stream:\n%s\nupstream stream:\n%s", got, want)
	}
	return first, equal
}

func TestSSEUnbufferedPingKept(t *testing.T) {
	gateway(t, RouteType)
	first, equal := checkSSE(t, interactive)
	if first > 500*time.Millisecond {
		t.Errorf("the first event took %v, want ≤ 500ms (the next one is %v later)", first, stepDelay)
	}
	if !equal {
		t.Error("the client's SSE bytes differ from the upstream's")
	}
}

func TestLongStream(t *testing.T) {
	gateway(t, RouteType)
	h.up.use(fakeanthropic.Options{})
	hd := cliHeaders(interactive.bearer, "u_5197000009")
	hd.Set(upstreamHeader, "long-stream")
	start := time.Now()
	resp := post(t, "/interactive/v1/messages", hd, simpleBody("A long one.", true))
	got := readAll(t, resp)
	took := time.Since(start)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, got)
	}
	if n := bytes.Count(got, []byte("event: ping\n")); n != longStreamPings {
		t.Errorf("the client got %d pings, want %d", n, longStreamPings)
	}
	if !bytes.HasSuffix(got, []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")) {
		t.Errorf("the stream did not complete; it ends %q", got[max(len(got)-80, 0):])
	}
	if !bytes.Equal(got, h.up.only(t).body) {
		t.Error("the client's bytes differ from the upstream's")
	}
	if took < longStreamPings*longStreamInterval {
		t.Errorf("the stream took %v, want ≥ %v", took, longStreamPings*longStreamInterval)
	}
}

func TestErrorPassthrough(t *testing.T) {
	gateway(t, RouteType)
	fixture, err := os.ReadFile("../../protocol/fixtures/binding-error.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		body   []byte
		status int
	}{
		{"429", simpleBody("Busy. [[fake:e429]]", false), http.StatusTooManyRequests},
		{"529", simpleBody("Overloaded. [[fake:e529]]", true), 529},
		{"binding 400", cliBody(3, unboundSig, ""), http.StatusBadRequest},
	} {
		t.Run(c.name, func(t *testing.T) {
			h.up.use(fakeanthropic.Options{Scripts: map[string][]fakeanthropic.Step{
				"e429": {{Status: http.StatusTooManyRequests, RetryAfter: "7"}},
				"e529": {{Status: 529}},
			}})
			resp := post(t, "/interactive/v1/messages?beta=true", cliHeaders(interactive.bearer, "u_5197000010"), c.body)
			got := readAll(t, resp)
			ex := h.up.only(t) // and so no retry
			if resp.StatusCode != c.status || ex.status != c.status {
				t.Errorf("status %d upstream, %d at the client, want %d", ex.status, resp.StatusCode, c.status)
			}
			if !bytes.Equal(got, ex.body) {
				t.Errorf("the client got %s, the upstream sent %s", got, ex.body)
			}
			for k, v := range ex.header {
				if !slices.Equal(resp.Header.Values(k), v) {
					t.Errorf("header %s: the client got %q, the upstream sent %q", k, resp.Header.Values(k), v)
				}
			}
			if c.status == http.StatusTooManyRequests && resp.Header.Get("Retry-After") != "7" {
				t.Errorf("retry-after is %q, want 7", resp.Header.Get("Retry-After"))
			}
			if c.name == "binding 400" && !bytes.Contains(got, bytes.TrimSpace(fixture)) {
				t.Errorf("the binding 400 lacks the fixture text: %s", got)
			}
		})
	}
}

// streamUsage streams a reply on interactive for person, from the fake
// (1000 input, 800 cache-read, 50 output tokens) or, with split, from
// splitUsageUpstream, and returns the token usage the gateway recorded.
func streamUsage(t *testing.T, person string, split bool) map[string]float64 {
	t.Helper()
	h.up.use(fakeanthropic.Options{DefaultUsage: fakeanthropic.Usage{Input: 1000, CacheRead: 800, Output: 50}})
	hd := cliHeaders(interactive.bearer, person)
	if split {
		hd.Set(upstreamHeader, "split-usage")
	}
	resp := post(t, "/interactive/v1/messages?beta=true", hd, simpleBody("Usage.", true))
	if b := readAll(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	h.up.only(t)
	return tokenUsage(t, person)
}

func TestUsageMetrics(t *testing.T) {
	gateway(t, RouteType)
	const person = "u_5197000011"
	got := streamUsage(t, person, false)
	// agentgateway's "input" is the whole prompt: input_tokens plus the
	// cache reads (llm.inputTokens).
	want := map[string]float64{"input": 1000 + 800, "input_cache_read": 800, "output": 50}
	if !maps.Equal(got, want) {
		t.Errorf("token usage for %s is %v, want %v", person, got, want)
	}
	if s := metric(t, "agentgateway_gen_ai_client_token_usage_sum", map[string]string{"person": person, "request_class": "main"}); len(s) == 0 {
		t.Error("no token usage sample has the request_class label")
	}
}

func TestRequestsMetricHasStatusAndRequestClass(t *testing.T) {
	gateway(t, RouteType)
	h.up.use(fakeanthropic.Options{})
	const person = "u_5197000012"
	bad := cliBody(3, unboundSig, "")
	resp := post(t, "/interactive/v1/messages?beta=true", cliHeaders(interactive.bearer, person), bad)
	if b := readAll(t, resp); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	s := metric(t, "agentgateway_requests_total", map[string]string{"person": person, "status": "400", "request_class": "main"})
	if len(s) != 1 || s[0].value != 1 {
		t.Fatalf("agentgateway_requests_total for the binding 400: %v, want one sample of 1", s)
	}
	t.Logf("agentgateway_requests_total labels: %v", slices.Sorted(maps.Keys(s[0].labels)))
}

func TestUnknownLabelValuesBecomeOther(t *testing.T) {
	gateway(t, RouteType)
	h.up.use(fakeanthropic.Options{})
	hd := cliHeaders(interactive.bearer, "chris")
	hd.Set("X-Luma-Agent", "labels-spike")
	hd.Del("X-Luma-Tier")
	hd.Set("X-Luma-Source", "<script>")
	resp := post(t, "/interactive/v1/messages", hd, simpleBody("Labels.", false))
	if b := readAll(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	s := metric(t, "agentgateway_requests_total", map[string]string{"agent": "labels-spike"})
	if len(s) != 1 {
		t.Fatalf("agentgateway_requests_total for agent labels-spike: %v, want one sample", s)
	}
	for _, k := range []string{"person", "tier", "source"} {
		if s[0].labels[k] != "other" {
			t.Errorf("label %s is %q, want other", k, s[0].labels[k])
		}
	}
	if s[0].labels["request_class"] != "main" {
		t.Errorf("label request_class is %q, want main", s[0].labels["request_class"])
	}

	// A repeated header is other too, even when each value is valid.
	hd = cliHeaders(interactive.bearer, "u_5197000017")
	hd.Set("X-Luma-Agent", "labels-spike-repeat")
	hd.Add("X-Luma-Tier", "fast")
	resp = post(t, "/interactive/v1/messages", hd, simpleBody("Labels again.", false))
	if b := readAll(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	s = metric(t, "agentgateway_requests_total", map[string]string{"agent": "labels-spike-repeat"})
	if len(s) != 1 || s[0].labels["tier"] != "other" || s[0].labels["person"] != "u_5197000017" {
		t.Errorf("a repeated x-luma-tier: %v, want one sample with tier other", s)
	}
}

// TestUpstreamTLSSwitch: with __UPSTREAM_TLS__ filled with {}, the gateway
// speaks TLS upstream, so the plain-http fake never gets a request.
func TestUpstreamTLSSwitch(t *testing.T) {
	gatewayWith(t, RouteType, "{}")
	fake := h.up.use(fakeanthropic.Options{})
	resp := post(t, "/interactive/v1/messages", cliHeaders(interactive.bearer, "u_5197000014"), simpleBody("TLS.", false))
	if b := readAll(t, resp); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status %d (%s), want 503: a TLS handshake with a plain-http upstream", resp.StatusCode, b)
	}
	expectNothingUpstream(t, fake)
}

// TestRouteTypes runs the body, SSE and usage checks under both route types
// and compares each with Observed, which also records why RouteType won.
func TestRouteTypes(t *testing.T) {
	if _, ok := Observed[RouteType]; !ok {
		t.Fatalf("RouteType %q has no observation", RouteType)
	}
	if Observed[RouteType].BodyByteEqual != BodyByteEqual {
		t.Errorf("Observed[%q].BodyByteEqual disagrees with BodyByteEqual", RouteType)
	}
	for _, rt := range []string{"detect", "messages"} {
		t.Run(rt, func(t *testing.T) {
			gateway(t, rt)
			want := Observed[rt]

			semantic, byteEqual := checkBody(t, interactive)
			if (semantic == nil) != want.BodySemanticExact || byteEqual != want.BodyByteEqual {
				t.Errorf("body: semantically exact %t (%v), byte-equal %t; observed %t, %t",
					semantic == nil, semantic, byteEqual, want.BodySemanticExact, want.BodyByteEqual)
			}

			first, equal := checkSSE(t, interactive)
			if exact := first <= 500*time.Millisecond && equal; exact != want.StreamExact {
				t.Errorf("stream: first event after %v, bytes equal %t; observed exact %t", first, equal, want.StreamExact)
			}

			// Each route type runs on its own gateway start, so its metrics
			// start empty and the people can repeat.
			if got := streamUsage(t, "u_5197000015", false); !maps.Equal(got, want.CumulativeUsage) {
				t.Errorf("cumulative usage: %v, observed %v", got, want.CumulativeUsage)
			}
			if got := streamUsage(t, "u_5197000016", true); !maps.Equal(got, want.SplitUsage) {
				t.Errorf("split usage: %v, observed %v", got, want.SplitUsage)
			}

			fake := h.up.use(fakeanthropic.Options{})
			lone := []byte(`{"model":"claude-haiku-5-5","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"cut \ud83d here"}]}]}`)
			resp := post(t, "/interactive/v1/messages", cliHeaders(interactive.bearer, "u_5197000013"), lone)
			b := readAll(t, resp)
			if resp.StatusCode != want.LoneSurrogateStatus {
				t.Errorf("lone surrogate: status %d (%s), observed %d", resp.StatusCode, b, want.LoneSurrogateStatus)
			}
			if recs := fake.Requests(); resp.StatusCode == http.StatusOK && (len(recs) != 1 || !bytes.Equal(recs[0].Body, lone)) {
				t.Error("lone surrogate: the body did not reach the fake as sent")
			}
		})
	}
}
