package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// PersonRef returns the opaque upstream ref of a person ("u_" plus 10 random
// hex digits, P21), creating it on first use. Upstream headers carry this and
// never a name.
func (s *Store) PersonRef(ctx context.Context, personID string) (string, error) {
	var existing string
	err := s.pool.QueryRow(ctx, `select ref from person_refs where person_id = $1`, personID).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	for range 5 {
		var b [5]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		candidate := "u_" + hex.EncodeToString(b[:])
		// The no-op update makes RETURNING yield the existing row on conflict.
		var ref string
		err := s.pool.QueryRow(ctx, `insert into person_refs (person_id, ref) values ($1, $2)
			on conflict (person_id) do update set person_id = excluded.person_id
			returning ref`, personID, candidate).Scan(&ref)
		if err == nil {
			return ref, nil
		}
		if isUnique(err, "person_refs_ref_key") {
			continue // two people drew the same 40 bits: draw again
		}
		return "", err
	}
	return "", fmt.Errorf("store: person ref: no unused ref after 5 draws")
}
