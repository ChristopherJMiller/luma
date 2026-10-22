package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ChristopherJMiller/luma/core/internal/protocol"
)

// Session mirrors a C4 `sessions` row.
type Session struct {
	ID              string
	RootID          string // equals ID for a root
	ParentID        *string
	ForkAtMessageID *string
	PersonID        string
	AgentID         string
	AgentVersion    string
	AgentSnapshot   json.RawMessage

	Tier         string
	Model        string
	HelperModel  string // snapshotted from the live models.yaml at Create (S11)
	Effort       string
	CacheTTL     string
	Workspace    string // the sandbox: "none" in phase 1 (S5)
	APIWorkspace string // "interactive" | "ci": gateway route and Anthropic workspace (S5)
	Source       string
	Incognito    bool
	BoundModels  []string

	Title           string
	SDKSessionID    *string
	ProfileSnapshot string
	Status          string
	TaintedAt       *time.Time
	LastTotals      *protocol.Totals
	ArchivedAt      *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
	LastActivityAt  time.Time
}

const sessionCols = `id, root_id, parent_id, fork_at_message_id, person_id, agent_id, agent_version,
	agent_snapshot, tier, model, helper_model, effort, cache_ttl, workspace, api_workspace, source,
	incognito, bound_models, title, sdk_session_id, profile_snapshot, status, tainted_at,
	last_totals, archived_at, created_at, updated_at, last_activity_at`

func scanSession(row pgx.Row) (Session, error) {
	var s Session
	var totals []byte
	err := row.Scan(&s.ID, &s.RootID, &s.ParentID, &s.ForkAtMessageID, &s.PersonID, &s.AgentID,
		&s.AgentVersion, &s.AgentSnapshot, &s.Tier, &s.Model, &s.HelperModel, &s.Effort, &s.CacheTTL,
		&s.Workspace, &s.APIWorkspace, &s.Source, &s.Incognito, &s.BoundModels, &s.Title,
		&s.SDKSessionID, &s.ProfileSnapshot, &s.Status, &s.TaintedAt, &totals, &s.ArchivedAt,
		&s.CreatedAt, &s.UpdatedAt, &s.LastActivityAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	if totals != nil {
		s.LastTotals = new(protocol.Totals)
		if err := json.Unmarshal(totals, s.LastTotals); err != nil {
			return Session{}, err
		}
	}
	return s, nil
}

// CreateSession inserts a session. An empty RootID makes it a root (RootID =
// ID); an empty Workspace is "none"; empty Status is "idle". It returns the
// stored row.
func (s *Store) CreateSession(ctx context.Context, in Session) (Session, error) {
	if in.RootID == "" {
		in.RootID = in.ID
	}
	if in.Workspace == "" {
		in.Workspace = "none"
	}
	if in.Status == "" {
		in.Status = "idle"
	}
	if in.BoundModels == nil {
		in.BoundModels = []string{}
	}
	if in.AgentSnapshot == nil {
		in.AgentSnapshot = json.RawMessage(`{}`)
	}
	var totals []byte
	if in.LastTotals != nil {
		b, err := json.Marshal(in.LastTotals)
		if err != nil {
			return Session{}, err
		}
		totals = b
	}
	return scanSession(s.pool.QueryRow(ctx, `
		insert into sessions (id, root_id, parent_id, fork_at_message_id, person_id, agent_id,
			agent_version, agent_snapshot, tier, model, helper_model, effort, cache_ttl, workspace,
			api_workspace, source, incognito, bound_models, title, sdk_session_id, profile_snapshot,
			status, tainted_at, last_totals, archived_at)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25)
		returning `+sessionCols,
		in.ID, in.RootID, in.ParentID, in.ForkAtMessageID, in.PersonID, in.AgentID, in.AgentVersion,
		[]byte(in.AgentSnapshot), in.Tier, in.Model, in.HelperModel, in.Effort, in.CacheTTL, in.Workspace,
		in.APIWorkspace, in.Source, in.Incognito, in.BoundModels, in.Title, in.SDKSessionID,
		in.ProfileSnapshot, in.Status, in.TaintedAt, totals, in.ArchivedAt))
}

// Session returns one session by id.
func (s *Store) Session(ctx context.Context, id string) (Session, error) {
	return scanSession(s.pool.QueryRow(ctx, `select `+sessionCols+` from sessions where id = $1`, id))
}

// TreeSessions returns every session of a tree, root first, then by creation.
func (s *Store) TreeSessions(ctx context.Context, rootID string) ([]Session, error) {
	rows, err := s.pool.Query(ctx, `select `+sessionCols+` from sessions where root_id = $1
		order by (id = root_id) desc, created_at, id`, rootID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		ses, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ses)
	}
	return out, rows.Err()
}

// RootCursor is where a page of ListRoots ends: the last listed root's
// updated_at and id. The next page lists the roots after it.
type RootCursor struct {
	UpdatedAt time.Time
	ID        string
}

