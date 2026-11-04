package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
)

// tierHarness is a fake harness with a declared tier and one run function per Run call.
type tierHarness struct {
	tier   api.Resumability
	runs   []func(context.Context, *api.Start, api.EventSink) error
	starts []*api.Start
}

func (h *tierHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "approval-close", Capabilities: api.Capabilities{Resumability: h.tier}}, nil
}

func (h *tierHarness) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	n := len(h.starts)
	h.starts = append(h.starts, start)
	if n >= len(h.runs) {
		return errors.New("test: unexpected Run")
	}
	return h.runs[n](ctx, start, sink)
}

// closeGated is the gated call the close tests park on.
func closeGated() api.ToolCall {
	return api.ToolCall{
		ID: "toolu_016xkvdCMPcYf5KZ8T4AX7ab", Tool: "mcp__room__create_issue",
		Args:      map[string]any{"title": "close", "body": "approved write"},
		Mediation: api.MediationRequiresApproval, IdempotencyKey: "toolu_016xkvdCMPcYf5KZ8T4AX7ab",
	}
}

func reissue(ctx context.Context, _ *api.Start, sink api.EventSink) error {
	_, err := sink.ToolCall(ctx, closeGated())
	return err
}

func head(t *testing.T, log eventlog.Store) int64 {
	t.Helper()
	h, err := log.Head()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func journalRecords(t *testing.T, log eventlog.Store) []eventlog.Record {
	t.Helper()
	recs, err := log.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// parkUndecided runs a turn that parks on closeGated and returns the undecided gate.
func parkUndecided(t *testing.T, log eventlog.Store) *ApprovalState {
	t.Helper()
	c, err := New(log, nil, WithSessionUID("s"))
	if err != nil {
		t.Fatal(err)
	}
	h := &tierHarness{tier: api.ResumabilityStatelessReplay, runs: []func(context.Context, *api.Start, api.EventSink) error{reissue}}
	if err := c.Exec(t.Context(), h, []api.Message{*api.TextMessage("user", "file an issue")}, head(t, log)); !errors.Is(err, api.ErrApprovalParked) {
		t.Fatalf("Exec = %v, want park", err)
	}
	state, err := InspectApproval(log, 0)
	if err != nil || state == nil || state.Request == nil || state.Decision != nil {
		t.Fatalf("InspectApproval = %+v, %v", state, err)
	}
	return state
}

func gateRef(state *ApprovalState) api.ApprovalRef {
	return api.ApprovalRef{ExecutionID: state.ExecutionID, ToolCallID: state.Call.ID, RequestSeq: state.Request.Seq}
}

func noExecutor(t *testing.T) Option {
	return WithToolExecutor(func(context.Context, ToolCallContext, api.ToolCall) (api.ToolResult, error) {
		t.Error("executor called")
		return api.ToolResult{}, errors.New("unexpected executor call")
	})
}

var tiers = []api.Resumability{api.ResumabilityStatelessReplay, api.ResumabilityRequiresMemorySnapshot}

// newTurn runs Exec with a new input on a harness of tier that answers and ends the turn. It
// returns the Start the harness got, or nil when Exec refused before running it.
func newTurn(t *testing.T, log eventlog.Store, tier api.Resumability) (*api.Start, error) {
	t.Helper()
	c, err := New(log, nil, WithSessionUID("s"))
	if err != nil {
		t.Fatal(err)
	}
	h := &tierHarness{tier: tier, runs: []func(context.Context, *api.Start, api.EventSink) error{
		func(ctx context.Context, _ *api.Start, sink api.EventSink) error {
			return sink.Output(ctx, "next answer")
		},
	}}
	err = c.Exec(t.Context(), h, []api.Message{*api.TextMessage("user", "next turn")}, head(t, log))
	if len(h.starts) == 0 {
		return nil, err
	}
	return h.starts[0], err
}

// requireNewTurn checks that a new turn runs after a close and that its Start.History carries the
// close receipt with code, so a stateless harness sees the call paired with its result.
func requireNewTurn(t *testing.T, log eventlog.Store, tier api.Resumability, code api.ToolResultCode) {
	t.Helper()
	start, err := newTurn(t, log, tier)
	if err != nil || start == nil {
		t.Fatalf("Exec after close = %v (ran=%v), want a new turn", err, start != nil)
	}
	found := false
	for _, ev := range start.History {
		if ev.Kind == api.EventToolResult && ev.Result.ID == closeGated().ID {
			found = ev.Result.Code == code && ev.Result.IsError
		}
	}
	if !found {
		t.Fatalf("Start.History lacks the %v close receipt: %v", code, journalKinds(t, log))
	}
	if kinds := journalKinds(t, log); kinds[len(kinds)-1] != api.EventEnd {
		t.Fatalf("new turn ends with %s, want END", kinds[len(kinds)-1])
	}
	if err := VerifyJournal(log); err != nil {
		t.Fatal(err)
	}
}

// TestCloseApprovalUnblocksSession covers a gate the restored harness can no longer re-issue: the
// harness ignored the deferral and answered, or it was resumed with a prompt that dropped the
// deferred call. The park leaves no terminal record, recovery diverges whatever the decision, and
// Exec refuses a new turn with ErrApprovalBlocked. CloseApproval receipts the gate, after which a
// new turn runs on either tier with the receipt in its history.
func TestCloseApprovalUnblocksSession(t *testing.T) {
	shapes := []struct {
		name string
		run  func(context.Context, *api.Start, api.EventSink) error
	}{
		{"answers without the call", func(ctx context.Context, _ *api.Start, sink api.EventSink) error {
			return sink.Output(ctx, "I could not file the issue.")
		}},
		{"emits nothing", func(context.Context, *api.Start, api.EventSink) error { return nil }},
	}
	for _, tier := range tiers {
		for _, decision := range []string{"approved", "denied", "undecided"} {
			for _, shape := range shapes {
				t.Run(fmt.Sprintf("%v/%s/%s", tier, decision, shape.name), func(t *testing.T) {
					log := eventlog.AsStore(eventlog.New())
					state := parkUndecided(t, log)
					if kinds := journalKinds(t, log); kinds[len(kinds)-1] != api.EventApprovalRequest {
						t.Fatalf("park ends with %s, want APPROVAL_REQUEST", kinds[len(kinds)-1])
					}
					if start, err := newTurn(t, log, tier); !errors.Is(err, ErrApprovalBlocked) || start != nil {
						t.Fatalf("Exec behind an open gate = %v, want ErrApprovalBlocked", err)
					}
					want := api.ToolResultCodeNotExecuted
					if decision != "undecided" {
						approved := decision == "approved"
						if approved {
							want = api.ToolResultCodeOutcomeUnknown
						}
						if _, err := Approve(log, api.ApprovalDecision{ExecutionID: state.ExecutionID, ToolCallID: state.Call.ID, RequestSeq: state.Request.Seq, Approved: approved}); err != nil {
							t.Fatal(err)
						}
					}
					c, err := New(log, nil, WithSessionUID("s"), noExecutor(t))
					if err != nil {
						t.Fatal(err)
					}
					h := &tierHarness{tier: api.ResumabilityStatelessReplay, runs: []func(context.Context, *api.Start, api.EventSink) error{shape.run}}
					_, err = c.Resume(t.Context(), h)
					if decision == "undecided" {
						if !errors.Is(err, api.ErrApprovalParked) || len(h.starts) != 0 {
							t.Fatalf("Resume = %v runs=%d, want a park without running the harness", err, len(h.starts))
						}
					} else if !errors.Is(err, ErrReplayDiverged) {
						t.Fatalf("Resume = %v, want ErrReplayDiverged", err)
					}
					if start, err := newTurn(t, log, tier); !errors.Is(err, ErrApprovalBlocked) || start != nil {
						t.Fatalf("Exec after failed recovery = %v, want ErrApprovalBlocked", err)
					}
					_, receipt, err := CloseApproval(log, gateRef(state), api.CallerIdentity{})
					if err != nil || receipt.Event.Result.Code != want {
						t.Fatalf("CloseApproval = %+v, %v; want %v", receipt.Event.Result, err, want)
					}
					requireNewTurn(t, log, tier, want)
				})
			}
		}
		// A refused park can leave a call-only cut (TOOL_CALL committed, APPROVAL_REQUEST lost).
		// It blocks new turns, CloseApproval cannot address it, and Resume repairs it into an
		// undecided gate without running the harness. Closing that gate unblocks the session.
		t.Run(fmt.Sprintf("%v/call-only cut is repaired then closed", tier), func(t *testing.T) {
			log := &requestFault{Store: eventlog.AsStore(eventlog.New()), fail: true}
			c, err := New(log, nil, WithSessionUID("s"))
			if err != nil {
				t.Fatal(err)
			}
			h := &tierHarness{tier: api.ResumabilityStatelessReplay, runs: []func(context.Context, *api.Start, api.EventSink) error{reissue}}
			if err := c.Exec(t.Context(), h, []api.Message{*api.TextMessage("user", "file an issue")}, 0); err == nil || errors.Is(err, api.ErrApprovalParked) {
				t.Fatalf("Exec = %v, want a refused park", err)
			}
			kinds := journalKinds(t, log)
			if kinds[len(kinds)-1] != api.EventToolCall {
				t.Fatalf("log ends with %s, want a call-only cut", kinds[len(kinds)-1])
			}
			log.fail = false
			if start, err := newTurn(t, log, tier); !errors.Is(err, ErrApprovalBlocked) || start != nil {
				t.Fatalf("Exec behind a call-only cut = %v, want ErrApprovalBlocked", err)
			}
			cut := api.ApprovalRef{ExecutionID: journalRecords(t, log)[0].Event.ExecutionID, ToolCallID: closeGated().ID, RequestSeq: int64(len(kinds) + 1)}
			if _, _, err := CloseApproval(log, cut, api.CallerIdentity{}); !errors.Is(err, ErrInvalidApprovalDecision) {
				t.Fatalf("CloseApproval on a cut = %v, want ErrInvalidApprovalDecision", err)
			}
			c, err = New(log, nil, WithSessionUID("s"))
			if err != nil {
				t.Fatal(err)
			}
			resume := &tierHarness{tier: api.ResumabilityStatelessReplay}
			if _, err := c.Resume(t.Context(), resume); !errors.Is(err, api.ErrApprovalParked) || len(resume.starts) != 0 {
				t.Fatalf("Resume = %v runs=%d, want a park without running the harness", err, len(resume.starts))
			}
			state, err := InspectApproval(log, 0)
			if err != nil || state == nil || state.Request == nil || state.Decision != nil {
				t.Fatalf("state=%+v err=%v, want a repaired undecided gate", state, err)
			}
			decision, receipt, err := CloseApproval(log, gateRef(state), api.CallerIdentity{})
			if err != nil || decision.Event.ApprovalResult.Approved || receipt.Event.Result.Code != api.ToolResultCodeNotExecuted {
				t.Fatalf("CloseApproval = %+v, %+v, %v", decision.Event, receipt.Event, err)
			}
			requireNewTurn(t, log, tier, api.ToolResultCodeNotExecuted)
		})
	}
	// Unchanged regression guard: a recovery that fails before any sink call (an adapter whose
	// gateway is not up yet) is retryable and needs no close.
	t.Run("a recovery that fails before any sink call is retryable", func(t *testing.T) {
		log := eventlog.AsStore(eventlog.New())
		state := parkUndecided(t, log)
		if _, err := Approve(log, api.ApprovalDecision{ExecutionID: state.ExecutionID, ToolCallID: state.Call.ID, RequestSeq: state.Request.Seq, Approved: true}); err != nil {
			t.Fatal(err)
		}
		calls := 0
		exec := WithToolExecutor(func(context.Context, ToolCallContext, api.ToolCall) (api.ToolResult, error) {
			calls++
			return api.ToolResult{Content: api.TextMessage("tool", "issue 7").Parts}, nil
		})
		unavailable := errors.New("test: gateway not connected")
		for i, run := range []func(context.Context, *api.Start, api.EventSink) error{
			func(context.Context, *api.Start, api.EventSink) error { return unavailable },
			reissue,
		} {
			c, err := New(log, nil, WithSessionUID("s"), exec)
			if err != nil {
				t.Fatal(err)
			}
			h := &tierHarness{tier: api.ResumabilityStatelessReplay, runs: []func(context.Context, *api.Start, api.EventSink) error{run}}
			_, err = c.Resume(t.Context(), h)
			if want := []error{unavailable, nil}[i]; !errors.Is(err, want) {
				t.Fatalf("Resume %d = %v, want %v", i, err, want)
			}
		}
		if state, err := InspectApproval(log, 0); err != nil || calls != 1 || state == nil || state.Receipt == nil {
			t.Fatalf("calls=%d state=%+v err=%v, want one execution and a receipt", calls, state, err)
		}
	})
}

// requestFault fails APPROVAL_REQUEST appends while fail is set.
type requestFault struct {
	eventlog.Store
	fail bool
}

func (s *requestFault) Append(head, fence int64, ev api.Event) (eventlog.Record, error) {
	if s.fail && ev.Kind == api.EventApprovalRequest {
		return eventlog.Record{}, errors.New("test: request append lost")
	}
	return s.Store.Append(head, fence, ev)
}

// closeStore counts fences and committed appends, can fail receipt appends, can let another writer (with
// its own, older fence) append just before a fence is minted, and can report itself deleted or
// return a tampered record.
type closeStore struct {
	eventlog.Store
	fences, appends int
	failReceipts    int
	interleave      bool
	deleted         bool
	tamper          bool
}

func (s *closeStore) NewFence() (int64, error) {
	s.fences++
	if s.interleave {
		h, err := s.Head()
		if err != nil {
			return 0, err
		}
		other, err := s.Store.NewFence()
		if err != nil {
			return 0, err
		}
		if _, err := s.Store.Append(h, other, api.Event{ExecutionID: "other", Kind: api.EventError, Err: &api.Error{Description: "concurrent writer"}}); err != nil {
			return 0, err
		}
	}
	return s.Store.NewFence()
}

func (s *closeStore) Append(head, fence int64, ev api.Event) (eventlog.Record, error) {
	if s.failReceipts > 0 && ev.Kind == api.EventToolResult {
		s.failReceipts--
		return eventlog.Record{}, errors.New("test: receipt append lost")
	}
	rec, err := s.Store.Append(head, fence, ev)
	if err == nil {
		s.appends++
	}
	return rec, err
}

func (s *closeStore) CheckWritable() error {
	if s.deleted {
		return eventlog.ErrDeleted
	}
	return eventlog.CheckWritable(s.Store)
}

func (s *closeStore) Read(from int64) ([]eventlog.Record, error) {
	recs, err := s.Store.Read(from)
	if err != nil || !s.tamper {
		return recs, err
	}
	out := append([]eventlog.Record(nil), recs...)
	for i := range out {
		if out[i].Event.Kind == api.EventToolCall {
			call := *out[i].Event.ToolCall
			call.Tool = "tampered"
			out[i].Event.ToolCall = &call
			break
		}
	}
	return out, nil
}

// closedGate parks an undecided gate on a closeStore and applies decide ("undecided", "approved"
// or "denied").
func closedGate(t *testing.T, decide string) (*closeStore, *ApprovalState) {
	t.Helper()
	log := &closeStore{Store: eventlog.AsStore(eventlog.New())}
	state := parkUndecided(t, log)
	if decide != "undecided" {
		if _, err := Approve(log, api.ApprovalDecision{ExecutionID: state.ExecutionID, ToolCallID: state.Call.ID, RequestSeq: state.Request.Seq, Approved: decide == "approved"}); err != nil {
			t.Fatal(err)
		}
	}
	log.fences, log.appends = 0, 0
	return log, state
}

// Each open state gets its receipt in one fence: an undecided gate a denial by the closer with the
// fixed reason and then NOT_EXECUTED, a denied gate NOT_EXECUTED, an approved gate OUTCOME_UNKNOWN.
// None calls an executor, and a later Resume serves the receipt to a re-issuing harness without
// calling one either.
func TestCloseApprovalStates(t *testing.T) {
	closer := api.CallerIdentity{Issuer: "https://issuer.example", Subject: "owner"}
	for _, tc := range []struct {
		state    string
		appends  int
		approved bool
		code     api.ToolResultCode
		text     string
	}{
		{"undecided", 2, false, api.ToolResultCodeNotExecuted, "Tool call was closed without running."},
		{"denied", 1, false, api.ToolResultCodeNotExecuted, "Tool call was closed without running."},
		{"approved", 1, true, api.ToolResultCodeOutcomeUnknown, "Tool call was closed; whether it ran is unknown."},
	} {
		t.Run(tc.state, func(t *testing.T) {
			log, state := closedGate(t, tc.state)
			before := head(t, log)
			decision, receipt, err := CloseApproval(log, gateRef(state), closer)
			if err != nil {
				t.Fatal(err)
			}
			if log.fences != 1 || log.appends != tc.appends || head(t, log) != before+int64(tc.appends) {
				t.Fatalf("fences=%d appends=%d head %d -> %d, want 1 fence and %d appends", log.fences, log.appends, before, head(t, log), tc.appends)
			}
			d := decision.Event.ApprovalResult
			if d.Approved != tc.approved || decision.Event.Kind != api.EventApprovalResult {
				t.Fatalf("decision = %+v", decision.Event)
			}
			if tc.state == "undecided" {
				if decision.Seq != before+1 || d.Reason != ApprovalCloseReason || ApprovalCloseReason != "approval closed" ||
					d.VerifiedApprover != closer || decision.Event.Actor != (api.IdentityRef{}) {
					t.Fatalf("host denial = %+v (actor %+v)", d, decision.Event.Actor)
				}
			} else if decision.Seq != state.Request.Seq+1 || d.VerifiedApprover != (api.CallerIdentity{}) {
				t.Fatalf("decision = %+v at %d, want the recorded decision", d, decision.Seq)
			}
			want := api.ToolResult{ID: state.Call.ID, IsError: true, Code: tc.code, Error: tc.text, Content: api.TextMessage("tool", tc.text).Parts,
				ApprovalRequestSeq: state.Request.Seq, ApprovalDecisionSeq: decision.Seq}
			if receipt.Seq != head(t, log) || receipt.Event.ExecutionID != state.ExecutionID || !reflect.DeepEqual(*receipt.Event.Result, want) {
				t.Fatalf("receipt = %+v at %d, want %+v", *receipt.Event.Result, receipt.Seq, want)
			}
			if got, err := InspectApproval(log, 0); err != nil || got.Receipt == nil || got.Receipt.Seq != receipt.Seq {
				t.Fatalf("InspectApproval = %+v, %v", got, err)
			}

			// Resume re-drives the closed execution: the re-issuing harness gets the decision and
			// the receipt and finishes the turn; no executor runs.
			c, err := New(log, nil, WithSessionUID("s"), noExecutor(t))
			if err != nil {
				t.Fatal(err)
			}
			var served api.ToolResult
			h := &tierHarness{tier: api.ResumabilityStatelessReplay, runs: []func(context.Context, *api.Start, api.EventSink) error{
				func(ctx context.Context, _ *api.Start, sink api.EventSink) error {
					res, err := sink.ToolCall(ctx, closeGated())
					if err != nil {
						return err
					}
					served = res
					return sink.Output(ctx, "closed")
				},
			}}
			if resumed, err := c.Resume(t.Context(), h); !resumed || err != nil {
				t.Fatalf("Resume after close = %v, %v", resumed, err)
			}
			if served.Code != tc.code || !served.IsError || served.Approval == nil || served.Approval.Approved != tc.approved {
				t.Fatalf("served = %+v", served)
			}
			if kinds := journalKinds(t, log); kinds[len(kinds)-1] != api.EventEnd {
				t.Fatalf("Resume after close ends with %s, want END", kinds[len(kinds)-1])
			}
			if err := VerifyJournal(log); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A gate that already has a receipt returns its decision and receipt with no fence and no append,
// whether the receipt is an executor's success, a denial's APPROVAL_DENIED or an earlier close.
func TestCloseApprovalReceiptedReturnsExisting(t *testing.T) {
	for _, tc := range []struct {
		name    string
		decide  string
		receipt func(t *testing.T, log eventlog.Store, ref api.ApprovalRef)
		code    api.ToolResultCode
	}{
		{"success", "approved", resumeReceipt, api.ToolResultCodeUnspecified},
		{"approval denied", "denied", resumeReceipt, api.ToolResultCodeApprovalDenied},
		{"earlier close", "undecided", func(t *testing.T, log eventlog.Store, ref api.ApprovalRef) {
			if _, _, err := CloseApproval(log, ref, api.CallerIdentity{}); err != nil {
				t.Fatal(err)
			}
		}, api.ToolResultCodeNotExecuted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, state := closedGate(t, tc.decide)
			tc.receipt(t, log, gateRef(state))
			if err := VerifyJournal(log); err != nil {
				t.Fatal(err)
			}
			want, err := InspectApproval(log, 0)
			if err != nil || want.Receipt == nil || want.Receipt.Event.Result.Code != tc.code {
				t.Fatalf("setup = %+v, %v", want, err)
			}
			log.fences, log.appends = 0, 0
			before := head(t, log)
			decision, receipt, err := CloseApproval(log, gateRef(state), api.CallerIdentity{Issuer: "i", Subject: "s"})
			if err != nil || !reflect.DeepEqual(decision, *want.Decision) || !reflect.DeepEqual(receipt, *want.Receipt) {
				t.Fatalf("CloseApproval = %+v, %+v, %v", decision, receipt, err)
			}
			if log.fences != 0 || log.appends != 0 || head(t, log) != before {
				t.Fatalf("fences=%d appends=%d head %d -> %d, want none", log.fences, log.appends, before, head(t, log))
			}
		})
	}
}

// resumeReceipt lets Resume write the gate's receipt: a re-issuing harness and a succeeding executor.
func resumeReceipt(t *testing.T, log eventlog.Store, _ api.ApprovalRef) {
	t.Helper()
	c, err := New(log, nil, WithSessionUID("s"), WithToolExecutor(func(context.Context, ToolCallContext, api.ToolCall) (api.ToolResult, error) {
		return api.ToolResult{Content: api.TextMessage("tool", "issue 7").Parts}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	h := &tierHarness{tier: api.ResumabilityStatelessReplay, runs: []func(context.Context, *api.Start, api.EventSink) error{reissue}}
	if _, err := c.Resume(t.Context(), h); err != nil {
		t.Fatal(err)
	}
}

// Refusals write nothing and mint no fence.
func TestCloseApprovalRefused(t *testing.T) {
	type setup func(t *testing.T) (*closeStore, api.ApprovalRef)
	open := func(mutate func(*closeStore, *api.ApprovalRef)) setup {
		return func(t *testing.T) (*closeStore, api.ApprovalRef) {
			log, state := closedGate(t, "approved")
			ref := gateRef(state)
			mutate(log, &ref)
			return log, ref
		}
	}
	for _, tc := range []struct {
		name   string
		setup  setup
		closer api.CallerIdentity
		want   error
	}{
		{"empty execution", open(func(_ *closeStore, r *api.ApprovalRef) { r.ExecutionID = "" }), api.CallerIdentity{}, ErrInvalidApprovalDecision},
		{"empty call", open(func(_ *closeStore, r *api.ApprovalRef) { r.ToolCallID = "" }), api.CallerIdentity{}, ErrInvalidApprovalDecision},
		{"zero request", open(func(_ *closeStore, r *api.ApprovalRef) { r.RequestSeq = 0 }), api.CallerIdentity{}, ErrInvalidApprovalDecision},
		{"wrong execution", open(func(_ *closeStore, r *api.ApprovalRef) { r.ExecutionID = "other" }), api.CallerIdentity{}, ErrInvalidApprovalDecision},
		{"wrong call", open(func(_ *closeStore, r *api.ApprovalRef) { r.ToolCallID = "other" }), api.CallerIdentity{}, ErrInvalidApprovalDecision},
		{"wrong request", open(func(_ *closeStore, r *api.ApprovalRef) { r.RequestSeq++ }), api.CallerIdentity{}, ErrInvalidApprovalDecision},
		{"half-set closer", open(func(*closeStore, *api.ApprovalRef) {}), api.CallerIdentity{Issuer: "i"}, api.ErrInvalidCallerIdentity},
		{"deleted", open(func(s *closeStore, _ *api.ApprovalRef) { s.deleted = true }), api.CallerIdentity{}, eventlog.ErrDeleted},
		{"tampered", open(func(s *closeStore, _ *api.ApprovalRef) { s.tamper = true }), api.CallerIdentity{}, ErrJournalTampered},
		{"call-only cut", func(t *testing.T) (*closeStore, api.ApprovalRef) {
			log := &closeStore{Store: gateLog(t, gateEvents()[:2]...)}
			return log, api.ApprovalRef{ExecutionID: "e", ToolCallID: "c", RequestSeq: 3}
		}, api.CallerIdentity{}, ErrInvalidApprovalDecision},
		{"superseded", func(t *testing.T) (*closeStore, api.ApprovalRef) {
			// An older execution's open gate followed by a later execution: the gate is not the
			// selected one.
			n := int64(0)
			later := []api.Event{
				{ExecutionID: "later", Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{InputCount: &n}},
				{ExecutionID: "later", Kind: api.EventEnd, End: &api.HarnessEnd{State: "COMPLETED"}},
			}
			log := &closeStore{Store: gateLog(t, append(gateEvents()[:3], later...)...)}
			return log, api.ApprovalRef{ExecutionID: "e", ToolCallID: "c", RequestSeq: 3}
		}, api.CallerIdentity{}, ErrInvalidApprovalDecision},
		{"inherited", func(t *testing.T) (*closeStore, api.ApprovalRef) {
			parent := gateLog(t, gateEvents()[:4]...)
			child := gateLog(t)
			if err := Fork(parent, child, 4); err != nil {
				t.Fatal(err)
			}
			return &closeStore{Store: child}, api.ApprovalRef{ExecutionID: "e", ToolCallID: "c", RequestSeq: 3}
		}, api.CallerIdentity{}, ErrInvalidApprovalDecision},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, ref := tc.setup(t)
			log.fences, log.appends = 0, 0
			before := head(t, log)
			if _, _, err := CloseApproval(log, ref, tc.closer); !errors.Is(err, tc.want) {
				t.Fatalf("CloseApproval = %v, want %v", err, tc.want)
			}
			if log.fences != 0 || log.appends != 0 || head(t, log) != before {
				t.Fatalf("fences=%d appends=%d head %d -> %d, want none", log.fences, log.appends, before, head(t, log))
			}
		})
	}
}

// Close and Approve are ordered by the journal: whichever commits first decides what the other
// does, and no ordering runs the executor.
func TestCloseApprovalRacesWithApprove(t *testing.T) {
	t.Run("close then approve", func(t *testing.T) {
		log, state := closedGate(t, "undecided")
		closer := api.CallerIdentity{Issuer: "i", Subject: "owner"}
		decision, _, err := CloseApproval(log, gateRef(state), closer)
		if err != nil {
			t.Fatal(err)
		}
		approve := api.ApprovalDecision{ExecutionID: state.ExecutionID, ToolCallID: state.Call.ID, RequestSeq: state.Request.Seq, Approved: true}
		if _, err := Approve(log, approve); !errors.Is(err, ErrInvalidApprovalDecision) {
			t.Fatalf("Approve after close = %v, want ErrInvalidApprovalDecision", err)
		}
		// The exact denial is an APR-7 retry and returns the host-written denial.
		retry := api.ApprovalDecision{ExecutionID: state.ExecutionID, ToolCallID: state.Call.ID, RequestSeq: state.Request.Seq, Reason: ApprovalCloseReason, VerifiedApprover: closer}
		if got, err := Approve(log, retry); err != nil || got.Seq != decision.Seq {
			t.Fatalf("exact retry = %+v, %v", got, err)
		}
	})
	for _, tc := range []struct {
		decide string
		code   api.ToolResultCode
	}{{"approved", api.ToolResultCodeOutcomeUnknown}, {"denied", api.ToolResultCodeNotExecuted}} {
		t.Run(tc.decide+" then close", func(t *testing.T) {
			log, state := closedGate(t, tc.decide)
			if _, receipt, err := CloseApproval(log, gateRef(state), api.CallerIdentity{}); err != nil || receipt.Event.Result.Code != tc.code {
				t.Fatalf("CloseApproval = %+v, %v; want %v", receipt.Event.Result, err, tc.code)
			}
		})
	}
	// Another writer appends between the scan and the write: the CAS at the scanned head fails and
	// nothing of the close is written.
	for _, decide := range []string{"undecided", "approved"} {
		t.Run("interleaved writer/"+decide, func(t *testing.T) {
			log, state := closedGate(t, decide)
			log.interleave = true
			if _, _, err := CloseApproval(log, gateRef(state), api.CallerIdentity{}); !errors.Is(err, eventlog.ErrConflict) {
				t.Fatalf("CloseApproval = %v, want ErrConflict", err)
			}
			if log.appends != 0 {
				t.Fatalf("close appended %d records after losing the CAS", log.appends)
			}
		})
	}
}

// A close of an undecided gate whose receipt append fails leaves a host-denied gate. A retry
// receipts it with NOT_EXECUTED, or returns the APPROVAL_DENIED receipt a Resume wrote meanwhile;
// either way there is exactly one decision.
func TestCloseApprovalPartialFailureConverges(t *testing.T) {
	for _, between := range []string{"retry", "resume then retry"} {
		t.Run(between, func(t *testing.T) {
			log, state := closedGate(t, "undecided")
			log.failReceipts = 1
			if _, _, err := CloseApproval(log, gateRef(state), api.CallerIdentity{}); err == nil {
				t.Fatal("CloseApproval succeeded with a lost receipt append")
			}
			got, err := InspectApproval(log, 0)
			if err != nil || got.Decision == nil || got.Decision.Event.ApprovalResult.Reason != ApprovalCloseReason || got.Receipt != nil {
				t.Fatalf("after partial failure = %+v, %v", got, err)
			}
			want := api.ToolResultCodeNotExecuted
			if between == "resume then retry" {
				resumeReceipt(t, log, gateRef(state))
				want = api.ToolResultCodeApprovalDenied
			}
			decision, receipt, err := CloseApproval(log, gateRef(state), api.CallerIdentity{})
			if err != nil || decision.Seq != got.Decision.Seq || receipt.Event.Result.Code != want {
				t.Fatalf("retry = %+v, %+v, %v; want %v", decision, receipt.Event.Result, err, want)
			}
			decisions := 0
			for _, k := range journalKinds(t, log) {
				if k == api.EventApprovalResult {
					decisions++
				}
			}
			if decisions != 1 {
				t.Fatalf("journal has %d decisions, want 1", decisions)
			}
		})
	}
}

// After a close, a recovery harness that cannot re-issue the call diverges, but Exec stays
// admitted: the gate has its receipt.
func TestCloseApprovalDivergingResumeKeepsExecAdmitted(t *testing.T) {
	log, state := closedGate(t, "approved")
	if _, _, err := CloseApproval(log, gateRef(state), api.CallerIdentity{}); err != nil {
		t.Fatal(err)
	}
	c, err := New(log, nil, WithSessionUID("s"), noExecutor(t))
	if err != nil {
		t.Fatal(err)
	}
	h := &tierHarness{tier: api.ResumabilityStatelessReplay, runs: []func(context.Context, *api.Start, api.EventSink) error{
		func(ctx context.Context, _ *api.Start, sink api.EventSink) error { return sink.Output(ctx, "no call") },
	}}
	if _, err := c.Resume(t.Context(), h); !errors.Is(err, ErrReplayDiverged) {
		t.Fatalf("Resume = %v, want ErrReplayDiverged", err)
	}
	requireNewTurn(t, log, api.ResumabilityStatelessReplay, api.ToolResultCodeOutcomeUnknown)
}

// A REQUIRES_MEMORY_SNAPSHOT harness restored at a closed gate is served from the execution's first
// effect (APR-16, APR-19). After a prefix, its re-issued gated call meets the recorded prefix and
// is refused as a historical rejection: Resume fails with no executor call, appends one ERROR and
// leaves the receipt, and Exec with inputs stays admitted. When the gate is the first effect,
// Resume serves the decision and receipt and finishes the turn.
func TestCloseApprovalMemoryTierResume(t *testing.T) {
	codes := map[string]api.ToolResultCode{
		"undecided": api.ToolResultCodeNotExecuted,
		"denied":    api.ToolResultCodeNotExecuted,
		"approved":  api.ToolResultCodeOutcomeUnknown,
	}
	for _, decision := range []string{"undecided", "denied", "approved"} {
		code := codes[decision]
		t.Run(decision+"/after prefix", func(t *testing.T) {
			log, state := mtParkUndecided(t)
			if decision != "undecided" {
				mtDecide(t, log, state, decision == "approved")
			}
			_, receipt, err := CloseApproval(log, gateRef(state), api.CallerIdentity{})
			if err != nil || receipt.Event.Result.Code != code {
				t.Fatalf("CloseApproval = %+v, %v; want %v", receipt.Event.Result, err, code)
			}
			before := mtHead(t, log)
			h := mtMemory(mtGateFirst)
			if resumed, err := mtController(t, log, mtNoExecutor(t)).Resume(t.Context(), h); !resumed || !errors.Is(err, ErrApprovalUnavailable) || len(h.starts) != 1 {
				t.Fatalf("Resume = %v, %v (runs=%d), want ErrApprovalUnavailable after one run", resumed, err, len(h.starts))
			}
			if appended := mtKinds(mtEvents(t, log))[before:]; len(appended) != 1 || appended[0] != api.EventError {
				t.Fatalf("Resume appended %v, want one ERROR", appended)
			}
			after, err := InspectApproval(log, 0)
			if err != nil || after.Receipt == nil || after.Receipt.Seq != receipt.Seq || after.Receipt.Event.Result.Code != code {
				t.Fatalf("InspectApproval = %+v, %v; want the close receipt unchanged", after, err)
			}
			requireNewTurn(t, log, api.ResumabilityRequiresMemorySnapshot, code)
		})
		t.Run(decision+"/gate is the first effect", func(t *testing.T) {
			log := eventlog.AsStore(eventlog.New())
			park := mtMemory(func(ctx context.Context, _ *api.Start, sink api.EventSink) error {
				_, err := sink.ToolCall(ctx, mtGated())
				return err
			})
			if err := mtController(t, log).Exec(t.Context(), park, []api.Message{*api.TextMessage("user", "file an issue")}, 0); !errors.Is(err, api.ErrApprovalParked) {
				t.Fatalf("Exec = %v, want park", err)
			}
			state, err := InspectApproval(log, 0)
			if err != nil || state == nil || state.Request == nil {
				t.Fatalf("InspectApproval = %+v, %v", state, err)
			}
			if decision != "undecided" {
				mtDecide(t, log, state, decision == "approved")
			}
			if _, _, err := CloseApproval(log, gateRef(state), api.CallerIdentity{}); err != nil {
				t.Fatal(err)
			}
			var served api.ToolResult
			h := mtMemory(func(ctx context.Context, _ *api.Start, sink api.EventSink) error {
				res, err := sink.ToolCall(ctx, mtGated())
				if err != nil {
					return err
				}
				served = res
				return sink.Output(ctx, "closed")
			})
			if resumed, err := mtController(t, log, mtNoExecutor(t)).Resume(t.Context(), h); !resumed || err != nil {
				t.Fatalf("Resume = %v, %v", resumed, err)
			}
			if served.Code != code || !served.IsError || served.Approval == nil || served.Approval.Approved != (decision == "approved") {
				t.Fatalf("served = %+v", served)
			}
			if kinds := mtKinds(mtEvents(t, log)); kinds[len(kinds)-1] != api.EventEnd {
				t.Fatalf("Resume ends with %s, want END", kinds[len(kinds)-1])
			}
			if err := VerifyJournal(log); err != nil {
				t.Fatal(err)
			}
		})
	}
}
