package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
)

// ErrApprovalBlocked rejects a new turn while this session owns an approval intent without receipt.
var ErrApprovalBlocked = errors.New("controller: unresolved approval blocks a new execution")

// ErrApprovalUnavailable is returned by Exec, Resume and Replay for approval on a
// non-STATELESS_REPLAY harness, or when replay/recovery reproduces a historical
// pre-intent approval rejection without consuming a different call's evidence.
var ErrApprovalUnavailable = errors.New("controller: approval requires STATELESS_REPLAY")

// ErrInvalidApprovalCall rejects missing or reused gated call identities before intent is written.
var ErrInvalidApprovalCall = errors.New("controller: invalid approval call identity")

// ErrApprovalReceiptReport rejects harness reports and ordinary executor replies carrying
// host-owned approval receipt fields or unsupported result codes, and an approved call's executor
// reply carrying POLICY_DENIED.
var ErrApprovalReceiptReport = errors.New("controller: approval receipts are host owned")

// ErrMissingToolExecutor is a recoverable host precondition, not an executor-failure receipt.
var ErrMissingToolExecutor = errors.New("controller: no tool executor configured")

// executorFailure is the model-facing Content of an approved call's EXECUTOR_ERROR receipt when
// the executor returned a Go error. The error text is kept in ToolResult.Error only.
const executorFailure = "Tool execution failed."

// hostEnded returns the error that ended the host's turn when the executor's err is that ending,
// and nil for an executor outcome. Only the state of the context Exec or Resume received, when the
// executor returns, counts. The turn ended when the host context is done and err is its error, or
// when the host deadline has passed and err is a deadline error: a host deadline can surface
// through I/O (a socket deadline) just before the context's own timer marks it done. Errors match
// by kind: an error from a per-call context the harness passed to ToolCall is the host's only when
// it is the same kind as the host's ending, even if the per-call deadline expired first, and an
// executor outcome otherwise.
func hostEnded(host context.Context, err error) error {
	hostErr := host.Err()
	if hostErr != nil && errors.Is(err, hostErr) {
		return hostErr
	}
	if deadline, ok := host.Deadline(); ok && !time.Now().Before(deadline) && errors.Is(err, context.DeadlineExceeded) {
		if hostErr != nil {
			return hostErr
		}
		return context.DeadlineExceeded
	}
	return nil
}

func approvalPark(state *ApprovalState) error {
	return &api.ApprovalParkedError{Ref: api.ApprovalRef{ExecutionID: state.ExecutionID, ToolCallID: state.Call.ID, RequestSeq: state.Request.Seq}}
}

func approvalReportError(result api.ToolResult) error {
	if result.Approval != nil || result.ApprovalRequestSeq != 0 || result.ApprovalDecisionSeq != 0 || (result.Code != api.ToolResultCodeUnspecified && result.Code != api.ToolResultCodeExecutorError) {
		return ErrApprovalReceiptReport
	}
	return nil
}

// approvalDescriptor is called only on the gate path. Ordinary identity-less histories must not
// acquire a new Describe dependency; modern invocations reuse their preflight descriptor.
func (s *liveSink) approvalDescriptor(ctx context.Context) error {
	if s.desc == nil {
		desc, err := describeHarness(ctx, s.har)
		if err != nil {
			return err
		}
		s.desc = &desc
	}
	if s.desc.Capabilities.Resumability != api.ResumabilityStatelessReplay {
		return ErrApprovalUnavailable
	}
	return nil
}

func (s *liveSink) parkTool(ctx context.Context, call api.ToolCall) (api.ToolResult, error) {
	if err := s.approvalDescriptor(ctx); err != nil {
		return api.ToolResult{}, err
	}
	if s.c.sessionUID == "" {
		return api.ToolResult{}, ErrMissingSessionUID
	}
	if s.legacyRecovery || call.ID == "" {
		return api.ToolResult{}, ErrInvalidApprovalCall
	}
	if call.IdempotencyKey == "" {
		return api.ToolResult{}, ErrMissingIdempotencyKey
	}
	if _, exists := s.calls[call.ID]; exists {
		return api.ToolResult{}, ErrInvalidApprovalCall
	}
	if _, err := s.append(api.Event{Kind: api.EventToolCall, ToolCall: &call}); err != nil {
		return api.ToolResult{}, err
	}
	s.calls[call.ID] = true
	// After intent commits, any failure must stop continuation: a handled append error must not
	// leave fallback effects or END beyond an open approval intent.
	req, err := s.append(api.Event{Kind: api.EventApprovalRequest, Approval: &api.ApprovalRequest{ToolCallID: call.ID}})
	if err != nil {
		s.guard.stop = err
		return api.ToolResult{}, err
	}
	s.guard.stop = &api.ApprovalParkedError{Ref: api.ApprovalRef{ExecutionID: s.executionID, ToolCallID: call.ID, RequestSeq: req.Seq}}
	return api.ToolResult{}, s.guard.stop
}

