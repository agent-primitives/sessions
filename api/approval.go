package api

import (
	"errors"
	"fmt"
)

// ApprovalRef identifies one host-owned approval request within the enclosing session.
// RequestSeq is the positive journal sequence, not a harness-chosen correlation number.
type ApprovalRef struct {
	ExecutionID string
	ToolCallID  string
	RequestSeq  int64
}

// ApprovalDecision is a caller's decision for one request. Identity records provenance only;
// it is not an authorization credential. The wire request uses explicit Approved presence so
// an omitted decision can be rejected; this domain value represents a supplied decision.
type ApprovalDecision struct {
	ExecutionID string
	ToolCallID  string
	RequestSeq  int64
	Approved    bool
	Reason      string
	Identity    IdentityRef
}

// ErrApprovalParked distinguishes a host-requested approval park from execution failure.
// A transport uses ApprovalParkedError.Ref, never error-string matching, for the park exchange.
var ErrApprovalParked = errors.New("approval parked")

// ApprovalParkedError carries the request correlation for a parked Run. It does not commit a
// request or decision; durable approval state belongs to the host's journal.
type ApprovalParkedError struct {
	Ref ApprovalRef
}

func (e *ApprovalParkedError) Error() string {
	return fmt.Sprintf("%v: execution=%q tool_call=%q request_seq=%d", ErrApprovalParked, e.Ref.ExecutionID, e.Ref.ToolCallID, e.Ref.RequestSeq)
}

func (e *ApprovalParkedError) Unwrap() error { return ErrApprovalParked }

// IsParkUnwind reports whether err contains only wrapped or joined parks for ref, with no
// independent failure elsewhere in the error tree. A matching park alone does not prove
// that the host has checkpointed compute or durably recorded a successful pause.
func IsParkUnwind(err error, ref ApprovalRef) bool {
	var parked *ApprovalParkedError
	if !errors.As(err, &parked) || parked == nil || parked.Ref != ref {
		return false
	}
	for err != nil {
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			for _, cause := range joined.Unwrap() {
				if !IsParkUnwind(cause, ref) {
					return false
				}
			}
			return true
		}
		err = errors.Unwrap(err)
	}
	return true
}
