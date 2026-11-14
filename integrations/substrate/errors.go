package ateadapter

import (
	"github.com/agent-primitives/sessions/api"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// computeFailure keeps peer text/details out of the message the service publishes
// for a classified error. Direct Go callers can still inspect the original cause.
// It deliberately does not implement GRPCStatus: Sessions owns the public status.
type computeFailure struct {
	kind  error
	cause error
}

func (e *computeFailure) Error() string        { return e.kind.Error() }
func (e *computeFailure) Unwrap() error        { return e.cause }
func (e *computeFailure) Is(target error) bool { return target == e.kind }

// computeError classifies only unambiguous transport/capacity codes at allocation,
// attach and clone boundaries. FAILED_PRECONDITION includes lifecycle prerequisites,
// and INVALID_ARGUMENT can be a request bug; neither proves invalid configuration.
func computeError(err error) error {
	var kind error
	switch status.Code(err) {
	case codes.Unavailable:
		kind = api.ErrComputeUnavailable
	case codes.ResourceExhausted:
		kind = api.ErrComputeResourceExhausted
	default:
		return err
	}
	return &computeFailure{kind: kind, cause: err}
}
