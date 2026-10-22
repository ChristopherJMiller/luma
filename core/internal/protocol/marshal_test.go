package protocol_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ChristopherJMiller/luma/core/internal/protocol"
)

func walk(t *testing.T, root any, path string) any {
	t.Helper()
	cur := root
	if path == "" {
		return cur
	}
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %q: %q is under a non-object %T", path, seg, cur)
		}
		v, present := m[seg]
		if !present {
			t.Fatalf("path %q: no key %q", path, seg)
		}
		cur = v
	}
	return cur
}

// A zero value must put [] or {} where the contract says an array or a map,
// never null (the TS side declares them non-nullable). The paths are typed by
// hand.
func TestZeroValuesEncodeContainersAsEmpty(t *testing.T) {
	zeroAgent := []string{"tools", "builtin_tools", "mcp", "untrusted_inputs"}
	tests := []struct {
		name   string
		v      any
		arrays []string
		maps   []string
	}{
		{"Message", protocol.Message{}, []string{"content"}, nil},
		{"Tree", protocol.Tree{}, []string{"messages", "pending_writes"}, []string{"last_event_ids"}},
		{"Profile", protocol.Profile{}, []string{"pending"}, nil},
		{"SessionList", protocol.SessionList{}, []string{"sessions"}, nil},
		{"AgentList", protocol.AgentList{}, []string{"agents"}, nil},
		{"AgentSummary", protocol.AgentSummary{}, []string{"tiers"}, nil},
		{"UserMessage", protocol.UserMessage{}, []string{"content", "attachment_ids"}, nil},
		{"HostSpec", protocol.HostSpec{}, append([]string{"webfetch_deny_domains"}, prefix("agent.", zeroAgent)...), []string{"agent.subagents"}},
		{"AgentSpec", protocol.AgentSpec{}, zeroAgent, []string{"subagents"}},
		{"SubagentSpec", protocol.SubagentSpec{}, []string{"tools"}, nil},
		{"TurnUsage", protocol.TurnUsage{}, nil, []string{"models"}},
		{"Totals", protocol.Totals{}, nil, []string{"models"}},
		{"TurnResult", protocol.TurnResult{}, []string{"content"}, []string{"usage.models", "totals.models"}},
		{"TurnSend", protocol.TurnSend{}, []string{"content"}, nil},
		{"Resume baseline", protocol.Resume{BaselineTotals: &protocol.Totals{}}, nil, []string{"baseline_totals.models"}},
		{"EvalSpec", protocol.EvalSpec{}, append([]string{"webfetch_deny_domains"}, prefix("agent.", zeroAgent)...), []string{"agent.subagents"}},
		{"EvalResult", protocol.EvalResult{}, []string{"failures"}, nil},
		{"EvalRun", protocol.EvalRun{}, []string{"results"}, nil},
		{"UsageReconciliation", protocol.UsageReconciliation{}, []string{"workspaces"}, nil},
		{"UsageReconciliationResult", protocol.UsageReconciliationResult{}, []string{"workspaces"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := json.Marshal(tt.v)
			if err != nil {
				t.Fatal(err)
			}
			var root any
			if err := json.Unmarshal(b, &root); err != nil {
				t.Fatal(err)
			}
			for _, p := range tt.arrays {
				if got := walk(t, root, p); !reflect.DeepEqual(got, []any{}) {
					t.Errorf("%s = %#v in %s, want []", p, got, b)
				}
			}
			for _, p := range tt.maps {
				if got := walk(t, root, p); !reflect.DeepEqual(got, map[string]any{}) {
					t.Errorf("%s = %#v in %s, want {}", p, got, b)
				}
			}
		})
	}
	// Nested: a HostOpen frame, a Tree with a message, and an EventFrame.
	b, err := json.Marshal(protocol.HostOpen{})
	if err != nil {
		t.Fatal(err)
	}
	var root any
	_ = json.Unmarshal(b, &root)
	if got := walk(t, root, "spec.agent.tools"); !reflect.DeepEqual(got, []any{}) {
		t.Errorf("host.open spec.agent.tools = %#v", got)
	}
	b, err = json.Marshal(protocol.Tree{Messages: []protocol.TreeMessage{{}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(b, &root)
	if got := walk(t, root, "messages"); !reflect.DeepEqual(got.([]any)[0].(map[string]any)["message"].(map[string]any)["content"], []any{}) {
		t.Errorf("tree message content = %s", b)
	}
	b, err = json.Marshal(protocol.EventFrame{Event: protocol.TurnResult{}})
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(b, &root)
	if got := walk(t, root, "event.content"); !reflect.DeepEqual(got, []any{}) {
		t.Errorf("event turn_result content = %s", b)
	}
}

func prefix(p string, ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = p + s
	}
	return out
}

// json.Marshal of any frame or client event carries its type tag, and equals
// what EncodeLinkFrame / EncodeClientEvent write.
func TestMarshalCarriesTheTypeTag(t *testing.T) {
	frames := []protocol.LinkFrame{
		protocol.HostOpen{}, protocol.TurnSend{}, protocol.TurnInterrupt{}, protocol.HostClose{},
		protocol.TreePurge{}, protocol.RPCResult{}, protocol.Shutdown{}, protocol.Hello{},
		protocol.HostState{}, protocol.EventFrame{Event: protocol.LinkTextDelta{}}, protocol.RPC{},
		protocol.TreePurged{},
	}
	wantTypes := []string{"host.open", "turn.send", "turn.interrupt", "host.close", "tree.purge",
		"rpc.result", "shutdown", "hello", "host.state", "event", "rpc", "tree.purged"}
	for i, f := range frames {
		b, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		var h struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(b, &h); err != nil || h.Type != wantTypes[i] {
			t.Errorf("%T marshals with type %q (%v), want %q", f, h.Type, err, wantTypes[i])
		}
		enc, err := protocol.EncodeLinkFrame(f)
		if err != nil {
			t.Fatal(err)
		}
		if string(enc) != string(b) {
			t.Errorf("%T: EncodeLinkFrame %s != json.Marshal %s", f, enc, b)
		}
	}
	for want, e := range map[string]protocol.ClientEvent{
		"user.message":   protocol.UserMessage{},
		"user.interrupt": protocol.UserInterrupt{},
	} {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		var h struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(b, &h); err != nil || h.Type != want {
			t.Errorf("%T marshals with type %q (%v), want %q", e, h.Type, err, want)
		}
	}
}

func TestClientContentIsTextOnly(t *testing.T) {
	_, err := protocol.DecodeClientEvent([]byte(`{"type":"user.message","client_id":"c","content":[{"type":"image","source":{}}],"attachment_ids":[]}`))
	if err == nil {
		t.Fatal("an image block in a client event decoded")
	}
}

// protocol/fixtures/limits.json is hand-written and shared with the TS test,
// so the two sides agree on every constant.
func TestLimitsMatchTheSharedFile(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "protocol", "fixtures", "limits.json"))
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]int64
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{
		"max_attachment_bytes":      protocol.MaxAttachmentBytes,
		"max_tree_attachment_bytes": protocol.MaxTreeAttachmentBytes,
		"max_image_megapixels":      protocol.MaxImageMegapixels,
		"max_image_long_edge":       protocol.MaxImageLongEdge,
		"max_tool_url_length":       protocol.MaxToolURLLength,
		"max_profile_bytes":         protocol.MaxProfileBytes,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("limits differ from protocol/fixtures/limits.json\n got: %v\nwant: %v", got, want)
	}
}

