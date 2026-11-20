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

// ErrAllocationNotFound includes attempts belonging to a different session.
var ErrAllocationNotFound = errors.New("sqlitelog: allocation attempt not found")

// ErrAllocationConflict means an observed ownership claim changes the durable plan or UID.
var ErrAllocationConflict = errors.New("sqlitelog: allocation confirmation conflict")

// Allocation is a write-ahead runtime operation. ResourceID retains the historical legacy handle;
// owned cleanup records live in AllocationResource rows.
type Allocation struct {
	ID, SessionUID, Backend, Operation, ResourceID string
	Confirmed                                      bool
}

// AllocationResource is one per-attempt claim. CanonicalID links an exact duplicate claim without
// deleting the retry receipt. Borrower is the reference of the resource this claim must outlive.
type AllocationResource struct {
	ID, AllocationID, Reference, UID, CanonicalID, Borrower string
	Ordinal                                                 int
	Confirmed, Released                                     bool
}

// migrateCleanupInventory adds the allocation ledger and inventory completeness/evidence columns.
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

// migrateCleanupIdentity adds the normalized provider-opaque resource graph. The resource shape is
// cut with its producer in schema v10; unreleased v10 predecessor columns never ship.
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

// BeginAllocation preserves legacy calls and records the complete owned-resource plan before I/O.
func (l *Log) BeginAllocation(ctx context.Context, backend, operation string, resources ...api.CleanupResource) (string, error) {
	return l.beginAllocation(ctx, backend, operation, true, resources)
}

// BeginUnpublishedAllocation records Fork ownership without creating a visible sessions row. The
// child's existing fork fence remains the publication boundary.
func (l *Log) BeginUnpublishedAllocation(ctx context.Context, backend, operation string, resources ...api.CleanupResource) (string, error) {
	return l.beginAllocation(ctx, backend, operation, false, resources)
}

