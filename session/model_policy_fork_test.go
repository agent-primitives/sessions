package session_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
)

type interruptedPolicyForkBackend struct {
	*local.Backend
	second  chan string
	release chan struct{}
	first   string
	calls   int
}

func (b *interruptedPolicyForkBackend) Fork(ctx context.Context, ref api.SnapshotRef, opts api.ForkOpts) (api.Incarnation, error) {
	b.calls++
	if b.calls == 2 {
		b.second <- b.first // First child is copied; hold the fan-out before metadata finalization.
		select {
		case <-b.release:
		case <-ctx.Done():
			return api.Incarnation{}, ctx.Err()
		}
		return api.Incarnation{}, errors.New("second clone failed")
	}
	b.first = opts.ChildSessionUID
	return b.Backend.Fork(ctx, ref, opts)
}

func TestForkChildModelPolicyBeforeFanoutFinishes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fork.db")
	store := openStore(t, path)
	backend := &interruptedPolicyForkBackend{Backend: local.New(sessionPolicyHarness{}), second: make(chan string, 1), release: make(chan struct{})}
	t.Cleanup(func() { _ = backend.Close() })
	var release sync.Once
	defer release.Do(func() { close(backend.release) })
	p := placement.New(backend, func(_ context.Context, req api.ModelRequest) (api.ModelResponse, error) {
		return api.ModelResponse{Message: *api.TextMessage("assistant", req.Model)}, nil
	}, placement.WithModelPolicy(api.ModelPolicy{Default: "default", Allowed: []string{"default", "chosen"}}))
	registry, err := placement.NewRegistry("policy", map[string]*placement.Placer{"policy": p})
	if err != nil {
		t.Fatal(err)
	}
	client := serveRegistry(t, store, registry)
	parent, err := client.CreateSession(t.Context(), &v1.CreateSessionRequest{Session: &v1.Session{Model: "chosen"}})
	if err != nil {
		t.Fatal(err)
	}
	forked := make(chan error, 1)
	go func() {
		_, err := client.Fork(t.Context(), &v1.ForkRequest{Session: parent.GetMetadata().GetUid(), Count: 2})
		forked <- err
	}()
	child := <-backend.second
	// Always unblock the second clone before reporting a failure, avoiding a stranded handler.
	info, lookupErr := store.SessionInfo(child)
	outputs := execOutputs(t, client, child, "while fork pending", info.LastSeq)
	release.Do(func() { close(backend.release) })
	forkErr := <-forked
	if lookupErr != nil || !info.ModelPolicy || info.Harness != "policy" || info.Model != "chosen" {
		t.Fatalf("visible child lost policy metadata: %+v, %v", info, lookupErr)
	}
	if len(outputs) != 1 || outputs[0] != "chosen" {
		t.Fatalf("partial child executed legacy/default model: %v", outputs)
	}
	if forkErr == nil {
		t.Fatal("second clone did not fail")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openStore(t, path)
	info, err = store.SessionInfo(child)
	if err != nil || !info.ModelPolicy || info.Model != "chosen" {
		t.Fatalf("failed-fork policy lost after reopen: %+v,%v", info, err)
	}
	if err := store.Session(child).Verify(); err != nil {
		t.Fatal(err)
	}
}