// Every Code* constant has copy, and nothing else does.
func TestEveryCodeHasCopy(t *testing.T) {
	codes := []protocol.ErrorCode{
		protocol.CodeRateLimited, protocol.CodeOverloaded, protocol.CodeConductorRestarted,
		protocol.CodeBudgetExhausted, protocol.CodeBudgetWarning, protocol.CodeMaintenance,
		protocol.CodeCapacity, protocol.CodeBusy, protocol.CodeAttachmentTooLarge,
		protocol.CodeImageTooLarge, protocol.CodePDFTooLong, protocol.CodeConversationAttachmentsFull,
		protocol.CodeUnsupportedAttachment, protocol.CodeMemoryFull, protocol.CodeMemoryDisabled,
		protocol.CodeMemoryPendingLimit, protocol.CodeForgetTooShort, protocol.CodeNotFound,
		protocol.CodeInvalidRequest, protocol.CodeRequestTooLarge, protocol.CodeUpstreamError,
		protocol.CodeNotInvited,
	}
	for _, c := range codes {
		if protocol.ErrorCopy[c] == "" {
			t.Errorf("code %q has no copy", c)
		}
	}
	if len(protocol.ErrorCopy) != len(codes) {
		t.Errorf("ErrorCopy has %d entries, %d codes", len(protocol.ErrorCopy), len(codes))
	}
}
