package session_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
)

func TestReaperRetainsIndependentProgressAfterStoreRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	store := openStore(t, path)
	good := &retryCleanup{Backend: local.New(echoagent.Harness{}), key: "good"}
	bad := &retryCleanup{Backend: local.New(echoagent.Harness{}), key: "bad", fail: true}
	t.Cleanup(func() { _ = good.Close(); _ = bad.Close() })
	registry, err := placement.NewRegistry("good", map[string]*placement.Placer{"good": placement.New(good, echoagent.Model), "bad": placement.New(bad, echoagent.Model)})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutSession(sqlitelog.SessionMeta{UID: "shared-session"}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"good", "bad"} {
		id, err := store.Session("shared-session").BeginAllocation(t.Context(), key, "create")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Session("shared-session").ConfirmAllocation(t.Context(), id, api.Incarnation{ID: "shared-session"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DeleteSession(t.Context(), "shared-session"); err != nil {
		t.Fatal(err)
	}
	if err := session.NewService(store, registry).ReapOnce(t.Context()); !errors.Is(err, errRelease) {
		t.Fatalf("partial cleanup=%v", err)
	}
	pending, err := store.Allocations(t.Context(), "shared-session")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Backend != "bad" {
		t.Fatalf("independent progress lost: %+v", pending)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openStore(t, path)
	bad.mu.Lock()
	bad.fail = false
	bad.mu.Unlock()
	// The released backend need not even remain configured on the replacement service.
	registry = echoRegistry(t, bad)
	if err := session.NewService(store, registry).ReapOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	info, err := store.SessionInfo("shared-session")
	if err != nil {
		t.Fatal(err)
	}
	if info.DeletionState != api.DeletionComplete {
		t.Fatalf("restart did not finish: %+v", info)
	}
	good.mu.Lock()
	calls := good.calls
	good.mu.Unlock()
	if calls != 1 {
		t.Fatalf("released backend called %d times", calls)
	}
}

func TestReaperPagesPastHundredAndPreservesRetainedChildren(t *testing.T) {
	store := openStore(t, ":memory:")
	registry, _ := deletionRegistry(t)
	svc := session.NewService(store, registry)
	for i := 0; i < 105; i++ {
		uid := fmt.Sprintf("empty-%03d", i)
		if err := store.PutSession(sqlitelog.SessionMeta{UID: uid}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DeleteSession(t.Context(), uid); err != nil {
			t.Fatal(err)
		}
	}
	client := serveRegistry(t, store, registry)
	parent := mustCreate(t, client)
	if err := deletionExec(t, client, parent, ""); err != nil {
		t.Fatal(err)
	}
	fork, err := client.Fork(t.Context(), &v1.ForkRequest{Session: parent})
	if err != nil {
		t.Fatal(err)
	}
	child := fork.Children[0].Metadata.Uid
	if _, err := client.DeleteSession(t.Context(), &v1.DeleteSessionRequest{Uid: parent}); err != nil {
		t.Fatal(err)
	}
	if err := svc.ReapOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if candidates, err := store.DeletionCandidates(t.Context(), "", 100); err != nil || len(candidates) != 0 {
		t.Fatalf("cleanup stopped at first page: %v, %v", candidates, err)
	}
	if err := deletionExec(t, client, child, ""); err != nil {
		t.Fatalf("parent cleanup damaged child: %v", err)
	}
	if _, err := client.Fork(t.Context(), &v1.ForkRequest{Session: child}); err != nil {
		t.Fatalf("retained child cannot fork: %v", err)
	}
}

func TestDuplicateReapersExcludeReleaseForSameSession(t *testing.T) {
	store := openStore(t, ":memory:")
	b := &retryCleanup{Backend: local.New(echoagent.Harness{}), entered: make(chan struct{}, 1), release: make(chan struct{})}
	t.Cleanup(func() { _ = b.Close() })
	registry := echoRegistry(t, b)
	if err := store.PutSession(sqlitelog.SessionMeta{UID: "pending"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Session("pending").BeginAllocation(t.Context(), b.CleanupKey(), "create"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteSession(t.Context(), "pending"); err != nil {
		t.Fatal(err)
	}
	svc := session.NewService(store, registry)
	done := make(chan error, 1)
	go func() { done <- svc.ReapOnce(t.Context()) }()
	select {
	case <-b.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not start")
	}
	if err := session.NewService(store, registry).ReapOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	close(b.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not stop")
	}
	b.mu.Lock()
	calls := b.calls
	b.mu.Unlock()
	if calls != 1 {
		t.Fatalf("duplicate release calls=%d", calls)
	}
}
