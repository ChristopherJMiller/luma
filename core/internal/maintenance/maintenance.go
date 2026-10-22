// Package maintenance is Core's maintenance mode (§5.2.6): while it is on,
// new sessions and turns are refused with `maintenance`, running turns are
// drained (then interrupted) and the conductor is told to close every CLI.
// The state survives restarts in settings.maintenance = {enabled, reason,
// since}. It is switched only through the loopback admin listener (admin.go),
// which `luma-core maintenance` calls inside the pod.
package maintenance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ChristopherJMiller/luma/core/internal/metrics"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/store"
)

const (
	// DefaultDrainTimeout is how long `maintenance on` waits for running
	// turns before it interrupts them.
	DefaultDrainTimeout = 120 * time.Second
	// interruptGrace is how long interrupted turns get to send their
	// turn_result (so their spend is ledgered) before the shutdown closes
	// their hosts anyway.
	interruptGrace = 10 * time.Second
	// drainPoll is how often the drain looks at the running turns.
	drainPoll = 250 * time.Millisecond

	settingKey = "maintenance"
)

// Settings is the settings table; *store.Store implements it.
type Settings interface {
	Setting(ctx context.Context, key string) (json.RawMessage, error)
	SetSetting(ctx context.Context, key string, value any) error
}

// Fleet is the conductor as maintenance drains it; *link.Server implements it.
type Fleet interface {
	RunningTurns() []string
	OpenHosts() int
	Interrupt(ctx context.Context, sessionID string) error
	Shutdown(ctx context.Context, reason string) error
}

// state is the persisted settings.maintenance value.
type state struct {
	Enabled bool      `json:"enabled"`
	Reason  string    `json:"reason"`
	Since   time.Time `json:"since"` // when it last changed between on and off
}

// Status is the admin API's answer.
type Status struct {
	Enabled      bool      `json:"enabled"`
	Reason       string    `json:"reason"`
	Since        time.Time `json:"since"`
	RunningTurns int       `json:"running_turns"`
	OpenHosts    int       `json:"open_hosts"`
}

// Line is what `luma-core maintenance` prints.
func (s Status) Line() string {
	return fmt.Sprintf("enabled=%t running_turns=%d open_hosts=%d", s.Enabled, s.RunningTurns, s.OpenHosts)
}

// Maintenance is the switch. It is a sessions.Gate.
type Maintenance struct {
	st    Settings
	fleet Fleet
	m     *metrics.Core
	clock func() time.Time
	poll  time.Duration
	grace time.Duration

	enabled atomic.Bool // read by every Create and Send
	mu      sync.Mutex  // guards state and orders its saves
	state   state
	drainMu sync.Mutex // one drain at a time
}

// New loads the saved state: a Core restarted in maintenance comes up in it.
// A saved value that does not decode is an error.
func New(ctx context.Context, st Settings, fleet Fleet, m *metrics.Core) (*Maintenance, error) {
	mt := &Maintenance{st: st, fleet: fleet, m: m, clock: time.Now, poll: drainPoll, grace: interruptGrace}
	raw, err := st.Setting(ctx, settingKey)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return nil, fmt.Errorf("maintenance: load: %w", err)
	default:
		if err := json.Unmarshal(raw, &mt.state); err != nil {
			return nil, fmt.Errorf("maintenance: settings.maintenance does not decode: %w", err)
		}
	}
	mt.publish()
	return mt, nil
}

// CheckNewTurn refuses Create and Send while maintenance is on (S1).
func (mt *Maintenance) CheckNewTurn(context.Context) *protocol.ErrorInfo {
	if !mt.enabled.Load() {
		return nil
	}
	return &protocol.ErrorInfo{Code: protocol.CodeMaintenance, Message: protocol.ErrorCopy[protocol.CodeMaintenance]}
}

// Enabled reports whether maintenance is on (for /v1/me, Task 15).
func (mt *Maintenance) Enabled() bool { return mt.enabled.Load() }

