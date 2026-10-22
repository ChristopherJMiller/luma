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
		// All of it, not just CONNECT: PUBLIC also holds TEMP on a new database.
		fmt.Sprintf(`revoke all on database %s from public`, d),
		`revoke all on schema public from public`,
		fmt.Sprintf(`grant connect on database %s to %s`, d, r),
		fmt.Sprintf(`grant usage on schema public to %s`, r),
		fmt.Sprintf(`grant select, insert, delete on sdk_transcript_entries to %s`, r),
		fmt.Sprintf(`grant usage on sequence %s to %s`, *seq, r),
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		// Two Core pods in a rolling update both run this at startup.
		if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended('luma-grant-conductor', 0))`); err != nil {
			return err
		}
		for _, q := range stmts {
			if _, err := tx.Exec(ctx, q); err != nil {
				return fmt.Errorf("store: grant conductor: %w", err)
			}
		}
		// REVOKE and GRANT only WARN when the caller does not own the object,
		// so read the result back instead of trusting the statements.
		var ok bool
		err := tx.QueryRow(ctx, `select
			has_database_privilege($1, $2, 'CONNECT')
			and has_schema_privilege($1, 'public', 'USAGE')
			and has_table_privilege($1, 'sdk_transcript_entries', 'SELECT, INSERT, DELETE')
			and has_sequence_privilege($1, $3, 'USAGE')
			and not has_table_privilege($1, 'sdk_transcript_entries', 'UPDATE')
			and not exists (select 1 from pg_database d, aclexplode(d.datacl) a
				where d.datname = $2 and a.grantee = 0)
			and not exists (select 1 from pg_namespace n, aclexplode(n.nspacl) a
				where n.nspname = 'public' and a.grantee = 0)`,
			role, db, *seq).Scan(&ok)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("store: grant conductor: privileges not as intended after granting (is the caller the database owner?)")
		}
		return nil
	})
}
