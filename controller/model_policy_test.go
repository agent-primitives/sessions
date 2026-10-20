package controller_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
)

type modelPolicyHarness struct {
	requests []string
	starts   *[]string
	caught   *[]error
}

func (modelPolicyHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "policy"}, nil
}
func (h modelPolicyHarness) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	if h.starts != nil {
		*h.starts = append(*h.starts, start.Model)
	}
	for _, model := range h.requests {
		_, err := sink.Model(ctx, api.ModelRequest{Model: model})
		if err != nil {
			if h.caught == nil {
				return err
			}
			*h.caught = append(*h.caught, err)
			*h.caught = append(*h.caught, sink.Output(ctx, "forbidden"), sink.Usage(ctx, api.Usage{}))
		}
	}
	return nil
}

func TestModelPolicyDefaultOverridesAndReplay(t *testing.T) {
	log := eventlog.AsStore(eventlog.New())
	var models, starts []string
	c, err := controller.New(log, func(_ context.Context, r api.ModelRequest) (api.ModelResponse, error) {
		models = append(models, r.Model)
		return api.ModelResponse{Message: *api.TextMessage("assistant", r.Model)}, nil
	}, controller.WithModelPolicy(api.ModelPolicy{Default: "default", Allowed: []string{"default", "chosen", "override"}}), controller.WithModelDefault("chosen"))
	if err != nil {
		t.Fatal(err)
	}
	h := modelPolicyHarness{requests: []string{"", "override"}, starts: &starts}
	if err := c.Exec(t.Context(), h, nil, 0); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(models, []string{"chosen", "override"}) {
		t.Fatalf("provider models = %v", models)
	}
	recs, _ := log.Read(1)
	if recs[0].Event.ExecutionStart.Model != "chosen" {
		t.Fatalf("recorded default = %#v", recs[0].Event.ExecutionStart)
	}
	if recs[1].Event.ModelCall.Model != "chosen" || recs[3].Event.ModelCall.Model != "override" {
		t.Fatalf("per-call models not retained: %#v", recs)
	}
	// Reconstruction is based on recorded invocation state, not a changed default or allowlist.
	replay, err := controller.New(log, nil, controller.WithModelPolicy(api.ModelPolicy{Default: "new", Allowed: []string{"new"}}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replay.Replay(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(starts, []string{"chosen", "chosen"}) || len(models) != 2 {
		t.Fatalf("starts/models = %v/%v", starts, models)
	}
	if err := log.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidModelIsFatalEvenWhenCaught(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "stream"}[streaming], func(t *testing.T) {
			log := eventlog.AsStore(eventlog.New())
			calls := 0
			fn := func(context.Context, api.ModelRequest) (api.ModelResponse, error) {
				calls++
				return api.ModelResponse{}, nil
			}
			var operational bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&operational, &slog.HandlerOptions{Level: slog.LevelDebug}))
			opts := []controller.Option{controller.WithModelPolicy(api.ModelPolicy{Default: "allowed", Allowed: []string{"allowed"}}), controller.WithLogger(logger)}
			if streaming {
				opts = append(opts, controller.WithStreamingModel(func(ctx context.Context, r api.ModelRequest, _ func(string)) (api.ModelResponse, error) {
					return fn(ctx, r)
				}))
			}
			c, err := controller.New(log, fn, opts...)
			if err != nil {
				t.Fatal(err)
			}
			var caught []error
			err = c.Exec(t.Context(), modelPolicyHarness{requests: []string{"forbidden", "allowed"}, caught: &caught}, nil, 0)
			if !errors.Is(err, api.ErrModelNotAllowed) {
				t.Fatalf("Exec = %v", err)
			}
			if !strings.Contains(operational.String(), `"error_kind":"model_policy_refused"`) || strings.Contains(operational.String(), `"error_kind":"harness_run_failed"`) {
				t.Fatalf("policy refusal log = %s", &operational)
			}
			if calls != 0 || len(caught) != 6 {
				t.Fatalf("provider calls/caught = %d/%v", calls, caught)
			}
			for _, err := range caught {
				if !errors.Is(err, api.ErrModelNotAllowed) {
					t.Fatalf("later effect = %v", err)
				}
			}
			recs, _ := log.Read(1)
			if len(recs) != 2 || recs[0].Event.Kind != api.EventExecutionStart || recs[1].Event.Kind != api.EventError {
				t.Fatalf("fatal refusal journal = %#v", recs)
			}
		})
	}
}

