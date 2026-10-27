package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/agent-primitives/sessions/api"
)

var (
	approverA = api.CallerIdentity{Issuer: "https://issuer.example", Subject: "approver-a"}
	approverB = api.CallerIdentity{Issuer: "https://issuer.example", Subject: "approver-b"}
)

// A new decision journals the verified approver it was given. An exact retry is bound to that
// approver: the same approver gets the existing record and appends nothing; a different approver,
// none, or an issuer that differs only by a trailing slash is a changed decision.
func TestApproveRecordsVerifiedApproverAndBindsRetries(t *testing.T) {
	log := gateLog(t, gateEvents()[:3]...)
	dec := api.ApprovalDecision{ExecutionID: "e", ToolCallID: "c", RequestSeq: 3, Approved: true, Reason: "ok", VerifiedApprover: approverA}
	r, err := Approve(log, dec)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Event.ApprovalResult.VerifiedApprover; got != approverA {
		t.Fatalf("recorded approver = %+v, want %+v", got, approverA)
	}
	records, err := log.Read(r.Seq)
	if err != nil || len(records) != 1 || records[0].Event.ApprovalResult.VerifiedApprover != approverA {
		t.Fatalf("journaled decision = %+v, %v", records, err)
	}
	retry, err := Approve(log, dec)
	if err != nil || retry.Seq != r.Seq || retry.Hash != r.Hash {
		t.Fatalf("exact retry = %+v, %v; want record %d", retry, err, r.Seq)
	}
	for name, approver := range map[string]api.CallerIdentity{
		"different subject":      approverB,
		"no approver":            {},
		"issuer not normalized":  {Issuer: approverA.Issuer + "/", Subject: approverA.Subject},
		"subject case preserved": {Issuer: approverA.Issuer, Subject: "Approver-A"},
	} {
		t.Run(name, func(t *testing.T) {
			changed := dec
			changed.VerifiedApprover = approver
			if _, err := Approve(log, changed); !errors.Is(err, ErrInvalidApprovalDecision) {
				t.Fatalf("retry by %+v = %v, want ErrInvalidApprovalDecision", approver, err)
			}
		})
	}
	if head, err := log.Head(); err != nil || head != r.Seq {
		t.Fatalf("retries appended: head %d, %v; want %d", head, err, r.Seq)
	}
	if err := VerifyJournal(log); err != nil {
		t.Fatalf("journal with a verified approver fails verification: %v", err)
	}
}

// A decision recorded without an approver (unauthenticated, or before the field existed) is not an
// exact retry for an authenticated caller; it stays one for an unauthenticated caller.
func TestApproveLegacyDecisionRetry(t *testing.T) {
	log := gateLog(t, gateEvents()[:4]...)
	legacy := api.ApprovalDecision{ExecutionID: "e", ToolCallID: "c", RequestSeq: 3, Approved: true}
	if r, err := Approve(log, legacy); err != nil || r.Seq != 4 {
		t.Fatalf("unauthenticated retry = %+v, %v; want record 4", r, err)
	}
	legacy.VerifiedApprover = approverA
	if _, err := Approve(log, legacy); !errors.Is(err, ErrInvalidApprovalDecision) {
		t.Fatalf("authenticated retry of an unattributed decision = %v, want ErrInvalidApprovalDecision", err)
	}
}

// A half-set approver is a host bug: it is refused before any append, and a recorded one is
// invalid evidence rather than a decision.
func TestApproveRefusesIncompleteVerifiedApprover(t *testing.T) {
	for name, approver := range map[string]api.CallerIdentity{"issuer only": {Issuer: "i"}, "subject only": {Subject: "s"}} {
		t.Run(name, func(t *testing.T) {
			log := gateLog(t, gateEvents()[:3]...)
			_, err := Approve(log, api.ApprovalDecision{ExecutionID: "e", ToolCallID: "c", RequestSeq: 3, Approved: true, VerifiedApprover: approver})
			if !errors.Is(err, api.ErrInvalidCallerIdentity) {
				t.Fatalf("Approve = %v, want ErrInvalidCallerIdentity", err)
			}
			if head, _ := log.Head(); head != 3 {
				t.Fatalf("refusal appended: head %d", head)
			}
			recorded := gateEvents()[3]
			recorded.ApprovalResult.VerifiedApprover = approver
			gateAppend(t, log, recorded)
			if _, err := InspectApproval(log, 0); !errors.Is(err, ErrInvalidExecutionLog) {
				t.Fatalf("recorded incomplete approver = %v, want ErrInvalidExecutionLog", err)
			}
		})
	}
}

