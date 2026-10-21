package store

import (
	"context"
	"errors"
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

// InsertAttachment stores an upload.
func (s *Store) InsertAttachment(ctx context.Context, a Attachment) error {
	_, err := s.pool.Exec(ctx, `insert into attachments (id, person_id, root_id, media_type, filename,
		size_bytes, width, height, pages, sha256, data) values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		a.ID, a.PersonID, a.RootID, a.MediaType, a.Filename, a.SizeBytes, a.Width, a.Height, a.Pages, a.SHA256, a.Data)
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

// BindAttachments binds a person's unbound attachments to a tree. It returns
// ErrNotFound (and binds nothing) if any id is missing, not the person's, or
// already bound to another tree.
func (s *Store) BindAttachments(ctx context.Context, personID, rootID string, ids []string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `update attachments set root_id = $3
			where id = any($2) and person_id = $1 and (root_id is null or root_id = $3)`, personID, ids, rootID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != int64(len(ids)) {
			return ErrNotFound
		}
		return nil
	})
}

// TreeAttachmentBytes is the total size of the attachments bound to a tree
// (the 20 MiB tree cap).
func (s *Store) TreeAttachmentBytes(ctx context.Context, rootID string) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `select coalesce(sum(size_bytes), 0)::bigint from attachments where root_id = $1`, rootID).Scan(&n)
	return n, err
}