// completeApproval executes only a scanned, committed decision. The executor gets the original
// session/key namespace; after an uncertain receipt write or a host cancellation it must durably
// deduplicate, because the next Resume calls it again with the same key.
func (s *liveSink) completeApproval(ctx context.Context, state *ApprovalState) (api.ToolResult, error) {
	if err := s.approvalDescriptor(ctx); err != nil {
		s.guard.stop = err
		return api.ToolResult{}, err
	}
	if s.c.sessionUID == "" {
		s.guard.stop = ErrMissingSessionUID
		return api.ToolResult{}, s.guard.stop
	}
	decision := state.Decision.Event.ApprovalResult
	var result api.ToolResult
	if !decision.Approved {
		const denial = "Tool execution denied by approval decision."
		result = api.ToolResult{IsError: true, Code: api.ToolResultCodeApprovalDenied, Error: denial, Content: api.TextMessage("tool", denial).Parts}
	} else {
		if s.c.tool == nil {
			s.guard.stop = ErrMissingToolExecutor
			return api.ToolResult{}, s.guard.stop
		}
		call, err := approvedCall(state)
		if err != nil {
			// The call no longer matches what was approved. Refuse before dialing and write no
			// receipt: this is evidence failure, not an executor outcome.
			s.guard.stop = err
			return api.ToolResult{}, err
		}
		denied, err := s.checkToolPolicy(ctx, call, &ToolPolicyApproval{Decision: *decision, DecisionSeq: state.Decision.Seq})
		if err != nil {
			// A transient refusal is a host precondition, like a missing executor: no receipt,
			// and the decision stays recoverable for the next Resume.
			s.guard.stop = err
			return api.ToolResult{}, err
		}
		if denied != nil {
			result = *denied
		} else {
			result, err = s.c.tool(ctx, ToolCallContext{SessionUID: s.c.sessionUID}, call)
			s.c.liveToolCalls++
			if err != nil {
				// A host-caused cancellation is not an executor outcome. Leave the approved
				// call without a receipt, as a crash at this point would, so the next Resume
				// re-drives it under the same key. The stop keeps the harness from continuing
				// past the open call.
				if hostErr := hostEnded(s.runCtx, err); hostErr != nil {
					s.guard.stop = hostErr
					return api.ToolResult{}, hostErr
				}
				// Like the ordinary path, a Go error discards any partial result. The error
				// text stays in Error; the model-facing Content is fixed so it never carries
				// host detail.
				result = api.ToolResult{IsError: true, Error: err.Error(), Content: api.TextMessage("tool", executorFailure).Parts}
			}
			if result.Code == api.ToolResultCodePolicyDenied {
				// POLICY_DENIED is host owned (TOOL-16): only the policy hook's refusal writes it.
				// Reject the reply before the code is normalized, as ordinary dispatch does, and
				// write no receipt: the decision stays recoverable and the next Resume calls the
				// executor again under the same key.
				s.guard.stop = ErrApprovalReceiptReport
				return api.ToolResult{}, s.guard.stop
			}
			result.Code = api.ToolResultCodeUnspecified
			if result.IsError {
				result.Code = api.ToolResultCodeExecutorError
			}
		}
	}
	result.ID = state.Call.ID
	result.ApprovalRequestSeq = state.Request.Seq
	result.ApprovalDecisionSeq = state.Decision.Seq
	result.Approval = nil
	receipt := result // Append may retain this pointer; transient reply decoration must stay separate.
	if _, err := s.append(api.Event{Kind: api.EventToolResult, Result: &receipt}); err != nil {
		s.guard.stop = err
		return api.ToolResult{}, err
	}
	// A decision is transient reply data only after its receipt commits. Return a copy so harness
	// code cannot change the authority kept in scanned evidence.
	approval := *decision
	approval.VerifiedApprover = api.CallerIdentity{} // host audit state, never sent to a harness (harnessHistory)
	result.Approval = &approval
	return result, nil
}

