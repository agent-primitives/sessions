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

// ErrAllocationConflict means confirmation lacks a session-scoped handle, would change the handle
// or a resource UID already recorded for this attempt, or names a resource its plan does not.
var ErrAllocationConflict = errors.New("sqlitelog: allocation confirmation conflict")

// Allocation is a write-ahead resource attempt. ResourceID retains the incarnation's
// session-scoped handle, not its compute-instance identity, address, credentials or routing metadata.
// An attempt with a resource plan leaves it empty; its UIDs live in allocation_resources.
type Allocation struct {
	ID, SessionUID, Backend, Operation, ResourceID string
	Confirmed                                      bool
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

// migrateCleanupIdentity adds the provider-opaque resource plan of each attempt: one row per
// resource, its UID once a backend proves it created it, and which resource borrows it. The
// canonical_resource_id column ships empty; a later cleanup change uses it to link duplicate claims.
func migrateCleanupIdentity(tx *sql.Tx) error {
	_, err := tx.Exec(`
		CREATE TABLE allocation_resources (
		  id TEXT PRIMARY KEY,
		  allocation_id TEXT NOT NULL,
		  ordinal INTEGER NOT NULL,
		  reference TEXT NOT NULL,
		  uid TEXT NOT NULL DEFAULT '',
		  confirmed INTEGER NOT NULL DEFAULT 0,
		  released INTEGER NOT NULL DEFAULT 0,
		  canonical_resource_id TEXT,
		  UNIQUE(allocation_id, ordinal)
		);
		CREATE INDEX allocation_resources_allocation ON allocation_resources(allocation_id);
		CREATE INDEX allocation_resources_identity ON allocation_resources(reference, uid);
		CREATE INDEX allocation_resources_canonical ON allocation_resources(canonical_resource_id);
		CREATE TABLE allocation_borrowers (
		  resource_id TEXT NOT NULL,
		  borrower_resource_id TEXT NOT NULL,
		  PRIMARY KEY(resource_id, borrower_resource_id)
		);
		CREATE INDEX allocation_borrowers_borrower ON allocation_borrowers(borrower_resource_id);`)
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
	return l.beginAllocation(ctx, backend, operation, true, nil)
}

// BeginPlannedAllocation is BeginAllocation with the resource plan of an owned backend (RT-4j).
func (l *Log) BeginPlannedAllocation(ctx context.Context, backend, operation string, resources ...api.CleanupResource) (string, error) {
	return l.beginAllocation(ctx, backend, operation, true, resources)
}

// BeginUnpublishedAllocation records a fork child's plan without creating its sessions row, so
// the child stays unlisted until its fork fence publishes it.
func (l *Log) BeginUnpublishedAllocation(ctx context.Context, backend, operation string, resources ...api.CleanupResource) (string, error) {
	return l.beginAllocation(ctx, backend, operation, false, resources)
}

func (l *Log) beginAllocation(ctx context.Context, backend, operation string, publish bool, resources []api.CleanupResource) (string, error) {
	if l.session == "" || operation == "" {
		return "", errors.New("sqlitelog: allocation requires session and operation")
	}
	if err := api.ValidateCleanupPlan(resources); err != nil {
		return "", err
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
	if publish {
		now := time.Now().UnixNano()
		if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(session,project,created_at,updated_at) VALUES(?,?,?,?)
			ON CONFLICT(session) DO NOTHING`, l.session, l.store.defaultProject, now, now); err != nil {
			return "", err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO allocations(id,session,backend,operation) VALUES(?,?,?,?)`, id, l.session, backend, operation); err != nil {
		return "", fmt.Errorf("sqlitelog: record allocation: %w", err)
	}
	rows := make(map[string]string, len(resources))
	for ordinal, resource := range resources {
		rowID, err := newLedgerID()
		if err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO allocation_resources(id,allocation_id,ordinal,reference) VALUES(?,?,?,?)`,
			rowID, id, ordinal, resource.Reference); err != nil {
			return "", fmt.Errorf("sqlitelog: record allocation resource: %w", err)
		}
		rows[resource.Reference] = rowID
	}
	for _, resource := range resources {
		if resource.Borrower == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO allocation_borrowers(resource_id,borrower_resource_id) VALUES(?,?)`,
			rows[resource.Reference], rows[resource.Borrower]); err != nil {
			return "", fmt.Errorf("sqlitelog: record allocation borrower: %w", err)
		}
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
//
// An attempt with a resource plan instead records the UIDs in in.Cleanup, which may be partial and
// may come with a runtime error. Unproven resources stay unconfirmed. Once every resource is proven,
// other confirmed attempts of the session whose claims this one repeats (a later Restore of the same
// actor) are compacted. A success with a handle but no evidence created nothing the backend can
// prove (it re-attached, or resumed a stateless session): its plan is dropped and it is confirmed
// and compacted as a handle attempt.
func (l *Log) ConfirmAllocation(ctx context.Context, attempt string, in api.Incarnation) error {
	tx, err := l.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id, backend string
	var confirmed, released bool
	err = tx.QueryRowContext(ctx, `SELECT resource_id,backend,confirmed,released FROM allocations WHERE id=? AND session=?`, attempt, l.session).Scan(&id, &backend, &confirmed, &released)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAllocationNotFound
	}
	if err != nil {
		return err
	}
	var planned bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM allocation_resources WHERE allocation_id=?)`, attempt).Scan(&planned); err != nil {
		return err
	}
	observed := in.Cleanup.Resources()
	if planned && len(observed) == 0 && in.ID != "" {
		var proven bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM allocation_resources WHERE allocation_id=? AND confirmed=1)`, attempt).Scan(&proven); err != nil {
			return err
		}
		if proven {
			return ErrAllocationConflict
		}
		if err := deletePlans(ctx, tx, attempt); err != nil {
			return err
		}
		planned = false
	}
	if planned {
		if err := confirmResources(ctx, tx, attempt, released, observed); err != nil || released {
			return err
		}
		if err := compactRepeatedClaims(ctx, tx, l.session, backend, attempt); err != nil {
			return err
		}
		return tx.Commit()
	}
	if in.ID == "" {
		return ErrAllocationConflict
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM allocations
		WHERE session=? AND backend=? AND resource_id=? AND confirmed=1 AND released=0 AND id<>?`, l.session, backend, in.ID, attempt); err != nil {
		return err
	}
	return tx.Commit()
}

// confirmResources records observed UIDs against the attempt's plan. A resource outside the plan, a
// different borrower or a changed UID is a conflict; repeating the same evidence is a no-op. On a
// released attempt only that repeat is accepted: a claim not already confirmed is a conflict.
func confirmResources(ctx context.Context, tx *sql.Tx, attempt string, released bool, observed []api.CleanupResource) error {
	if len(observed) == 0 {
		return nil
	}
	for _, resource := range observed {
		var rowID, uid, borrower string
		var confirmed bool
		err := tx.QueryRowContext(ctx, `SELECT r.id,r.uid,r.confirmed,COALESCE(b.reference,'')
			FROM allocation_resources r
			LEFT JOIN allocation_borrowers e ON e.resource_id=r.id
			LEFT JOIN allocation_resources b ON b.id=e.borrower_resource_id
			WHERE r.allocation_id=? AND r.reference=?`, attempt, resource.Reference).Scan(&rowID, &uid, &confirmed, &borrower)
		if errors.Is(err, sql.ErrNoRows) || err == nil && (borrower != resource.Borrower || uid != "" && uid != resource.UID || released && !confirmed) {
			return ErrAllocationConflict
		}
		if err != nil {
			return err
		}
		if released {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE allocation_resources SET uid=?,confirmed=1 WHERE id=?`, resource.UID, rowID); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE allocations SET confirmed=NOT EXISTS
		(SELECT 1 FROM allocation_resources WHERE allocation_id=? AND confirmed=0) WHERE id=?`, attempt, attempt)
	return err
}

// deletePlans removes the resource rows and borrower edges of the given attempts.
func deletePlans(ctx context.Context, tx *sql.Tx, attempts ...string) error {
	for _, attempt := range attempts {
		if _, err := tx.ExecContext(ctx, `DELETE FROM allocation_borrowers WHERE resource_id IN
			(SELECT id FROM allocation_resources WHERE allocation_id=?)`, attempt); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM allocation_resources WHERE allocation_id=?`, attempt); err != nil {
			return err
		}
	}
	return nil
}

