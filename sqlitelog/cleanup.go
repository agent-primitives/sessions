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

// ErrCleanupPending classifies incomplete inventory that prevents cleanup. The allocation
// recorder does not itself run cleanup or report its completion.
var ErrCleanupPending = errors.New("sqlitelog: cleanup pending")

// ErrInventoryIncomplete also satisfies ErrCleanupPending. Only explicit operator evidence,
// never an empty allocation ledger, can certify an untracked session's inventory.
var ErrInventoryIncomplete = fmt.Errorf("%w: inventory incomplete", ErrCleanupPending)

// ErrAllocationNotFound includes compacted attempts and attempts belonging to a different session;
// callers must not confirm or release another session's resources.
var ErrAllocationNotFound = errors.New("sqlitelog: allocation attempt not found")

// ErrAllocationConflict means confirmation lacks a session-scoped handle or would change
// the handle already recorded for this attempt.
var ErrAllocationConflict = errors.New("sqlitelog: allocation confirmation conflict")

// Allocation is a write-ahead resource attempt. ResourceID retains the incarnation's
// session-scoped handle, not its compute-instance identity, address, credentials or routing metadata.
type Allocation struct {
	ID, SessionUID, Backend, Operation string
	ResourceKind                       api.CleanupResourceKind
	ResourceNamespace, ResourceID      string
	ResourceUID                        string
	Confirmed                          bool
}

// migrateCleanupInventory adds the allocation ledger and inventory completeness/evidence columns.
// Existing inventory is not evidence of complete allocation coverage.
// The explicit default-value UPDATE provides a deterministic trigger hook for rollback tests.
func migrateCleanupInventory(tx *sql.Tx) error {
	_, err := tx.Exec(`
		ALTER TABLE sessions ADD COLUMN inventory_complete INTEGER NOT NULL DEFAULT 0;
		UPDATE sessions SET inventory_complete = 0;
		ALTER TABLE sessions ADD COLUMN inventory_evidence TEXT NOT NULL DEFAULT '';
		CREATE TABLE allocations (
		  id TEXT PRIMARY KEY, session TEXT NOT NULL, backend TEXT NOT NULL,
		  operation TEXT NOT NULL, resource_id TEXT NOT NULL DEFAULT '',
		  confirmed INTEGER NOT NULL DEFAULT 0, released INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX allocations_session ON allocations(session, released, id);`)
	return err
}

func migrateCleanupIdentity(tx *sql.Tx) error {
	_, err := tx.Exec(`
		ALTER TABLE allocations ADD COLUMN resource_kind TEXT NOT NULL DEFAULT '';
		ALTER TABLE allocations ADD COLUMN resource_namespace TEXT NOT NULL DEFAULT '';
		ALTER TABLE allocations ADD COLUMN resource_uid TEXT NOT NULL DEFAULT '';`)
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
func (l *Log) BeginAllocation(ctx context.Context, backend, operation string, targets ...api.CleanupResource) (string, error) {
	if l.session == "" || operation == "" {
		return "", errors.New("sqlitelog: allocation requires session and operation")
	}
	if len(targets) > 1 {
		return "", errors.New("sqlitelog: allocation accepts one resource target")
	}
	var target api.CleanupResource
	if len(targets) == 1 {
		target = targets[0]
		if err := target.ValidateTarget(); err != nil {
			return "", fmt.Errorf("sqlitelog: allocation target: %w", err)
		}
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO allocations(
		id,session,backend,operation,resource_kind,resource_namespace,resource_id
	) VALUES(?,?,?,?,?,?,?)`, id, l.session, backend, operation, target.Kind, target.Namespace, target.Name); err != nil {
		return "", fmt.Errorf("sqlitelog: record allocation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

// ConfirmAllocation may acknowledge a late response after DELETE, but cannot reassign an attempt
// or replace its session-scoped handle. It retains this attempt and compacts other confirmed,
// unreleased attempts for the same (session, backend, handle), regardless of operation. Identical retries
// succeed while the attempt is retained; compacted IDs return ErrAllocationNotFound.
func (l *Log) ConfirmAllocation(ctx context.Context, attempt string, in api.Incarnation) error {
	resourceID := in.ID
	resourceKind := api.CleanupResourceKind("")
	resourceNamespace, resourceUID := "", ""
	if in.CleanupResource != nil {
		resource := *in.CleanupResource
		if err := resource.ValidateOwned(); err != nil {
			return ErrAllocationConflict
		}
		resourceKind = resource.Kind
		resourceNamespace = resource.Namespace
		resourceID = resource.Name
		resourceUID = resource.UID
	}
	if resourceID == "" {
		return ErrAllocationConflict
	}
	tx, err := l.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id, backend, namespace, uid string
	var kind api.CleanupResourceKind
	var confirmed, released bool
	err = tx.QueryRowContext(ctx, `SELECT resource_id,resource_kind,resource_namespace,resource_uid,backend,confirmed,released
		FROM allocations WHERE id=? AND session=?`, attempt, l.session).Scan(&id, &kind, &namespace, &uid, &backend, &confirmed, &released)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAllocationNotFound
	}
	if err != nil {
		return err
	}
	if confirmed {
		if id != resourceID || kind != resourceKind || namespace != resourceNamespace || uid != resourceUID {
			return ErrAllocationConflict
		}
		return nil
	}
	if released {
		return ErrAllocationConflict
	}
	if (resourceKind != "" && (kind != resourceKind || namespace != resourceNamespace || id != resourceID)) ||
		(resourceKind == "" && (kind != "" || namespace != "" || uid != "")) {
		return ErrAllocationConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE allocations SET
		resource_kind=?,resource_namespace=?,resource_id=?,resource_uid=?,confirmed=1
		WHERE id=? AND session=?`, resourceKind, resourceNamespace, resourceID, resourceUID, attempt, l.session); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM allocations
		WHERE session=? AND backend=? AND resource_kind=? AND resource_namespace=? AND resource_id=? AND resource_uid=?
		AND confirmed=1 AND released=0 AND id<>?`, l.session, backend, resourceKind, resourceNamespace, resourceID, resourceUID, attempt); err != nil {
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

const allocationQuery = `SELECT id,session,backend,operation,resource_kind,resource_namespace,resource_id,resource_uid,confirmed FROM allocations
	WHERE session=? AND released=0 AND id>? ORDER BY id LIMIT ?`

func (s *Store) allocations(ctx context.Context, uid, after string, limit int) ([]Allocation, error) {
	rows, err := s.db.QueryContext(ctx, allocationQuery, uid, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Allocation
	for rows.Next() {
		var a Allocation
		if err := rows.Scan(&a.ID, &a.SessionUID, &a.Backend, &a.Operation, &a.ResourceKind,
			&a.ResourceNamespace, &a.ResourceID, &a.ResourceUID, &a.Confirmed); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ReleaseAllocation acknowledges a successful safe backend release, even for an unconfirmed
// attempt when the backend can prove that safe. Released history remains as durable evidence;
// a late confirmation can retire an earlier unreleased attempt before it is released.
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
