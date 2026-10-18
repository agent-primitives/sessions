package controller_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
)

// dedupAll declares durable dedup for every tool, keeping the Resume re-drive that tests written
// before WithDurableDedup exercise.
var dedupAll = controller.WithDurableDedup(func(string) bool { return true })

// receiptCrashStore simulates a host that dies after the executor applied a call's effect but
// before its TOOL_RESULT became durable: that append and every later one fail, so the journal ends
// at the TOOL_CALL, without even the best-effort ERROR.
type receiptCrashStore struct {
	eventlog.Store
	callID  string
	crashed bool
}

func (s *receiptCrashStore) Append(expectedLastSeq, fence int64, ev api.Event) (eventlog.Record, error) {
	if !s.crashed && ev.Kind == api.EventToolResult && ev.Result != nil && ev.Result.ID == s.callID {
		s.crashed = true
	}
	if s.crashed {
		return eventlog.Record{}, errCrashed
	}
	return s.Store.Append(expectedLastSeq, fence, ev)
}

// countingCallHarness counts how often the controller reaches the harness.
type countingCallHarness struct {
	callHarness
	describes, runs int
}

func (h *countingCallHarness) Describe(ctx context.Context) (api.Descriptor, error) {
	h.describes++
	return h.callHarness.Describe(ctx)
}

func (h *countingCallHarness) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	h.runs++
	return h.callHarness.Run(ctx, start, sink)
}

func dedupCalls() []api.ToolCall {
	return []api.ToolCall{
		{ID: "call-lookup", Tool: "lookup", Mediation: api.MediationControllerMediated, IdempotencyKey: "key-lookup"},
		{ID: "call-send", Tool: "send", Mediation: api.MediationControllerMediated, IdempotencyKey: "key-send"},
	}
}

// crashBetweenCallAndResult runs a turn that calls lookup then send, with no durable dedup
// declaration, and crashes after crashCallID's executor ran but before its result was journaled.
// The executor applied that effect once; the journal ends with its unanswered TOOL_CALL.
func crashBetweenCallAndResult(t *testing.T, crashCallID string, effects map[string]int) eventlog.Store {
	t.Helper()
	log := memStore(t)
	executor := func(_ context.Context, _ controller.ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
		effects[call.Tool]++
		return api.ToolResult{Output: map[string]any{"text": call.Tool}}, nil
	}
	crash := &receiptCrashStore{Store: log, callID: crashCallID}
	c, err := controller.New(crash, echoModel, controller.WithSessionUID("s"), controller.WithToolExecutor(executor))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Exec(t.Context(), &callHarness{calls: dedupCalls()}, []api.Message{msg("go")}, 0); !errors.Is(err, errCrashed) {
		t.Fatalf("Exec = %v, want the simulated crash", err)
	}
	recs, err := log.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	last := recs[len(recs)-1].Event
	if last.Kind != api.EventToolCall || last.ToolCall.ID != crashCallID {
		t.Fatalf("journal ends with %s, want the unanswered TOOL_CALL %q", last.Kind, crashCallID)
	}
	return log
}

