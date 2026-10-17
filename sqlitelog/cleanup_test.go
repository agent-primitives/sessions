package sqlitelog

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
)

// Losing an unconfirmed attempt on reopen would let cleanup falsely certify absence.
func TestAllocationIntentAndConfirmationSurviveReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.db")
	s := deletionStore(t, path)
	attempt, err := s.Session("implicit").BeginAllocation(ctx, "route-a", "create")
	if err != nil || attempt == "" {
		t.Fatalf("begin = %q, %v", attempt, err)
	}
	info := deletionInfo(t, s, "implicit")
	if !info.InventoryComplete || info.Project != "tenant" || info.LastSeq != 0 {
		t.Fatalf("implicit metadata = %+v", info)
	}
	second, err := s.Session("implicit").BeginAllocation(ctx, "route-b", "restore")
	if err != nil || second == attempt {
		t.Fatalf("second = %q, %v", second, err)
	}
	s.Close()
	s = deletionStore(t, path)
	got, err := s.Allocations(ctx, "implicit")
	if err != nil || len(got) != 2 {
		t.Fatalf("reopened allocations = %+v, %v", got, err)
	}
	var found bool
	for _, a := range got {
		if a.ID == attempt {
			found = a.SessionUID == "implicit" && a.Backend == "route-a" && a.Operation == "create" && !a.Confirmed && a.ResourceID == ""
		}
	}
	if !found {
		t.Fatalf("lost exact intent: %+v", got)
	}
	if err := s.Session("other").ConfirmAllocation(ctx, attempt, api.Incarnation{ID: "owned"}); !errors.Is(err, ErrAllocationNotFound) {
		t.Fatalf("foreign confirmation = %v", err)
	}
	if err := s.Session("implicit").ConfirmAllocation(ctx, attempt, api.Incarnation{}); !errors.Is(err, ErrAllocationConflict) {
		t.Fatalf("empty identity = %v", err)
	}
	if err := s.Session("implicit").ConfirmAllocation(ctx, attempt, api.Incarnation{ID: "owned", Address: "secret", CallMetadata: map[string]string{"token": "private"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Session("implicit").ConfirmAllocation(ctx, attempt, api.Incarnation{ID: "owned", Address: "changed"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Session("implicit").ConfirmAllocation(ctx, attempt, api.Incarnation{ID: "replacement"}); !errors.Is(err, ErrAllocationConflict) {
		t.Fatalf("identity mutation = %v", err)
	}
	s.Close()
	s = deletionStore(t, path)
	got, err = s.Allocations(ctx, "implicit")
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, a := range got {
		if a.ID == attempt {
			found = a.Confirmed && a.ResourceID == "owned"
		}
	}
	if !found {
		t.Fatalf("confirmation lost on reopen: %+v", got)
	}
	if err := s.ReleaseAllocation(ctx, "other", attempt); !errors.Is(err, ErrAllocationNotFound) {
		t.Fatalf("foreign release = %v", err)
	}
	if err := s.ReleaseAllocation(ctx, "implicit", attempt); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseAllocation(ctx, "implicit", attempt); err != nil {
		t.Fatal(err)
	}
	got, err = s.Allocations(ctx, "implicit")
	if err != nil || len(got) != 1 || got[0].ID != second {
		t.Fatalf("unreleased = %+v, %v", got, err)
	}
	s.Close()
	s = deletionStore(t, path)
	got, err = s.Allocations(ctx, "implicit")
	if err != nil || len(got) != 1 || got[0].ID != second {
		t.Fatalf("reopened release = %+v, %v", got, err)
	}
}

// Completion must not erase pending effects or rewrite audit history when the worker retries.
func TestCompleteDeletionRequiresReleasedAttemptsAndPreservesAudit(t *testing.T) {
	ctx := context.Background()
	s := deletionStore(t, ":memory:")
	attempt, err := s.Session("s").BeginAllocation(ctx, "route", "create")
	if err != nil {
		t.Fatal(err)
	}
	approvalAppend(t, s.Session("s"), 0, 0, api.Event{Kind: api.EventInput, Message: api.TextMessage("user", "audit")})
	if err := s.CompleteDeletion(ctx, "s"); !errors.Is(err, ErrCleanupPending) {
		t.Fatalf("active completion = %v", err)
	}
	if _, err := s.DeleteSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	before := captureDeletion(t, s, "s")
	if _, err := s.Session("s").BeginAllocation(ctx, "route", "restore"); !errors.Is(err, eventlog.ErrDeleted) {
		t.Fatalf("tombstone allocation = %v", err)
	}
	if err := s.CompleteDeletion(ctx, "s"); !errors.Is(err, ErrCleanupPending) {
		t.Fatalf("unconfirmed completion = %v", err)
	}
	assertDeletionUnchanged(t, s, "s", before)
	// A delayed acknowledgement remains valid after DELETE, without reopening admission.
	if err := s.Session("s").ConfirmAllocation(ctx, attempt, api.Incarnation{ID: "actor"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteDeletion(ctx, "s"); !errors.Is(err, ErrCleanupPending) {
		t.Fatalf("owned completion = %v", err)
	}
	if err := s.RecordCleanupError(ctx, "s", strings.Repeat("x", 2000)); err != nil {
		t.Fatal(err)
	}
	if n := len(deletionInfo(t, s, "s").CleanupError); n == 0 || n > 1024 {
		t.Fatalf("unbounded diagnostic = %d", n)
	}
	if err := s.ReleaseAllocation(ctx, "s", attempt); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteDeletion(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	after := captureDeletion(t, s, "s")
	if after.Info.DeletionState != api.DeletionComplete || after.Info.ComputeState != api.ComputeTerminated || after.Info.DeletionCompletedAt.IsZero() || after.Info.CleanupError != "" {
		t.Fatalf("completed = %+v", after.Info)
	}
	if !reflect.DeepEqual(before.Events, after.Events) || before.Fence != after.Fence || before.Info.LastSeq != after.Info.LastSeq || before.Info.DeletionSeq != after.Info.DeletionSeq || !before.Info.UpdatedAt.Equal(after.Info.UpdatedAt) || !before.Info.DeletionRequestedAt.Equal(after.Info.DeletionRequestedAt) {
		t.Fatal("completion changed audit/cursor/timestamps")
	}
	if err := s.CompleteDeletion(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	assertDeletionUnchanged(t, s, "s", after)
	if err := s.Session("s").CheckAdmissible(); !errors.Is(err, eventlog.ErrDeleted) {
		t.Fatalf("COMPLETE admission = %v", err)
	}
	if err := s.Session("s").Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupCandidatesAndMissingSessions(t *testing.T) {
	ctx := context.Background()
	s := deletionStore(t, ":memory:")
	for _, uid := range []string{"c", "a", "b", "active"} {
		if err := s.PutSession(SessionMeta{UID: uid}); err != nil {
			t.Fatal(err)
		}
		if uid != "active" {
			if _, err := s.DeleteSession(ctx, uid); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.CompleteDeletion(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	got, err := s.DeletionCandidates(ctx, "", 1)
	if err != nil || !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("page 1 = %v, %v", got, err)
	}
	got, err = s.DeletionCandidates(ctx, "a", 1)
	if err != nil || !reflect.DeepEqual(got, []string{"c"}) {
		t.Fatalf("page 2 = %v, %v", got, err)
	}
	for _, err := range []error{s.CompleteDeletion(ctx, "missing"), s.CertifyInventory(ctx, "missing", "reviewed namespaces"), s.RecordCleanupError(ctx, "missing", "failure")} {
		if !errors.Is(err, ErrSessionNotFound) {
			t.Fatalf("unknown = %v", err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Session("cancel").BeginAllocation(canceled, "route", "create"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled begin = %v", err)
	}
	if _, err := s.SessionInfo("cancel"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("canceled begin created metadata = %v", err)
	}
}

// Each trigger fails after an earlier write: transaction rollback must retain the entire old state.
func TestCleanupTransactionsRollback(t *testing.T) {
	ctx := context.Background()
	s := deletionStore(t, ":memory:")
	approvalExec(t, s.db, `CREATE TRIGGER fail_attempt BEFORE INSERT ON allocations BEGIN SELECT RAISE(ABORT,'intent failed'); END`)
	if _, err := s.Session("new").BeginAllocation(ctx, "route", "create"); err == nil {
		t.Fatal("intent trigger ignored")
	}
	if _, err := s.SessionInfo("new"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("intent leaked implicit session: %v", err)
	}
	approvalExec(t, s.db, `DROP TRIGGER fail_attempt`)
	if err := s.PutSession(SessionMeta{UID: "s"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	before := captureDeletion(t, s, "s")
	approvalExec(t, s.db, `CREATE TRIGGER fail_complete BEFORE UPDATE OF deletion_completed_at ON sessions BEGIN SELECT RAISE(ABORT,'completion failed'); END`)
	if err := s.CompleteDeletion(ctx, "s"); err == nil {
		t.Fatal("completion trigger ignored")
	}
	assertDeletionUnchanged(t, s, "s", before)
}

func openV4Fixture(t *testing.T, path string) {
	t.Helper()
	blob, err := os.ReadFile("testdata/approval-v3.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, blob, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// Preserve the historical rung: this fixture has never passed through v5 Open.
	if err := migrateV3ToV4(tx); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO sessions(session, project) VALUES('legacy','tenant'); PRAGMA user_version=4`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// A migration failure during the old-row update rolls back the new column and schema stamp.
func TestCleanupV5MigrationRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v4.db")
	openV4Fixture(t, path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	approvalExec(t, db, `CREATE TRIGGER fail_inventory BEFORE UPDATE ON sessions BEGIN SELECT RAISE(ABORT,'inventory migration failed'); END`)
	s, err := Open(path)
	if err == nil {
		s.Close()
		t.Fatal("migration ignored inventory trigger")
	}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 4 {
		t.Fatalf("failed migration stamp=%d %v", version, err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name='inventory_complete'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed migration leaked column=%d %v", count, err)
	}
	approvalExec(t, db, `DROP TRIGGER fail_inventory`)
	s = deletionStore(t, path)
	if deletionInfo(t, s, "legacy").InventoryComplete {
		t.Fatal("migration retry certified old inventory")
	}
}

// A genuine v4 row is not evidence of complete write-ahead tracking, even with an empty ledger.
func TestCleanupV4InventoryRequiresExplicitCertificate(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v4.db")
	openV4Fixture(t, path)
	s := deletionStore(t, path)
	info := deletionInfo(t, s, "legacy")
	if info.InventoryComplete {
		t.Fatal("legacy empty ledger inferred complete")
	}
	attempt, err := s.Session("legacy").BeginAllocation(ctx, "route", "restore")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Session("legacy").ConfirmAllocation(ctx, attempt, api.Incarnation{ID: "verified"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseAllocation(ctx, "legacy", attempt); err != nil {
		t.Fatal(err)
	}
	if deletionInfo(t, s, "legacy").InventoryComplete {
		t.Fatal("individual resource confirmation certified inventory")
	}
	if _, err := s.DeleteSession(ctx, "legacy"); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteDeletion(ctx, "legacy"); !errors.Is(err, ErrInventoryIncomplete) {
		t.Fatalf("legacy completion = %v", err)
	}
	if err := s.CertifyInventory(ctx, "legacy", " \n\t"); !errors.Is(err, ErrInventoryIncomplete) {
		t.Fatalf("empty certificate = %v", err)
	}
	if err := s.CertifyInventory(ctx, "legacy", "operator checked all namespaces, overrides and orphan forks"); err != nil {
		t.Fatal(err)
	}
	var evidence string
	if err := s.db.QueryRow(`SELECT inventory_evidence FROM sessions WHERE session='legacy'`).Scan(&evidence); err != nil || evidence != "operator checked all namespaces, overrides and orphan forks" {
		t.Fatalf("certificate = %q, %v", evidence, err)
	}
	s.Close()
	s = deletionStore(t, path)
	if !deletionInfo(t, s, "legacy").InventoryComplete {
		t.Fatal("certificate lost on reopen")
	}
	var persisted string
	if err := s.db.QueryRow(`SELECT inventory_evidence FROM sessions WHERE session='legacy'`).Scan(&persisted); err != nil || persisted != evidence {
		t.Fatalf("certificate evidence lost on reopen = %q, %v", persisted, err)
	}
	if err := s.CompleteDeletion(ctx, "legacy"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session("new").BeginAllocation(ctx, "route", "create"); err != nil {
		t.Fatal(err)
	}
	if !deletionInfo(t, s, "new").InventoryComplete {
		t.Fatal("new implicit row is incomplete")
	}
}
