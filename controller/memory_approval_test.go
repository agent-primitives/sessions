package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
)

// mtRun is one Run of an mtHarness.
type mtRun func(context.Context, *api.Start, api.EventSink) error

// mtHarness declares a resumability tier and plays one script per Run call.
type mtHarness struct {
	tier   api.Resumability
	runs   []mtRun
	starts []*api.Start
}

func (h *mtHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "agent", Capabilities: api.Capabilities{Resumability: h.tier}}, nil
}

func (h *mtHarness) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	n := len(h.starts)
	h.starts = append(h.starts, start)
	if n >= len(h.runs) {
		return errors.New("mtHarness: unexpected Run")
	}
	return h.runs[n](ctx, start, sink)
}

func mtMemory(runs ...mtRun) *mtHarness {
	return &mtHarness{tier: api.ResumabilityRequiresMemorySnapshot, runs: runs}
}

// mtGated is the approval-gated call the harness parks on.
func mtGated() api.ToolCall {
	return api.ToolCall{
		ID:             "toolu_016xkvdCMPcYf5KZ8T4AX7ab",
		Tool:           "mcp__room__create_issue",
		Args:           map[string]any{"title": "memory tier", "body": "approved write"},
		Mediation:      api.MediationRequiresApproval,
		IdempotencyKey: "toolu_016xkvdCMPcYf5KZ8T4AX7ab",
	}
}

// mtPrefix is what the turn emits before the gated call: text and a tool the harness ran itself.
func mtPrefix(ctx context.Context, sink api.EventSink) error {
	if err := sink.Output(ctx, "I'll file the issue."); err != nil {
		return err
	}
	return sink.Report(ctx, api.ToolResult{ID: "toolu_read", Content: api.TextMessage("tool", "README contents").Parts})
}

// mtGateFirst is a restored harness's continuation: the gated call, then live text.
func mtGateFirst(ctx context.Context, _ *api.Start, sink api.EventSink) error {
	if _, err := sink.ToolCall(ctx, mtGated()); err != nil {
		return err
	}
	return sink.Output(ctx, "Filed.")
}

func mtEvents(t *testing.T, log eventlog.Store) []api.Event {
	t.Helper()
	recs, err := log.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	events := make([]api.Event, 0, len(recs))
	for _, r := range recs {
		events = append(events, r.Event)
	}
	return events
}

func mtKinds(events []api.Event) []api.EventKind {
	kinds := make([]api.EventKind, 0, len(events))
	for _, ev := range events {
		kinds = append(kinds, ev.Kind)
	}
	return kinds
}

func mtHead(t *testing.T, log eventlog.Store) int64 {
	t.Helper()
	head, err := log.Head()
	if err != nil {
		t.Fatal(err)
	}
	return head
}

