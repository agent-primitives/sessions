package controller

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/sqlitelog"
)

// decidedGateLog returns a journal with an approved, decided call "c" and no receipt.
func decidedGateLog(t *testing.T) eventlog.Store {
	t.Helper()
	log := gateLog(t, gateEvents()[:3]...)
	if _, err := Approve(log, api.ApprovalDecision{ExecutionID: "e", ToolCallID: "c", RequestSeq: 3, Approved: true}); err != nil {
		t.Fatal(err)
	}
	return log
}

func journalKinds(t *testing.T, log eventlog.Store) []api.EventKind {
	t.Helper()
	recs, err := log.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]api.EventKind, 0, len(recs))
	for _, r := range recs {
		kinds = append(kinds, r.Event.Kind)
	}
	return kinds
}

func journalReceipt(t *testing.T, log eventlog.Store) *api.ToolResult {
	t.Helper()
	recs, err := log.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	var receipt *api.ToolResult
	for _, r := range recs {
		if r.Event.Kind == api.EventToolResult {
			if receipt != nil {
				t.Fatalf("more than one receipt: %+v", recs)
			}
			receipt = r.Event.Result
		}
	}
	return receipt
}

// receiptHarness re-issues the gated call and, on a receipt, outputs its code. A ToolCall error is
// swallowed and the harness tries to continue, the worst case for an unreceipted approved call.
func receiptHarness(lateErrs *[]error) *approvalHarness {
	return &approvalHarness{fn: func(ctx context.Context, _ *api.Start, s api.EventSink) error {
		result, err := s.ToolCall(ctx, *gateEvents()[1].ToolCall)
		if err != nil {
			*lateErrs = allGateMethods(context.Background(), s)
			return nil
		}
		return s.Output(ctx, fmt.Sprintf("code=%d:%v", result.Code, result.IsError))
	}}
}

var failedContent = api.TextMessage("tool", "Tool execution failed.").Parts

