package sessions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/ChristopherJMiller/luma/core/internal/auth"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/store"
)

// Fork starts a branch of a conversation (P3, P4): edit and regenerate are
// both a fork at a user message M of the session's history, after which the
// client resends M edited or unchanged. The fork is a new session of the
// same tree whose history is everything before M. It copies the source's
// snapshot (agent, models, effort, TTL, workspaces, source, incognito, bound
// models, title and profile_snapshot, byte for byte) and its taint (P9), and
// starts idle; nothing is sent to the conductor until its first message,
// whose host.open resumes the fork point (forkResume).
//
// Another person's session, a tree being deleted, and an M that is not in
// the session's history are ErrNotFound; an M that is not a user message is
// ErrInvalid; M of the running turn is ErrBusy.
func (s *Service) Fork(ctx context.Context, p auth.Person, sessionID, atMessageID string) (protocol.Session, error) {
	if atMessageID == "" {
		return protocol.Session{}, fmt.Errorf("%w: at_message is required", ErrInvalid)
	}
	var (
		src     store.Session
		at      protocol.Message
		tainted bool
	)
	// Through the source's actor: its running turn, its newest message and
	// its taint (one held in memory after a failed write included, S46) are
	// the actor's.
	if err := s.call(ctx, sessionID, func(ctx context.Context, a *actor) error {
		m, err := a.forkPoint(ctx, p, atMessageID)
		if err != nil {
			return err
		}
		src, at, tainted = *a.sess, m, a.tainted()
		return nil
	}); err != nil {
		return protocol.Session{}, err
	}
	taintedAt := src.TaintedAt
	if taintedAt == nil && tainted {
		now := s.clock()
		taintedAt = &now
	}
	row, err := s.st.CreateSession(ctx, store.Session{
		ID: newID("ses_", s.clock()), RootID: src.RootID, ParentID: &src.ID, ForkAtMessageID: &at.ID,
		PersonID: src.PersonID, AgentID: src.AgentID, AgentVersion: src.AgentVersion, AgentSnapshot: src.AgentSnapshot,
		Tier: src.Tier, Model: src.Model, HelperModel: src.HelperModel, Effort: src.Effort, CacheTTL: src.CacheTTL,
		Workspace: src.Workspace, APIWorkspace: src.APIWorkspace, Source: src.Source, Incognito: src.Incognito,
		BoundModels: slices.Clone(src.BoundModels), Title: src.Title, ProfileSnapshot: src.ProfileSnapshot,
		Status: "idle", TaintedAt: taintedAt,
	})
	// A delete that began after the check removes the new row with the tree,
	// or the insert finds the root gone (S39).
	if err == nil && s.deletions.has(src.RootID) {
		return protocol.Session{}, ErrNotFound
	}
	if err != nil {
		if _, rootErr := s.st.Session(ctx, src.RootID); errors.Is(rootErr, store.ErrNotFound) {
			return protocol.Session{}, ErrNotFound
		}
		return protocol.Session{}, err
	}
	slog.Info("sessions: forked", "session", row.ID, "from", src.ID, "root", row.RootID, "tainted", row.TaintedAt != nil)
	return view(row), nil
}

// forkPoint checks a fork of this session at message atID for p and returns
// the message: the session must be p's and its tree not being deleted, the
// message must be in the session's history and a user message, and not the
// running turn's.
func (a *actor) forkPoint(ctx context.Context, p auth.Person, atID string) (protocol.Message, error) {
	if err := a.reload(ctx); err != nil {
		return protocol.Message{}, err
	}
	if a.sess.PersonID != p.ID || a.s.deletions.has(a.sess.RootID) {
		return protocol.Message{}, ErrNotFound
	}
	at, err := a.historyMessage(ctx, atID)
	if err != nil {
		return protocol.Message{}, err
	}
	if at.Role != "user" {
		return protocol.Message{}, fmt.Errorf("%w: at_message must be a user message", ErrInvalid)
	}
	// Only this session's own newest turn can be running: an M further back
	// in the history is followed by a later user message, so its turn ended.
	if a.turn != nil && a.turn.id == at.TurnID {
		return protocol.Message{}, ErrBusy
	}
	return at, nil
}

// historyMessage returns message id if it is in the session's history: its
// own messages and, for a fork, everything before its fork point, found by
// following parent_message_id back from the newest message. Anything else,
// another branch's message or another person's, is ErrNotFound.
func (a *actor) historyMessage(ctx context.Context, id string) (protocol.Message, error) {
	next, err := a.newestMessage(ctx)
	if err != nil {
		return protocol.Message{}, err
	}
	for next != nil {
		m, err := a.s.st.Message(ctx, *next)
		if errors.Is(err, store.ErrNotFound) {
			return protocol.Message{}, ErrNotFound
		}
		if err != nil {
			return protocol.Message{}, err
		}
		if m.ID == id {
			return m, nil
		}
		next = m.ParentMessageID
	}
	return protocol.Message{}, ErrNotFound
}

// forkParent is the parent of a fork's first message: the last message
// before its fork point M (M's own parent), or none when M was the first
// message (P4). A session that is not a fork has none.
func (a *actor) forkParent(ctx context.Context) (*string, error) {
	if a.sess.ForkAtMessageID == nil {
		return nil, nil
	}
	at, err := a.s.st.Message(ctx, *a.sess.ForkAtMessageID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return at.ParentMessageID, nil
}

// forkResume is the resume of a fork the SDK has not seen yet (its first
// open, and any reopen before the host reports the fork's own session id):
// the owner of the last assistant message before M, resumed at that
// message's sdk_uuid with forkSession (P4, Spike S8: FORK_STRATEGY =
// resumeSessionAt), with the owner's last_totals as the informational
// baseline (S31). The owner is the session that holds the message, which for
// a fork of a fork may be an ancestor. Assistant messages without an
// sdk_uuid (a turn that never reached a result, S45) are skipped, and so is
// one whose owner has no sdk_session_id; with nothing resumable before M the
// fork is a fresh session (nil).
func (a *actor) forkResume(ctx context.Context) (*protocol.Resume, error) {
	next, err := a.forkParent(ctx)
	if err != nil {
		return nil, err
	}
	for next != nil {
		m, err := a.s.st.Message(ctx, *next)
		if err != nil {
			return nil, err
		}
		if m.Role == "assistant" && m.SDKUUID != nil {
			owner, err := a.s.st.Session(ctx, m.SessionID)
			if err != nil {
				return nil, err
			}
			if owner.SDKSessionID != nil {
				r := &protocol.Resume{SDKSessionID: *owner.SDKSessionID, Fork: &protocol.ResumeFork{AtSDKUUID: *m.SDKUUID}}
				if owner.LastTotals != nil {
					t := *owner.LastTotals
					r.BaselineTotals = &t
				}
				return r, nil
			}
			slog.Warn("sessions: a fork point's session has no sdk_session_id; looking further back",
				"session", a.id, "owner", owner.ID, "message", m.ID)
		}
		next = m.ParentMessageID
	}
	return nil, nil
}
