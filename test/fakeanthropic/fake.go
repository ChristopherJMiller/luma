// Package fakeanthropic is a scripted, recording stand-in for the Anthropic
// Messages API that every offline Luma test runs the real Claude Code CLI
// against. It speaks the API's wire format (the SSE event sequence, usage
// with cache reads and writes, signed thinking blocks, the error envelope),
// records every request byte for byte, and checks thinking bindings the
// way preserved thinking does: a thinking block's signature is an HMAC over
// the canonical {system, tools, messages} prefix it was generated after,
// chained to the thinking before it, and a replayed block whose prefix
// changed, or that breaks the chain, is a 400.
//
// Replies are deterministic: no clock, no randomness. See README.md.
package fakeanthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Options configure a Server. The zero value serves empty default replies,
// records in memory only, and signs with an empty key.
type Options struct {
	// RecordDir, if set, also gets every request as <seq>.body (the bytes
	// received) and <seq>.meta.json (the rest of the Recorded), with seq
	// zero-padded to six digits.
	RecordDir string
	// Scripts maps a marker name to its steps (see Step and README
	// "Scripts").
	Scripts map[string][]Step
	// DefaultText answers a request that carries no marker.
	DefaultText string
	// DefaultUsage is the usage of a reply whose step sets none.
	DefaultUsage Usage
	// StepDelay is the pause before each SSE event after message_start.
	StepDelay time.Duration
	// BindingKey keys the thinking-signature HMAC. The binding check is
	// always on.
	BindingKey []byte
	// SearchResults answer every auto-answered web search.
	SearchResults []SearchResult
}

// Recorded is one request as the fake received it.
type Recorded struct {
	// Seq numbers requests from 1 in the order their bodies were read;
	// ids derive from it.
	Seq    int
	Method string
	// Path is the request URI: the path and any query, as sent.
	Path string
	// Header is as net/http parsed it: names canonicalised (Anthropic-Beta),
	// values verbatim, and no Host, which is in Host.
	Header http.Header
	Body   []byte
	// BindingOK reports that the request reached the binding check (a
	// messages or count_tokens request that parsed and passed validation)
	// and every replayed thinking block in it passed.
	BindingOK bool
	// Host is the request's Host header (net/http moves it out of Header).
	Host string
}

// Server is the fake. It is an http.Handler and safe for concurrent use.
type Server struct {
	o    Options
	mu   sync.Mutex
	seq  int
	next map[string]int // script name -> index of its next step
	recs []Recorded
}

// maxBody is above the API's 32 MB request limit, so the gateway's large
// body tests reach the fake whole.
const maxBody = 64 << 20

// thinkingBindingBeta is the beta under which every reply carries
// input_transformations, empty when nothing was dropped.
const thinkingBindingBeta = "thinking-binding-controls-2026-08-01"

// New returns a fake configured by o.
func New(o Options) *Server {
	return &Server{o: o, next: map[string]int{}}
}

// Requests returns every request received so far, in Seq order. The
// records share their Body and Header with the fake; do not modify them.
func (s *Server) Requests() []Recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Clone(s.recs)
	slices.SortFunc(out, func(a, b Recorded) int { return a.Seq - b.Seq })
	return out
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()

	requestID := fmt.Sprintf("req_fake_%d", seq)
	w.Header().Set("request-id", requestID)
	rec := Recorded{Seq: seq, Method: r.Method, Path: r.URL.RequestURI(), Header: r.Header.Clone(), Body: body, Host: r.Host}

	if readErr != nil {
		if s.record(w, rec, requestID) {
			writeError(w, http.StatusRequestEntityTooLarge, requestID, "request_too_large", "Request exceeds the maximum allowed number of bytes.")
		}
		return
	}

	countTokens := false
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages":
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/count_tokens":
		countTokens = true
	default:
		if s.record(w, rec, requestID) {
			writeError(w, http.StatusNotFound, requestID, "not_found_error", fmt.Sprintf("fakeanthropic: no route for %s %s", r.Method, r.URL.Path))
		}
		return
	}

	req, err := parseRequest(body)
	if err != nil {
		if s.record(w, rec, requestID) {
			writeError(w, http.StatusBadRequest, requestID, "invalid_request_error", err.Error())
		}
		return
	}
	sig, fail := checkBinding(s.o.BindingKey, &req.bound)
	rec.BindingOK = fail == nil
	if !s.record(w, rec, requestID) {
		return
	}
	if fail != nil {
		writeError(w, http.StatusBadRequest, requestID, "invalid_request_error", fail.message())
		return
	}

	if countTokens {
		writeBody(w, http.StatusOK, map[string]int{"input_tokens": len(Canonical(body)) / 4})
		return
	}

	st, err := s.step(req.bound.decoded)
	if err != nil {
		writeError(w, http.StatusBadRequest, requestID, "invalid_request_error", err.Error())
		return
	}
	if st.Status != 0 && st.Status != http.StatusOK {
		if st.RetryAfter != "" {
			w.Header().Set("retry-after", st.RetryAfter)
		}
		typ, msg := errorFor(st)
		writeError(w, st.Status, requestID, typ, msg)
		return
	}

	rep := s.build(seq, req, st, sig, r.Header)
	if req.stream {
		rep.writeSSE(r.Context(), w, s.o.StepDelay)
	} else {
		rep.writeJSON(w)
	}
}

