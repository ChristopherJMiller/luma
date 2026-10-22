package sessions

import (
	"strings"
	"testing"
	"time"
)

func TestNowContextUsesConfiguredTimezone(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 10, 19, 2, 30, 0, 0, time.UTC)
	if got, want := NowContext(clock, la), "<luma-now>2026-10-18 (Sunday) 19:30 PDT</luma-now>"; got != want {
		t.Fatalf("NowContext = %q, want %q", got, want)
	}
	if got, want := NowContext(clock, nil), "<luma-now>2026-10-19 (Monday) 02:30 UTC</luma-now>"; got != want {
		t.Fatalf("NowContext without a zone = %q, want %q", got, want)
	}

	// The same clock through a Send: the turn's appended block is the
	// configured zone's evening, not the pod's UTC morning.
	h := newHarness(t)
	ses := h.newSession(family)
	h.send(family, ses.ID, "what day is it?")
	content := h.lastSend().Content
	last := content[len(content)-1]
	if last.Type != "text" || !strings.Contains(last.Text, "2026-10-18") || strings.Contains(last.Text, "2026-10-19") {
		t.Fatalf("appended context = %+v, want the 2026-10-18 Los Angeles date", last)
	}
	if user := h.messages(ses.ID, "user")[0]; strings.Contains(user.Content[0].Text, "luma-now") || len(user.Content) != 1 {
		t.Fatalf("the stored user message carries the volatile context: %+v", user.Content)
	}
}
