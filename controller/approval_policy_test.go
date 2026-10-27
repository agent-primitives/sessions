package controller

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-primitives/sessions/api"
)

// policyRecorder is a WithToolPolicy hook that returns verdicts in order and records its inputs.
type policyRecorder struct {
	verdicts []ToolPolicyDecision
	inputs   []ToolPolicyInput
}

func (p *policyRecorder) hook(_ context.Context, in ToolPolicyInput) ToolPolicyDecision {
	p.inputs = append(p.inputs, in)
	if len(p.verdicts) == 0 {
		return ToolPolicyDecision{Verdict: ToolPolicyAllow}
	}
	d := p.verdicts[0]
	p.verdicts = p.verdicts[1:]
	return d
}

// A transient refusal on an approved call writes no receipt, ERROR or END, and leaves the decision
// recoverable: the next Resume asks the hook again and, once it allows, runs the executor once.
// The hook sees the approved call and its committed decision.
func TestApprovalToolPolicyTransientRefusalKeepsDecision(t *testing.T) {
	for _, tc := range []struct {
		name    string
		verdict ToolPolicyDecision
	}{
		{"transient", ToolPolicyDecision{Verdict: ToolPolicyTransientRefusal, Reason: "token refresh failed"}},
		{"unspecified verdict", ToolPolicyDecision{}},
		{"unknown verdict", ToolPolicyDecision{Verdict: 99}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := decidedGateLog(t)
			effects := 0
			executor := func(context.Context, ToolCallContext, api.ToolCall) (api.ToolResult, error) {
				effects++
				return api.ToolResult{Output: map[string]any{"text": "done"}}, nil
			}
			policy := &policyRecorder{verdicts: []ToolPolicyDecision{tc.verdict}}
			c, err := New(log, nil, WithSessionUID("s"), WithToolExecutor(executor), WithToolPolicy(policy.hook))
			if err != nil {
				t.Fatal(err)
			}
			var lateErrs []error
			resumed, err := c.Resume(t.Context(), receiptHarness(&lateErrs))
			if !resumed || !errors.Is(err, ErrToolPolicyRefused) {
				t.Fatalf("refused Resume = %v, %v; want true, ErrToolPolicyRefused", resumed, err)
			}
			if !strings.Contains(err.Error(), tc.verdict.Reason) {
				t.Fatalf("refusal lost its reason: %v", err)
			}
			if effects != 0 {
				t.Fatalf("executor ran %d times after a refusal", effects)
			}
			want := []api.EventKind{api.EventExecutionStart, api.EventToolCall, api.EventApprovalRequest, api.EventApprovalResult}
			if got := journalKinds(t, log); !reflect.DeepEqual(got, want) {
				t.Fatalf("journal after transient refusal = %v, want %v", got, want)
			}
			for _, late := range lateErrs {
				if !errors.Is(late, ErrToolPolicyRefused) {
					t.Fatalf("sink not sealed after refusal: %v", late)
				}
			}
			if state, err := InspectApproval(log, 0); err != nil || state.Receipt != nil || state.Decision == nil {
				t.Fatalf("approved call not recoverable: %+v %v", state, err)
			}
			if len(policy.inputs) != 1 {
				t.Fatalf("hook calls = %d, want 1", len(policy.inputs))
			}
			in := policy.inputs[0]
			wantCall := *gateEvents()[1].ToolCall
			if in.Scope.SessionUID != "s" || in.ExecutionID != "e" || !reflect.DeepEqual(in.Call, wantCall) || in.Caller != (api.CallerIdentity{}) ||
				in.Approval == nil || !reflect.DeepEqual(in.Approval.Decision, api.ApprovalResult{ToolCallID: "c", Approved: true, RequestSeq: 3}) || in.Approval.DecisionSeq != 4 {
				t.Fatalf("hook input = %+v (approval %+v)", in, in.Approval)
			}

			resumed, err = c.Resume(t.Context(), receiptHarness(&lateErrs))
			if !resumed || err != nil {
				t.Fatalf("Resume after the refusal cleared = %v, %v", resumed, err)
			}
			receipt := journalReceipt(t, log)
			if len(policy.inputs) != 2 || effects != 1 || receipt == nil || receipt.IsError || receipt.Code != api.ToolResultCodeUnspecified {
				t.Fatalf("hook calls=%d effects=%d receipt=%+v", len(policy.inputs), effects, receipt)
			}
			if err := VerifyJournal(log); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A definitive refusal on an approved call records a POLICY_DENIED receipt bound to the decision,
// without calling the executor. Later Resumes serve it without asking the hook or the executor,
// and replay accepts it.
func TestApprovalToolPolicyDefinitiveRefusalIsDurable(t *testing.T) {
	log := decidedGateLog(t)
	effects := 0
	executor := func(context.Context, ToolCallContext, api.ToolCall) (api.ToolResult, error) {
		effects++
		return api.ToolResult{}, nil
	}
	policy := &policyRecorder{verdicts: []ToolPolicyDecision{{Verdict: ToolPolicyDefinitiveRefusal, Reason: "grant revoked"}}}
	c, err := New(log, nil, WithSessionUID("s"), WithToolExecutor(executor), WithToolPolicy(policy.hook))
	if err != nil {
		t.Fatal(err)
	}
	var lateErrs []error
	resumed, err := c.Resume(t.Context(), receiptHarness(&lateErrs))
	if !resumed || err != nil || len(lateErrs) != 0 {
		t.Fatalf("Resume = %v, %v (harness errors %v)", resumed, err, lateErrs)
	}
	receipt := journalReceipt(t, log)
	wantContent := api.TextMessage("tool", "Tool execution refused by host policy.").Parts
	if receipt == nil || !receipt.IsError || receipt.Code != api.ToolResultCodePolicyDenied || receipt.Error != "grant revoked" ||
		!reflect.DeepEqual(receipt.Content, wantContent) || receipt.ID != "c" || receipt.ApprovalRequestSeq != 3 || receipt.ApprovalDecisionSeq != 4 {
		t.Fatalf("receipt = %+v", receipt)
	}
	if effects != 0 || len(policy.inputs) != 1 {
		t.Fatalf("effects=%d hook calls=%d", effects, len(policy.inputs))
	}
	want := []api.EventKind{api.EventExecutionStart, api.EventToolCall, api.EventApprovalRequest, api.EventApprovalResult, api.EventToolResult, api.EventOutput, api.EventEnd}
	if got := journalKinds(t, log); !reflect.DeepEqual(got, want) {
		t.Fatalf("journal = %v, want %v", got, want)
	}
	if state, err := InspectApproval(log, 0); err != nil || state.Receipt == nil {
		t.Fatalf("receipt not bound to the approval: %+v %v", state, err)
	}

	before := journalKinds(t, log)
	if resumed, err := c.Resume(t.Context(), receiptHarness(&lateErrs)); resumed || err != nil {
		t.Fatalf("second Resume = %v, %v; want a no-op", resumed, err)
	}
	if effects != 0 || len(policy.inputs) != 1 || !reflect.DeepEqual(journalKinds(t, log), before) {
		t.Fatalf("receipted refusal re-asked or re-dispatched: effects=%d hook calls=%d", effects, len(policy.inputs))
	}
	outputs, err := c.Replay(t.Context(), receiptHarness(&lateErrs))
	if err != nil || !reflect.DeepEqual(outputs, []string{"code=3:true"}) {
		t.Fatalf("Replay = %v, %v", outputs, err)
	}
	if effects != 0 || len(policy.inputs) != 1 {
		t.Fatalf("replay reached the hook or executor: effects=%d hook calls=%d", effects, len(policy.inputs))
	}
	if err := VerifyJournal(log); err != nil {
		t.Fatal(err)
	}
}

// A denied approval never reaches the hook: the APPROVAL_DENIED receipt needs no policy check.
func TestApprovalToolPolicyNotAskedForDenial(t *testing.T) {
	log := gateLog(t, gateEvents()[:3]...)
	if _, err := Approve(log, api.ApprovalDecision{ExecutionID: "e", ToolCallID: "c", RequestSeq: 3, Approved: false}); err != nil {
		t.Fatal(err)
	}
	policy := &policyRecorder{verdicts: []ToolPolicyDecision{{Verdict: ToolPolicyDefinitiveRefusal}}}
	c, err := New(log, nil, WithSessionUID("s"), WithToolExecutor(func(context.Context, ToolCallContext, api.ToolCall) (api.ToolResult, error) {
		t.Error("executor called for a denial")
		return api.ToolResult{}, nil
	}), WithToolPolicy(policy.hook))
	if err != nil {
		t.Fatal(err)
	}
	var lateErrs []error
	if resumed, err := c.Resume(t.Context(), receiptHarness(&lateErrs)); !resumed || err != nil {
		t.Fatalf("Resume = %v, %v", resumed, err)
	}
	if receipt := journalReceipt(t, log); receipt == nil || receipt.Code != api.ToolResultCodeApprovalDenied || len(policy.inputs) != 0 {
		t.Fatalf("receipt=%+v hook calls=%d", receipt, len(policy.inputs))
	}
}

// Replay and recovery accept POLICY_DENIED only as the outcome of an approval, not of a denial.
func TestValidateApprovalReceiptPolicyDenied(t *testing.T) {
	approved := &api.ApprovalResult{ToolCallID: "c", Approved: true, RequestSeq: 3}
	denied := &api.ApprovalResult{ToolCallID: "c", RequestSeq: 3}
	for _, tc := range []struct {
		name     string
		decision *api.ApprovalResult
		result   api.ToolResult
		ok       bool
	}{
		{"approved policy denial", approved, api.ToolResult{IsError: true, Code: api.ToolResultCodePolicyDenied}, true},
		{"approved policy denial without is_error", approved, api.ToolResult{Code: api.ToolResultCodePolicyDenied}, false},
		{"denied decision", denied, api.ToolResult{IsError: true, Code: api.ToolResultCodePolicyDenied}, false},
		{"approved unknown code", approved, api.ToolResult{IsError: true, Code: 4}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateApprovalReceipt(tc.decision, &tc.result)
			if (err == nil) != tc.ok {
				t.Fatalf("validateApprovalReceipt = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

// resultHarness re-issues the gated call and keeps every result it is handed.
func resultHarness(results *[]api.ToolResult, errs *[]error) *approvalHarness {
	return &approvalHarness{fn: func(ctx context.Context, _ *api.Start, s api.EventSink) error {
		result, err := s.ToolCall(ctx, *gateEvents()[1].ToolCall)
		if err != nil {
			*errs = append(*errs, err)
			return err
		}
		*results = append(*results, result)
		return s.Output(ctx, "done")
	}}
}

// A hook's POLICY_DENIED receipt on an approved call is handed to the harness, served by later
// Resumes and by Replay exactly as the host wrote it.
func TestApprovalToolPolicyDeniedReceiptServedAsWritten(t *testing.T) {
	log := decidedGateLog(t)
	policy := &policyRecorder{verdicts: []ToolPolicyDecision{{Verdict: ToolPolicyDefinitiveRefusal, Reason: "grant revoked"}}}
	c, err := New(log, nil, WithSessionUID("s"), WithToolPolicy(policy.hook),
		WithToolExecutor(func(context.Context, ToolCallContext, api.ToolCall) (api.ToolResult, error) {
			t.Error("executor called after a definitive refusal")
			return api.ToolResult{}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	var live, replayed []api.ToolResult
	var errs []error
	if resumed, err := c.Resume(t.Context(), resultHarness(&live, &errs)); !resumed || err != nil {
		t.Fatalf("Resume = %v, %v (%v)", resumed, err, errs)
	}
	want := api.ToolResult{ID: "c", IsError: true, Code: api.ToolResultCodePolicyDenied, Error: "grant revoked",
		Content: api.TextMessage("tool", "Tool execution refused by host policy.").Parts, ApprovalRequestSeq: 3, ApprovalDecisionSeq: 4}
	if receipt := journalReceipt(t, log); receipt == nil || !reflect.DeepEqual(*receipt, want) {
		t.Fatalf("journaled receipt = %+v, want %+v", receipt, want)
	}
	decision := api.ApprovalResult{ToolCallID: "c", Approved: true, RequestSeq: 3}
	wantServed := want
	wantServed.Approval = &decision
	if len(live) != 1 || !reflect.DeepEqual(live[0], wantServed) {
		t.Fatalf("Resume handed the harness %+v, want %+v", live, wantServed)
	}
	if _, err := c.Replay(t.Context(), resultHarness(&replayed, &errs)); err != nil {
		t.Fatal(err)
	}
	if len(replayed) != 1 || !reflect.DeepEqual(replayed[0], wantServed) || len(policy.inputs) != 1 {
		t.Fatalf("Replay served %+v (hook calls %d), want %+v", replayed, len(policy.inputs), wantServed)
	}
	if err := VerifyJournal(log); err != nil {
		t.Fatal(err)
	}
}

// POLICY_DENIED is host owned on the approved path too: an executor reply carrying it is refused
// before the code is normalized, with no receipt, ERROR or END, and the decision stays recoverable.
// The next Resume calls the executor again under the same key.
func TestApprovalExecutorPolicyDeniedIsRejected(t *testing.T) {
	for _, tc := range []struct {
		name    string
		isError bool
		policy  bool
	}{
		{"is_error", true, false},
		{"not is_error", false, false},
		{"is_error with allowing hook", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := decidedGateLog(t)
			var keys []string
			replies := []api.ToolResult{{IsError: tc.isError, Code: api.ToolResultCodePolicyDenied, Error: "forged"}, {Output: map[string]any{"text": "done"}}}
			executor := func(_ context.Context, _ ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
				keys = append(keys, call.IdempotencyKey)
				r := replies[0]
				replies = replies[1:]
				return r, nil
			}
			opts := []Option{WithSessionUID("s"), WithToolExecutor(executor)}
			if tc.policy {
				opts = append(opts, WithToolPolicy((&policyRecorder{}).hook))
			}
			c, err := New(log, nil, opts...)
			if err != nil {
				t.Fatal(err)
			}
			var lateErrs []error
			resumed, err := c.Resume(t.Context(), receiptHarness(&lateErrs))
			if !resumed || !errors.Is(err, ErrApprovalReceiptReport) {
				t.Fatalf("Resume = %v, %v; want true, ErrApprovalReceiptReport", resumed, err)
			}
			want := []api.EventKind{api.EventExecutionStart, api.EventToolCall, api.EventApprovalRequest, api.EventApprovalResult}
			if got := journalKinds(t, log); !reflect.DeepEqual(got, want) {
				t.Fatalf("journal after a forged POLICY_DENIED = %v, want %v", got, want)
			}
			for _, late := range lateErrs {
				if !errors.Is(late, ErrApprovalReceiptReport) {
					t.Fatalf("sink not sealed after the rejection: %v", late)
				}
			}
			if state, err := InspectApproval(log, 0); err != nil || state.Receipt != nil || state.Decision == nil {
				t.Fatalf("approved call not recoverable: %+v %v", state, err)
			}

			if resumed, err := c.Resume(t.Context(), receiptHarness(&lateErrs)); !resumed || err != nil {
				t.Fatalf("Resume after the executor was fixed = %v, %v", resumed, err)
			}
			receipt := journalReceipt(t, log)
			if receipt == nil || receipt.IsError || receipt.Code != api.ToolResultCodeUnspecified || !reflect.DeepEqual(keys, []string{"k", "k"}) {
				t.Fatalf("receipt=%+v executor keys=%v", receipt, keys)
			}
			if err := VerifyJournal(log); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A hook that mutates nested arguments of an approved call, including typed containers a shallow
// clone would share with the journal's in-memory record, changes neither the call the executor
// runs nor the journal the decision was checked against.
func TestApprovalToolPolicyCannotChangeNestedArgs(t *testing.T) {
	typed := func() map[string]any {
		return map[string]any{"nested": map[string]string{"to": "original"}, "list": []string{"original"}}
	}
	events := gateEvents()[:3]
	call := *events[1].ToolCall
	call.Args = typed()
	events[1].ToolCall = &call
	log := gateLog(t, events...)
	if _, err := Approve(log, api.ApprovalDecision{ExecutionID: "e", ToolCallID: "c", RequestSeq: 3, Approved: true}); err != nil {
		t.Fatal(err)
	}
	var seen []map[string]any
	executor := func(_ context.Context, _ ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
		seen = append(seen, call.Args)
		return api.ToolResult{}, nil
	}
	hook := func(_ context.Context, in ToolPolicyInput) ToolPolicyDecision {
		for _, v := range in.Call.Args {
			switch v := v.(type) {
			case map[string]string:
				v["to"] = "mutated"
			case map[string]any:
				v["to"] = "mutated"
			case []string:
				v[0] = "mutated"
			case []any:
				v[0] = "mutated"
			}
		}
		return ToolPolicyDecision{Verdict: ToolPolicyAllow}
	}
	c, err := New(log, nil, WithSessionUID("s"), WithToolExecutor(executor), WithToolPolicy(hook))
	if err != nil {
		t.Fatal(err)
	}
	var lateErrs []error
	h := &approvalHarness{fn: func(ctx context.Context, _ *api.Start, s api.EventSink) error {
		if _, err := s.ToolCall(ctx, call); err != nil {
			lateErrs = append(lateErrs, err)
			return err
		}
		return s.Output(ctx, "done")
	}}
	if resumed, err := c.Resume(t.Context(), h); !resumed || err != nil {
		t.Fatalf("Resume = %v, %v (%v)", resumed, err, lateErrs)
	}
	if len(seen) != 1 || !reflect.DeepEqual(seen[0], typed()) {
		t.Fatalf("executor saw %#v, want %#v", seen, typed())
	}
	if err := VerifyJournal(log); err != nil {
		t.Fatalf("journal changed under the hook: %v", err)
	}
}

// An approved call whose journal form differs from what the executor receives (a number the
// journal's float64 numbers would round, in any spelling, a float32 whose JSON form is another
// float64, a string that is not valid UTF-8) or that the guard cannot compare (a struct written
// field by field, a value written by its own MarshalJSON method) is refused
// transiently before the hook, as on an ordinary call: no receipt, and the decision stays
// recoverable.
func TestApprovalToolPolicyRefusesInexactArgs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		amount any
	}{
		{"int64", int64(1<<53 + 1)},
		{"json.Number", json.Number("9007199254740993")},
		{"json.Number exponent", json.Number("9007199254740993e0")},
		{"json.Number decimal integer", json.Number("9007199254740993.0")},
		{"float32", float32(1e20)},
		{"invalid UTF-8", "a\xffb"},
		{"float32 in a struct", struct{ Rate float32 }{1e20}},
		{"pointer to a struct", &struct{ Amount int64 }{1<<53 + 1}},
		{"json.Marshaler written as another number", writtenOne(100)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := gateEvents()[:3]
			call := *events[1].ToolCall
			call.Args = map[string]any{"amount": tc.amount}
			events[1].ToolCall = &call
			log := gateLog(t, events...)
			if _, err := Approve(log, api.ApprovalDecision{ExecutionID: "e", ToolCallID: "c", RequestSeq: 3, Approved: true}); err != nil {
				t.Fatal(err)
			}
			effects := 0
			executor := func(context.Context, ToolCallContext, api.ToolCall) (api.ToolResult, error) {
				effects++
				return api.ToolResult{}, nil
			}
			policy := &policyRecorder{}
			c, err := New(log, nil, WithSessionUID("s"), WithToolExecutor(executor), WithToolPolicy(policy.hook))
			if err != nil {
				t.Fatal(err)
			}
			h := &approvalHarness{fn: func(ctx context.Context, _ *api.Start, s api.EventSink) error {
				if _, err := s.ToolCall(ctx, call); err != nil {
					return err
				}
				return s.Output(ctx, "done")
			}}
			if resumed, err := c.Resume(t.Context(), h); !resumed || !errors.Is(err, ErrToolPolicyRefused) || !strings.Contains(err.Error(), "cannot verify") {
				t.Fatalf("Resume = %v, %v; want true, ErrToolPolicyRefused for a value the guard cannot verify", resumed, err)
			}
			if effects != 0 || len(policy.inputs) != 0 {
				t.Fatalf("effects=%d hook calls=%d", effects, len(policy.inputs))
			}
			if state, err := InspectApproval(log, 0); err != nil || state.Receipt != nil || state.Decision == nil {
				t.Fatalf("approved call not recoverable: %+v %v", state, err)
			}
		})
	}
}
