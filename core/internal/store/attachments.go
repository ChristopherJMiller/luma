package store

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// Attachment mirrors a C4 `attachments` row. RootID is nil until the
// attachment is bound to a conversation tree.
type Attachment struct {
	ID        string
	PersonID  string
	RootID    *string
	MediaType string
	Filename  string
	SizeBytes int64
	Width     *int
	Height    *int
	Pages     *int
	SHA256    string
	Data      []byte
	CreatedAt time.Time
}

var (
	// ErrTreeFull: the bind would take the tree's attachments past its cap.
	ErrTreeFull = errors.New("store: the tree's attachments are full")
	// ErrBound: the attachment belongs to a conversation, so only deleting
	// the tree removes it.
	ErrBound = errors.New("store: the attachment is bound to a conversation")
)

// InsertAttachment stores an upload. It copies the row in with COPY, which
// encodes the bytes once; an INSERT's bytea parameter is encoded twice (the
// query builder, then the wire buffer), a third copy of a 20 MiB upload.
func (s *Store) InsertAttachment(ctx context.Context, a Attachment) error {
	_, err := s.pool.CopyFrom(ctx, pgx.Identifier{"attachments"},
		[]string{"id", "person_id", "root_id", "media_type", "filename", "size_bytes", "width", "height", "pages", "sha256", "data"},
		pgx.CopyFromRows([][]any{{a.ID, a.PersonID, a.RootID, a.MediaType, a.Filename, a.SizeBytes, a.Width, a.Height, a.Pages, a.SHA256, a.Data}}))
	return err
}

// Attachment returns an attachment, bytes included.
func (s *Store) Attachment(ctx context.Context, id string) (Attachment, error) {
	var a Attachment
	err := s.pool.QueryRow(ctx, `select id, person_id, root_id, media_type, filename, size_bytes, width,
		height, pages, sha256, data, created_at from attachments where id = $1`, id).
		Scan(&a.ID, &a.PersonID, &a.RootID, &a.MediaType, &a.Filename, &a.SizeBytes, &a.Width,
			&a.Height, &a.Pages, &a.SHA256, &a.Data, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Attachment{}, ErrNotFound
	}
	return a, err
}

// AttachmentMeta returns a person's attachment without its bytes (Data is
// nil). Another person's attachment, or a missing one, is ErrNotFound.
func (s *Store) AttachmentMeta(ctx context.Context, personID, id string) (Attachment, error) {
	var a Attachment
	err := s.pool.QueryRow(ctx, `select id, person_id, root_id, media_type, filename, size_bytes, width,
		height, pages, sha256, created_at from attachments where id = $1 and person_id = $2`, id, personID).
		Scan(&a.ID, &a.PersonID, &a.RootID, &a.MediaType, &a.Filename, &a.SizeBytes, &a.Width,
			&a.Height, &a.Pages, &a.SHA256, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Attachment{}, ErrNotFound
	}
	return a, err
}

// BindAttachments binds a person's attachments to a tree for one new
// message, each unbound or already bound to this tree, as long as the tree
// stays within maxTreeBytes. It binds nothing and returns ErrNotFound if
// any id is missing, not the person's, or bound to another tree (also when
// another tree takes one meanwhile), and ErrTreeFull past the cap.
// Duplicate ids count once.
//
// The cap is on what the tree's requests carry, so every message counts
// its attachments: the tree's use is the attachments its user messages
// reference, once per message (a re-sent attachment is in the transcript
// again), plus the ones bound to it that no message references yet (a
// send whose message was never stored), other than this message's own,
// which count with it.
//
// Binds into one tree are serialised until commit by an advisory lock:
// two sessions of a tree (a fork and its parent) send through different
// actors, and each must see the other's bytes. The key is not the root
// session's event lock (S38), so a bind never waits on the root's appends.
func (s *Store) BindAttachments(ctx context.Context, personID, rootID string, ids []string, maxTreeBytes int64) error {
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended('attachments:' || $1, 0))`, rootID); err != nil {
			return err
		}
		var found, unbound int
		var adding, used int64
		if err := tx.QueryRow(ctx, `select count(*)::int, (count(*) filter (where root_id is null))::int,
				coalesce(sum(size_bytes), 0)::bigint
			from attachments where id = any($2) and person_id = $1 and (root_id is null or root_id = $3)`,
			personID, ids, rootID).Scan(&found, &unbound, &adding); err != nil {
			return err
		}
		if found != len(ids) {
			return ErrNotFound
		}
		if err := tx.QueryRow(ctx, treeUseSQL, rootID, ids).Scan(&used); err != nil {
			return err
		}
		if used+adding > maxTreeBytes {
			return ErrTreeFull
		}
		if s.bindHook != nil {
			s.bindHook(ctx, tx)
		}
		tag, err := tx.Exec(ctx, `update attachments set root_id = $2 where id = any($1) and root_id is null`, ids, rootID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != int64(unbound) {
			return ErrNotFound // another tree bound one of them after the check
		}
		return nil
	})
}

// treeUseSQL is a tree's attachment bytes ($1 the root): each user
// message's attachment parts (MessagePart {type:"attachment", id}), plus the
// tree's attachments no message references, but those in $2.
const treeUseSQL = `select
	coalesce((select sum(a.size_bytes)
		from messages m
		join sessions s on s.id = m.session_id
		cross join lateral jsonb_array_elements(m.content) as part
		join attachments a on a.id = part->>'id'
		where s.root_id = $1 and m.role = 'user' and part->>'type' = 'attachment'), 0)::bigint
	+ coalesce((select sum(a.size_bytes)
		from attachments a
		where a.root_id = $1 and a.id <> all($2)
			and not exists (select 1 from messages m join sessions s on s.id = m.session_id
				where s.root_id = $1 and m.role = 'user'
					and m.content @> jsonb_build_array(jsonb_build_object('type', 'attachment', 'id', a.id)))), 0)::bigint`

// DeleteUnboundAttachment deletes a person's upload that no message has
// taken yet. Another person's attachment, or a missing one, is ErrNotFound;
// one bound to a conversation is ErrBound.
func (s *Store) DeleteUnboundAttachment(ctx context.Context, personID, id string) error {
	tag, err := s.pool.Exec(ctx, `delete from attachments where id = $1 and person_id = $2 and root_id is null`, id, personID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	if _, err := s.AttachmentMeta(ctx, personID, id); err != nil {
		return err // ErrNotFound, or a read failure
	}
	return ErrBound
}

// TreeAttachmentBytes is the total size of the attachments bound to a tree
// (the 20 MiB tree cap).
func (s *Store) TreeAttachmentBytes(ctx context.Context, rootID string) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `select coalesce(sum(size_bytes), 0)::bigint from attachments where root_id = $1`, rootID).Scan(&n)
	return n, err
}