// Resume after a crash between TOOL_CALL and TOOL_RESULT re-runs the call only when the executor
// declares durable dedup for that tool. Otherwise it refuses before Describe, Run or any append,
// and the untouched journal can still be recovered later by a declaring executor. A first attempt
// of an undeclared tool, including one Resume reaches after a re-drive, is unaffected.
func TestResumeRedrivesTrailingToolCallOnlyWhenDeclared(t *testing.T) {
	declare := func(tools ...string) []controller.Option {
		return []controller.Option{controller.WithDurableDedup(func(tool string) bool {
			for _, declared := range tools {
				if tool == declared {
					return true
				}
			}
			return false
		})}
	}
	for _, tc := range []struct {
		name        string
		crashCallID string
		declaration []controller.Option
		refused     bool
		wantEffects map[string]int // after Resume, including the original attempt
	}{
		{"declared", "call-send", []controller.Option{dedupAll}, false, map[string]int{"lookup": 1, "send": 2}},
		{"undeclared", "call-send", nil, true, map[string]int{"lookup": 1, "send": 1}},
		{"false", "call-send", []controller.Option{controller.WithDurableDedup(func(string) bool { return false })}, true, map[string]int{"lookup": 1, "send": 1}},
		{"nil predicate", "call-send", []controller.Option{controller.WithDurableDedup(nil)}, true, map[string]int{"lookup": 1, "send": 1}},
		{"mixed, undeclared tool trailing", "call-send", declare("lookup"), true, map[string]int{"lookup": 1, "send": 1}},
		{"mixed, declared tool trailing", "call-lookup", declare("lookup"), false, map[string]int{"lookup": 2, "send": 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			effects := map[string]int{}
			log := crashBetweenCallAndResult(t, tc.crashCallID, effects)
			before, err := log.Read(1)
			if err != nil {
				t.Fatal(err)
			}
			var redriven []controller.ToolCallContext
			executor := func(_ context.Context, scope controller.ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
				effects[call.Tool]++
				redriven = append(redriven, scope)
				return api.ToolResult{Output: map[string]any{"text": call.Tool}}, nil
			}
			opts := append([]controller.Option{controller.WithSessionUID("s"), controller.WithToolExecutor(executor)}, tc.declaration...)
			c, err := controller.New(log, echoModel, opts...)
			if err != nil {
				t.Fatal(err)
			}
			h := &countingCallHarness{callHarness: callHarness{calls: dedupCalls()}}
			resumed, err := c.Resume(t.Context(), h)
			if !reflect.DeepEqual(effects, tc.wantEffects) {
				t.Fatalf("executor effects = %v, want %v", effects, tc.wantEffects)
			}
			after, readErr := log.Read(1)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tc.refused {
				if resumed || !errors.Is(err, controller.ErrUnansweredToolCall) {
					t.Fatalf("Resume = %v, %v; want refusal with ErrUnansweredToolCall", resumed, err)
				}
				const want = "tool call has no recorded result; repeating it could repeat the action"
				if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), `"send"`) {
					t.Fatalf("refusal %q must state %q and name the tool", err, want)
				}
				if h.describes != 0 || h.runs != 0 {
					t.Fatalf("refusal reached the harness: describes=%d runs=%d", h.describes, h.runs)
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatalf("refusal changed the journal: %d records before, %d after", len(before), len(after))
				}
				// The refusal leaves the intent recoverable: declaring the tool re-drives it once.
				declared, err := controller.New(log, echoModel, controller.WithSessionUID("s"), controller.WithToolExecutor(executor), dedupAll)
				if err != nil {
					t.Fatal(err)
				}
				if resumed, err := declared.Resume(t.Context(), &callHarness{calls: dedupCalls()}); !resumed || err != nil {
					t.Fatalf("declared Resume after refusal = %v, %v", resumed, err)
				}
				if effects["send"] != 2 || effects["lookup"] != 1 {
					t.Fatalf("declared Resume effects = %v, want send re-driven once and lookup served", effects)
				}
				return
			}
			if !resumed || err != nil {
				t.Fatalf("Resume = %v, %v; want a completed re-drive", resumed, err)
			}
			for _, scope := range redriven {
				if scope.SessionUID != "s" {
					t.Fatalf("re-drive scope = %+v, want the recorded session", scope)
				}
			}
			if got := after[len(after)-1].Event; got.Kind != api.EventEnd {
				t.Fatalf("journal ends with %s, want END", got.Kind)
			}
			if err := log.Verify(); err != nil {
				t.Fatal(err)
			}
			if resumed, err := c.Resume(t.Context(), h); resumed || err != nil {
				t.Fatalf("completed Resume = %v, %v; want a no-op", resumed, err)
			}
		})
	}
}

// Without a tool executor there is no action to repeat, so Resume keeps its previous behavior: the
// re-issued call fails as on the live path, not with ErrUnansweredToolCall.
func TestResumeWithoutToolExecutorDoesNotRefuseTrailingToolCall(t *testing.T) {
	log := crashBetweenCallAndResult(t, "call-send", map[string]int{})
	c, err := controller.New(log, echoModel, controller.WithSessionUID("s"))
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := c.Resume(t.Context(), &callHarness{calls: dedupCalls()})
	if !resumed || err == nil || errors.Is(err, controller.ErrUnansweredToolCall) || !strings.Contains(err.Error(), "no tool executor configured") {
		t.Fatalf("Resume = %v, %v; want the live missing-executor failure", resumed, err)
	}
}