// record keeps rec in memory and in RecordDir. On a RecordDir failure it
// answers 500 and reports false.
func (s *Server) record(w http.ResponseWriter, rec Recorded, requestID string) bool {
	s.mu.Lock()
	s.recs = append(s.recs, rec)
	s.mu.Unlock()
	if err := s.writeRecord(rec); err != nil {
		writeError(w, http.StatusInternalServerError, requestID, "api_error", "fakeanthropic: record: "+err.Error())
		return false
	}
	return true
}

func (s *Server) writeRecord(rec Recorded) error {
	if s.o.RecordDir == "" {
		return nil
	}
	if err := os.MkdirAll(s.o.RecordDir, 0o750); err != nil {
		return err
	}
	base := filepath.Join(s.o.RecordDir, fmt.Sprintf("%06d", rec.Seq))
	if err := os.WriteFile(base+".body", rec.Body, 0o600); err != nil {
		return err
	}
	meta := struct {
		Seq       int         `json:"seq"`
		Method    string      `json:"method"`
		Path      string      `json:"path"`
		Header    http.Header `json:"header"`
		BindingOK bool        `json:"binding_ok"`
		Host      string      `json:"host"`
	}{rec.Seq, rec.Method, rec.Path, rec.Header, rec.BindingOK, rec.Host}
	return os.WriteFile(base+".meta.json", marshal(meta), 0o600)
}

// request is what the fake reads from a messages or count_tokens body.
type request struct {
	bound     boundRequest
	model     string
	stream    bool
	webSearch string // the name of a web_search_* server tool, if any
}

func parseRequest(body []byte) (*request, error) {
	root, ok := parseJSON(body)
	if !ok || root.kind != '{' {
		return nil, errors.New("fakeanthropic: the request body is not a JSON object")
	}
	var r request
	msgs := root.get("messages")
	if msgs == nil {
		return nil, errors.New("messages: Field required")
	}
	if msgs.kind != '[' {
		return nil, errors.New("messages: Input should be a valid list")
	}
	r.bound.messages = msgs.elems
	r.bound.decoded = make([]apiMessage, len(msgs.elems))
	for i, m := range msgs.elems {
		if m.kind != '{' || json.Unmarshal(m.src, &r.bound.decoded[i]) != nil {
			return nil, fmt.Errorf("messages.%d: Input should be a valid dictionary", i)
		}
	}
	r.bound.system = root.get("system")
	if tools := root.get("tools"); tools != nil {
		r.bound.tools = tools
		for _, t := range tools.elems {
			if strings.HasPrefix(t.get("type").str(), "web_search_") {
				r.webSearch = t.get("name").str()
				break
			}
		}
	}
	r.model = root.get("model").str() // a non-string model echoes as ""
	if s := root.get("stream"); s != nil && s.kind == '0' && string(s.raw) == "true" {
		r.stream = true
	}
	if err := checkToolResults(msgs.elems); err != nil {
		return nil, err
	}
	return &r, nil
}

// invalidRequest is a validation failure, answered as a 400
// invalid_request_error with the API's own wording.
type invalidRequest string

func (e invalidRequest) Error() string { return string(e) }

