package session_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
)

func TestReaperStopsAttemptIterationOnCancellation(t *testing.T) {
	store := openStore(t, ":memory:")
	b := &retryCleanup{Backend: local.New(echoagent.Harness{}), entered: make(chan struct{}, 1), release: make(chan struct{})}
	t.Cleanup(func() { _ = b.Close() })
	registry := echoRegistry(t, b)
	if err := store.PutSession(sqlitelog.SessionMeta{UID: "pending"}); err != nil {
		t.Fatal(err)
	}
	for range 101 {
		if _, err := store.Session("pending").BeginAllocation(t.Context(), b.CleanupKey(), "create"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DeleteSession(t.Context(), "pending"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- session.NewService(store, registry).ReapOnce(ctx) }()
	select {
	case <-b.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no cleanup attempt")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled cleanup=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not stop")
	}
	b.mu.Lock()
	calls := b.calls
	b.mu.Unlock()
	if calls != 1 {
		t.Fatalf("cancelled pass continued %d release calls", calls)
	}
}

func TestReaperBoundsFailureSummary(t *testing.T) {
	store := openStore(t, ":memory:")
	registry, _ := deletionRegistry(t)
	if err := store.PutSession(sqlitelog.SessionMeta{UID: "unsupported"}); err != nil {
		t.Fatal(err)
	}
	for range 150 {
		if _, err := store.Session("unsupported").BeginAllocation(t.Context(), "", "create"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DeleteSession(t.Context(), "unsupported"); err != nil {
		t.Fatal(err)
	}
	err := session.NewService(store, registry).ReapOnce(t.Context())
	if !errors.Is(err, placement.ErrCleanupUnavailable) {
		t.Fatalf("lost failure cause: %v", err)
	}
	if n := len(err.Error()); n > 2048 {
		t.Fatalf("failure summary grows with allocation count: %d bytes", n)
	}
}