// ListRoots lists a person's conversations (tree roots), most recently
// updated first, ties by id descending, after the cursor when one is given
// (keyset paging, C1 GET /v1/sessions?after=). Archived ones are included
// only when asked.
func (s *Store) ListRoots(ctx context.Context, personID string, includeArchived bool, after *RootCursor, limit int) ([]Session, error) {
	var afterAt *time.Time
	afterID := ""
	if after != nil {
		afterAt, afterID = &after.UpdatedAt, after.ID
	}
	rows, err := s.pool.Query(ctx, `select `+sessionCols+` from sessions
		where person_id = $1 and id = root_id and ($2 or archived_at is null)
			and ($3::timestamptz is null or (updated_at, id) < ($3::timestamptz, $4::text))
		order by updated_at desc, id desc limit $5`, personID, includeArchived, afterAt, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		ses, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ses)
	}
	return out, rows.Err()
}

// RunningSessions returns the ids of sessions whose status is "running": the
// ones a restarted Core must reconcile.
func (s *Store) RunningSessions(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `select id from sessions where status = 'running' order by id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) updateSession(ctx context.Context, id, set string, args ...any) error {
	return updateSessionOn(ctx, s.pool, id, set, args...)
}

func updateSessionOn(ctx context.Context, db execer, id, set string, args ...any) error {
	return execOne(ctx, db, `update sessions set `+set+`, updated_at = now() where id = $1`,
		append([]any{id}, args...)...)
}

// SetSDKSessionID records the SDK session id a host reported (S2). Task 9a
// builds HostSpec.resume from it on every reopen.
func (s *Store) SetSDKSessionID(ctx context.Context, sessionID, sdkSessionID string) error {
	return s.updateSession(ctx, sessionID, `sdk_session_id = $2`, sdkSessionID)
}

// SetLastTotals records the SDK's running totals from turn_result (S2); they
// are the next HostSpec.resume's baseline.
func (s *Store) SetLastTotals(ctx context.Context, sessionID string, t protocol.Totals) error {
	return setLastTotals(ctx, s.pool, sessionID, t)
}

func setLastTotals(ctx context.Context, db execer, sessionID string, t protocol.Totals) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return updateSessionOn(ctx, db, sessionID, `last_totals = $2`, b)
}

// SetSessionStatus sets the status (idle, running, requires_action, error).
// A move to running, a turn's start, also stamps last_activity_at, and bumps
// the tree root's updated_at in the same transaction: the history list
// orders conversations by their last turn anywhere in the tree (S54).
func (s *Store) SetSessionStatus(ctx context.Context, sessionID, status string) error {
	const set = `status = $2, last_activity_at = case when $2 = 'running' then now() else last_activity_at end`
	if status != "running" {
		return s.updateSession(ctx, sessionID, set, status)
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := updateSessionOn(ctx, tx, sessionID, set, status); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `update sessions set updated_at = now()
			where id = (select root_id from sessions where id = $1) and id <> $1`, sessionID)
		return err
	})
}

// SetSessionTitle sets the title.
func (s *Store) SetSessionTitle(ctx context.Context, sessionID, title string) error {
	return s.updateSession(ctx, sessionID, `title = $2`, title)
}

// TaintSession stamps tainted_at once; it is never cleared (Core owns taint).
func (s *Store) TaintSession(ctx context.Context, sessionID string) error {
	return s.updateSession(ctx, sessionID, `tainted_at = coalesce(tainted_at, now())`)
}

// ArchiveSession archives (or, with archived false, restores) a conversation.
func (s *Store) ArchiveSession(ctx context.Context, sessionID string, archived bool) error {
	return s.updateSession(ctx, sessionID, `archived_at = case when $2 then coalesce(archived_at, now()) else null end`, archived)
}

// AddBoundModel appends a model to bound_models if it is not there (P29).
func (s *Store) AddBoundModel(ctx context.Context, sessionID, model string) error {
	return s.updateSession(ctx, sessionID, `bound_models = case when $2 = any(bound_models) then bound_models else array_append(bound_models, $2) end`, model)
}

// DeleteTree removes a conversation tree from live storage in one
// transaction: its sessions, messages, events and attachments, and the SDK
// transcripts keyed by the root. Memory writes and ledger rows survive with
// session_id NULL; so do usage anomalies. It returns ErrNotFound unless
// rootID is a root owned by personID.
func (s *Store) DeleteTree(ctx context.Context, personID, rootID string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var one int
		err := tx.QueryRow(ctx, `select 1 from sessions where id = $1 and root_id = id and person_id = $2 for update`,
			rootID, personID).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		stmts := []string{
			`update usage_anomalies set session_id = null where session_id in (select id from sessions where root_id = $1)`,
			`delete from sdk_transcript_entries where project_key = $1`,
			// attachments, messages, events, client_events cascade; memory_writes
			// and cost_ledger are set null by their foreign keys.
			`delete from attachments where root_id = $1`,
			`delete from sessions where root_id = $1`,
		}
		for _, q := range stmts {
			if _, err := tx.Exec(ctx, q, rootID); err != nil {
				return err
			}
		}
		return nil
	})
}