func mtController(t *testing.T, log eventlog.Store, opts ...Option) *Controller {
	t.Helper()
	c, err := New(log, nil, append([]Option{WithSessionUID("s")}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// mtExecutor counts executor calls and answers each with a fixed result.
func mtExecutor(calls *int) Option {
	return WithToolExecutor(func(context.Context, ToolCallContext, api.ToolCall) (api.ToolResult, error) {
		*calls++
		return api.ToolResult{Content: api.TextMessage("tool", "issue 7").Parts}, nil
	})
}

func mtNoExecutor(t *testing.T) Option {
	return WithToolExecutor(func(context.Context, ToolCallContext, api.ToolCall) (api.ToolResult, error) {
		t.Error("executor called")
		return api.ToolResult{}, nil
	})
}

// mtParkUndecided runs a memory-tier turn that emits the prefix and parks on the gated call.
func mtParkUndecided(t *testing.T) (eventlog.Store, *ApprovalState) {
	t.Helper()
	log := eventlog.AsStore(eventlog.New())
	h := mtMemory(func(ctx context.Context, _ *api.Start, sink api.EventSink) error {
		if err := mtPrefix(ctx, sink); err != nil {
			return err
		}
		_, err := sink.ToolCall(ctx, mtGated())
		return err
	})
	if err := mtController(t, log).Exec(t.Context(), h, []api.Message{*api.TextMessage("user", "file an issue")}, 0); !errors.Is(err, api.ErrApprovalParked) {
		t.Fatalf("Exec = %v, want park", err)
	}
	state, err := InspectApproval(log, 0)
	if err != nil || state == nil || state.Request == nil || state.Decision != nil {
		t.Fatalf("InspectApproval = %+v, %v", state, err)
	}
	return log, state
}

func mtDecide(t *testing.T, log eventlog.Store, state *ApprovalState, approved bool) {
	t.Helper()
	if _, err := Approve(log, api.ApprovalDecision{ExecutionID: state.ExecutionID, ToolCallID: state.Call.ID, RequestSeq: state.Request.Seq, Approved: approved}); err != nil {
		t.Fatal(err)
	}
}

// mtPark parks a memory-tier turn after its prefix and records the decision.
func mtPark(t *testing.T, approved bool) eventlog.Store {
	t.Helper()
	log, state := mtParkUndecided(t)
	mtDecide(t, log, state, approved)
	return log
}

// mtAssertOpenGate asserts the gate is still decided without a receipt.
func mtAssertOpenGate(t *testing.T, log eventlog.Store) {
	t.Helper()
	state, err := InspectApproval(log, 0)
	if err != nil || state == nil || state.Decision == nil || state.Receipt != nil {
		t.Fatalf("state=%+v err=%v, want a decided gate without receipt", state, err)
	}
}

func TestMemoryTierApprovalTierAdmission(t *testing.T) {
	for _, tier := range []api.Resumability{api.ResumabilityStatelessReplay, api.ResumabilityRequiresMemorySnapshot} {
		t.Run(string(tier)+"/parks", func(t *testing.T) {
			log := eventlog.AsStore(eventlog.New())
			h := &mtHarness{tier: tier, runs: []mtRun{func(ctx context.Context, _ *api.Start, sink api.EventSink) error {
				_, err := sink.ToolCall(ctx, mtGated())
				return err
			}}}
			if err := mtController(t, log).Exec(t.Context(), h, nil, 0); !errors.Is(err, api.ErrApprovalParked) {
				t.Fatalf("Exec = %v, want park", err)
			}
			if got := mtKinds(mtEvents(t, log)); got[len(got)-2] != api.EventToolCall || got[len(got)-1] != api.EventApprovalRequest {
				t.Fatalf("journal %v, want TOOL_CALL, APPROVAL_REQUEST at the tail", got)
			}
		})
	}
	for _, tier := range []api.Resumability{"", "RESUMABILITY_FUTURE"} {
		t.Run("refused/"+string(tier)+"/live", func(t *testing.T) {
			log := eventlog.AsStore(eventlog.New())
			h := &mtHarness{tier: tier, runs: []mtRun{func(ctx context.Context, _ *api.Start, sink api.EventSink) error {
				_, err := sink.ToolCall(ctx, mtGated())
				return err
			}}}
			if err := mtController(t, log).Exec(t.Context(), h, nil, 0); !errors.Is(err, ErrApprovalUnavailable) {
				t.Fatalf("Exec = %v, want ErrApprovalUnavailable", err)
			}
			for _, ev := range mtEvents(t, log) {
				if ev.Kind == api.EventToolCall || ev.Kind == api.EventApprovalRequest {
					t.Fatalf("refused gate recorded %s", ev.Kind)
				}
			}
		})
		t.Run("refused/"+string(tier)+"/recovery", func(t *testing.T) {
			log := mtPark(t, true)
			head := mtHead(t, log)
			h := &mtHarness{tier: tier}
			if _, err := mtController(t, log, mtNoExecutor(t)).Resume(t.Context(), h); !errors.Is(err, ErrApprovalUnavailable) {
				t.Fatalf("Resume = %v, want ErrApprovalUnavailable", err)
			}
			if len(h.starts) != 0 || mtHead(t, log) != head {
				t.Fatalf("runs=%d head=%d, want 0, %d", len(h.starts), mtHead(t, log), head)
			}
		})
	}
}

func TestMemoryTierApprovalContinuation(t *testing.T) {
	t.Run("approved call is host executed once", func(t *testing.T) {
		log := mtPark(t, true)
		head := mtHead(t, log)
		calls := 0
		h := mtMemory(mtGateFirst)
		var result api.ToolResult
		h.runs[0] = func(ctx context.Context, _ *api.Start, sink api.EventSink) error {
			var err error
			if result, err = sink.ToolCall(ctx, mtGated()); err != nil {
				return err
			}
			return sink.Output(ctx, "Filed.")
		}
		if resumed, err := mtController(t, log, mtExecutor(&calls)).Resume(t.Context(), h); !resumed || err != nil {
			t.Fatalf("Resume = %v, %v", resumed, err)
		}
		if calls != 1 || result.Approval == nil || !result.Approval.Approved || (&api.Message{Parts: result.Content}).Text() != "issue 7" {
			t.Fatalf("calls=%d result=%+v, want one host execution and the approved receipt", calls, result)
		}
		events := mtEvents(t, log)
		if got := mtKinds(events[head:]); len(got) != 3 || got[0] != api.EventToolResult || got[1] != api.EventOutput || got[2] != api.EventEnd {
			t.Fatalf("appended %v, want TOOL_RESULT, OUTPUT, END", got)
		}
		if state, err := InspectApproval(log, 0); err != nil || state == nil || state.Receipt == nil || !state.Completed {
			t.Fatalf("state=%+v err=%v, want a receipted, completed gate", state, err)
		}
	})
	t.Run("denied call returns the denial receipt without dispatch", func(t *testing.T) {
		log := mtPark(t, false)
		head := mtHead(t, log)
		var result api.ToolResult
		h := mtMemory(func(ctx context.Context, _ *api.Start, sink api.EventSink) error {
			var err error
			result, err = sink.ToolCall(ctx, mtGated())
			return err
		})
		if _, err := mtController(t, log, mtNoExecutor(t)).Resume(t.Context(), h); err != nil {
			t.Fatalf("Resume = %v", err)
		}
		if !result.IsError || result.Code != api.ToolResultCodeApprovalDenied || result.Approval == nil || result.Approval.Approved {
			t.Fatalf("result=%+v, want the APPROVAL_DENIED receipt", result)
		}
		if got := mtKinds(mtEvents(t, log)[head:]); len(got) != 2 || got[0] != api.EventToolResult || got[1] != api.EventEnd {
			t.Fatalf("appended %v, want TOOL_RESULT, END", got)
		}
	})
	t.Run("stateless replay still serves the prefix first", func(t *testing.T) {
		log := mtPark(t, true)
		gateOnly := &mtHarness{tier: api.ResumabilityStatelessReplay, runs: []mtRun{mtGateFirst}}
		if _, err := mtController(t, log, mtNoExecutor(t)).Resume(t.Context(), gateOnly); err == nil || errors.Is(err, ErrReplayDiverged) {
			t.Fatalf("Resume = %v, want the historical pre-intent refusal", err)
		}
		mtAssertOpenGate(t, log)
		calls := 0
		full := &mtHarness{tier: api.ResumabilityStatelessReplay, runs: []mtRun{func(ctx context.Context, start *api.Start, sink api.EventSink) error {
			if err := mtPrefix(ctx, sink); err != nil {
				return err
			}
			return mtGateFirst(ctx, start, sink)
		}}}
		if _, err := mtController(t, log, mtExecutor(&calls)).Resume(t.Context(), full); err != nil || calls != 1 {
			t.Fatalf("Resume = %v calls=%d, want full-prefix recovery", err, calls)
		}
	})
	t.Run("a recovery that fails before any sink call is retryable", func(t *testing.T) {
		log := mtPark(t, true)
		unavailable := errors.New("gateway not connected")
		calls := 0
		h := mtMemory(func(context.Context, *api.Start, api.EventSink) error { return unavailable }, mtGateFirst)
		if _, err := mtController(t, log, mtExecutor(&calls)).Resume(t.Context(), h); !errors.Is(err, unavailable) {
			t.Fatalf("Resume = %v, want %v", err, unavailable)
		}
		mtAssertOpenGate(t, log)
		if _, err := mtController(t, log, mtExecutor(&calls)).Resume(t.Context(), h); err != nil || calls != 1 {
			t.Fatalf("Resume = %v calls=%d, want one host execution", err, calls)
		}
	})
	t.Run("replay of a completed memory-tier gate serves the recorded stream", func(t *testing.T) {
		log := gateLog(t, gateEvents()...)
		h := mtMemory(func(ctx context.Context, _ *api.Start, sink api.EventSink) error {
			result, err := sink.ToolCall(ctx, *gateEvents()[1].ToolCall)
			if err == nil && (result.Approval == nil || !result.Approval.Approved) {
				err = errors.New("replayed receipt without its decision")
			}
			return err
		})
		if _, err := mtController(t, log, mtNoExecutor(t)).Replay(t.Context(), h); err != nil {
			t.Fatalf("Replay = %v", err)
		}
	})
}

func TestMemoryTierApprovalWrongFirstCall(t *testing.T) {
	differentArgs := mtGated()
	differentArgs.Args = map[string]any{"title": "memory tier", "body": "a different write"}
	ordinary := mtGated()
	ordinary.Mediation = api.MediationControllerMediated
	otherID := mtGated()
	otherID.ID, otherID.IdempotencyKey = "toolu_other", "toolu_other"
	keyless := api.ToolCall{ID: "toolu_other", Tool: "write", Mediation: api.MediationControllerMediated}
	toolCall := func(call api.ToolCall) func(context.Context, api.EventSink) error {
		return func(ctx context.Context, sink api.EventSink) error {
			_, err := sink.ToolCall(ctx, call)
			return err
		}
	}
	for _, tc := range []struct {
		name  string
		first func(context.Context, api.EventSink) error
	}{
		{"output", func(ctx context.Context, sink api.EventSink) error {
			return sink.Output(ctx, "I could not file the issue.")
		}},
		{"report", func(ctx context.Context, sink api.EventSink) error {
			return sink.Report(ctx, api.ToolResult{ID: mtGated().ID, Content: api.TextMessage("tool", "issue 7").Parts})
		}},
		{"report with receipt fields", func(ctx context.Context, sink api.EventSink) error {
			return sink.Report(ctx, api.ToolResult{ID: "toolu_x", Code: api.ToolResultCodeApprovalDenied})
		}},
		{"model", func(ctx context.Context, sink api.EventSink) error {
			_, err := sink.Model(ctx, api.ModelRequest{})
			return err
		}},
		{"usage", func(ctx context.Context, sink api.EventSink) error { return sink.Usage(ctx, api.Usage{InputTokens: 1}) }},
		{"tool call with a different id", toolCall(otherID)},
		{"tool call with different args", toolCall(differentArgs)},
		{"tool call with different mediation", toolCall(ordinary)},
		{"keyless tool call", toolCall(keyless)},
		{"no call", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := mtPark(t, true)
			head := mtHead(t, log)
			var firstErr, laterErr error
			h := mtMemory(func(ctx context.Context, _ *api.Start, sink api.EventSink) error {
				if tc.first == nil {
					return nil
				}
				firstErr = tc.first(ctx, sink)
				// Divergence is sticky (EXE-14): the gated call cannot follow a wrong first call.
				_, laterErr = sink.ToolCall(ctx, mtGated())
				return nil // a harness that handles errors cannot finish the turn either
			}, mtGateFirst)
			calls := 0
			resumed, err := mtController(t, log, mtExecutor(&calls)).Resume(t.Context(), h)
			if !resumed || !errors.Is(err, ErrReplayDiverged) {
				t.Fatalf("Resume = %v, %v, want ErrReplayDiverged", resumed, err)
			}
			if tc.first != nil && (!errors.Is(firstErr, ErrReplayDiverged) || !errors.Is(laterErr, ErrReplayDiverged)) {
				t.Fatalf("first=%v later=%v, want sticky ErrReplayDiverged", firstErr, laterErr)
			}
			if got := mtKinds(mtEvents(t, log)[head:]); len(got) != 1 || got[0] != api.EventError {
				t.Fatalf("appended %v, want one ERROR", got)
			}
			if calls != 0 {
				t.Fatalf("executor calls=%d, want 0", calls)
			}
			mtAssertOpenGate(t, log)
			// ERROR does not end the execution (EXE-6): a compliant restore still completes it.
			if _, err := mtController(t, log, mtExecutor(&calls)).Resume(t.Context(), h); err != nil || calls != 1 {
				t.Fatalf("compliant Resume = %v calls=%d", err, calls)
			}
		})
	}
}

// mtParkTwice parks twice in one execution: an ordinary call and gate g1 precede gate g2, so a
// continuation at g2 skips a prefix holding both call IDs.
func mtParkTwice(t *testing.T) (eventlog.Store, *int) {
	t.Helper()
	g1, g2 := mtGated(), mtGated()
	g1.ID, g1.IdempotencyKey = "g1", "g1"
	g2.ID, g2.IdempotencyKey = "g2", "g2"
	log := eventlog.AsStore(eventlog.New())
	calls := new(int)
	h := mtMemory(
		func(ctx context.Context, _ *api.Start, sink api.EventSink) error {
			if _, err := sink.ToolCall(ctx, api.ToolCall{ID: "ord", Tool: "read", Mediation: api.MediationControllerMediated, IdempotencyKey: "ord"}); err != nil {
				return err
			}
			_, err := sink.ToolCall(ctx, g1)
			return err
		},
		func(ctx context.Context, _ *api.Start, sink api.EventSink) error {
			if _, err := sink.ToolCall(ctx, g1); err != nil {
				return err
			}
			_, err := sink.ToolCall(ctx, g2)
			return err
		},
	)
	if err := mtController(t, log, mtExecutor(calls)).Exec(t.Context(), h, nil, 0); !errors.Is(err, api.ErrApprovalParked) {
		t.Fatalf("Exec = %v, want park", err)
	}
	state, err := InspectApproval(log, 0)
	if err != nil || state == nil || state.Call.ID != "g1" {
		t.Fatalf("first gate=%+v %v", state, err)
	}
	mtDecide(t, log, state, true)
	if _, err := mtController(t, log, mtExecutor(calls)).Resume(t.Context(), h); !errors.Is(err, api.ErrApprovalParked) {
		t.Fatalf("Resume = %v, want the second park", err)
	}
	state, err = InspectApproval(log, 0)
	if err != nil || state == nil || state.Call.ID != "g2" || state.Decision != nil {
		t.Fatalf("second gate=%+v %v", state, err)
	}
	mtDecide(t, log, state, true)
	if *calls != 2 {
		t.Fatalf("executor calls=%d, want ord and g1", *calls)
	}
	return log, calls
}

func TestMemoryTierApprovalSecondParkSelectsLatestGate(t *testing.T) {
	log, calls := mtParkTwice(t)
	g1 := mtGated()
	g1.ID, g1.IdempotencyKey = "g1", "g1"
	head := mtHead(t, log)
	stale := mtMemory(func(ctx context.Context, _ *api.Start, sink api.EventSink) error {
		_, err := sink.ToolCall(ctx, g1)
		return err
	})
	if _, err := mtController(t, log, mtExecutor(calls)).Resume(t.Context(), stale); !errors.Is(err, ErrReplayDiverged) {
		t.Fatalf("Resume with the earlier gate first = %v, want ErrReplayDiverged", err)
	}
	if got := mtKinds(mtEvents(t, log)[head:]); len(got) != 1 || got[0] != api.EventError {
		t.Fatalf("appended %v, want one ERROR", got)
	}
	g2 := mtGated()
	g2.ID, g2.IdempotencyKey = "g2", "g2"
	h := mtMemory(func(ctx context.Context, _ *api.Start, sink api.EventSink) error {
		_, err := sink.ToolCall(ctx, g2)
		return err
	})
	if _, err := mtController(t, log, mtExecutor(calls)).Resume(t.Context(), h); err != nil || *calls != 3 {
		t.Fatalf("Resume = %v calls=%d", err, *calls)
	}
}

// Continuation skips the prefix, so the call IDs serving it would have recorded are seeded:
// reusing one is refused before any append, exactly as after a full replay.
func TestMemoryTierApprovalContinuationKeepsCallIDs(t *testing.T) {
	gated := func(id string) api.ToolCall {
		call := mtGated()
		call.ID, call.IdempotencyKey = id, "new-"+id
		return call
	}
	for _, tc := range []struct {
		name string
		next func(context.Context, api.EventSink) error
		want error
	}{
		{"gated call reusing a prefix gate id", func(ctx context.Context, sink api.EventSink) error {
			_, err := sink.ToolCall(ctx, gated("g1"))
			return err
		}, ErrInvalidApprovalCall},
		{"ordinary call reusing a prefix gate id", func(ctx context.Context, sink api.EventSink) error {
			_, err := sink.ToolCall(ctx, api.ToolCall{ID: "g1", Tool: "read", Mediation: api.MediationControllerMediated, IdempotencyKey: "new"})
			return err
		}, ErrInvalidApprovalCall},
		{"gated call reusing a prefix ordinary id", func(ctx context.Context, sink api.EventSink) error {
			_, err := sink.ToolCall(ctx, gated("ord"))
			return err
		}, ErrInvalidApprovalCall},
		{"report for a prefix gate id", func(ctx context.Context, sink api.EventSink) error {
			return sink.Report(ctx, api.ToolResult{ID: "g1"})
		}, ErrApprovalReceiptReport},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, calls := mtParkTwice(t)
			g2 := mtGated()
			g2.ID, g2.IdempotencyKey = "g2", "g2"
			head := mtHead(t, log)
			var nextErr error
			h := mtMemory(func(ctx context.Context, _ *api.Start, sink api.EventSink) error {
				if _, err := sink.ToolCall(ctx, g2); err != nil {
					return err
				}
				nextErr = tc.next(ctx, sink)
				return nil
			})
			_, err := mtController(t, log, mtExecutor(calls)).Resume(t.Context(), h)
			if !errors.Is(nextErr, tc.want) {
				t.Fatalf("reuse = %v, want %v", nextErr, tc.want)
			}
			// The g2 receipt commits before the refusal; nothing follows for the refused effect.
			want := []api.EventKind{api.EventToolResult, api.EventEnd}
			if tc.want == ErrApprovalReceiptReport {
				// A gated-ID report stops the turn (TOOL-3); no ERROR or END follows the receipt.
				want = want[:1]
				if !errors.Is(err, ErrApprovalReceiptReport) {
					t.Fatalf("Resume = %v, want ErrApprovalReceiptReport", err)
				}
			} else if err != nil {
				t.Fatalf("Resume = %v", err)
			}
			got := mtKinds(mtEvents(t, log)[head:])
			if len(got) != len(want) || got[0] != want[0] || got[len(got)-1] != want[len(want)-1] {
				t.Fatalf("appended %v, want %v", got, want)
			}
			if state, err := InspectApproval(log, 0); err != nil || state == nil || state.Call.ID != "g2" || state.Receipt == nil {
				t.Fatalf("state=%+v err=%v, want the receipted g2 gate", state, err)
			}
		})
	}
}