// checkToolResults enforces the API's rule that every tool_use of an
// assistant message is answered by a tool_result in the next message, a
// user message. It is the one structural validation the fake models
// (README "Looser than the API").
func checkToolResults(msgs []*node) error {
	for i, m := range msgs {
		c := m.get("content")
		if m.get("role").str() != "assistant" || c == nil || c.kind != '[' {
			continue
		}
		var uses []string
		for _, b := range c.elems {
			if b.get("type").str() == "tool_use" {
				uses = append(uses, b.get("id").str())
			}
		}
		if len(uses) == 0 {
			continue
		}
		answered := map[string]bool{}
		if i+1 < len(msgs) && msgs[i+1].get("role").str() == "user" {
			if nc := msgs[i+1].get("content"); nc != nil && nc.kind == '[' {
				for _, b := range nc.elems {
					if b.get("type").str() == "tool_result" {
						answered[b.get("tool_use_id").str()] = true
					}
				}
			}
		}
		var missing []string
		for _, id := range uses {
			if !answered[id] {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			return invalidRequest(fmt.Sprintf("messages.%d: `tool_use` ids were found without `tool_result` blocks immediately after: %s. "+
				"Each `tool_use` block must have a corresponding `tool_result` block in the next message.", i, strings.Join(missing, ", ")))
		}
	}
	return nil
}

// step picks the reply for a request: the next step of the marked script,
// or the default reply when there is no marker. A marker naming a missing
// script, or one whose steps are used up, is an error.
func (s *Server) step(msgs []apiMessage) (Step, error) {
	name, ok := scriptName(msgs)
	if !ok {
		return Step{Text: s.o.DefaultText}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	steps := s.o.Scripts[name]
	i := s.next[name]
	if i >= len(steps) {
		return Step{}, fmt.Errorf("fakeanthropic: script %q has no step %d (it has %d)", name, i+1, len(steps))
	}
	s.next[name] = i + 1
	return steps[i], nil
}

// build renders a step as a reply. sig is the HMAC of this request's
// prefix, carried by the thinking block.
func (s *Server) build(seq int, req *request, st Step, sig string, hdr http.Header) *reply {
	rep := &reply{id: fmt.Sprintf("msg_fake_%d", seq), model: req.model, usage: s.o.DefaultUsage}
	if st.Usage != nil {
		rep.usage = *st.Usage
	}
	if st.Model != "" {
		rep.model = st.Model
	}
	if !st.NoThinking {
		if st.RedactedThinking {
			rep.blocks = append(rep.blocks, redactedThinking(sig))
		} else {
			rep.blocks = append(rep.blocks, thinking(sig))
		}
	}
	rep.stopReason = "end_turn"
	if req.webSearch != "" {
		// Auto-answer the server tool: one server_tool_use and
		// web_search_tool_result pair per search the usage reports, as the
		// API sends them. A step's ToolUses do not apply.
		rep.webSearches = max(rep.usage.WebSearchRequests, 1)
		query := map[string]any{"query": lastUserText(req.bound.decoded)}
		for i := range rep.webSearches {
			id := fmt.Sprintf("srvtoolu_fake_%d_%d", seq, i)
			rep.blocks = append(rep.blocks, toolUse("server_tool_use", id, req.webSearch, query), webSearchToolResult(id, s.o.SearchResults))
		}
		rep.blocks = append(rep.blocks, text(st.Text))
	} else {
		if st.Text != "" || len(st.ToolUses) == 0 {
			rep.blocks = append(rep.blocks, text(st.Text))
		}
		for i, tu := range st.ToolUses {
			rep.blocks = append(rep.blocks, toolUse("tool_use", fmt.Sprintf("toolu_fake_%d_%d", seq, i), tu.Name, tu.Input))
		}
		if len(st.ToolUses) > 0 {
			rep.stopReason = "tool_use"
		}
	}
	if st.StopReason != "" {
		rep.stopReason = st.StopReason
	}
	switch {
	case st.InputTransformations != nil:
		t := st.InputTransformations
		rep.transformations = &t
	case hasBeta(hdr, thinkingBindingBeta):
		t := []map[string]any{}
		rep.transformations = &t
	}
	return rep
}

func hasBeta(h http.Header, beta string) bool {
	for _, v := range h.Values("anthropic-beta") {
		for _, b := range strings.Split(v, ",") {
			if strings.TrimSpace(b) == beta {
				return true
			}
		}
	}
	return false
}
