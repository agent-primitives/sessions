package session

import (
	"errors"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/placement"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// statusCause chooses the boundary code without accepting a backend transport status,
// while retaining the original cause for direct Go callers. Causes do not cross RPCs.
type statusCause struct {
	cause  error
	status *status.Status
}

func (e *statusCause) Error() string              { return e.status.Err().Error() }
func (e *statusCause) Unwrap() error              { return e.cause }
func (e *statusCause) GRPCStatus() *status.Status { return e.status }

func withStatusCause(code codes.Code, err error) error {
	return statusError(code, err, err.Error())
}

// statusError constructs fresh host-owned details: backend codes, reasons and metadata
// are not authoritative. The operation mapper still owns code and message precedence.
func statusError(code codes.Code, err error, message string) *statusCause {
	st := status.New(code, message)
	if reason := errorReason(code, err); reason != "" {
		// These fixed, valid ErrorInfo strings always marshal successfully.
		withDetails, detailErr := st.WithDetails(&errdetails.ErrorInfo{Domain: "agentsessions", Reason: reason})
		if detailErr == nil {
			st = withDetails
		}
	}
	return &statusCause{cause: err, status: st}
}

func errorReason(code codes.Code, err error) string {
	type mapping struct {
		cause  error
		reason string
	}
	var reasons []mapping
	switch code {
	case codes.Aborted:
		reasons = []mapping{
			{eventlog.ErrConflict, "STALE_CURSOR"},
			{placement.ErrSessionBusy, "SESSION_BUSY"},
			{api.ErrComputeBusy, "COMPUTE_BUSY"},
			// A checkpoint supersedes a live turn by fencing it; report the operation,
			// not its incidental fence or canceled model call.
			{placement.ErrCheckpointing, "CHECKPOINT_IN_PROGRESS"},
			{eventlog.ErrFenced, "FENCED"},
			{api.ErrSnapshotSuperseded, "SNAPSHOT_SUPERSEDED"},
		}
	case codes.FailedPrecondition:
		reasons = []mapping{
			{eventlog.ErrDeleted, "SESSION_DELETED"},
			{api.ErrMemoryStateLost, "MEMORY_STATE_LOST"},
			{api.ErrInvalidCheckpoint, "CHECKPOINT_INVALID"},
			{api.ErrComputeConfigInvalid, "COMPUTE_CONFIG_INVALID"},
		}
	case codes.Unavailable:
		reasons = []mapping{
			{api.ErrCheckpointPending, "CHECKPOINT_PENDING"},
			{placement.ErrHarnessUnavailable, "HARNESS_UNAVAILABLE"},
			{api.ErrComputeUnavailable, "COMPUTE_UNAVAILABLE"},
		}
	case codes.ResourceExhausted:
		if errors.Is(err, api.ErrComputeResourceExhausted) {
			return "COMPUTE_RESOURCE_EXHAUSTED"
		}
	}
	for _, entry := range reasons {
		if errors.Is(err, entry.cause) {
			return entry.reason
		}
	}
	return ""
}

// runtimeFailureCode is only the unclassified runtime fallback. Existing lifecycle
// refusals and Describe failures must win before these compute-start categories.
func runtimeFailureCode(err error) codes.Code {
	switch {
	case errors.Is(err, api.ErrComputeUnavailable):
		return codes.Unavailable
	case errors.Is(err, api.ErrComputeResourceExhausted):
		return codes.ResourceExhausted
	case errors.Is(err, api.ErrComputeConfigInvalid):
		return codes.FailedPrecondition
	default:
		return codes.Internal
	}
}
