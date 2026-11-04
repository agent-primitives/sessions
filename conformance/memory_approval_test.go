package conformance_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/sqlitelog"
)

type approvalRun func(context.Context, api.EventSink) error

// scriptedApprovalHarness declares a resumability tier and plays one script per Run. Its state
// outlives a Run, standing in for a REQUIRES_MEMORY_SNAPSHOT harness restored from its snapshot.
type scriptedApprovalHarness struct {
	tier api.Resumability
	mu   sync.Mutex
	runs []approvalRun
	n    int
}

func (h *scriptedApprovalHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "approval-agent", Capabilities: api.Capabilities{Resumability: h.tier}}, nil
}

func (h *scriptedApprovalHarness) Run(ctx context.Context, _ *api.Start, sink api.EventSink) error {
	h.mu.Lock()
	n := h.n
	h.n++
	h.mu.Unlock()
	if n >= len(h.runs) {
		return errors.New("scripted harness: unexpected Run")
	}
	return h.runs[n](ctx, sink)
}

func gatedIssueCall() api.ToolCall {
	return api.ToolCall{
		ID:             "toolu_issue",
		Tool:           "create_issue",
		Args:           map[string]any{"title": "memory tier", "body": "approved write"},
		Mediation:      api.MediationRequiresApproval,
		IdempotencyKey: "toolu_issue",
	}
}

// parkPrefix is the parked turn: text, a reported tool, then the gated call.
func parkPrefix(ctx context.Context, sink api.EventSink) error {
	if err := sink.Output(ctx, "I'll file the issue."); err != nil {
		return err
	}
	if err := sink.Report(ctx, api.ToolResult{ID: "toolu_read", Content: api.TextMessage("tool", "README contents").Parts}); err != nil {
		return err
	}
	_, err := sink.ToolCall(ctx, gatedIssueCall())
	return err
}

// gatedFirst is a restored memory-tier continuation: the gated call first, then live text.
func gatedFirst(receipt *api.ToolResult) approvalRun {
	return func(ctx context.Context, sink api.EventSink) error {
		result, err := sink.ToolCall(ctx, gatedIssueCall())
		if err != nil {
			return err
		}
		*receipt = result
		return sink.Output(ctx, "Done.")
	}
}

type approvalFixture struct {
	path     string
	store    *sqlitelog.Store
	log      eventlog.Store
	executed atomic.Int32
}