// A host cancellation while an approved call is in flight is not an executor outcome: no receipt,
// ERROR or END is journaled, and the next Resume re-drives the call once under the same key,
// whatever the durable dedup declaration says (APR-14).
func TestApprovalHostCancelLeavesCallPending(t *testing.T) {
	for _, dedup := range []bool{true, false} {
		t.Run(fmt.Sprintf("durable_dedup=%v", dedup), func(t *testing.T) {
			log := decidedGateLog(t)
			attempts := 0
			started := make(chan struct{})
			executor := func(ctx context.Context, scope ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
				attempts++
				if scope.SessionUID != "s" || call.IdempotencyKey != "k" {
					t.Errorf("redrive lost scope or key: %+v %+v", scope, call)
				}
				if attempts == 1 {
					close(started)
					<-ctx.Done()
					return api.ToolResult{}, fmt.Errorf("executor: %w", ctx.Err())
				}
				return api.ToolResult{Output: map[string]any{"text": "done"}}, nil
			}
			c, err := New(log, nil, WithSessionUID("s"), WithToolExecutor(executor), WithDurableDedup(func(string) bool { return dedup }))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			go func() { <-started; cancel() }()
			var lateErrs []error
			resumed, err := c.Resume(ctx, receiptHarness(&lateErrs))
			if !resumed || !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled Resume = %v, %v; want true, context.Canceled", resumed, err)
			}
			want := []api.EventKind{api.EventExecutionStart, api.EventToolCall, api.EventApprovalRequest, api.EventApprovalResult}
			if got := journalKinds(t, log); !reflect.DeepEqual(got, want) {
				t.Fatalf("journal after host cancellation = %v, want %v", got, want)
			}
			if len(lateErrs) == 0 {
				t.Fatal("harness did not observe the cancellation")
			}
			for _, late := range lateErrs {
				if !errors.Is(late, context.Canceled) {
					t.Fatalf("sink not sealed after host cancellation: %v", late)
				}
			}
			if state, err := InspectApproval(log, 0); err != nil || state.Receipt != nil || state.Decision == nil {
				t.Fatalf("approved call not pending: %+v %v", state, err)
			}

			resumed, err = c.Resume(t.Context(), receiptHarness(&lateErrs))
			if !resumed || err != nil {
				t.Fatalf("redrive Resume = %v, %v", resumed, err)
			}
			receipt := journalReceipt(t, log)
			if attempts != 2 || receipt == nil || receipt.IsError || receipt.Code != api.ToolResultCodeUnspecified || receipt.Output["text"] != "done" {
				t.Fatalf("redrive attempts=%d receipt=%+v", attempts, receipt)
			}
			if resumed, err := c.Resume(t.Context(), receiptHarness(&lateErrs)); resumed || err != nil || attempts != 2 {
				t.Fatalf("completed call re-driven: resumed=%v err=%v attempts=%d", resumed, err, attempts)
			}
			if err := VerifyJournal(log); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Only a host cancellation is spared a receipt. An executor error while the host context is live,
// or an unrelated error returned after it ended, is the executor's outcome and becomes a durable
// EXECUTOR_ERROR receipt with fixed model-facing Content; the Go error stays in Error.
func TestApprovalExecutorErrorReceipt(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cancelHost bool
		run        func(ctx context.Context) (api.ToolResult, error)
		wantError  string
	}{
		{
			name:      "host live",
			run:       func(context.Context) (api.ToolResult, error) { return api.ToolResult{}, errors.New("offline") },
			wantError: "offline",
		},
		{
			name: "host live, partial result discarded",
			run: func(context.Context) (api.ToolResult, error) {
				return api.ToolResult{Output: map[string]any{"partial": true}, Content: api.TextMessage("tool", "/srv/secret").Parts}, errors.New("offline")
			},
			wantError: "offline",
		},
		{
			name: "executor deadline, host live",
			run: func(ctx context.Context) (api.ToolResult, error) {
				own, cancel := context.WithTimeout(ctx, time.Nanosecond)
				defer cancel()
				<-own.Done()
				return api.ToolResult{}, fmt.Errorf("executor: %w", own.Err())
			},
			wantError: "executor: context deadline exceeded",
		},
		{
			name:       "unrelated error after host cancel",
			cancelHost: true,
			run: func(ctx context.Context) (api.ToolResult, error) {
				<-ctx.Done()
				return api.ToolResult{}, errors.New("executor crashed")
			},
			wantError: "executor crashed",
		},
		{
			name:       "executor deadline after host cancel",
			cancelHost: true,
			run: func(ctx context.Context) (api.ToolResult, error) {
				<-ctx.Done()
				return api.ToolResult{}, fmt.Errorf("executor: %w", context.DeadlineExceeded)
			},
			wantError: "executor: context deadline exceeded",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := decidedGateLog(t)
			attempts := 0
			started := make(chan struct{})
			c, err := New(log, nil, WithSessionUID("s"), WithToolExecutor(func(ctx context.Context, _ ToolCallContext, _ api.ToolCall) (api.ToolResult, error) {
				attempts++
				close(started)
				return tc.run(ctx)
			}))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancelHost {
				go func() { <-started; cancel() }()
			}
			var seen *api.ToolResult
			h := &approvalHarness{fn: func(ctx context.Context, _ *api.Start, s api.EventSink) error {
				result, err := s.ToolCall(ctx, *gateEvents()[1].ToolCall)
				if err != nil {
					return err
				}
				seen = &result
				return s.Output(ctx, "handled")
			}}
			resumed, err := c.Resume(ctx, h)
			if !resumed {
				t.Fatalf("Resume did not run: %v", err)
			}
			if tc.cancelHost != (err != nil) {
				t.Fatalf("Resume err = %v (host cancelled %v)", err, tc.cancelHost)
			}
			receipt := journalReceipt(t, log)
			for name, r := range map[string]*api.ToolResult{"journal": receipt, "harness": seen} {
				if r == nil {
					t.Fatalf("%s: no receipt", name)
				}
				if !r.IsError || r.Code != api.ToolResultCodeExecutorError || r.Error != tc.wantError || !reflect.DeepEqual(r.Content, failedContent) || r.Output != nil {
					t.Fatalf("%s receipt = %+v", name, r)
				}
			}
			if receipt.ApprovalRequestSeq != 3 || receipt.ApprovalDecisionSeq != 4 || receipt.Approval != nil {
				t.Fatalf("receipt correlation = %+v", receipt)
			}
			if resumed, err := c.Resume(t.Context(), h); err != nil || attempts != 1 {
				t.Fatalf("receipted failure re-driven: resumed=%v err=%v attempts=%d", resumed, err, attempts)
			}
		})
	}
}

// A receipt journaled before executor-error receipts carried Content stays as it was: the chain
// verifies, Replay and Resume serve it without the executor, and nothing rewrites it.
func TestApprovalLegacyExecutorErrorReceiptReplays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	journal, err := sqlitelog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	log := journal.Session("s")
	events := gateEvents()
	// The old writer's shape: no Content on an approved call's executor-error receipt.
	events[4].Result = &api.ToolResult{ID: "c", IsError: true, Code: api.ToolResultCodeExecutorError, Error: "offline", ApprovalRequestSeq: 3, ApprovalDecisionSeq: 4}
	gateAppend(t, log, events[:3]...)
	if _, err := Approve(log, api.ApprovalDecision{ExecutionID: "e", ToolCallID: "c", RequestSeq: 3, Approved: true}); err != nil {
		t.Fatal(err)
	}
	fence, err := log.NewFence()
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range []api.Event{events[4], {ExecutionID: "e", Kind: api.EventOutput, Message: api.TextMessage("assistant", "code=2:true")}, events[5]} {
		head, err := log.Head()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := log.Append(head, fence, ev); err != nil {
			t.Fatal(err)
		}
	}
	recorded, err := log.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	journal, err = sqlitelog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	log = journal.Session("s")
	if err := VerifyJournal(log); err != nil {
		t.Fatal(err)
	}

	c, err := New(log, nil, WithSessionUID("s"), WithToolExecutor(func(context.Context, ToolCallContext, api.ToolCall) (api.ToolResult, error) {
		t.Error("served receipt invoked the executor")
		return api.ToolResult{}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	var lateErrs []error
	outputs, err := c.Replay(t.Context(), receiptHarness(&lateErrs))
	if err != nil || len(outputs) != 1 || outputs[0] != "code=2:true" {
		t.Fatalf("replay = %v, %v", outputs, err)
	}
	if resumed, err := c.Resume(t.Context(), receiptHarness(&lateErrs)); resumed || err != nil {
		t.Fatalf("complete journal resumed: %v %v", resumed, err)
	}
	after, err := log.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(recorded) || after[len(after)-1].Hash != recorded[len(recorded)-1].Hash {
		t.Fatal("legacy journal changed")
	}
	if receipt := journalReceipt(t, log); receipt.Content != nil || receipt.Error != "offline" {
		t.Fatalf("legacy receipt rewritten: %+v", receipt)
	}
}

// A harness may narrow the context it passes to ToolCall (api.Harness). Cancelling that per-call
// context while Resume's context is live ends the call, not the turn, so the executor's error is a
// durable EXECUTOR_ERROR receipt and the call is not re-driven.
func TestApprovalHarnessCallCancelIsExecutorError(t *testing.T) {
	log := decidedGateLog(t)
	attempts := 0
	var cancelCall context.CancelFunc
	c, err := New(log, nil, WithSessionUID("s"), WithToolExecutor(func(ctx context.Context, _ ToolCallContext, _ api.ToolCall) (api.ToolResult, error) {
		attempts++
		cancelCall()
		<-ctx.Done()
		return api.ToolResult{}, fmt.Errorf("executor: %w", ctx.Err())
	}))
	if err != nil {
		t.Fatal(err)
	}
	var seen *api.ToolResult
	h := &approvalHarness{fn: func(ctx context.Context, _ *api.Start, s api.EventSink) error {
		callCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		cancelCall = cancel
		result, err := s.ToolCall(callCtx, *gateEvents()[1].ToolCall)
		if err != nil {
			return err
		}
		seen = &result
		return s.Output(ctx, "handled")
	}}
	if resumed, err := c.Resume(t.Context(), h); !resumed || err != nil {
		t.Fatalf("Resume = %v, %v; want the receipt delivered", resumed, err)
	}
	receipt := journalReceipt(t, log)
	for name, r := range map[string]*api.ToolResult{"journal": receipt, "harness": seen} {
		if r == nil || !r.IsError || r.Code != api.ToolResultCodeExecutorError || r.Error != "executor: context canceled" || !reflect.DeepEqual(r.Content, failedContent) {
			t.Fatalf("%s receipt = %+v", name, r)
		}
	}
	want := []api.EventKind{api.EventExecutionStart, api.EventToolCall, api.EventApprovalRequest, api.EventApprovalResult, api.EventToolResult, api.EventOutput, api.EventEnd}
	if got := journalKinds(t, log); !reflect.DeepEqual(got, want) {
		t.Fatalf("journal = %v, want %v", got, want)
	}
	if resumed, err := c.Resume(t.Context(), h); resumed || err != nil || attempts != 1 {
		t.Fatalf("receipted failure re-driven: resumed=%v err=%v attempts=%d", resumed, err, attempts)
	}
}

// deadlineCtx reports a fixed deadline over its embedded context. With a live context and a passed
// deadline it is the window in which a deadline enforced by I/O (a socket deadline) has fired but
// the context's timer has not.
type deadlineCtx struct {
	context.Context
	deadline time.Time
}

func (c deadlineCtx) Deadline() (time.Time, bool) { return c.deadline, true }

// A host deadline can surface as the executor's deadline error just before Resume's context
// reports it, or after the host cancelled. A passed host deadline plus a deadline error is the
// host's, so nothing is journaled and Resume returns the host error. Any other error, or a
// deadline error before the host deadline, stays an EXECUTOR_ERROR receipt, even after the host
// cancelled.
func TestApprovalHostDeadlinePassed(t *testing.T) {
	deadlineErr := fmt.Errorf("read: %w", context.DeadlineExceeded)
	for _, tc := range []struct {
		name      string
		deadline  time.Duration
		cancelled bool
		err       error
		wantErr   error // nil wants an EXECUTOR_ERROR receipt
	}{
		{name: "host deadline passed", deadline: -time.Second, err: deadlineErr, wantErr: context.DeadlineExceeded},
		{name: "host deadline passed, unrelated error", deadline: -time.Second, err: errors.New("offline")},
		{name: "host deadline ahead", deadline: time.Hour, err: deadlineErr},
		{name: "host cancelled, deadline passed", deadline: -time.Second, cancelled: true, err: deadlineErr, wantErr: context.Canceled},
		{name: "host cancelled, deadline ahead", deadline: time.Hour, cancelled: true, err: deadlineErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := decidedGateLog(t)
			hostCtx, cancel := context.WithCancel(t.Context())
			defer cancel()
			c, err := New(log, nil, WithSessionUID("s"), WithToolExecutor(func(context.Context, ToolCallContext, api.ToolCall) (api.ToolResult, error) {
				if tc.cancelled {
					cancel() // the host ends the turn while the executor runs
				}
				return api.ToolResult{}, tc.err
			}))
			if err != nil {
				t.Fatal(err)
			}
			h := &approvalHarness{fn: func(ctx context.Context, _ *api.Start, s api.EventSink) error {
				if _, err := s.ToolCall(ctx, *gateEvents()[1].ToolCall); err != nil {
					return err
				}
				return s.Output(ctx, "handled")
			}}
			resumed, err := c.Resume(deadlineCtx{Context: hostCtx, deadline: time.Now().Add(tc.deadline)}, h)
			receipt := journalReceipt(t, log)
			if tc.wantErr != nil {
				if !resumed || !errors.Is(err, tc.wantErr) || receipt != nil {
					t.Fatalf("Resume = %v, %v, receipt %+v; want %v and no receipt", resumed, err, receipt, tc.wantErr)
				}
				if state, err := InspectApproval(log, 0); err != nil || state.Receipt != nil || state.Decision == nil {
					t.Fatalf("approved call not pending: %+v %v", state, err)
				}
				return
			}
			if !resumed || (!tc.cancelled && err != nil) || receipt == nil || receipt.Code != api.ToolResultCodeExecutorError || receipt.Error != tc.err.Error() || !reflect.DeepEqual(receipt.Content, failedContent) {
				t.Fatalf("Resume = %v, %v, receipt %+v; want EXECUTOR_ERROR", resumed, err, receipt)
			}
		})
	}
}

// The host classifies an executor error by the recovery context's state when the executor returns,
// not by which context ended the call. A per-call deadline that expires first still yields a
// deadline error the host cannot tell from its own: returned while Resume's context is live it is
// an EXECUTOR_ERROR receipt, returned after Resume's deadline it leaves the call pending (APR-14).
// After Resume is cancelled instead, the per-call deadline error matches neither rule and is
// receipted.
func TestApprovalCallDeadlineBeforeHostEnds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		host    func(context.Context) (context.Context, context.CancelFunc)
		wait    bool  // the executor returns only after Resume's context is done
		wantErr error // nil wants an EXECUTOR_ERROR receipt
	}{
		{name: "returned while host live", host: func(ctx context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(ctx, time.Hour)
		}},
		{name: "returned after host deadline", wait: true, wantErr: context.DeadlineExceeded, host: func(ctx context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(ctx, 200*time.Millisecond)
		}},
		{name: "returned after host cancel", wait: true, host: func(ctx context.Context) (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithTimeout(ctx, time.Hour)
			go func() { time.Sleep(200 * time.Millisecond); cancel() }()
			return ctx, cancel
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := decidedGateLog(t)
			hostCtx, cancel := tc.host(t.Context())
			defer cancel()
			attempts := 0
			c, err := New(log, nil, WithSessionUID("s"), WithToolExecutor(func(ctx context.Context, _ ToolCallContext, _ api.ToolCall) (api.ToolResult, error) {
				attempts++
				if attempts > 1 {
					return api.ToolResult{Content: api.TextMessage("tool", "done").Parts}, nil
				}
				<-ctx.Done()
				if hostCtx.Err() != nil {
					t.Error("host context ended before the per-call deadline")
				}
				if tc.wait {
					<-hostCtx.Done()
				}
				return api.ToolResult{}, fmt.Errorf("executor: %w", ctx.Err())
			}))
			if err != nil {
				t.Fatal(err)
			}
			h := &approvalHarness{fn: func(ctx context.Context, _ *api.Start, s api.EventSink) error {
				callCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
				defer cancel()
				if _, err := s.ToolCall(callCtx, *gateEvents()[1].ToolCall); err != nil {
					return err
				}
				return s.Output(ctx, "handled")
			}}
			resumed, err := c.Resume(hostCtx, h)
			receipt := journalReceipt(t, log)
			if tc.wantErr == nil {
				if !resumed || receipt == nil || receipt.Code != api.ToolResultCodeExecutorError || receipt.Error != "executor: context deadline exceeded" || !reflect.DeepEqual(receipt.Content, failedContent) {
					t.Fatalf("Resume = %v, %v, receipt %+v; want EXECUTOR_ERROR", resumed, err, receipt)
				}
				return
			}
			if !resumed || !errors.Is(err, tc.wantErr) || receipt != nil {
				t.Fatalf("Resume = %v, %v, receipt %+v; want %v and no receipt", resumed, err, receipt, tc.wantErr)
			}
			if resumed, err := c.Resume(t.Context(), h); !resumed || err != nil || attempts != 2 {
				t.Fatalf("re-drive: resumed=%v err=%v attempts=%d; want one more attempt", resumed, err, attempts)
			}
			if receipt := journalReceipt(t, log); receipt == nil || receipt.IsError {
				t.Fatalf("re-driven receipt = %+v; want success", receipt)
			}
		})
	}
}
