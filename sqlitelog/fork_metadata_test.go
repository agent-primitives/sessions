package sqlitelog_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/agent-primitives/sessions/sqlitelog"
)

func TestForkSessionMetadataCommitsWithFirstFence(t *testing.T) {
	s := mustOpen(t, ":memory:")
	log := s.ForkSession(sqlitelog.SessionMeta{UID: "child", Harness: "agent", Model: "chosen", ModelPolicy: true, ParentUID: "parent"})
	if _, err := s.SessionInfo("child"); !errors.Is(err, sqlitelog.ErrSessionNotFound) {
		t.Fatalf("unprovisioned child was published: %v", err)
	}
	if fence, err := log.NewFence(); err != nil || fence != 1 {
		t.Fatalf("first fence = %d,%v", fence, err)
	}
	info, err := s.SessionInfo("child")
	if err != nil || !info.ModelPolicy || info.Model != "chosen" || info.Harness != "agent" || info.ParentUID != "parent" {
		t.Fatalf("first child row lacks inherited metadata: %+v,%v", info, err)
	}
}

func TestForkSessionFenceFailureRollsBackMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fork.db")
	s := mustOpen(t, path)
	// The metadata insert would precede the fence upsert. Reject that update to prove both share
	// one transaction, rather than leaving a row exposed after a partially failed initialization.
	if _, err := rawDB(t, path).Exec(`CREATE TRIGGER reject_fork_fence BEFORE UPDATE ON sessions BEGIN SELECT RAISE(ABORT,'fence failed'); END`); err != nil {
		t.Fatal(err)
	}
	log := s.ForkSession(sqlitelog.SessionMeta{UID: "child", ModelPolicy: true})
	if _, err := log.NewFence(); err == nil {
		t.Fatal("fence failure ignored")
	}
	if _, err := s.SessionInfo("child"); !errors.Is(err, sqlitelog.ErrSessionNotFound) {
		t.Fatalf("failed initialization exposed row: %v", err)
	}
}