// approvedCall returns a copy of the call an approved decision authorizes, for the executor. The
// decision names the request that follows the journaled TOOL_CALL record in the verified hash
// chain, so that record's content_hash, computed over the call's RFC 8785 canonical arguments with
// its ID, tool, mediation and idempotency key, is the approved hash. Recomputing it over the copy
// right before dispatch refuses arguments that changed after the journal was verified, such as a
// map the in-memory store shares with a harness that mutated it; the executor receives exactly
// that copy. A mismatch wraps ErrJournalTampered.
func approvedCall(state *ApprovalState) (api.ToolCall, error) {
	record := state.callRecord
	if record.Seq <= 0 || record.Event.Kind != api.EventToolCall || record.Event.ToolCall != state.Call {
		return api.ToolCall{}, fmt.Errorf("%w: approved call has no journaled record", ErrReplayDiverged)
	}
	call := *state.Call
	call.Args = cloneArgs(call.Args)
	record.Event.ToolCall = &call
	if err := verifyRecords(record.PrevHash, []eventlog.Record{record}); err != nil {
		return api.ToolCall{}, fmt.Errorf("approved tool call does not match its journaled record: %w", err)
	}
	return call, nil
}

func approvalEvidence(scan *approvalScan, executionID string) map[string]*ApprovalState {
	states := make(map[string]*ApprovalState)
	for _, state := range scan.states {
		if state.ExecutionID == executionID {
			states[state.Call.ID] = state
		}
	}
	return states
}

// consumeApprovalPrefix uses the scanned record envelopes for authority; stream position only
// establishes deterministic ordering. Pending gates are intercepted by Resume before Run.
func consumeApprovalPrefix(stream []api.Event, index *int, state *ApprovalState) error {
	if state == nil || state.Request == nil || state.Decision == nil {
		return fmt.Errorf("%w: missing approval decision evidence", ErrReplayDiverged)
	}
	for _, kind := range []api.EventKind{api.EventApprovalRequest, api.EventApprovalResult} {
		if *index >= len(stream) || stream[*index].Kind != kind {
			return fmt.Errorf("%w: missing recorded %s", ErrReplayDiverged, kind)
		}
		(*index)++
	}
	return nil
}

func serveApproval(state *ApprovalState, receipt *api.ToolResult) (api.ToolResult, error) {
	if state == nil || state.Request == nil || state.Decision == nil || state.Receipt == nil {
		return api.ToolResult{}, fmt.Errorf("%w: incomplete approval receipt evidence", ErrReplayDiverged)
	}
	result, err := recordedToolResult(state.Call, receipt)
	if err != nil {
		return api.ToolResult{}, err
	}
	if result.ApprovalRequestSeq != state.Request.Seq || result.ApprovalDecisionSeq != state.Decision.Seq {
		return api.ToolResult{}, fmt.Errorf("%w: approval receipt sequence mismatch", ErrReplayDiverged)
	}
	if err := validateApprovalReceipt(state.Decision.Event.ApprovalResult, &result); err != nil {
		return api.ToolResult{}, err
	}
	decision := *state.Decision.Event.ApprovalResult
	decision.VerifiedApprover = api.CallerIdentity{}
	result.Approval = &decision
	return result, nil
}

// harnessHistory returns the journal events a harness receives as Start.History. A recorded
// decision's verified approver is host audit state, so it is cleared on a copy of each
// APPROVAL_RESULT that carries one; the journaled events, whose hashes cover it, are never
// modified. Resume and Replay pass a prefix of the whole journal, so the result's capacity is
// capped at its length: an in-process harness that reslices Start.History to its capacity must not
// reach later records, such as the current execution's decision. Without an approver in the
// prefix, the input's backing array is reused.
func harnessHistory(events []api.Event) []api.Event {
	var out []api.Event
	for i, event := range events {
		if event.ApprovalResult == nil || event.ApprovalResult.VerifiedApprover == (api.CallerIdentity{}) {
			continue
		}
		if out == nil {
			out = make([]api.Event, len(events))
			copy(out, events)
		}
		decision := *event.ApprovalResult
		decision.VerifiedApprover = api.CallerIdentity{}
		out[i].ApprovalResult = &decision
	}
	if out == nil {
		return events[:len(events):len(events)]
	}
	return out
}

// pendingApproval repairs a call-only cut with the captured scan head and existing fence.
// It does not need a harness or executor, and never mints a fence on invalid evidence.
func (c *Controller) pendingApproval(scan *approvalScan, executionID string) (bool, error) {
	state := scan.selected
	if state == nil || state.ExecutionID != executionID || state.Receipt != nil {
		return false, nil
	}
	if state.Inherited {
		return false, ErrInheritedToolIntent
	}
	if state.Request == nil {
		r, err := c.log.Append(scan.head, c.fence, api.Event{ExecutionID: state.ExecutionID, Kind: api.EventApprovalRequest, Approval: &api.ApprovalRequest{ToolCallID: state.Call.ID}})
		if err != nil {
			return true, err
		}
		c.observe(r)
		state.Request = &r
	}
	if state.Decision == nil {
		return true, approvalPark(state)
	}
	return false, nil
}
