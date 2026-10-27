package controller_test

import (
	"context"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/wire"
)

var (
	historyApproverA = api.CallerIdentity{Issuer: "https://issuer.example", Subject: "approver-a"}
	historyApproverB = api.CallerIdentity{Issuer: "https://issuer.example", Subject: "approver-b"}
)

func historyGatedCall(id string) api.ToolCall {
	return api.ToolCall{ID: id, Tool: "write", IdempotencyKey: "k-" + id, Mediation: api.MediationRequiresApproval}
}

// historyHarness re-emits each recorded execution's gated call and records every Start.History,
// and, as in-process harness code can, Start.History resliced to its capacity.
type historyHarness struct {
	calls      map[string]string // execution ID -> gated call ID
	histories  [][]api.Event
	capacities [][]api.Event
}

func (*historyHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "history", Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}}, nil
}

func (h *historyHarness) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	h.histories = append(h.histories, start.History)
	h.capacities = append(h.capacities, start.History[:cap(start.History)])
	if id, ok := h.calls[start.ExecutionID]; ok {
		if _, err := sink.ToolCall(ctx, historyGatedCall(id)); err != nil {
			return err
		}
	}
	return nil
}

// A journaled decision's verified approver stays in the host's journal: Resume, a later Exec and
// Replay send earlier decisions to the harness in Start.History without it, in process and over
// harnesswire, and the journal keeps both approvers and still verifies.
func TestStartHistoryOmitsVerifiedApprover(t *testing.T) {
	for name, wrap := range map[string]func(*testing.T, api.Harness) api.Harness{
		"in process":  func(_ *testing.T, h api.Harness) api.Harness { return h },
		"harnesswire": legacyWireHarness,
	} {
		t.Run(name, func(t *testing.T) {
			n := int64(0)
			callA, callB := historyGatedCall("a"), historyGatedCall("b")
			log := eventlog.AsStore(eventlog.New())
			for _, event := range []api.Event{
				{ExecutionID: "e1", Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{InputCount: &n}},
				{ExecutionID: "e1", Kind: api.EventToolCall, ToolCall: &callA},
				{ExecutionID: "e1", Kind: api.EventApprovalRequest, Approval: &api.ApprovalRequest{ToolCallID: "a"}},
				{ExecutionID: "e1", Kind: api.EventApprovalResult, ApprovalResult: &api.ApprovalResult{ToolCallID: "a", RequestSeq: 3, Approved: true, VerifiedApprover: historyApproverA}},
				{ExecutionID: "e1", Kind: api.EventToolResult, Result: &api.ToolResult{ID: "a", ApprovalRequestSeq: 3, ApprovalDecisionSeq: 4}},
				{ExecutionID: "e1", Kind: api.EventEnd, End: &api.HarnessEnd{State: "COMPLETED"}},
				{ExecutionID: "e2", Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{InputCount: &n}},
				{ExecutionID: "e2", Kind: api.EventToolCall, ToolCall: &callB},
				{ExecutionID: "e2", Kind: api.EventApprovalRequest, Approval: &api.ApprovalRequest{ToolCallID: "b"}},
			} {
				head, err := log.Head()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := log.Append(head, 0, event); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := controller.Approve(log, api.ApprovalDecision{ExecutionID: "e2", ToolCallID: "b", RequestSeq: 9, Approved: true, VerifiedApprover: historyApproverB}); err != nil {
				t.Fatal(err)
			}

			har := &historyHarness{calls: map[string]string{"e1": "a", "e2": "b"}}
			remote := wrap(t, har)
			executor := controller.WithToolExecutor(func(context.Context, controller.ToolCallContext, api.ToolCall) (api.ToolResult, error) {
				return api.ToolResult{Output: map[string]any{"text": "done"}}, nil
			})
			c, err := controller.New(log, nil, controller.WithSessionUID("s"), executor)
			if err != nil {
				t.Fatal(err)
			}
			if resumed, err := c.Resume(t.Context(), remote); err != nil || !resumed {
				t.Fatalf("Resume = %v, %v", resumed, err)
			}
			head, err := log.Head()
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Exec(t.Context(), remote, nil, head); err != nil {
				t.Fatalf("Exec: %v", err)
			}
			replayer, err := controller.New(log, nil, controller.WithSessionUID("s"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := replayer.Replay(t.Context(), remote); err != nil {
				t.Fatalf("Replay: %v", err)
			}

			// Resume of e2 (1 decision), Exec (2), Replay of e1 (0), e2 (1) and the Exec turn (2).
			want := []int{1, 2, 0, 1, 2}
			if len(har.histories) != len(want) {
				t.Fatalf("harness ran %d times, want %d", len(har.histories), len(want))
			}
			for i, history := range har.histories {
				decisions := 0
				for _, event := range history {
					if event.ApprovalResult == nil {
						continue
					}
					decisions++
					if got := event.ApprovalResult.VerifiedApprover; got != (api.CallerIdentity{}) {
						t.Errorf("run %d: Start.History decision for %q carries approver %+v", i, event.ApprovalResult.ToolCallID, got)
					}
					if got := wire.EventToProto(event).GetApprovalResult().GetVerifiedApprover(); got != nil {
						t.Errorf("run %d: wire form of the history decision carries approver %v", i, got)
					}
				}
				if decisions != want[i] {
					t.Errorf("run %d: Start.History holds %d decisions, want %d", i, decisions, want[i])
				}
			}

			records, err := log.Read(1)
			if err != nil {
				t.Fatal(err)
			}
			journaled := map[string]api.CallerIdentity{}
			for _, r := range records {
				if r.Event.ApprovalResult != nil {
					journaled[r.Event.ApprovalResult.ToolCallID] = r.Event.ApprovalResult.VerifiedApprover
				}
			}
			if journaled["a"] != historyApproverA || journaled["b"] != historyApproverB {
				t.Fatalf("journaled approvers = %+v, want a=%+v b=%+v", journaled, historyApproverA, historyApproverB)
			}
			if err := controller.VerifyJournal(log); err != nil {
				t.Fatalf("VerifyJournal: %v", err)
			}
		})
	}
}

// An in-process harness that reslices Start.History to its capacity reaches no verified approver
// on Resume or Replay: neither when the history is empty because the execution is the journal's
// first, nor when an approver-free history precedes a later verified decision.
func TestStartHistoryCapacityOmitsVerifiedApprover(t *testing.T) {
	n := int64(0)
	callA, callB := historyGatedCall("a"), historyGatedCall("b")
	for name, tc := range map[string]struct {
		events   []api.Event
		decision api.ApprovalDecision
		runs     int // Resume of the pending execution, then Replay of every execution
	}{
		"empty history": {
			events: []api.Event{
				{ExecutionID: "e1", Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{InputCount: &n}},
				{ExecutionID: "e1", Kind: api.EventToolCall, ToolCall: &callA},
				{ExecutionID: "e1", Kind: api.EventApprovalRequest, Approval: &api.ApprovalRequest{ToolCallID: "a"}},
			},
			decision: api.ApprovalDecision{ExecutionID: "e1", ToolCallID: "a", RequestSeq: 3, Approved: true, VerifiedApprover: historyApproverA},
			runs:     2,
		},
		"approver-free history": {
			events: []api.Event{
				{ExecutionID: "e1", Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{InputCount: &n}},
				{ExecutionID: "e1", Kind: api.EventToolCall, ToolCall: &callA},
				{ExecutionID: "e1", Kind: api.EventApprovalRequest, Approval: &api.ApprovalRequest{ToolCallID: "a"}},
				{ExecutionID: "e1", Kind: api.EventApprovalResult, ApprovalResult: &api.ApprovalResult{ToolCallID: "a", RequestSeq: 3, Approved: true}},
				{ExecutionID: "e1", Kind: api.EventToolResult, Result: &api.ToolResult{ID: "a", ApprovalRequestSeq: 3, ApprovalDecisionSeq: 4}},
				{ExecutionID: "e1", Kind: api.EventEnd, End: &api.HarnessEnd{State: "COMPLETED"}},
				{ExecutionID: "e2", Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{InputCount: &n}},
				{ExecutionID: "e2", Kind: api.EventToolCall, ToolCall: &callB},
				{ExecutionID: "e2", Kind: api.EventApprovalRequest, Approval: &api.ApprovalRequest{ToolCallID: "b"}},
			},
			decision: api.ApprovalDecision{ExecutionID: "e2", ToolCallID: "b", RequestSeq: 9, Approved: true, VerifiedApprover: historyApproverB},
			runs:     3,
		},
	} {
		t.Run(name, func(t *testing.T) {
			log := eventlog.AsStore(eventlog.New())
			for _, event := range tc.events {
				head, err := log.Head()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := log.Append(head, 0, event); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := controller.Approve(log, tc.decision); err != nil {
				t.Fatal(err)
			}

			har := &historyHarness{calls: map[string]string{"e1": "a", "e2": "b"}}
			executor := controller.WithToolExecutor(func(context.Context, controller.ToolCallContext, api.ToolCall) (api.ToolResult, error) {
				return api.ToolResult{Output: map[string]any{"text": "done"}}, nil
			})
			c, err := controller.New(log, nil, controller.WithSessionUID("s"), executor)
			if err != nil {
				t.Fatal(err)
			}
			if resumed, err := c.Resume(t.Context(), har); err != nil || !resumed {
				t.Fatalf("Resume = %v, %v", resumed, err)
			}
			replayer, err := controller.New(log, nil, controller.WithSessionUID("s"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := replayer.Replay(t.Context(), har); err != nil {
				t.Fatalf("Replay: %v", err)
			}

			if len(har.capacities) != tc.runs {
				t.Fatalf("harness ran %d times, want %d", len(har.capacities), tc.runs)
			}
			for i, reachable := range har.capacities {
				for _, event := range reachable {
					if event.ApprovalResult != nil && event.ApprovalResult.VerifiedApprover != (api.CallerIdentity{}) {
						t.Errorf("run %d: Start.History (len %d) resliced to capacity %d reaches decision %q with approver %+v",
							i, len(har.histories[i]), len(reachable), event.ApprovalResult.ToolCallID, event.ApprovalResult.VerifiedApprover)
					}
				}
			}
			state, err := controller.InspectApproval(log, 0)
			if err != nil {
				t.Fatal(err)
			}
			if state == nil || state.Decision.Event.ApprovalResult.VerifiedApprover != tc.decision.VerifiedApprover {
				t.Fatalf("journaled decision lost its approver: %+v", state)
			}
		})
	}
}