func (l *Log) beginAllocation(ctx context.Context, backend, operation string, publish bool, resources []api.CleanupResource) (string, error) {
	if l.session == "" || operation == "" {
		return "", errors.New("sqlitelog: allocation requires session and operation")
	}
	if len(resources) > 0 {
		if err := api.ValidateCleanupPlan(resources); err != nil {
			return "", err
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
	ids := make(map[string]string, len(resources))
	for ordinal, resource := range resources {
		resourceID, err := newLedgerID()
		if err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO allocation_resources(id,allocation_id,ordinal,reference) VALUES(?,?,?,?)`,
			resourceID, id, ordinal, resource.Reference); err != nil {
			return "", fmt.Errorf("sqlitelog: record allocation resource: %w", err)
		}
		ids[resource.Reference] = resourceID
	}
	for _, resource := range resources {
		if resource.Borrower == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO allocation_borrowers(resource_id,borrower_resource_id) VALUES(?,?)`,
			ids[resource.Reference], ids[resource.Borrower]); err != nil {
			return "", fmt.Errorf("sqlitelog: record allocation borrower: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

// ConfirmAllocation records partial owned evidence even after DELETE. A confirmation never resurrects
// a session, adopts the current name, or converts unknown evidence into a legacy handle.
func (l *Log) ConfirmAllocation(ctx context.Context, attempt string, in api.Incarnation) error {
	tx, err := l.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var backend string
	var released bool
	if err := tx.QueryRowContext(ctx, `SELECT backend,released FROM allocations WHERE id=? AND session=?`, attempt, l.session).
		Scan(&backend, &released); errors.Is(err, sql.ErrNoRows) {
		return ErrAllocationNotFound
	} else if err != nil {
		return err
	}
	var planned int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM allocation_resources WHERE allocation_id=?`, attempt).Scan(&planned); err != nil {
		return err
	}
	if planned == 0 {
		if err := confirmLegacyAllocation(ctx, tx, l.session, attempt, in); err != nil {
			return err
		}
		return tx.Commit()
	}
	observed := in.Cleanup.Resources()
	if len(observed) == 0 {
		return tx.Commit()
	}
	if released {
		return ErrAllocationConflict
	}
	for _, resource := range observed {
		var rowID, uid, borrower string
		var confirmed, resourceReleased bool
		if err := tx.QueryRowContext(ctx, `SELECT r.id,r.uid,COALESCE(b.reference,''),r.confirmed,r.released
			FROM allocation_resources r
			LEFT JOIN allocation_borrowers e ON e.resource_id=r.id
			LEFT JOIN allocation_resources b ON b.id=e.borrower_resource_id
			WHERE r.allocation_id=? AND r.reference=?`, attempt, resource.Reference).
			Scan(&rowID, &uid, &borrower, &confirmed, &resourceReleased); errors.Is(err, sql.ErrNoRows) {
			return ErrAllocationConflict
		} else if err != nil {
			return err
		}
		if borrower != resource.Borrower {
			return ErrAllocationConflict
		}
		if resourceReleased || confirmed && uid != resource.UID {
			return ErrAllocationConflict
		}
		if confirmed {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE allocation_resources SET uid=?,confirmed=1 WHERE id=?`, resource.UID, rowID); err != nil {
			return err
		}
	}
	if err := aliasConfirmedClaims(ctx, tx, l.session, backend, attempt); err != nil {
		return err
	}
	if err := refreshAllocationConfirmation(ctx, tx, attempt); err != nil {
		return err
	}
	return tx.Commit()
}

func confirmLegacyAllocation(ctx context.Context, tx *sql.Tx, session, attempt string, in api.Incarnation) error {
	var id, backend string
	var confirmed bool
	if err := tx.QueryRowContext(ctx, `SELECT resource_id,backend,confirmed FROM allocations WHERE id=? AND session=?`, attempt, session).
		Scan(&id, &backend, &confirmed); errors.Is(err, sql.ErrNoRows) {
		return ErrAllocationNotFound
	} else if err != nil {
		return err
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
	if _, err := tx.ExecContext(ctx, `UPDATE allocations SET resource_id=?,confirmed=1 WHERE id=? AND session=?`, in.ID, attempt, session); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM allocations
		WHERE session=? AND backend=? AND resource_id=? AND confirmed=1 AND released=0 AND id<>?`, session, backend, in.ID, attempt)
	return err
}

func aliasConfirmedClaims(ctx context.Context, tx *sql.Tx, session, backend, attempt string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM allocation_resources WHERE allocation_id=? ORDER BY ordinal DESC`, attempt)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := aliasCleanupClaim(ctx, tx, session, backend, id, map[string]bool{}); err != nil {
			return err
		}
	}
	return nil
}

func aliasCleanupClaim(ctx context.Context, tx *sql.Tx, session, backend, resourceID string, visiting map[string]bool) error {
	var reference, uid, canonical, borrowerID string
	var confirmed, released bool
	var borrower sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT r.reference,r.uid,COALESCE(r.canonical_resource_id,''),r.confirmed,r.released,
		COALESCE(e.borrower_resource_id,'') FROM allocation_resources r
		LEFT JOIN allocation_borrowers e ON e.resource_id=r.id WHERE r.id=?`, resourceID).
		Scan(&reference, &uid, &canonical, &confirmed, &released, &borrower); err != nil {
		return err
	}
	if !confirmed || released {
		return nil
	}
	if canonical != "" {
		return nil
	}
	if borrower.Valid && borrower.String != "" {
		borrowerID = borrower.String
		var borrowerConfirmed bool
		if err := tx.QueryRowContext(ctx, `SELECT confirmed FROM allocation_resources WHERE id=?`, borrowerID).Scan(&borrowerConfirmed); err != nil {
			return err
		}
		if !borrowerConfirmed {
			return nil // a partial result cannot be canonicalized until its borrower exists
		}
		if visiting[resourceID] {
			return ErrAllocationConflict
		}
		visiting[resourceID] = true
		if err := aliasCleanupClaim(ctx, tx, session, backend, borrowerID, visiting); err != nil {
			return err
		}
		delete(visiting, resourceID)
	}
	var candidate string
	err := tx.QueryRowContext(ctx, `SELECT r.id FROM allocation_resources r JOIN allocations a ON a.id=r.allocation_id
		WHERE a.session=? AND a.backend=? AND r.reference=? AND r.uid=? AND r.confirmed=1
		  AND r.released=0 AND r.id<>? ORDER BY r.id LIMIT 1`,
		session, backend, reference, uid, resourceID).Scan(&candidate)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	root, err := canonicalResourceRoot(ctx, tx, candidate)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE allocation_resources SET canonical_resource_id=? WHERE id=?`, root, resourceID); err != nil {
		return err
	}
	cycle, err := canonicalBorrowCycle(ctx, tx, session)
	if err != nil {
		return err
	}
	if cycle {
		return ErrAllocationConflict
	}
	return nil
}

func canonicalResourceRoot(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	for i := 0; id != "" && i < 1_000; i++ {
		var canonical sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT canonical_resource_id FROM allocation_resources WHERE id=?`, id).Scan(&canonical); errors.Is(err, sql.ErrNoRows) {
			return "", ErrAllocationConflict
		} else if err != nil {
			return "", err
		}
		if !canonical.Valid || canonical.String == "" {
			return id, nil
		}
		id = canonical.String
	}
	return "", ErrAllocationConflict
}

func canonicalBorrowCycle(ctx context.Context, tx *sql.Tx, session string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT e.resource_id,e.borrower_resource_id
		FROM allocation_borrowers e JOIN allocation_resources r ON r.id=e.resource_id
		JOIN allocations a ON a.id=r.allocation_id WHERE a.session=?`, session)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	graph := map[string]string{}
	for rows.Next() {
		var resource, borrower string
		if err := rows.Scan(&resource, &borrower); err != nil {
			return false, err
		}
		resourceRoot, err := canonicalResourceRoot(ctx, tx, resource)
		if err != nil {
			return false, err
		}
		borrowerRoot, err := canonicalResourceRoot(ctx, tx, borrower)
		if err != nil {
			return false, err
		}
		if previous, exists := graph[resourceRoot]; exists && previous != borrowerRoot {
			return false, ErrAllocationConflict
		}
		graph[resourceRoot] = borrowerRoot
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	state := map[string]uint8{}
	var visit func(string) bool
	visit = func(id string) bool {
		switch state[id] {
		case 1:
			return true
		case 2:
			return false
		}
		state[id] = 1
		if next := graph[id]; next != "" && visit(next) {
			return true
		}
		state[id] = 2
		return false
	}
	for id := range graph {
		if visit(id) {
			return true, nil
		}
	}
	return false, nil
}

func refreshAllocationConfirmation(ctx context.Context, tx *sql.Tx, attempt string) error {
	_, err := tx.ExecContext(ctx, `UPDATE allocations SET confirmed=
		CASE WHEN NOT EXISTS (SELECT 1 FROM allocation_resources WHERE allocation_id=? AND confirmed=0) THEN 1 ELSE 0 END
		WHERE id=?`, attempt, attempt)
	return err
}

// AllocationResources returns the durable claims for one session-scoped attempt.
func (s *Store) AllocationResources(ctx context.Context, uid, attempt string) ([]AllocationResource, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.id,r.allocation_id,r.ordinal,r.reference,r.uid,
		COALESCE(r.canonical_resource_id,''),r.confirmed,r.released,COALESCE(b.reference,'')
		FROM allocation_resources r JOIN allocations a ON a.id=r.allocation_id
		LEFT JOIN allocation_borrowers e ON e.resource_id=r.id
		LEFT JOIN allocation_resources b ON b.id=e.borrower_resource_id
		WHERE a.session=? AND r.allocation_id=? ORDER BY r.ordinal`, uid, attempt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AllocationResource
	for rows.Next() {
		var resource AllocationResource
		if err := rows.Scan(&resource.ID, &resource.AllocationID, &resource.Ordinal, &resource.Reference,
			&resource.UID, &resource.CanonicalID, &resource.Confirmed, &resource.Released, &resource.Borrower); err != nil {
			return nil, err
		}
		out = append(out, resource)
	}
	return out, rows.Err()
}

// CleanupResourceBlocked reports whether a resource has an unreleased borrower after canonicalization.
func (s *Store) CleanupResourceBlocked(ctx context.Context, uid, attempt, reference string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.id,r.allocation_id,r.reference,r.confirmed,r.released,
		COALESCE(r.canonical_resource_id,'') FROM allocation_resources r
		JOIN allocations a ON a.id=r.allocation_id WHERE a.session=?`, uid)
	if err != nil {
		return false, err
	}
	resources := map[string]AllocationResource{}
	for rows.Next() {
		var resource AllocationResource
		if err := rows.Scan(&resource.ID, &resource.AllocationID, &resource.Reference, &resource.Confirmed,
			&resource.Released, &resource.CanonicalID); err != nil {
			rows.Close()
			return false, err
		}
		resources[resource.ID] = resource
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	root := func(id string) string {
		seen := map[string]bool{}
		for id != "" && !seen[id] {
			seen[id] = true
			resource := resources[id]
			if resource.CanonicalID == "" {
				return id
			}
			id = resource.CanonicalID
		}
		return ""
	}
	target := ""
	for id, resource := range resources {
		if resource.AllocationID == attempt && resource.Reference == reference {
			target = root(id)
			break
		}
	}
	if target == "" {
		return false, nil
	}
	edges, err := s.db.QueryContext(ctx, `SELECT e.resource_id,e.borrower_resource_id FROM allocation_borrowers e
		JOIN allocation_resources r ON r.id=e.resource_id JOIN allocations a ON a.id=r.allocation_id
		WHERE a.session=?`, uid)
	if err != nil {
		return false, err
	}
	defer edges.Close()
	for edges.Next() {
		var resourceID, borrowerID string
		if err := edges.Scan(&resourceID, &borrowerID); err != nil {
			return false, err
		}
		if root(resourceID) != target {
			continue
		}
		borrowerRoot := root(borrowerID)
		for id, borrower := range resources {
			if root(id) == borrowerRoot && borrower.Confirmed && !borrower.Released {
				return true, nil
			}
		}
	}
	return false, edges.Err()
}

// Allocations returns all unreleased attempts, including unknown outcomes and backend routes.
func (s *Store) Allocations(ctx context.Context, uid string) ([]Allocation, error) {
	return s.allocations(ctx, uid, "", -1)
}

// AllocationPage bounds cleanup memory while preserving durable attempts and independent progress.
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

// ReleaseAllocation acknowledges release of a legacy attempt or all owned claims for this attempt.
func (s *Store) ReleaseAllocation(ctx context.Context, uid, attempt string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE allocations SET released=1 WHERE id=? AND session=?`, attempt, uid)
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
	resourceRows, err := tx.QueryContext(ctx, `SELECT id FROM allocation_resources WHERE allocation_id=?`, attempt)
	if err != nil {
		return err
	}
	var resourceIDs []string
	for resourceRows.Next() {
		var id string
		if err := resourceRows.Scan(&id); err != nil {
			resourceRows.Close()
			return err
		}
		resourceIDs = append(resourceIDs, id)
	}
	if err := resourceRows.Close(); err != nil {
		return err
	}
	for _, id := range resourceIDs {
		root, err := canonicalResourceRoot(ctx, tx, id)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE allocation_resources SET released=1
			WHERE id=? OR canonical_resource_id=?`, root, root); err != nil {
			return err
		}
	}
	return tx.Commit()
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
