package placement_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/sqlitelog"
)

// Every turn reattaches to local compute, including turns refused by the cursor CAS.
// Successful reattachments must not grow either the live inventory or the stored table.
func TestAllocationInventoryAcrossRepeatedExecAndResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	s, err := sqlitelog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := newLocalPlacer(t, echoagent.Harness{})
	log := s.Session("s")
	check := func(operation string) {
		t.Helper()
		rows, err := s.Allocations(t.Context(), "s")
		if err != nil || len(rows) != 1 {
			t.Errorf("%s live inventory = %+v, %v; want one row", operation, rows, err)
		} else if !rows[0].Confirmed || rows[0].ResourceID != "s" || rows[0].Backend != "local" || rows[0].Operation != operation {
			t.Errorf("%s inventory = %+v", operation, rows[0])
		}
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM allocations`).Scan(&count); err != nil || count != 1 {
			t.Errorf("%s stored rows = %d, %v; want one", operation, count, err)
		}
	}
	for range 3 {
		head, err := log.Head()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Exec(t.Context(), log, "s", []api.Message{*api.TextMessage("user", "hello")}, head); err != nil {
			t.Fatal(err)
		}
		check("create")
	}
	head, err := log.Head()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(t.Context(), log, "s", []api.Message{*api.TextMessage("user", "stale")}, 0); !errors.Is(err, eventlog.ErrConflict) {
		t.Fatalf("stale Exec = %v; want cursor conflict", err)
	}
	if after, err := log.Head(); err != nil || after != head {
		t.Fatalf("stale Exec changed journal: %d -> %d, %v", head, after, err)
	}
	check("create")
	for range 2 {
		if err := p.Resume(t.Context(), log, "s"); err != nil {
			t.Fatal(err)
		}
		check("restore")
	}
}
