package sessions

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/ChristopherJMiller/luma/core/internal/auth"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/store"
)

// Tree returns a person's conversation tree, the whole of it, for
// assistant-ui's branches (C1 /tree, P3): every message of every session of
// the tree with its parent and session, oldest first; head_id, the newest
// message of the most recently active session that has one; each session's
// newest event id; and the pending memory writes of the tree's sessions.
// rootID may name any session of the tree. Another person's tree, and one
// being deleted or gone by the end of the reads, is ErrNotFound (S39): a
// half-deleted tree is never returned.
//
// Each session's part is read through its actor, the single writer of its
// messages and events, so a stream opened since its last_event_ids entry
// replays exactly what its messages do not show yet. The pending writes are
// read after every event id: a write that changes in between is in the tree
// and in a replayed memory.write, which the client applies idempotently.
func (s *Service) Tree(ctx context.Context, p auth.Person, rootID string) (protocol.Tree, error) {
	row, err := s.owned(ctx, p, rootID)
	if err != nil {
		return protocol.Tree{}, err
	}
	root := row.RootID
	if s.deletions.has(root) {
		return protocol.Tree{}, ErrNotFound
	}
	sessions, err := s.st.TreeSessions(ctx, root)
	if err != nil {
		return protocol.Tree{}, err
	}
	if len(sessions) == 0 {
		return protocol.Tree{}, ErrNotFound
	}
	out := protocol.Tree{LastEventIDs: make(map[string]int64, len(sessions))}
	var head *store.Session
	var headID *string
	for i := range sessions {
		ses := &sessions[i]
		var (
			last int64
			msgs []protocol.Message
		)
		if err := s.call(ctx, ses.ID, func(ctx context.Context, a *actor) error {
			var err error
			last, msgs, err = a.treeSnapshot(ctx)
			return err
		}); err != nil {
			return protocol.Tree{}, err
		}
		out.LastEventIDs[ses.ID] = last
		for _, m := range msgs {
			out.Messages = append(out.Messages, protocol.TreeMessage{Message: m, ParentID: m.ParentMessageID, SessionID: m.SessionID})
		}
		// Sessions come root first, then by creation: a later one wins a tie.
		if n := len(msgs); n > 0 && (head == nil || !ses.LastActivityAt.Before(head.LastActivityAt)) {
			head, headID = ses, &msgs[n-1].ID
		}
	}
	out.HeadID = headID
	slices.SortFunc(out.Messages, func(x, y protocol.TreeMessage) int {
		return cmp.Or(x.Message.CreatedAt.Compare(y.Message.CreatedAt), strings.Compare(x.Message.ID, y.Message.ID))
	})
	writes, err := s.st.PendingTreeWrites(ctx, root)
	if err != nil {
		return protocol.Tree{}, err
	}
	for _, w := range writes {
		out.PendingWrites = append(out.PendingWrites, w.Protocol())
	}
	// A delete that began during the reads, or finished (the root goes with
	// every session of its tree), leaves nothing to show.
	if s.deletions.has(root) {
		return protocol.Tree{}, ErrNotFound
	}
	if _, err := s.st.Session(ctx, root); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return protocol.Tree{}, ErrNotFound
		}
		return protocol.Tree{}, err
	}
	return out, nil
}

// treeSnapshot is the session's part of a tree: its newest event id, then
// its messages, the running turn's assistant message as the actor holds it
// (the stored row's content lags until the turn ends). A copy: the actor goes
// on changing its own.
func (a *actor) treeSnapshot(ctx context.Context) (int64, []protocol.Message, error) {
	last, err := a.s.st.LastEventID(ctx, a.id)
	if err != nil {
		return 0, nil, err
	}
	msgs, err := a.s.st.Messages(ctx, a.id)
	if err != nil {
		return 0, nil, err
	}
	if t := a.turn; t != nil && t.asst != nil {
		for i := range msgs {
			if msgs[i].ID == t.asst.ID {
				live := *t.asst
				live.Content = slices.Clone(t.asst.Content)
				msgs[i] = live
			}
		}
	}
	return last, msgs, nil
}