func TestMemoryTierApprovalGateMustBeStreamTail(t *testing.T) {
	stray := api.Event{ExecutionID: "e", Kind: api.EventOutput, Message: api.TextMessage("assistant", "past the gate")}
	t.Run("resume", func(t *testing.T) {
		log := gateLog(t, append(gateEvents()[:4], stray)...)
		head := mtHead(t, log)
		h := mtMemory()
		if _, err := mtController(t, log, mtNoExecutor(t)).Resume(t.Context(), h); !errors.Is(err, ErrInvalidExecutionLog) {
			t.Fatalf("Resume = %v, want ErrInvalidExecutionLog", err)
		}
		if len(h.starts) != 0 || mtHead(t, log) != head {
			t.Fatalf("runs=%d head=%d, want 0, %d", len(h.starts), mtHead(t, log), head)
		}
	})
	// The approval scan refuses a continuation past an open gate first; continueAtGate checks the
	// tail itself so it never serves an effect recorded after a gate it completes live.
	decided := &ApprovalState{ExecutionID: "e", Call: gateEvents()[1].ToolCall, Decision: &eventlog.Record{}}
	call, request, result := gateEvents()[1], gateEvents()[2], gateEvents()[3]
	for _, tc := range []struct {
		name   string
		stream []api.Event
	}{
		{"stray effect after the gate", []api.Event{call, request, result, stray}},
		{"missing request", []api.Event{call, result}},
		{"missing decision", []api.Event{call, request}},
		{"no gate call", []api.Event{stray, request, result}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &resumeSink{live: liveSink{calls: make(map[string]bool)}, stream: tc.stream}
			if err := s.continueAtGate(decided, "e"); !errors.Is(err, ErrInvalidExecutionLog) || s.awaitingGate || s.i != 0 {
				t.Fatalf("continueAtGate = %v awaiting=%v i=%d, want ErrInvalidExecutionLog and no position", err, s.awaitingGate, s.i)
			}
		})
	}
}