func (f *approvalFixture) controller(t *testing.T) *controller.Controller {
	t.Helper()
	c, err := controller.New(f.log, (&countModel{}).call, controller.WithSessionUID("memory-approval"),
		controller.WithToolExecutor(func(context.Context, controller.ToolCallContext, api.ToolCall) (api.ToolResult, error) {
			f.executed.Add(1)
			return api.ToolResult{Content: api.TextMessage("tool", "issue 7").Parts}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// reopen closes and reopens the file store, as a host restarting to recover a parked session does.
func (f *approvalFixture) reopen(t *testing.T) {
	t.Helper()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := sqlitelog.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	f.store, f.log = store, store.Session("s")
}

func (f *approvalFixture) kinds(t *testing.T, from int64) []api.EventKind {
	t.Helper()
	recs, err := f.log.Read(from + 1)
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]api.EventKind, 0, len(recs))
	for _, r := range recs {
		kinds = append(kinds, r.Event.Kind)
	}
	return kinds
}

func (f *approvalFixture) head(t *testing.T) int64 {
	t.Helper()
	head, err := f.log.Head()
	if err != nil {
		t.Fatal(err)
	}
	return head
}

// parkOverWire runs the parking turn through Harness.Connect and returns the gate.
func parkOverWire(t *testing.T, h *scriptedApprovalHarness) (*approvalFixture, *controller.ApprovalState) {
	t.Helper()
	store, path := openFile(t)
	f := &approvalFixture{path: path, store: store, log: store.Session("s")}
	err := f.controller(t).Exec(t.Context(), wireHarnessFrom(t, h), []api.Message{*api.TextMessage("user", "file an issue")}, 0)
	if !errors.Is(err, api.ErrApprovalParked) {
		t.Fatalf("Exec = %v, want an approval park", err)
	}
	state, err := controller.InspectApproval(f.log, 0)
	if err != nil || state == nil || state.Request == nil || state.Decision != nil || state.Head != f.head(t) {
		t.Fatalf("gate=%+v err=%v, want an undecided gate at the head", state, err)
	}
	return f, state
}

// decide records the decision the way the session Approve path writes it, then reopens the store.
func (f *approvalFixture) decide(t *testing.T, state *controller.ApprovalState, approved bool) {
	t.Helper()
	if _, err := controller.Approve(f.log, api.ApprovalDecision{ExecutionID: state.ExecutionID, ToolCallID: state.Call.ID, RequestSeq: state.Request.Seq, Approved: approved}); err != nil {
		t.Fatal(err)
	}
	f.reopen(t)
}

func (f *approvalFixture) assertOpenGate(t *testing.T) {
	t.Helper()
	state, err := controller.InspectApproval(f.log, 0)
	if err != nil || state == nil || state.Decision == nil || state.Receipt != nil {
		t.Fatalf("gate=%+v err=%v, want a decided gate without receipt", state, err)
	}
}

// TestWireMemoryTierApproval is the APR-16 conformance check over Harness.Connect: a
// REQUIRES_MEMORY_SNAPSHOT harness parks on an approval-gated call, and after the decision and a
// host restart, Resume serves from the gate, so the restored harness's first call is the gated
// call and it does not re-emit its prefix. STATELESS_REPLAY recovery still serves from the start.
func TestWireMemoryTierApproval(t *testing.T) {
	memory := func(runs ...approvalRun) *scriptedApprovalHarness {
		return &scriptedApprovalHarness{tier: api.ResumabilityRequiresMemorySnapshot, runs: append([]approvalRun{parkPrefix}, runs...)}
	}
	t.Run("park", func(t *testing.T) {
		f, _ := parkOverWire(t, memory())
		kinds := f.kinds(t, 0)
		if n := len(kinds); n < 2 || kinds[n-2] != api.EventToolCall || kinds[n-1] != api.EventApprovalRequest {
			t.Fatalf("journal %v, want TOOL_CALL, APPROVAL_REQUEST at the tail", kinds)
		}
		if f.executed.Load() != 0 {
			t.Fatalf("executor calls=%d before a decision", f.executed.Load())
		}
	})
	t.Run("approve", func(t *testing.T) {
		var receipt api.ToolResult
		h := memory(gatedFirst(&receipt))
		f, state := parkOverWire(t, h)
		f.decide(t, state, true)
		head := f.head(t)
		if resumed, err := f.controller(t).Resume(t.Context(), wireHarnessFrom(t, h)); !resumed || err != nil {
			t.Fatalf("Resume = %v, %v", resumed, err)
		}
		if f.executed.Load() != 1 || receipt.IsError || receipt.Approval == nil || !receipt.Approval.Approved {
			t.Fatalf("executor calls=%d receipt=%+v, want one host execution and an approved receipt", f.executed.Load(), receipt)
		}
		if got := f.kinds(t, head); len(got) != 3 || got[0] != api.EventToolResult || got[1] != api.EventOutput || got[2] != api.EventEnd {
			t.Fatalf("appended %v, want TOOL_RESULT, OUTPUT, END", got)
		}
	})
	t.Run("deny", func(t *testing.T) {
		var receipt api.ToolResult
		h := memory(gatedFirst(&receipt))
		f, state := parkOverWire(t, h)
		f.decide(t, state, false)
		head := f.head(t)
		if _, err := f.controller(t).Resume(t.Context(), wireHarnessFrom(t, h)); err != nil {
			t.Fatalf("Resume = %v", err)
		}
		if f.executed.Load() != 0 || !receipt.IsError || receipt.Code != api.ToolResultCodeApprovalDenied || receipt.Approval == nil || receipt.Approval.Approved {
			t.Fatalf("executor calls=%d receipt=%+v, want an APPROVAL_DENIED receipt without dispatch", f.executed.Load(), receipt)
		}
		if got := f.kinds(t, head); len(got) != 3 || got[0] != api.EventToolResult || got[1] != api.EventOutput || got[2] != api.EventEnd {
			t.Fatalf("appended %v, want TOOL_RESULT, OUTPUT, END", got)
		}
	})
	t.Run("wrong_first_call", func(t *testing.T) {
		var receipt api.ToolResult
		h := memory(func(ctx context.Context, sink api.EventSink) error {
			return sink.Output(ctx, "I could not file the issue.")
		}, gatedFirst(&receipt))
		f, state := parkOverWire(t, h)
		f.decide(t, state, true)
		head := f.head(t)
		if _, err := f.controller(t).Resume(t.Context(), wireHarnessFrom(t, h)); !errors.Is(err, controller.ErrReplayDiverged) {
			t.Fatalf("Resume = %v, want ErrReplayDiverged", err)
		}
		if got := f.kinds(t, head); len(got) != 1 || got[0] != api.EventError {
			t.Fatalf("appended %v, want one ERROR", got)
		}
		f.assertOpenGate(t)
		if _, err := f.controller(t).Resume(t.Context(), wireHarnessFrom(t, h)); err != nil || f.executed.Load() != 1 {
			t.Fatalf("compliant Resume = %v executor calls=%d", err, f.executed.Load())
		}
	})
	t.Run("stateless_unchanged", func(t *testing.T) {
		var receipt api.ToolResult
		h := &scriptedApprovalHarness{tier: api.ResumabilityStatelessReplay, runs: []approvalRun{
			parkPrefix,
			gatedFirst(&receipt),
			func(ctx context.Context, sink api.EventSink) error {
				if err := parkPrefix(ctx, sink); err != nil {
					return err
				}
				return sink.Output(ctx, "Done.")
			},
		}}
		f, state := parkOverWire(t, h)
		f.decide(t, state, true)
		head := f.head(t)
		// EXE-21 serves the recorded prefix first; the gated call reproduces the historical
		// pre-intent refusal, as before APR-16.
		if _, err := f.controller(t).Resume(t.Context(), wireHarnessFrom(t, h)); !errors.Is(err, controller.ErrApprovalUnavailable) {
			t.Fatalf("Resume = %v, want ErrApprovalUnavailable", err)
		}
		if got := f.kinds(t, head); len(got) != 1 || got[0] != api.EventError || f.executed.Load() != 0 {
			t.Fatalf("appended %v executor calls=%d, want one ERROR and no dispatch", got, f.executed.Load())
		}
		f.assertOpenGate(t)
		if _, err := f.controller(t).Resume(t.Context(), wireHarnessFrom(t, h)); err != nil || f.executed.Load() != 1 {
			t.Fatalf("full-prefix Resume = %v executor calls=%d", err, f.executed.Load())
		}
	})
}
