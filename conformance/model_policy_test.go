package conformance_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/harness/chatagent"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/sqlitelog"
)

func TestReferenceHarnessHonorsStartModel(t *testing.T) {
	for _, tc := range []struct {
		name   string
		har    api.Harness
		legacy string
	}{
		{"echo", echoagent.Harness{}, "echo"}, {"chat", chatagent.Harness{Model: "original"}, "original"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, enabled := range []bool{false, true} {
				log := eventlog.AsStore(eventlog.New())
				var got string
				var opts []controller.Option
				if enabled {
					opts = append(opts, controller.WithModelPolicy(api.ModelPolicy{Default: "selected", Allowed: []string{tc.legacy, "selected"}}))
				}
				c, err := controller.New(log, func(_ context.Context, r api.ModelRequest) (api.ModelResponse, error) {
					got = r.Model
					return api.ModelResponse{}, nil
				}, opts...)
				if err != nil {
					t.Fatal(err)
				}
				if err := c.Exec(t.Context(), tc.har, nil, 0); err != nil {
					t.Fatal(err)
				}
				want := tc.legacy
				if enabled {
					want = "selected"
				}
				if got != want {
					t.Fatalf("policy=%v provider model=%q, want %q", enabled, got, want)
				}
			}
		})
	}
}

// This exercises both real Harness.Connect transport and SQLite serialization, so a dropped
// Start/default field cannot pass simply because in-process Go structs agree.
func TestWireModelDefaultSurvivesReopenRecoveryReplayAndFork(t *testing.T) {
	store, path := openFile(t)
	log := store.Session("parent")
	har := wireHarnessFrom(t, chatagent.Harness{Model: "original"})
	calls := 0
	fn := func(_ context.Context, r api.ModelRequest) (api.ModelResponse, error) {
		calls++
		if r.Model != "selected" {
			t.Errorf("provider model = %q", r.Model)
		}
		return api.ModelResponse{Message: *api.TextMessage("assistant", "ok")}, nil
	}
	live, err := controller.New(log, fn, controller.WithModelPolicy(api.ModelPolicy{Default: "selected", Allowed: []string{"selected", "original"}}))
	if err != nil {
		t.Fatal(err)
	}
	if err := live.Exec(t.Context(), har, []api.Message{*api.TextMessage("user", "hi")}, 0); err != nil {
		t.Fatal(err)
	}
	recs, err := log.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	if recs[0].Event.ExecutionStart.Model != "selected" || recs[2].Event.ModelCall.Model != "selected" {
		t.Fatalf("persisted models = %#v", recs)
	}
	// A real on-disk unfinished invocation with its completed call, before END.
	pending := store.Session("pending")
	fence, err := pending.NewFence()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs[:len(recs)-1] {
		if _, err := pending.Append(r.Seq-1, fence, r.Event); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = sqlitelog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	changed := api.ModelPolicy{Default: "new", Allowed: []string{"new"}}
	pending = store.Session("pending")
	recovering, err := controller.New(pending, fn, controller.WithModelPolicy(changed))
	if err != nil {
		t.Fatal(err)
	}
	if resumed, err := recovering.Resume(t.Context(), har); !resumed || err != nil {
		t.Fatalf("Resume = %v,%v", resumed, err)
	}
	before, _ := pending.Read(1)
	if _, err := recovering.Replay(t.Context(), har); err != nil {
		t.Fatal(err)
	}
	after, _ := pending.Read(1)
	if calls != 1 || !reflect.DeepEqual(before, after) {
		t.Fatal("reconstruction called provider or changed journal")
	}
	child := store.Session("child")
	head, _ := pending.Head()
	if err := controller.Fork(pending, child, head); err != nil {
		t.Fatal(err)
	}
	replay, err := controller.New(child, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replay.Replay(t.Context(), har); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("fork repeated provider: %d", calls)
	}
	if err := child.Verify(); err != nil {
		t.Fatal(err)
	}
}

type invalidModelWireHarness struct{}

func (invalidModelWireHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "invalid"}, nil
}
func (invalidModelWireHarness) Run(ctx context.Context, _ *api.Start, sink api.EventSink) error {
	_, _ = sink.Model(ctx, api.ModelRequest{Model: "forbidden"})
	_ = sink.Output(ctx, "must not commit")
	return nil // Even a peer that swallows the stream failure must not produce successful END.
}
func TestWireInvalidModelHasNoEffect(t *testing.T) {
	log := eventlog.AsStore(eventlog.New())
	calls := 0
	c, err := controller.New(log, func(context.Context, api.ModelRequest) (api.ModelResponse, error) {
		calls++
		return api.ModelResponse{}, nil
	}, controller.WithModelPolicy(api.ModelPolicy{Default: "allowed", Allowed: []string{"allowed"}}))
	if err != nil {
		t.Fatal(err)
	}
	err = c.Exec(t.Context(), wireHarnessFrom(t, invalidModelWireHarness{}), nil, 0)
	if !errors.Is(err, api.ErrModelNotAllowed) || calls != 0 {
		t.Fatalf("Exec/provider calls = %v/%d", err, calls)
	}
	recs, _ := log.Read(1)
	if len(recs) != 2 || recs[1].Event.Kind != api.EventError {
		t.Fatalf("rejection journal = %#v", recs)
	}
}