// A refused park can leave a call-only cut. Resume repairs it into a gate and parks without a
// suspend at the gate, so the restored harness may already be past the call: after Approve its
// first call is not the gated call, and recovery fails closed with the gate unreceipted.
func TestMemoryTierApprovalRepairedCallOnlyCut(t *testing.T) {
	log := &mtRequestFault{Store: eventlog.AsStore(eventlog.New()), fail: true}
	h := mtMemory(
		func(ctx context.Context, _ *api.Start, sink api.EventSink) error {
			if _, err := sink.ToolCall(ctx, mtGated()); err != nil {
				return sink.Output(ctx, "I could not file the issue.")
			}
			return nil
		},
		func(ctx context.Context, _ *api.Start, sink api.EventSink) error {
			return sink.Output(ctx, "I could not file the issue.")
		},
	)
	if err := mtController(t, log).Exec(t.Context(), h, nil, 0); err == nil || errors.Is(err, api.ErrApprovalParked) {
		t.Fatalf("Exec = %v, want a refused park", err)
	}
	if kinds := mtKinds(mtEvents(t, log)); kinds[len(kinds)-1] != api.EventToolCall {
		t.Fatalf("journal ends with %s, want a call-only cut", kinds[len(kinds)-1])
	}
	log.fail = false
	if _, err := mtController(t, log).Resume(t.Context(), h); !errors.Is(err, api.ErrApprovalParked) || len(h.starts) != 1 {
		t.Fatalf("Resume = %v runs=%d, want a repaired park without Run", err, len(h.starts))
	}
	state, err := InspectApproval(log, 0)
	if err != nil || state == nil || state.Request == nil || state.Decision != nil {
		t.Fatalf("state=%+v err=%v, want a repaired undecided gate", state, err)
	}
	mtDecide(t, log, state, true)
	if _, err := mtController(t, log, mtNoExecutor(t)).Resume(t.Context(), h); !errors.Is(err, ErrReplayDiverged) {
		t.Fatalf("Resume = %v, want ErrReplayDiverged", err)
	}
	mtAssertOpenGate(t, log)
}

