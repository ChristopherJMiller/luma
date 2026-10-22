package sessions

import (
	"time"
	// The pod runs in UTC and distroless has no zoneinfo, so the binary
	// carries its own (P14): LUMA_TIMEZONE must load wherever Core runs.
	_ "time/tzdata"
)

// NowContext is the volatile context appended to every user turn (P14), in
// the configured zone: "<luma-now>2026-10-18 (Sunday) 19:30 PDT</luma-now>".
// A nil loc is UTC.
func NowContext(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	return "<luma-now>" + t.In(loc).Format("2006-01-02 (Monday) 15:04 MST") + "</luma-now>"
}