// borrowerOf is the SQL for the reference of the resource that borrows row r, empty if none.
func borrowerOf(r string) string {
	return `COALESCE((SELECT b.reference FROM allocation_borrowers e JOIN allocation_resources b
		ON b.id=e.borrower_resource_id WHERE e.resource_id=` + r + `.id),'')`
}

// compactRepeatedClaims deletes the session's other confirmed, unreleased planned attempts whose
// every claim (reference, UID and borrower) this fully confirmed attempt also holds.
func compactRepeatedClaims(ctx context.Context, tx *sql.Tx, session, backend, attempt string) error {
	rows, err := tx.QueryContext(ctx, `SELECT a.id FROM allocations a
		WHERE a.session=? AND a.backend=? AND a.id<>? AND a.confirmed=1 AND a.released=0
		AND EXISTS(SELECT 1 FROM allocations WHERE id=? AND confirmed=1)
		AND EXISTS(SELECT 1 FROM allocation_resources WHERE allocation_id=a.id)
		AND NOT EXISTS(SELECT 1 FROM allocation_resources o WHERE o.allocation_id=a.id AND NOT EXISTS(
		  SELECT 1 FROM allocation_resources n WHERE n.allocation_id=? AND n.confirmed=1
		  AND n.reference=o.reference AND n.uid=o.uid AND `+borrowerOf("n")+`=`+borrowerOf("o")+`))`, session, backend, attempt, attempt, attempt)
	if err != nil {
		return err
	}
	var repeated []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		repeated = append(repeated, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if err := deletePlans(ctx, tx, repeated...); err != nil {
		return err
	}
	for _, id := range repeated {
		if _, err := tx.ExecContext(ctx, `DELETE FROM allocations WHERE id=?`, id); err != nil {
			return err
		}
	}
	return nil
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

const allocationQuery = `SELECT id,session,backend,operation,resource_id,confirmed FROM allocations
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
		if err := rows.Scan(&a.ID, &a.SessionUID, &a.Backend, &a.Operation, &a.ResourceID, &a.Confirmed); err != nil {
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
