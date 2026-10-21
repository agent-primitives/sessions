package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/agent-primitives/sessions/api"
)

// approvedArgsLog journals an approved, unexecuted gated call "c" whose arguments are args.
func approvedArgsLog(t *testing.T, args map[string]any) []api.Event {
	t.Helper()
	events := gateEvents()[:3]
	events[1].ToolCall.Args = args
	return events
}

// countingExecutor records every call it is asked to run.
func countingExecutor(calls *[]api.ToolCall) ToolFunc {
	return func(_ context.Context, _ ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
		*calls = append(*calls, call)
		return api.ToolResult{Output: map[string]any{"text": "done"}}, nil
	}
}

// An approval authorizes the journaled call's arguments, not whatever the harness re-emits on
// Resume: a re-emitted call with different arguments diverges before the executor is called, and
// no receipt is written.
func TestApprovalRefusesChangedArguments(t *testing.T) {
	for name, emitted := range map[string]map[string]any{
		"changed value": {"text": "evil"},
		"added key":     {"text": "hello", "to": "everyone"},
		"no arguments":  nil,
	} {
		t.Run(name, func(t *testing.T) {
			log := gateLog(t, approvedArgsLog(t, map[string]any{"text": "hello"})...)
			if _, err := Approve(log, api.ApprovalDecision{ExecutionID: "e", ToolCallID: "c", RequestSeq: 3, Approved: true}); err != nil {
				t.Fatal(err)
			}
			var calls []api.ToolCall
			c, err := New(log, nil, WithSessionUID("s"), WithToolExecutor(countingExecutor(&calls)))
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Resume(t.Context(), &approvalHarness{fn: func(ctx context.Context, _ *api.Start, s api.EventSink) error {
				call := *gateEvents()[1].ToolCall
				call.Args = emitted
				_, err := s.ToolCall(ctx, call)
				return err
			}})
			if !errors.Is(err, ErrReplayDiverged) {
				t.Fatalf("Resume with changed arguments = %v, want ErrReplayDiverged", err)
			}
			if len(calls) != 0 {
				t.Fatalf("executor ran a call that was not approved: %+v", calls)
			}
			if receipt := journalReceipt(t, log); receipt != nil {
				t.Fatalf("divergence wrote a receipt: %+v", receipt)
			}
		})
	}
}