// Status is the current state and the conductor's load. Enabled is the
// gate's own flag, which a failed save leaves closed.
func (mt *Maintenance) Status() Status {
	mt.mu.Lock()
	s := mt.state
	mt.mu.Unlock()
	return Status{Enabled: mt.enabled.Load(), Reason: s.Reason, Since: s.Since,
		RunningTurns: len(mt.fleet.RunningTurns()), OpenHosts: mt.fleet.OpenHosts()}
}

// On refuses new work at once, saves the state, waits up to drain for the
// running turns, interrupts the rest (giving them a short grace to finish),
// then sends shutdown{reason:"maintenance"}. An Off during the drain stops
// it. If the state cannot be saved, the gate stays closed and the error is
// returned before any drain.
func (mt *Maintenance) On(ctx context.Context, reason string, drain time.Duration) (Status, error) {
	if err := mt.set(ctx, true, reason); err != nil {
		return mt.Status(), err
	}
	slog.Info("maintenance: on", "drain_timeout", drain.String())
	mt.drainMu.Lock()
	defer mt.drainMu.Unlock()

	if !mt.wait(ctx, drain) {
		return mt.Status(), ctx.Err()
	}
	if !mt.enabled.Load() {
		return mt.Status(), nil
	}
	if running := mt.fleet.RunningTurns(); len(running) > 0 {
		slog.Info("maintenance: interrupting the turns still running", "turns", len(running))
		for _, id := range running {
			if err := mt.fleet.Interrupt(ctx, id); err != nil {
				slog.Warn("maintenance: interrupt failed", "session", id, "err", err)
			}
		}
		if !mt.wait(ctx, mt.grace) {
			return mt.Status(), ctx.Err()
		}
		if !mt.enabled.Load() {
			return mt.Status(), nil
		}
	}
	if err := mt.fleet.Shutdown(ctx, "maintenance"); err != nil {
		return mt.Status(), fmt.Errorf("maintenance: shutdown: %w", err)
	}
	slog.Info("maintenance: conductor told to close every CLI")
	return mt.Status(), nil
}

// Off clears the flag; hosts reopen lazily on the next turn. If the state
// cannot be saved, maintenance stays on and the error is returned.
func (mt *Maintenance) Off(ctx context.Context) (Status, error) {
	if err := mt.set(ctx, false, ""); err != nil {
		return mt.Status(), err
	}
	slog.Info("maintenance: off")
	return mt.Status(), nil
}

// set saves the new state. Turning on closes the gate before the save, and
// turning off opens it only after, so a failed save never lets work in.
func (mt *Maintenance) set(ctx context.Context, on bool, reason string) error {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	next := state{Enabled: on, Reason: reason, Since: mt.state.Since}
	if on != mt.state.Enabled || next.Since.IsZero() {
		next.Since = mt.clock().UTC()
	}
	if on {
		mt.enabled.Store(true)
		mt.m.MaintenanceEnabled.Set(1)
	}
	if err := mt.st.SetSetting(ctx, settingKey, next); err != nil {
		return fmt.Errorf("maintenance: save: %w", err)
	}
	mt.state = next
	mt.publish()
	return nil
}

// publish mirrors the state into the flag and the gauge.
func (mt *Maintenance) publish() {
	mt.enabled.Store(mt.state.Enabled)
	v := 0.0
	if mt.state.Enabled {
		v = 1
	}
	mt.m.MaintenanceEnabled.Set(v)
}

// wait returns once no turn runs, maintenance is off, or d has passed. It
// returns false only if ctx ended.
func (mt *Maintenance) wait(ctx context.Context, d time.Duration) bool {
	deadline := mt.clock().Add(d)
	for mt.enabled.Load() && len(mt.fleet.RunningTurns()) > 0 {
		left := deadline.Sub(mt.clock())
		if left <= 0 {
			return true
		}
		t := time.NewTimer(min(mt.poll, left))
		select {
		case <-ctx.Done():
			t.Stop()
			return false
		case <-t.C:
		}
	}
	return true
}
