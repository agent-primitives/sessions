package session_test

import (
	"context"
	"database/sql"
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
	for _, source := range []string{"policy", "open"} {
		t.Run(source, func(t *testing.T) { checkForkChildModelAdmission(t, source) })
	}
}

func checkForkChildModelAdmission(t *testing.T, source string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fork.db")
	store := openStore(t, path)
	backend := &interruptedPolicyForkBackend{Backend: local.New(sessionPolicyHarness{}), second: make(chan string, 1), release: make(chan struct{})}
	t.Cleanup(func() { _ = backend.Close() })
	var release sync.Once
	defer release.Do(func() { close(backend.release) })
	model := func(_ context.Context, req api.ModelRequest) (api.ModelResponse, error) {
		return api.ModelResponse{Message: *api.TextMessage("assistant", req.Model)}, nil
	}
	p := placement.New(backend, model, placement.WithModelPolicy(api.ModelPolicy{Default: "default", Allowed: []string{"default", "chosen"}}))
	registry, err := placement.NewRegistry(source, map[string]*placement.Placer{"policy": p, "open": placement.New(backend, model)})
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
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var flags int
	err = db.QueryRow(`SELECT model_policy FROM sessions WHERE session=?`, child).Scan(&flags)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	updates, execErr := collectPublicExec(client, &v1.ExecRequest{Session: child, Harness: "policy", ExpectedLastSeq: &info.LastSeq})
	var outputs []string
	for _, update := range updates {
		event := update.GetRecord().GetEvent()
		if event.GetKind() == v1.EventKind_EVENT_OUTPUT {
			for _, part := range event.GetMessage().GetParts() {
				outputs = append(outputs, part.GetText().GetText())
			}
		}
	}
	release.Do(func() { close(backend.release) })
	forkErr := <-forked
	wantFlags := 2
	if source == "policy" {
		wantFlags = 3
	}
	if lookupErr != nil || info.ModelPolicy != (source == "policy") || !info.ModelPolicyEligible || flags != wantFlags || info.Harness != source || info.Model != "chosen" {
		t.Fatalf("visible child lost policy metadata: %+v, %v", info, lookupErr)
	}
	if execErr != nil || len(outputs) != 1 || outputs[0] != "chosen" {
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
	if err != nil || info.ModelPolicy != (source == "policy") || !info.ModelPolicyEligible || info.Model != "chosen" {
		t.Fatalf("failed-fork policy lost after reopen: %+v,%v", info, err)
	}
	if err := store.Session(child).Verify(); err != nil {
		t.Fatal(err)
	}
}
