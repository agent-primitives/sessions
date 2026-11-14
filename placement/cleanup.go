package placement

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
)

// allocationRecorder is optional so standalone event logs retain their existing behavior.
// Durable logs record an unresolved attempt before compute IO, even if its response is lost.
type allocationRecorder interface {
	BeginAllocation(ctx context.Context, backend, operation string) (string, error)
	ConfirmAllocation(ctx context.Context, attempt string, in api.Incarnation) error
}

func (p *Placer) allocate(ctx context.Context, log eventlog.Store, operation string, effect func() (api.Incarnation, error)) (api.Incarnation, error) {
	recorder, tracked := log.(allocationRecorder)
	if !tracked {
		in, err := effect()
		return in, allocationStartError(ctx, err)
	}
	key := ""
	if cleanup, ok := p.backend.(api.CleanupRuntime); ok {
		key = cleanup.CleanupKey()
	}
	// Unsupported backends still run, but their attempts remain unreleasable rather than
	// falling back to a potentially destructive name-only Stop.
	attempt, err := recorder.BeginAllocation(ctx, key, operation)
	if err != nil {
		return api.Incarnation{}, allocationStartError(ctx, fmt.Errorf("placement: begin %s allocation: %w", operation, err))
	}
	in, err := effect()
	if err != nil {
		return in, allocationStartError(ctx, err)
	}
	// The runtime has returned a known handle. Persist it even if the caller stopped
	// waiting, but bound the write independently so cancellation cannot strand this call.
	confirmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := recorder.ConfirmAllocation(confirmCtx, attempt, in); err != nil {
		return in, fmt.Errorf("placement: confirm %s allocation: %w", operation, err)
	}
	return in, nil
}

// CallerContextError records the caller's outcome at failure, independently of any
// context error in Cause. Both causes remain available to direct Go callers.
// Error omits backend diagnostics: caller context statuses are not INTERNAL and
// therefore do not pass through the authorization sanitizer.
type CallerContextError struct {
	CallerErr error
	Cause     error
}

func (e *CallerContextError) Error() string {
	return fmt.Sprintf("%v: %v", ErrAdmissionInterrupted, e.CallerErr)
}
func (e *CallerContextError) Unwrap() []error {
	return []error{ErrAdmissionInterrupted, e.CallerErr, e.Cause}
}

// callerContextError records cancellation at the operation boundary that returned err. Callers
// apply it before any later persistence or cleanup so those failures keep their own classification.
func callerContextError(ctx context.Context, err error) error {
	if err != nil && ctx.Err() != nil {
		return &CallerContextError{CallerErr: ctx.Err(), Cause: err}
	}
	return err
}

// Only the caller's context during start is an admission interruption. Confirmation
// uses an independent deadline, and checkpoint supersession remains ABORTED.
func allocationStartError(ctx context.Context, err error) error {
	if err != nil && errors.Is(context.Cause(ctx), errSuperseded) {
		return fmt.Errorf("%w (compute start ended with: %v)", errSuperseded, err)
	}
	return callerContextError(ctx, err)
}