// mtRequestFault fails APPROVAL_REQUEST appends while fail is set.
type mtRequestFault struct {
	eventlog.Store
	fail bool
}

func (s *mtRequestFault) Append(head, fence int64, ev api.Event) (eventlog.Record, error) {
	if s.fail && ev.Kind == api.EventApprovalRequest {
		return eventlog.Record{}, errors.New("request append lost")
	}
	return s.Store.Append(head, fence, ev)
}

// APR-15: a forked child does not own an inherited memory-tier gate, decided or not.
func TestMemoryTierApprovalInheritedGateRefused(t *testing.T) {
	for _, cut := range []int{3, 4} {
		parent := gateLog(t, gateEvents()[:cut]...)
		child := gateLog(t)
		if err := Fork(parent, child, int64(cut)); err != nil {
			t.Fatal(err)
		}
		head := mtHead(t, child)
		h := mtMemory()
		if _, err := mtController(t, child, mtNoExecutor(t)).Resume(t.Context(), h); !errors.Is(err, ErrInheritedToolIntent) {
			t.Fatalf("cut %d: Resume = %v, want ErrInheritedToolIntent", cut, err)
		}
		if len(h.starts) != 0 || mtHead(t, child) != head {
			t.Fatalf("cut %d: runs=%d head=%d, want 0, %d", cut, len(h.starts), mtHead(t, child), head)
		}
	}
}
