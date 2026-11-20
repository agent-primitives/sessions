package placement

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/internal/callerctx"
)

// allocationRecorder is optional so standalone event logs retain their existing behavior.
// Durable logs record an unresolved operation and, for owned backends, its complete resource plan
// before compute I/O.
type allocationRecorder interface {
	BeginAllocation(ctx context.Context, backend, operation string, resources ...api.CleanupResource) (string, error)
	ConfirmAllocation(ctx context.Context, attempt string, in api.Incarnation) error
}

type forkAllocationRecorder interface {
	BeginUnpublishedAllocation(ctx context.Context, backend, operation string, resources ...api.CleanupResource) (string, error)
	ConfirmAllocation(ctx context.Context, attempt string, in api.Incarnation) error
}

func cleanupOperation(operation string) api.CleanupOperation {
	switch operation {
	case "create":
		return api.CleanupCreate
	case "restore":
		return api.CleanupRestore
	case "fork":
		return api.CleanupFork
	default:
		return api.CleanupOperation(operation)
	}
}

func (p *Placer) allocate(ctx context.Context, log eventlog.Store, sessionUID, operation string, effect func() (api.Incarnation, error)) (api.Incarnation, error) {
	recorder, tracked := log.(allocationRecorder)
	if !tracked {
		in, err := effect()
		return in, allocationStartError(ctx, err)
	}
	key := ""
	var resources []api.CleanupResource
	owned := false
	if cleanup, ok := p.backend.(api.ResourceCleanupRuntime); ok && cleanup.CleanupKey() != "" {
		key = cleanup.CleanupKey()
		resources = append([]api.CleanupResource(nil), cleanup.CleanupResources(cleanupOperation(operation), sessionUID)...)
		if err := api.ValidateCleanupPlan(resources); err != nil {
			return api.Incarnation{}, allocationStartError(ctx, fmt.Errorf("placement: %s allocation plan: %w", operation, err))
		}
		owned = true
	} else if cleanup, ok := p.backend.(api.CleanupRuntime); ok {
		key = cleanup.CleanupKey()
	}
	attempt, err := recorder.BeginAllocation(ctx, key, operation, resources...)
	if err != nil {
		return api.Incarnation{}, allocationStartError(ctx, fmt.Errorf("placement: begin %s allocation: %w", operation, err))
	}
	in, effectErr := effect()
	// Legacy handle cleanup keeps its existing success-only confirmation. Identity-bearing cleanup
	// must also confirm a UID returned with an error, because the control-plane effect may have landed.
	if effectErr != nil && !owned {
		return in, allocationStartError(ctx, effectErr)
	}
	confirmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	confirmErr := recorder.ConfirmAllocation(confirmCtx, attempt, in)
	if confirmErr != nil {
		wrapped := fmt.Errorf("placement: confirm %s allocation: %w", operation, confirmErr)
		if effectErr != nil {
			return in, errors.Join(allocationStartError(ctx, effectErr), wrapped)
		}
		return in, wrapped
	}
	return in, allocationStartError(ctx, effectErr)
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
// uses an independent deadline, and checkpoint supersession remains ABORTED. A runtime
// that tears down before returning marks a failure the caller was still live for, so a
// context that ended during that teardown does not replace the primary failure.
func allocationStartError(ctx context.Context, err error) error {
	if callerctx.LiveAtFailure(err) {
		return err
	}
	if err != nil && errors.Is(context.Cause(ctx), errSuperseded) {
		return fmt.Errorf("%w (compute start ended with: %v)", errSuperseded, err)
	}
	return callerContextError(ctx, err)
}
