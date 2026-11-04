package controller

import (
	"fmt"
	"math"
	"time"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
)

// ApprovalCloseReason is the reason of the denial CloseApproval writes for an undecided gate. It
// marks the decision as a close rather than an approver's denial; an Approve(false) that supplies
// this exact reason is indistinguishable from it.
const ApprovalCloseReason = "approval closed"

// The fixed Error and model-facing Content of the receipts CloseApproval writes.
const (
	closedNotExecuted    = "Tool call was closed without running."
	closedOutcomeUnknown = "Tool call was closed; whether it ran is unknown."
)

// CloseApproval closes the approval gate ref names with a host receipt, so a session whose
// harness cannot re-issue the gated call can take a new turn (Exec refuses one with
// ErrApprovalBlocked while the gate has no receipt). It never calls an executor or a harness.
//
// An undecided gate gets a denial with reason ApprovalCloseReason and closer as its verified
// approver, then a NOT_EXECUTED receipt. A denied gate gets a NOT_EXECUTED receipt. An approved
// gate with no receipt gets OUTCOME_UNKNOWN: the journal cannot show that no executor call
// happened, and the receipt revokes the approval for any later recovery. A gate that already has
// a receipt returns its decision and receipt without a fence or an append, whatever wrote them.
//
// Validation, chain verification and the scan happen before NewFence, so a refused or repeated
// close does not evict an existing writer. A missing, cut, inherited, superseded or completed gate
// returns ErrInvalidApprovalDecision, a tampered journal ErrJournalTampered. Appends use the
// scanned head for CAS, so a concurrent writer makes the close fail with ErrConflict or
// ErrFenced; if a denial committed before a failed receipt append, a retry closes the denied gate.
// Callers must serialize this operation with other session work, as for Approve.
func CloseApproval(log eventlog.Store, ref api.ApprovalRef, closer api.CallerIdentity) (decision, receipt eventlog.Record, err error) {
	// Durable deletion authority applies even to retries that would write nothing.
	if err := eventlog.CheckWritable(log); err != nil {
		return eventlog.Record{}, eventlog.Record{}, err
	}
	if ref.ExecutionID == "" || ref.ToolCallID == "" || ref.RequestSeq <= 0 {
		return eventlog.Record{}, eventlog.Record{}, fmt.Errorf("%w: execution, call and positive request sequence are required", ErrInvalidApprovalDecision)
	}
	if closer != (api.CallerIdentity{}) && !closer.Valid() {
		return eventlog.Record{}, eventlog.Record{}, fmt.Errorf("controller: closer: %w", api.ErrInvalidCallerIdentity)
	}
	records, err := readVerifiedPrefix(log, math.MaxInt64)
	if err != nil {
		return eventlog.Record{}, eventlog.Record{}, err
	}
	scan, err := scanApprovalRecords(records, 0)
	if err != nil {
		return eventlog.Record{}, eventlog.Record{}, err
	}
	var target *ApprovalState
	for _, state := range scan.states {
		if state.ExecutionID == ref.ExecutionID && state.Call.ID == ref.ToolCallID && state.Request != nil && state.Request.Seq == ref.RequestSeq {
			target = state
			break
		}
	}
	if target == nil || target.Inherited {
		return eventlog.Record{}, eventlog.Record{}, fmt.Errorf("%w: request is not owned by this session", ErrInvalidApprovalDecision)
	}
	if target.Receipt != nil {
		// The scan only binds a receipt to a gate with a decision.
		return *target.Decision, *target.Receipt, nil
	}
	if target != scan.selected || target.Completed {
		return eventlog.Record{}, eventlog.Record{}, fmt.Errorf("%w: request is superseded or completed", ErrInvalidApprovalDecision)
	}
	fence, err := log.NewFence()
	if err != nil {
		return eventlog.Record{}, eventlog.Record{}, err
	}
	head := scan.head
	if target.Decision == nil {
		decision, err = log.Append(head, fence, api.Event{
			ExecutionID: ref.ExecutionID, Timestamp: time.Now().UTC(), Kind: api.EventApprovalResult,
			ApprovalResult: &api.ApprovalResult{
				ToolCallID: ref.ToolCallID, RequestSeq: ref.RequestSeq,
				Reason: ApprovalCloseReason, VerifiedApprover: closer,
			},
		})
		if err != nil {
			return eventlog.Record{}, eventlog.Record{}, err
		}
		head = decision.Seq
	} else {
		decision = *target.Decision
	}
	code, text := api.ToolResultCodeNotExecuted, closedNotExecuted
	if decision.Event.ApprovalResult.Approved {
		code, text = api.ToolResultCodeOutcomeUnknown, closedOutcomeUnknown
	}
	receipt, err = log.Append(head, fence, api.Event{
		ExecutionID: ref.ExecutionID, Timestamp: time.Now().UTC(), Kind: api.EventToolResult,
		Result: &api.ToolResult{
			ID: ref.ToolCallID, IsError: true, Code: code, Error: text, Content: api.TextMessage("tool", text).Parts,
			ApprovalRequestSeq: ref.RequestSeq, ApprovalDecisionSeq: decision.Seq,
		},
	})
	if err != nil {
		return eventlog.Record{}, eventlog.Record{}, err
	}
	return decision, receipt, nil
}
