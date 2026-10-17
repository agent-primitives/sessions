package sqlitelog

import (
	"reflect"
	"testing"
)

func TestAllocationPageSkipsReleasedAndPreservesCursor(t *testing.T) {
	store := deletionStore(t, ":memory:")
	if err := store.PutSession(SessionMeta{UID: "s"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if _, err := store.db.Exec(`INSERT INTO allocations(id,session,backend,operation) VALUES(?,'s','','create')`, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ReleaseAllocation(t.Context(), "s", "b"); err != nil {
		t.Fatal(err)
	}
	var ids []string
	cursor := ""
	for {
		page, err := store.AllocationPage(t.Context(), "s", cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		if len(page) != 1 {
			t.Fatalf("page size=%d", len(page))
		}
		ids = append(ids, page[0].ID)
		cursor = page[0].ID
	}
	if !reflect.DeepEqual(ids, []string{"a", "c"}) {
		t.Fatalf("allocation pages=%v", ids)
	}
}

func TestLateCleanupDiagnosticCannotOverwriteCompletion(t *testing.T) {
	store := deletionStore(t, ":memory:")
	if err := store.PutSession(SessionMeta{UID: "s"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteSession(t.Context(), "s"); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteDeletion(t.Context(), "s"); err != nil {
		t.Fatal(err)
	}
	// Deterministic worker interleaving: B completed before A persisted its older failure.
	if err := store.RecordCleanupError(t.Context(), "s", "worker A failed before worker B completed"); err != nil {
		t.Fatal(err)
	}
	info, err := store.SessionInfo("s")
	if err != nil {
		t.Fatal(err)
	}
	if info.CleanupError != "" {
		t.Fatalf("late worker corrupted completion: %q", info.CleanupError)
	}
}