func TestModelPolicyResumeChecksOnlyLiveEffects(t *testing.T) {
	original := eventlog.AsStore(eventlog.New())
	calls := 0
	fn := func(context.Context, api.ModelRequest) (api.ModelResponse, error) {
		calls++
		return api.ModelResponse{Message: *api.TextMessage("assistant", "ok")}, nil
	}
	c, err := controller.New(original, fn, controller.WithModelPolicy(api.ModelPolicy{Default: "old", Allowed: []string{"old"}}))
	if err != nil {
		t.Fatal(err)
	}
	h := modelPolicyHarness{requests: []string{"", ""}}
	if err := c.Exec(t.Context(), h, nil, 0); err != nil {
		t.Fatal(err)
	}
	recs, _ := original.Read(1)
	for _, tc := range []struct {
		name    string
		cut     int
		wantErr bool
	}{{"recorded results", len(recs) - 1, false}, {"trailing intent", 4, true}, {"new live call", 3, true}} {
		t.Run(tc.name, func(t *testing.T) {
			log := eventlog.AsStore(eventlog.NewFrom(recs[:tc.cut]))
			before, _ := log.Head()
			var operational bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&operational, &slog.HandlerOptions{Level: slog.LevelDebug}))
			resume, err := controller.New(log, fn, controller.WithModelPolicy(api.ModelPolicy{Default: "new", Allowed: []string{"new"}}), controller.WithLogger(logger))
			if err != nil {
				t.Fatal(err)
			}
			var starts []string
			var caught []error
			run := h
			run.starts = &starts
			if tc.wantErr {
				run.caught = &caught
			}
			resumed, err := resume.Resume(t.Context(), run)
			if !resumed || (tc.wantErr && !errors.Is(err, api.ErrModelNotAllowed)) || (!tc.wantErr && err != nil) {
				t.Fatalf("Resume = %v,%v", resumed, err)
			}
			if calls != 2 || !reflect.DeepEqual(starts, []string{"old"}) {
				t.Fatalf("provider calls/starts = %d/%v", calls, starts)
			}
			if tc.wantErr {
				if !strings.Contains(operational.String(), `"error_kind":"model_policy_refused"`) {
					t.Fatalf("recovery refusal log = %s", &operational)
				}
				if len(caught) != 3 {
					t.Fatalf("caught model/output/usage errors = %v", caught)
				}
				for _, err := range caught {
					if !errors.Is(err, api.ErrModelNotAllowed) {
						t.Fatalf("later recovery effect = %v", err)
					}
				}
			}
			after, _ := log.Read(1)
			if len(after) != int(before)+1 {
				t.Fatalf("unexpected effects: %d -> %d", before, len(after))
			}
			want := api.EventEnd
			if tc.wantErr {
				want = api.EventError
			}
			if after[len(after)-1].Event.Kind != want {
				t.Fatalf("last event = %s", after[len(after)-1].Event.Kind)
			}
		})
	}
}

func TestPrePolicyJournalRemainsUnvalidatedOnResume(t *testing.T) {
	log := eventlog.AsStore(eventlog.New())
	c, err := controller.New(log, func(context.Context, api.ModelRequest) (api.ModelResponse, error) {
		return api.ModelResponse{Message: *api.TextMessage("assistant", "old")}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	h := modelPolicyHarness{requests: []string{"unlisted", ""}}
	if err := c.Exec(t.Context(), h, nil, 0); err != nil {
		t.Fatal(err)
	}
	recs, _ := log.Read(1)
	cut := eventlog.AsStore(eventlog.NewFrom(recs[:3]))
	calls := 0
	resume, err := controller.New(cut, func(_ context.Context, r api.ModelRequest) (api.ModelResponse, error) {
		calls++
		if r.Model != "" {
			t.Fatalf("old empty request changed to %q", r.Model)
		}
		return api.ModelResponse{}, nil
	}, controller.WithModelPolicy(api.ModelPolicy{Default: "new", Allowed: []string{"new"}}))
	if err != nil {
		t.Fatal(err)
	}
	var starts []string
	h.starts = &starts
	if _, err := resume.Resume(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !reflect.DeepEqual(starts, []string{""}) {
		t.Fatalf("old behavior = %d/%v", calls, starts)
	}
}
