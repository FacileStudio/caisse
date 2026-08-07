// Package pg stores caisse's webhook dedupe ledger in PostgreSQL.
//
// It is a [caisse.EventStore] over one small table and nothing else. It takes a
// *sql.DB, so it works with whatever driver and pool the app already has —
// GORM hands one over with db.DB().
//
// The package imports only database/sql: adopting it costs no dependency.
package pg

import (
	"context"
	"database/sql"
	stderrors "errors"
	"fmt"
	"time"
)

// Schema creates the ledger. Apply it through the app's own migrations —
// tronc/migrate, goose, whatever is already there — rather than at boot.
const Schema = `
CREATE TABLE IF NOT EXISTS caisse_events (
	id         text PRIMARY KEY,
	status     text NOT NULL DEFAULT 'pending',
	claimed_at timestamptz NOT NULL DEFAULT now(),
	done_at    timestamptz
);
CREATE INDEX IF NOT EXISTS caisse_events_pending_idx
	ON caisse_events (claimed_at) WHERE status = 'pending';
`

// DefaultStaleAfter is how long a claim may sit unconfirmed before another
// delivery may take it over.
//
// It exists because a process killed between claiming an event and finishing it
// would otherwise leave that event claimed forever, and Stripe's retries would
// all bounce off it — the order silently never ships. The window has to be
// comfortably longer than the slowest handler, or a retry starts work that is
// still running.
const DefaultStaleAfter = 15 * time.Minute

// Store is the PostgreSQL event store.
type Store struct {
	db         *sql.DB
	staleAfter time.Duration
}

// New returns a store over db. A staleAfter of zero means [DefaultStaleAfter].
func New(db *sql.DB, staleAfter time.Duration) *Store {
	if staleAfter <= 0 {
		staleAfter = DefaultStaleAfter
	}
	return &Store{db: db, staleAfter: staleAfter}
}

// EnsureSchema applies [Schema]. It is here for tests and local development;
// production schema changes belong in the app's migrations.
func EnsureSchema(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, Schema); err != nil {
		return fmt.Errorf("caisse/pg: ensure schema: %w", err)
	}
	return nil
}

// Begin claims eventID for processing and reports whether the claim is ours.
//
// The insert and the staleness check are one statement, so two deliveries of
// the same event racing each other cannot both win: PostgreSQL settles the
// conflict, and the loser gets no row back.
func (s *Store) Begin(ctx context.Context, eventID string) (bool, error) {
	const query = `
		INSERT INTO caisse_events (id, status, claimed_at)
		VALUES ($1, 'pending', now())
		ON CONFLICT (id) DO UPDATE
		   SET claimed_at = now()
		 WHERE caisse_events.status = 'pending'
		   AND caisse_events.claimed_at < now() - make_interval(secs => $2)
		RETURNING id`

	var claimed string
	err := s.db.QueryRowContext(ctx, query, eventID, s.staleAfter.Seconds()).Scan(&claimed)
	switch {
	case stderrors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("caisse/pg: claim %s: %w", eventID, err)
	}
	return true, nil
}

// Done records eventID as handled.
func (s *Store) Done(ctx context.Context, eventID string) error {
	const query = `UPDATE caisse_events SET status = 'done', done_at = now() WHERE id = $1`
	if _, err := s.db.ExecContext(ctx, query, eventID); err != nil {
		return fmt.Errorf("caisse/pg: confirm %s: %w", eventID, err)
	}
	return nil
}

// Fail releases the claim on eventID so Stripe's retry can pick it up.
//
// It deletes rather than flags: the row's only job is to say "somebody has this
// or somebody finished it", and a failed attempt means neither.
func (s *Store) Fail(ctx context.Context, eventID string) error {
	const query = `DELETE FROM caisse_events WHERE id = $1 AND status = 'pending'`
	if _, err := s.db.ExecContext(ctx, query, eventID); err != nil {
		return fmt.Errorf("caisse/pg: release %s: %w", eventID, err)
	}
	return nil
}

// Purge deletes handled events older than olderThan and reports how many went.
//
// The ledger only grows otherwise. Handled events are worth keeping for a while
// — long enough to outlive Stripe's retry window, which runs to about three
// days — and worth nothing after that.
func (s *Store) Purge(ctx context.Context, olderThan time.Duration) (int64, error) {
	const query = `DELETE FROM caisse_events WHERE status = 'done' AND done_at < now() - make_interval(secs => $1)`
	result, err := s.db.ExecContext(ctx, query, olderThan.Seconds())
	if err != nil {
		return 0, fmt.Errorf("caisse/pg: purge: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("caisse/pg: purge: %w", err)
	}
	return deleted, nil
}
