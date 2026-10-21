package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// GrantConductor applies the C4 grants for the conductor's login role. It is
// idempotent, and the role must already exist (the operator creates it).
//
// The conductor reads and writes only sdk_transcript_entries. It can read
// every tree's transcripts, which is accepted for Chris-only phase 1 and must
// be scoped (row-level security, or Core-mediated access) before a second
// person is onboarded.
func (s *Store) GrantConductor(ctx context.Context, role string) error {
	r := pgx.Identifier{role}.Sanitize()
	var db string
	if err := s.pool.QueryRow(ctx, `select current_database()`).Scan(&db); err != nil {
		return err
	}
	d := pgx.Identifier{db}.Sanitize()
	var seq *string
	if err := s.pool.QueryRow(ctx, `select pg_get_serial_sequence('sdk_transcript_entries', 'seq')`).Scan(&seq); err != nil {
		return err
	}
	if seq == nil {
		return fmt.Errorf("store: sdk_transcript_entries.seq has no sequence")
	}
	stmts := []string{
		fmt.Sprintf(`revoke connect on database %s from public`, d),
		`revoke all on schema public from public`,
		fmt.Sprintf(`grant connect on database %s to %s`, d, r),
		fmt.Sprintf(`grant usage on schema public to %s`, r),
		fmt.Sprintf(`grant select, insert, delete on sdk_transcript_entries to %s`, r),
		fmt.Sprintf(`grant usage on sequence %s to %s`, *seq, r),
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		for _, q := range stmts {
			if _, err := tx.Exec(ctx, q); err != nil {
				return fmt.Errorf("store: grant conductor: %w", err)
			}
		}
		return nil
	})
}