// The executor receives a copy of the approved call only after its content_hash is recomputed and
// matches the journaled TOOL_CALL record the decision chains over. The in-memory store keeps the
// harness's argument map, so a harness that changes that map after the journal was verified changes
// the "recorded" call too, and the identity check alone compares the map with itself.
func TestApprovalDispatchRechecksApprovedHash(t *testing.T) {
	shared := map[string]any{"text": "hello", "nested": map[string]any{"to": []any{"a"}}}
	log := gateLog(t, approvedArgsLog(t, shared)...)
	if _, err := Approve(log, api.ApprovalDecision{ExecutionID: "e", ToolCallID: "c", RequestSeq: 3, Approved: true}); err != nil {
		t.Fatal(err)
	}
	before, err := log.Head()
	if err != nil {
		t.Fatal(err)
	}
	var calls []api.ToolCall
	c, err := New(log, nil, WithSessionUID("s"), WithToolExecutor(countingExecutor(&calls)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Resume(t.Context(), &approvalHarness{fn: func(ctx context.Context, _ *api.Start, s api.EventSink) error {
		shared["text"] = "evil"
		call := *gateEvents()[1].ToolCall
		call.Args = shared
		_, err := s.ToolCall(ctx, call)
		return err
	}})
	if !errors.Is(err, ErrJournalTampered) {
		t.Fatalf("Resume with arguments changed in memory = %v, want ErrJournalTampered", err)
	}
	if len(calls) != 0 {
		t.Fatalf("executor ran arguments that do not match the approved hash: %+v", calls)
	}
	if after, _ := log.Head(); after != before {
		t.Fatalf("refusal appended records: head %d -> %d, %v", before, after, journalKinds(t, log))
	}

	// Unchanged arguments run once, and the executor's copy does not share the journal's maps.
	shared["text"] = "hello"
	calls = nil
	c, err = New(log, nil, WithSessionUID("s"), WithToolExecutor(countingExecutor(&calls)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Resume(t.Context(), &approvalHarness{fn: func(ctx context.Context, _ *api.Start, s api.EventSink) error {
		call := *gateEvents()[1].ToolCall
		call.Args = shared
		_, err := s.ToolCall(ctx, call)
		return err
	}}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Args["text"] != "hello" {
		t.Fatalf("approved call ran %d times: %+v", len(calls), calls)
	}
	calls[0].Args["nested"].(map[string]any)["to"].([]any)[0] = "b"
	if got := shared["nested"].(map[string]any)["to"].([]any)[0]; got != "a" {
		t.Fatalf("executor copy shares the journaled arguments: %v", got)
	}
}

// A decision names one request; it cannot authorize another call. Call B is parked in a later
// execution after call A was approved and executed.
func TestApprovalForOneCallCannotAuthorizeAnother(t *testing.T) {
	n := int64(0)
	events := gateEvents()[:3]
	log := gateLog(t, events...)
	a := api.ApprovalDecision{ExecutionID: "e", ToolCallID: "c", RequestSeq: 3, Approved: true}
	decisionA, err := Approve(log, a)
	if err != nil {
		t.Fatal(err)
	}
	receipt := gateEvents()[4]
	receipt.Result.ApprovalDecisionSeq = decisionA.Seq
	fence, err := log.NewFence()
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range []api.Event{
		receipt,
		{ExecutionID: "e", Kind: api.EventEnd, End: &api.HarnessEnd{State: "COMPLETED"}},
		{ExecutionID: "f", Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{InputCount: &n}},
		{ExecutionID: "f", Kind: api.EventToolCall, ToolCall: &api.ToolCall{ID: "b", Tool: "write", IdempotencyKey: "kb", Mediation: api.MediationRequiresApproval}},
		{ExecutionID: "f", Kind: api.EventApprovalRequest, Approval: &api.ApprovalRequest{ToolCallID: "b"}},
	} {
		head, _ := log.Head()
		if _, err := log.Append(head, fence, ev); err != nil {
			t.Fatal(err)
		}
	}
	state, err := InspectApproval(log, 0)
	if err != nil || state.Call.ID != "b" || state.Decision != nil {
		t.Fatalf("selected=%+v %v", state, err)
	}
	head, _ := log.Head()
	for name, decision := range map[string]api.ApprovalDecision{
		"A's call and request in B's execution": {ExecutionID: "f", ToolCallID: "c", RequestSeq: 3, Approved: true},
		"A's call with B's request":             {ExecutionID: "f", ToolCallID: "c", RequestSeq: state.Request.Seq, Approved: true},
		"B's call with A's request":             {ExecutionID: "f", ToolCallID: "b", RequestSeq: 3, Approved: true},
		"B's call in A's execution":             {ExecutionID: "e", ToolCallID: "b", RequestSeq: state.Request.Seq, Approved: true},
	} {
		if _, err := Approve(log, decision); !errors.Is(err, ErrInvalidApprovalDecision) {
			t.Fatalf("%s: Approve = %v, want ErrInvalidApprovalDecision", name, err)
		}
	}
	// Retrying A's exact decision returns A's record and appends nothing for B.
	if retry, err := Approve(log, a); err != nil || retry.Seq != decisionA.Seq {
		t.Fatalf("retry of A = %+v, %v", retry, err)
	}
	if after, _ := log.Head(); after != head {
		t.Fatalf("invalid decisions appended: head %d -> %d", head, after)
	}
	var calls []api.ToolCall
	c, err := New(log, nil, WithSessionUID("s"), WithToolExecutor(countingExecutor(&calls)))
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := c.Resume(t.Context(), &approvalHarness{})
	if !resumed || !errors.Is(err, api.ErrApprovalParked) || len(calls) != 0 {
		t.Fatalf("Resume of B without its decision = %v, %v, calls %d; want parked, no call", resumed, err, len(calls))
	}
}
