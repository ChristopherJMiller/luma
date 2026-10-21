package protocol_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/ChristopherJMiller/luma/core/internal/protocol"
)

// A fixture is hand-written JSON in protocol/fixtures. rt decodes it into its
// Go type and encodes it again; typ is the wire `type` the frame or event must
// carry ("" for a plain body).
type fixture struct {
	file string
	typ  string
	rt   func([]byte) ([]byte, error)
}

func plain[T any](data []byte) ([]byte, error) {
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

func event(data []byte) ([]byte, error) {
	e, err := protocol.DecodeEvent(data)
	if err != nil {
		return nil, err
	}
	return json.Marshal(e)
}

func linkFrame(data []byte) ([]byte, error) {
	f, err := protocol.DecodeLinkFrame(data)
	if err != nil {
		return nil, err
	}
	return protocol.EncodeLinkFrame(f)
}

func clientEvent(data []byte) ([]byte, error) {
	e, err := protocol.DecodeClientEvent(data)
	if err != nil {
		return nil, err
	}
	return protocol.EncodeClientEvent(e)
}

var clientFixtures = []fixture{
	{"event-session-status.json", "session.status", event},
	{"event-message-user.json", "message.user", event},
	{"event-message-started.json", "message.started", event},
	{"event-message-text-delta.json", "message.text_delta", event},
	{"event-tool-started.json", "tool.started", event},
	{"event-tool-finished.json", "tool.finished", event},
	{"event-message-completed.json", "message.completed", event},
	{"event-model-switched.json", "model.switched", event},
	{"event-memory-write.json", "memory.write", event},
	{"event-usage.json", "usage", event},
	{"event-error.json", "error", event},
	{"client-user-message.json", "user.message", clientEvent},
	{"client-user-interrupt.json", "user.interrupt", clientEvent},
	{"create-session.json", "", plain[protocol.CreateSession]},
	{"create-session-minimal.json", "", plain[protocol.CreateSession]},
	{"session.json", "", plain[protocol.Session]},
	{"session-list.json", "", plain[protocol.SessionList]},
	{"session-patch.json", "", plain[protocol.SessionPatch]},
	{"fork-request.json", "", plain[protocol.ForkRequest]},
	{"send-result.json", "", plain[protocol.SendResult]},
	{"tree.json", "", plain[protocol.Tree]},
	{"tree-empty.json", "", plain[protocol.Tree]},
	{"attachment.json", "", plain[protocol.Attachment]},
	{"attachment-pdf.json", "", plain[protocol.Attachment]},
	{"profile.json", "", plain[protocol.Profile]},
	{"profile-update.json", "", plain[protocol.ProfileUpdate]},
	{"memory-write.json", "", plain[protocol.MemoryWrite]},
	{"me.json", "", plain[protocol.Me]},
	{"agent-summary.json", "", plain[protocol.AgentSummary]},
	{"agent-list.json", "", plain[protocol.AgentList]},
	{"error-info.json", "", plain[protocol.ErrorInfo]},
}

var linkFixtures = []fixture{
	{"host-open.json", "host.open", linkFrame},
	{"host-open-resume.json", "host.open", linkFrame},
	{"host-open-resume-plain.json", "host.open", linkFrame},
	{"turn-send.json", "turn.send", linkFrame},
	{"turn-interrupt.json", "turn.interrupt", linkFrame},
	{"host-close.json", "host.close", linkFrame},
	{"tree-purge.json", "tree.purge", linkFrame},
	{"rpc-result-applied.json", "rpc.result", linkFrame},
	{"rpc-result-full.json", "rpc.result", linkFrame},
	{"rpc-result-rejected.json", "rpc.result", linkFrame},
	{"rpc-result-pending.json", "rpc.result", linkFrame},
	{"rpc-result-disabled.json", "rpc.result", linkFrame},
	{"rpc-result-error.json", "rpc.result", linkFrame},
	{"shutdown.json", "shutdown", linkFrame},
	{"hello.json", "hello", linkFrame},
	{"host-state-ready.json", "host.state", linkFrame},
	{"host-state-ready-sdk.json", "host.state", linkFrame},
	{"host-state-closed.json", "host.state", linkFrame},
	{"event-message-started.json", "event", linkFrame},
	{"event-text-delta.json", "event", linkFrame},
	{"event-tool-started.json", "event", linkFrame},
	{"event-tool-finished.json", "event", linkFrame},
	{"event-tool-finished-bare.json", "event", linkFrame},
	{"event-main-request-usage.json", "event", linkFrame},
	{"event-model-switched.json", "event", linkFrame},
	{"event-turn-result.json", "event", linkFrame},
	{"event-turn-result-null-basis.json", "event", linkFrame},
	{"event-error.json", "event", linkFrame},
	{"event-error-plain.json", "event", linkFrame},
	{"rpc.json", "rpc", linkFrame},
}

var internalFixtures = []fixture{
	{"eval-spec.json", "", plain[protocol.EvalSpec]},
	{"eval-run.json", "", plain[protocol.EvalRun]},
	{"usage-reconciliation.json", "", plain[protocol.UsageReconciliation]},
	{"usage-reconciliation-result.json", "", plain[protocol.UsageReconciliationResult]},
}

func fixtureDir(t *testing.T, sub string) string {
	t.Helper()
	return filepath.Join("..", "..", "..", "protocol", "fixtures", sub)
}

func jsonEqual(t *testing.T, want, got []byte) {
	t.Helper()
	var w, g any
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("fixture is not JSON: %v", err)
	}
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("encoded output is not JSON: %v", err)
	}
	if !reflect.DeepEqual(w, g) {
		t.Errorf("round trip changed the frame\nwant: %s\n got: %s", want, got)
	}
}

