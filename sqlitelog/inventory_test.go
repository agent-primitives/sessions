package sqlitelog

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/agent-primitives/sessions/api"
)

// Coverage cannot be inferred from an empty ledger, metadata, or later tracked effects.
func TestInventoryRequiresExplicitReconciliation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inventory.db")
	openV4Fixture(t, path)
	s := deletionStore(t, path)
	if err := s.PutSession(SessionMeta{UID: "fresh"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOwnedSession(SessionMeta{UID: "owned"}, api.CallerIdentity{Issuer: "issuer", Subject: "owner"}, false); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []string{"legacy", "fresh", "owned"} {
		if deletionInfo(t, s, uid).InventoryComplete {
			t.Fatalf("%s was auto-certified", uid)
		}
		attempt, err := s.Session(uid).BeginAllocation(t.Context(), "route", "restore")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Session(uid).ConfirmAllocation(t.Context(), attempt, api.Incarnation{ID: "resource"}); err != nil {
			t.Fatal(err)
		}
		if err := s.ReleaseAllocation(t.Context(), uid, attempt); err != nil {
			t.Fatal(err)
		}
		if deletionInfo(t, s, uid).InventoryComplete {
			t.Fatalf("release certified %s", uid)
		}
	}
	if err := s.CertifyInventory(t.Context(), "legacy", " \n\t"); !errors.Is(err, ErrInventoryIncomplete) {
		t.Fatalf("empty evidence = %v", err)
	}
	if err := s.CertifyInventory(t.Context(), "missing", "reviewed"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("unknown = %v", err)
	}
	if err := s.CertifyInventory(t.Context(), "legacy", "operator reconciled all namespaces, overrides and failed forks"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = deletionStore(t, path)
	for _, uid := range []string{"legacy", "fresh", "owned"} {
		if got := deletionInfo(t, s, uid).InventoryComplete; got != (uid == "legacy") {
			t.Fatalf("reopened %s complete=%v", uid, got)
		}
	}
	var evidence string
	if err := s.db.QueryRow("SELECT inventory_evidence FROM sessions WHERE session='legacy'").Scan(&evidence); err != nil || evidence != "operator reconciled all namespaces, overrides and failed forks" {
		t.Fatalf("evidence = %q, %v", evidence, err)
	}
}

func TestAllocationIntentRollbackDoesNotLeaveImplicitMetadata(t *testing.T) {
	s := deletionStore(t, ":memory:")
	approvalExec(t, s.db, "CREATE TRIGGER fail_attempt BEFORE INSERT ON allocations BEGIN SELECT RAISE(ABORT,'intent failed'); END")
	if _, err := s.Session("new").BeginAllocation(t.Context(), "route", "create"); err == nil {
		t.Fatal("intent trigger ignored")
	}
	if _, err := s.SessionInfo("new"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("intent leaked metadata: %v", err)
	}
}