// Recovery and replay serve a decision that carries a verified approver, and the chain verifies.
// The harness receives the decision without the approver, in process as over the wire.
func TestVerifiedApproverRecoveryAndReplay(t *testing.T) {
	log := gateLog(t, approvedArgsLog(t, map[string]any{"text": "hello"})...)
	if _, err := Approve(log, api.ApprovalDecision{ExecutionID: "e", ToolCallID: "c", RequestSeq: 3, Approved: true, VerifiedApprover: approverA}); err != nil {
		t.Fatal(err)
	}
	var seen []*api.ApprovalResult
	harness := &approvalHarness{fn: func(ctx context.Context, _ *api.Start, s api.EventSink) error {
		call := *gateEvents()[1].ToolCall
		call.Args = map[string]any{"text": "hello"}
		res, err := s.ToolCall(ctx, call)
		if err != nil {
			return err
		}
		seen = append(seen, res.Approval)
		return nil
	}}
	var calls []api.ToolCall
	c, err := New(log, nil, WithSessionUID("s"), WithToolExecutor(countingExecutor(&calls)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Resume(t.Context(), harness); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("executor ran %d times, want 1", len(calls))
	}
	if err := VerifyJournal(log); err != nil {
		t.Fatalf("VerifyJournal: %v", err)
	}
	replayer, err := New(log, nil, WithSessionUID("s"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replayer.Replay(t.Context(), harness); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("replay ran the executor: %d calls", len(calls))
	}
	if len(seen) != 2 {
		t.Fatalf("harness saw %d decisions, want 2 (recovery and replay)", len(seen))
	}
	for i, decision := range seen {
		if decision == nil || !decision.Approved || decision.RequestSeq != 3 || decision.VerifiedApprover != (api.CallerIdentity{}) {
			t.Fatalf("decision %d sent to the harness = %+v, want approved without an approver", i, decision)
		}
	}
	state, err := InspectApproval(log, 0)
	if err != nil || state == nil || state.Decision.Event.ApprovalResult.VerifiedApprover != approverA {
		t.Fatalf("journaled decision lost its approver: %+v, %v", state, err)
	}
}

// harnessHistory clears the approver on copies only; the journaled events keep it. A journal
// prefix comes back with its capacity capped at its length.
func TestHarnessHistoryCopiesDecisions(t *testing.T) {
	plain := gateEvents()
	if got := harnessHistory(plain); &got[0] != &plain[0] {
		t.Fatal("history without an approver was copied")
	}
	if got := harnessHistory(plain[:2]); len(got) != 2 || cap(got) != 2 {
		t.Fatalf("prefix history len %d cap %d, want both 2", len(got), cap(got))
	}
	if got := harnessHistory(plain[:0]); cap(got) != 0 {
		t.Fatalf("empty history cap %d, want 0", cap(got))
	}
	events := gateEvents()
	events[3].ApprovalResult.VerifiedApprover = approverA
	recorded := events[3].ApprovalResult
	got := harnessHistory(events)
	if got[3].ApprovalResult == recorded || got[3].ApprovalResult.VerifiedApprover != (api.CallerIdentity{}) {
		t.Fatalf("harness decision = %+v, want a copy without the approver", got[3].ApprovalResult)
	}
	if !got[3].ApprovalResult.Approved || got[3].ApprovalResult.RequestSeq != 3 || got[1].ToolCall != events[1].ToolCall {
		t.Fatalf("harness history lost other fields: %+v", got[3].ApprovalResult)
	}
	if events[3].ApprovalResult != recorded || recorded.VerifiedApprover != approverA {
		t.Fatalf("journaled decision changed: %+v", recorded)
	}
}

// A pre-dispatch tool policy hook is host code: it receives the committed decision with its
// verified approver, as a host would use to judge who approved the call. A definitive refusal
// still records POLICY_DENIED, and the harness gets that receipt's decision without the approver,
// on recovery and on replay, while the journaled decision keeps it.
func TestVerifiedApproverToolPolicy(t *testing.T) {
	log := gateLog(t, gateEvents()[:3]...)
	if _, err := Approve(log, api.ApprovalDecision{ExecutionID: "e", ToolCallID: "c", RequestSeq: 3, Approved: true, VerifiedApprover: approverA}); err != nil {
		t.Fatal(err)
	}
	var seen []api.ToolResult
	harness := &approvalHarness{fn: func(ctx context.Context, _ *api.Start, s api.EventSink) error {
		res, err := s.ToolCall(ctx, *gateEvents()[1].ToolCall)
		if err != nil {
			return err
		}
		seen = append(seen, res)
		return nil
	}}
	var calls []api.ToolCall
	policy := &policyRecorder{verdicts: []ToolPolicyDecision{{Verdict: ToolPolicyDefinitiveRefusal, Reason: "approver not allowed"}}}
	c, err := New(log, nil, WithSessionUID("s"), WithToolExecutor(countingExecutor(&calls)), WithToolPolicy(policy.hook))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Resume(t.Context(), harness); err != nil {
		t.Fatal(err)
	}
	if len(policy.inputs) != 1 || policy.inputs[0].Approval == nil || policy.inputs[0].Approval.Decision.VerifiedApprover != approverA {
		t.Fatalf("hook inputs = %+v, want one with the verified approver", policy.inputs)
	}
	if len(calls) != 0 {
		t.Fatalf("executor ran %d times after a definitive refusal", len(calls))
	}
	if _, err := c.Replay(t.Context(), harness); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(policy.inputs) != 1 || len(calls) != 0 {
		t.Fatalf("replay reached the hook or executor: hook calls=%d effects=%d", len(policy.inputs), len(calls))
	}
	if len(seen) != 2 {
		t.Fatalf("harness saw %d receipts, want 2 (recovery and replay)", len(seen))
	}
	for i, res := range seen {
		if res.Code != api.ToolResultCodePolicyDenied || res.Approval == nil || !res.Approval.Approved || res.Approval.VerifiedApprover != (api.CallerIdentity{}) {
			t.Fatalf("receipt %d sent to the harness = %+v (decision %+v), want POLICY_DENIED without an approver", i, res, res.Approval)
		}
	}
	state, err := InspectApproval(log, 0)
	if err != nil || state == nil || state.Receipt == nil || state.Receipt.Event.Result.Code != api.ToolResultCodePolicyDenied ||
		state.Decision.Event.ApprovalResult.VerifiedApprover != approverA {
		t.Fatalf("journaled approval = %+v, %v; want a POLICY_DENIED receipt and the approver kept", state, err)
	}
	if err := VerifyJournal(log); err != nil {
		t.Fatal(err)
	}
}