func TestFixturesRoundTrip(t *testing.T) {
	for sub, list := range map[string][]fixture{"client": clientFixtures, "link": linkFixtures, "internal": internalFixtures} {
		for _, fx := range list {
			t.Run(sub+"/"+fx.file, func(t *testing.T) {
				data, err := os.ReadFile(filepath.Join(fixtureDir(t, sub), fx.file))
				if err != nil {
					t.Fatal(err)
				}
				if fx.typ != "" {
					var h struct {
						Type string `json:"type"`
					}
					if err := json.Unmarshal(data, &h); err != nil || h.Type != fx.typ {
						t.Fatalf("fixture type = %q (%v), want %q", h.Type, err, fx.typ)
					}
				}
				out, err := fx.rt(data)
				if err != nil {
					t.Fatalf("decode/encode: %v", err)
				}
				jsonEqual(t, data, out)
			})
		}
	}
}

// Every fixture on disk is listed above, and every listed one exists, so a new
// frame cannot be added to the directory without a Go round trip.
func TestEveryFixtureIsTested(t *testing.T) {
	for sub, list := range map[string][]fixture{"client": clientFixtures, "link": linkFixtures, "internal": internalFixtures} {
		listed := map[string]bool{}
		for _, fx := range list {
			if listed[fx.file] {
				t.Errorf("%s/%s listed twice", sub, fx.file)
			}
			listed[fx.file] = true
		}
		entries, err := os.ReadDir(fixtureDir(t, sub))
		if err != nil {
			t.Fatal(err)
		}
		var onDisk []string
		for _, e := range entries {
			onDisk = append(onDisk, e.Name())
			if !listed[e.Name()] {
				t.Errorf("%s/%s is not in the Go fixture table", sub, e.Name())
			}
		}
		sort.Strings(onDisk)
		for f := range listed {
			if i := sort.SearchStrings(onDisk, f); i == len(onDisk) || onDisk[i] != f {
				t.Errorf("%s/%s is listed but missing on disk", sub, f)
			}
		}
	}
}

// Every wire type of C1 and C2 has at least one fixture.
func TestEveryWireTypeHasAFixture(t *testing.T) {
	wantEvents := []string{"session.status", "message.user", "message.started", "message.text_delta",
		"tool.started", "tool.finished", "message.completed", "model.switched", "memory.write", "usage", "error"}
	wantClient := []string{"user.message", "user.interrupt"}
	wantFrames := []string{"host.open", "turn.send", "turn.interrupt", "host.close", "tree.purge", "rpc.result",
		"shutdown", "hello", "host.state", "event", "rpc"}
	wantLinkEvents := []string{"message_started", "text_delta", "tool_started", "tool_finished",
		"main_request_usage", "model_switched", "turn_result", "error"}

	have := map[string]bool{}
	for _, fx := range clientFixtures {
		have["client:"+fx.typ] = true
	}
	for _, fx := range linkFixtures {
		have["frame:"+fx.typ] = true
	}
	for _, name := range wantEvents {
		if !have["client:"+name] {
			t.Errorf("no client fixture for event %s", name)
		}
	}
	for _, name := range wantClient {
		if !have["client:"+name] {
			t.Errorf("no client fixture for client event %s", name)
		}
	}
	for _, name := range wantFrames {
		if !have["frame:"+name] {
			t.Errorf("no link fixture for frame %s", name)
		}
	}
	for _, name := range wantLinkEvents {
		found := false
		for _, fx := range linkFixtures {
			if fx.typ != "event" {
				continue
			}
			data, err := os.ReadFile(filepath.Join(fixtureDir(t, "link"), fx.file))
			if err != nil {
				t.Fatal(err)
			}
			var env struct {
				Event struct {
					Type string `json:"type"`
				} `json:"event"`
			}
			if err := json.Unmarshal(data, &env); err != nil {
				t.Fatal(err)
			}
			found = found || env.Event.Type == name
		}
		if !found {
			t.Errorf("no link fixture for link event %s", name)
		}
	}
}

