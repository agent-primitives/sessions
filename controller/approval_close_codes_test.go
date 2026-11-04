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

// Replay and recovery accept NOT_EXECUTED only after a denial and OUTCOME_UNKNOWN only after an
// approval, each with is_error: the receipts CloseApproval writes.
func TestValidateApprovalReceiptCloseCodes(t *testing.T) {
	approved := &api.ApprovalResult{ToolCallID: "c", Approved: true, RequestSeq: 3}
	denied := &api.ApprovalResult{ToolCallID: "c", RequestSeq: 3}
	for _, tc := range []struct {
		name     string
		decision *api.ApprovalResult
		result   api.ToolResult
		ok       bool
	}{
		{"denied not executed", denied, api.ToolResult{IsError: true, Code: api.ToolResultCodeNotExecuted}, true},
		{"approved outcome unknown", approved, api.ToolResult{IsError: true, Code: api.ToolResultCodeOutcomeUnknown}, true},
		{"denied not executed without is_error", denied, api.ToolResult{Code: api.ToolResultCodeNotExecuted}, false},
		{"approved outcome unknown without is_error", approved, api.ToolResult{Code: api.ToolResultCodeOutcomeUnknown}, false},
		{"approved not executed", approved, api.ToolResult{IsError: true, Code: api.ToolResultCodeNotExecuted}, false},
		{"denied outcome unknown", denied, api.ToolResult{IsError: true, Code: api.ToolResultCodeOutcomeUnknown}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateApprovalReceipt(tc.decision, &tc.result)
			if (err == nil) != tc.ok {
				t.Fatalf("validateApprovalReceipt = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

var closeCodes = []api.ToolResultCode{api.ToolResultCodeNotExecuted, api.ToolResultCodeOutcomeUnknown}

// The close codes are host owned: a harness report or the reply to an ordinary host-executed call
// carrying one is refused, and nothing is journaled for it.
func TestCloseCodesRefusedFromReportsAndOrdinaryReplies(t *testing.T) {
	for _, code := range closeCodes {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			if err := approvalReportError(api.ToolResult{Code: code, IsError: true}); !errors.Is(err, ErrApprovalReceiptReport) {
				t.Fatalf("approvalReportError = %v, want ErrApprovalReceiptReport", err)
			}
			call := api.ToolCall{ID: "o", Tool: "send", Mediation: api.MediationControllerMediated, IdempotencyKey: "k"}
			for name, h := range map[string]*approvalHarness{
				"ordinary reply": {fn: func(ctx context.Context, _ *api.Start, s api.EventSink) error {
					_, err := s.ToolCall(ctx, call)
					return err
				}},
				"report": {fn: func(ctx context.Context, _ *api.Start, s api.EventSink) error {
					return s.Report(ctx, api.ToolResult{ID: "r", Code: code, IsError: true})
				}},
			} {
				log := eventlog.AsStore(eventlog.New())
				c, err := New(log, nil, WithSessionUID("s"), WithToolExecutor(func(context.Context, ToolCallContext, api.ToolCall) (api.ToolResult, error) {
					return api.ToolResult{Code: code, IsError: true}, nil
				}))
				if err != nil {
					t.Fatal(err)
				}
				if err := c.Exec(t.Context(), h, nil, 0); !errors.Is(err, ErrApprovalReceiptReport) {
					t.Fatalf("%s: Exec = %v, want ErrApprovalReceiptReport", name, err)
				}
				for _, kind := range journalKinds(t, log) {
					if kind == api.EventToolResult {
						t.Fatalf("%s: a close code reached the journal", name)
					}
				}
			}
		})
	}
}

// An approved call's executor cannot write a close code: its reply is an executor failure and
// becomes an EXECUTOR_ERROR receipt with is_error, whatever is_error the executor set (APR-12). A
// reply that claimed success carries no failure content, so the receipt gets the fixed failure
// text and drops the success output (APR-14); a failure reply keeps its content and error.
func TestApprovalExecutorCloseCodeIsNormalized(t *testing.T) {
	content := api.TextMessage("tool", "executor said").Parts
	for _, code := range closeCodes {
		for _, tc := range []struct {
			name    string
			isError bool
			err     string // the executor's Error
			want    string // the receipt's Error
		}{
			{"is_error=true", true, "forged", "forged"},
			{"is_error=false", false, "", executorHostOwnedCode},
			{"is_error=false with error", false, "forged", "forged"},
		} {
			t.Run(fmt.Sprintf("%v/%s", code, tc.name), func(t *testing.T) {
				log := decidedGateLog(t)
				attempts := 0
				c, err := New(log, nil, WithSessionUID("s"), WithToolExecutor(func(context.Context, ToolCallContext, api.ToolCall) (api.ToolResult, error) {
					attempts++
					return api.ToolResult{Code: code, IsError: tc.isError, Error: tc.err, Output: map[string]any{"text": "done"}, Content: content}, nil
				}))
				if err != nil {
					t.Fatal(err)
				}
				var lateErrs []error
				if resumed, err := c.Resume(t.Context(), receiptHarness(&lateErrs)); !resumed || err != nil {
					t.Fatalf("Resume = %v, %v", resumed, err)
				}
				receipt := journalReceipt(t, log)
				if receipt == nil || receipt.Code != api.ToolResultCodeExecutorError || !receipt.IsError || receipt.Error != tc.want {
					t.Fatalf("receipt = %+v, want EXECUTOR_ERROR with is_error and error %q", receipt, tc.want)
				}
				if tc.isError {
					if !reflect.DeepEqual(receipt.Content, content) || receipt.Output["text"] != "done" {
						t.Fatalf("failure reply not kept: %+v", receipt)
					}
				} else if !reflect.DeepEqual(receipt.Content, failedContent) || receipt.Output != nil {
					t.Fatalf("claimed success not replaced by the fixed failure: %+v", receipt)
				}
				if resumed, err := c.Resume(t.Context(), receiptHarness(&lateErrs)); resumed || err != nil || attempts != 1 {
					t.Fatalf("receipted call re-driven: resumed=%v err=%v attempts=%d", resumed, err, attempts)
				}
				if err := VerifyJournal(log); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
