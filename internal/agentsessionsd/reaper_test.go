package agentsessionsd

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/sqlitelog"
)

func TestRunRecoversUnfinishedForksBeforeListening(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	store, err := sqlitelog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutSession(sqlitelog.SessionMeta{UID: "parent", Harness: "echo"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginFork(t.Context(), "parent", []sqlitelog.SessionMeta{{UID: "unfinished", ParentUID: "parent", Harness: "echo"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	listened := false
	err = Run([]string{"-journal", path, "-addr", "127.0.0.1:0"}, withContext(ctx), withListening(func(string) {
		listened = true
		defer cancel()
		observer, err := sqlitelog.Open(path)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = observer.Close() }()
		info, err := observer.SessionInfo("unfinished")
		if err != nil {
			t.Errorf("recovery did not precede serving: %v", err)
			return
		}
		if info.DeletionState != api.DeletionPending && info.DeletionState != api.DeletionComplete {
			t.Errorf("unfinished child admitted: %+v", info)
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !listened {
		t.Fatal("daemon never served")
	}
	// Run must join cleanup before its deferred store/backend closures. Reopening and reading
	// after return also ensures no cleanup goroutine still owns a live service resource.
	observer, err := sqlitelog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = observer.Close() }()
	if _, err := observer.SessionInfo("unfinished"); err != nil {
		t.Fatal(err)
	}
}