func TestUnknownTypeRejected(t *testing.T) {
	tests := []struct {
		name string
		run  func() error
	}{
		{"link frame", func() error {
			_, err := protocol.DecodeLinkFrame([]byte(`{"type":"host.explode","session_id":"x"}`))
			return err
		}},
		{"link frame without type", func() error { _, err := protocol.DecodeLinkFrame([]byte(`{"session_id":"x"}`)); return err }},
		{"link event", func() error {
			_, err := protocol.DecodeLinkFrame([]byte(`{"type":"event","session_id":"x","turn_id":"t","event":{"type":"mystery"}}`))
			return err
		}},
		{"client event", func() error {
			_, err := protocol.DecodeEvent([]byte(`{"id":1,"session_id":"x","type":"message.vanished","created_at":"2026-10-21T09:00:00Z"}`))
			return err
		}},
		{"user event", func() error { _, err := protocol.DecodeClientEvent([]byte(`{"type":"user.shout"}`)); return err }},
		{"content block", func() error {
			_, err := protocol.DecodeLinkFrame([]byte(`{"type":"turn.send","session_id":"x","turn_id":"t","content":[{"type":"audio"}],"untrusted":false}`))
			return err
		}},
		{"message part", func() error {
			var m protocol.Message
			return json.Unmarshal([]byte(`{"content":[{"type":"video"}]}`), &m)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.run(); err == nil {
				t.Fatal("decoded without error")
			}
		})
	}
}

// A frame built in Go encodes with its type tag and nothing else added.
func TestEncodeAddsTheTypeTag(t *testing.T) {
	got, err := protocol.EncodeLinkFrame(protocol.TurnInterrupt{SessionID: "ses_x"})
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, []byte(`{"type":"turn.interrupt","session_id":"ses_x"}`), got)

	got, err = protocol.EncodeLinkFrame(protocol.EventFrame{SessionID: "ses_x", TurnID: "turn_x", Event: protocol.LinkTextDelta{Text: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, []byte(`{"type":"event","session_id":"ses_x","turn_id":"turn_x","event":{"type":"text_delta","text":"hi"}}`), got)

	got, err = protocol.EncodeClientEvent(protocol.UserInterrupt{})
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, []byte(`{"type":"user.interrupt"}`), got)
}

func TestDecodedValuesAreWhatTheFixtureSays(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(fixtureDir(t, "link"), "event-error.json"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := protocol.DecodeLinkFrame(data)
	if err != nil {
		t.Fatal(err)
	}
	ev, ok := f.(protocol.EventFrame)
	if !ok {
		t.Fatalf("frame is %T, want EventFrame", f)
	}
	le, ok := ev.Event.(protocol.LinkError)
	if !ok || le.Code != protocol.CodeUpstreamError || !le.Retryable || le.Detail != "thinking_binding" {
		t.Fatalf("event = %#v", ev.Event)
	}

	data, err = os.ReadFile(filepath.Join(fixtureDir(t, "client"), "event-usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	e, err := protocol.DecodeEvent(data)
	if err != nil {
		t.Fatal(err)
	}
	u, ok := e.Body.(protocol.Usage)
	if !ok || e.ID != 110 || u.TurnID != "turn_01J9ZQ3M5K7N8P2R4T6V8W0XYT" || u.CostUSD != 0.0125 {
		t.Fatalf("event = %#v", e)
	}
}

func TestMaxAttachmentBytes(t *testing.T) {
	if protocol.MaxAttachmentBytes != 20*1024*1024 {
		t.Fatalf("MaxAttachmentBytes = %d, want 20 MiB", protocol.MaxAttachmentBytes)
	}
}

// The C3 table, typed by hand: an intended duplicate of the copy.
func TestErrorCopyEqualsTheC3Table(t *testing.T) {
	want := map[string]string{
		"rate_limited":                  "Luma is getting a lot of requests right now. Wait a moment and try again.",
		"overloaded":                    "Claude is overloaded right now. Try again in a minute.",
		"conductor_restarted":           "Luma restarted while answering. Your conversation is safe — send your message again.",
		"budget_exhausted":              "Luma has used this month's budget. It will be back on {resets_at, local date}.",
		"budget_warning":                "Luma has used {pct}% of this month's budget.",
		"maintenance":                   "Luma is down for maintenance. Try again later.",
		"capacity":                      "Luma is busy with other conversations. Try again in a minute.",
		"busy":                          "Luma is still answering in this conversation.",
		"attachment_too_large":          "That file is over 20 MB.",
		"image_too_large":               "That image is too large. Photos up to 40 megapixels work.",
		"pdf_too_long":                  "That PDF has more than {max} pages, the most Luma reads in this mode.",
		"conversation_attachments_full": "This conversation already holds 20 MB of files. Start a new chat to add more.",
		"unsupported_attachment":        "Luma can read photos, PDFs and text files.",
		"memory_full":                   "The profile is full. Ask the person to tidy it in Luma's profile page.",
		"memory_disabled":               "Memory is off in this conversation.",
		"memory_pending_limit":          "Several memory changes are already waiting for the person's review. Don't save more in this turn.",
		"forget_too_short":              "Say at least 3 characters of what to forget.",
		"upstream_error":                "Something went wrong talking to Claude. Try again.",
		"not_invited":                   "This account isn't invited to Luma.",
	}
	got := map[string]string{}
	for code, copy := range protocol.ErrorCopy {
		got[string(code)] = copy
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ErrorCopy differs from the C3 table:\n got: %v\nwant: %v", got, want)
	}
}
