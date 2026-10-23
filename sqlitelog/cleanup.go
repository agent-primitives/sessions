package sqlitelog

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/agent-primitives/sessions/api"
)

// ErrCleanupPending means cleanup cannot yet complete: the session is active, an allocation
// remains unreleased, inventory is incomplete, or an owned fork batch is unfinished.
var ErrCleanupPending = errors.New("sqlitelog: cleanup pending")

// ErrInventoryIncomplete also satisfies ErrCleanupPending. Only explicit operator evidence,
// never an empty allocation ledger, can certify an untracked session's inventory.
var ErrInventoryIncomplete = fmt.Errorf("%w: inventory incomplete", ErrCleanupPending)

// ErrAllocationNotFound includes attempts belonging to a different session; callers must not
// confirm or release another session's resources.
var ErrAllocationNotFound = errors.New("sqlitelog: allocation attempt not found")

// ErrAllocationConflict means confirmation lacks an immutable resource ID or would change one.
var ErrAllocationConflict = errors.New("sqlitelog: allocation confirmation conflict")

// Allocation is a write-ahead resource attempt. Only the opaque resource ID is retained from an
// incarnation, not its address, worker credentials or routing metadata.
type Allocation struct {
	ID, SessionUID, Backend, Operation, ResourceID string
	Confirmed                                      bool
}

// migrateCleanupInventory adds the allocation ledger and reserves internal publication and
// cleanup metadata. Existing inventory is not evidence of complete allocation coverage.
func migrateCleanupInventory(tx *sql.Tx) error {
	_, err := tx.Exec(`
		ALTER TABLE sessions ADD COLUMN inventory_complete INTEGER NOT NULL DEFAULT 0;
		UPDATE sessions SET inventory_complete = 0;
		ALTER TABLE sessions ADD COLUMN inventory_evidence TEXT NOT NULL DEFAULT '';
		ALTER TABLE sessions ADD COLUMN cleanup_error TEXT NOT NULL DEFAULT '';
		ALTER TABLE sessions ADD COLUMN fork_batch TEXT NOT NULL DEFAULT '';
		CREATE TABLE allocations (
		  id TEXT PRIMARY KEY, session TEXT NOT NULL, backend TEXT NOT NULL,
		  operation TEXT NOT NULL, resource_id TEXT NOT NULL DEFAULT '',
		  confirmed INTEGER NOT NULL DEFAULT 0, released INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX allocations_session ON allocations(session, released, id);
		CREATE TABLE fork_batches (
		  id TEXT PRIMARY KEY, parent TEXT NOT NULL, status TEXT NOT NULL
		);
		CREATE INDEX fork_batches_parent ON fork_batches(parent, status);
		CREATE INDEX sessions_fork_batch ON sessions(fork_batch);
		CREATE INDEX sessions_deletion ON sessions(deletion_state, session);`)
	return err
}

func newLedgerID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

// BeginAllocation durably records an intent before runtime IO. Implicit logs receive an incomplete
// metadata row in the same transaction; a tombstone can never be resurrected by an attempt.
func (l *Log) BeginAllocation(ctx context.Context, backend, operation string) (string, error) {
	if l.session == "" || operation == "" {
		return "", errors.New("sqlitelog: allocation requires session and operation")
	}
	id, err := newLedgerID()
	if err != nil {
		return "", err
	}
	tx, err := l.store.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err := checkWritable(tx, l.session); err != nil {
		return "", err
	}
	now := time.Now().UnixNano()
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(session,project,created_at,updated_at) VALUES(?,?,?,?)
		ON CONFLICT(session) DO NOTHING`, l.session, l.store.defaultProject, now, now); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO allocations(id,session,backend,operation) VALUES(?,?,?,?)`, id, l.session, backend, operation); err != nil {
		return "", fmt.Errorf("sqlitelog: record allocation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

// ConfirmAllocation may acknowledge a late response after DELETE, but cannot reassign an attempt
// or replace the resource ID. Repeating an identical confirmation is harmless.
func (l *Log) ConfirmAllocation(ctx context.Context, attempt string, in api.Incarnation) error {
	if in.ID == "" {
		return ErrAllocationConflict
	}
	tx, err := l.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id string
	var confirmed, released bool
	err = tx.QueryRowContext(ctx, `SELECT resource_id,confirmed,released FROM allocations WHERE id=? AND session=?`, attempt, l.session).Scan(&id, &confirmed, &released)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAllocationNotFound
	}
	if err != nil {
		return err
	}
	if confirmed {
		if id != in.ID {
			return ErrAllocationConflict
		}
		return nil
	}
	if released {
		return ErrAllocationConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE allocations SET resource_id=?,confirmed=1 WHERE id=? AND session=?`, in.ID, attempt, l.session); err != nil {
		return err
	}
	return tx.Commit()
}

// Allocations returns all unreleased attempts, including unknown outcomes and backend routes.
func (s *Store) Allocations(ctx context.Context, uid string) ([]Allocation, error) {
	return s.allocations(ctx, uid, "", -1)
}

// AllocationPage bounds cleanup memory while preserving durable attempts and independent progress.
// after is the last attempt ID from the preceding page, including attempts that failed to release.
func (s *Store) AllocationPage(ctx context.Context, uid, after string, limit int) ([]Allocation, error) {
	if limit <= 0 {
		limit = DefaultPageSize
	}
	if limit > MaxPageSize {
		limit = MaxPageSize
	}
	return s.allocations(ctx, uid, after, limit)
}

func (s *Store) allocations(ctx context.Context, uid, after string, limit int) ([]Allocation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,session,backend,operation,resource_id,confirmed FROM allocations WHERE session=? AND released=0 AND id>? ORDER BY id LIMIT ?`, uid, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Allocation
	for rows.Next() {
		var a Allocation
		if err := rows.Scan(&a.ID, &a.SessionUID, &a.Backend, &a.Operation, &a.ResourceID, &a.Confirmed); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ReleaseAllocation acknowledges a successful safe backend release, even for an unconfirmed
// attempt when the backend can prove that safe. The attempt remains as durable evidence.
func (s *Store) ReleaseAllocation(ctx context.Context, uid, attempt string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE allocations SET released=1 WHERE id=? AND session=?`, attempt, uid)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrAllocationNotFound
	}
	return nil
}

// CertifyInventory trusts explicit operator reconciliation across all applicable namespaces,
// overrides and failed forks. It does not discover, adopt or count resources on the operator's behalf.
func (s *Store) CertifyInventory(ctx context.Context, uid, evidence string) error {
	if strings.TrimSpace(evidence) == "" {
		return ErrInventoryIncomplete
	}
	return s.updateCleanup(ctx, uid, `UPDATE sessions SET inventory_complete=1,inventory_evidence=? WHERE session=?`, evidence)
}

func (s *Store) updateCleanup(ctx context.Context, uid, query, value string) error {
	result, err := s.db.ExecContext(ctx, query, value, uid)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrSessionNotFound, uid)
	}
	return nil
}
