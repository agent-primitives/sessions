package placement

import (
	"context"
	"errors"
	"fmt"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
)

// ErrCleanupUnavailable means no opt-in runtime serves the recorded backend configuration.
// It is not proof of absence: the allocation must remain pending until its route is restored.
var ErrCleanupUnavailable = errors.New("placement: safe session cleanup unavailable")

// ReleaseSession resolves only an exact opt-in backend configuration key. It never describes,
// provisions, or falls back to a default harness or generic Stop. Equal keys promise
// interchangeable no-resource release, so aliases and shared local backends need only one call.
// The caller owns session exclusion; this method can run inside WithSessionGuard.
func (r *Registry) ReleaseSession(ctx context.Context, key, sessionUID string) error {
	if key == "" {
		return ErrCleanupUnavailable
	}
	var target api.CleanupRuntime
	r.mu.RLock()
	for _, p := range r.byName {
		if cleanup, ok := p.backend.(api.CleanupRuntime); ok && cleanup.CleanupKey() == key {
			target = cleanup
			break
		}
	}
	r.mu.RUnlock()
	if target == nil {
		return fmt.Errorf("%w: backend %q", ErrCleanupUnavailable, key)
	}
	return target.ReleaseSession(ctx, sessionUID)
}

// allocationRecorder is optional so standalone event logs retain their existing behavior.
// Durable logs record an unresolved attempt before compute IO, even if its response is lost.
type allocationRecorder interface {
	BeginAllocation(ctx context.Context, backend, operation string) (string, error)
	ConfirmAllocation(ctx context.Context, attempt string, in api.Incarnation) error
}

func (p *Placer) allocate(ctx context.Context, log eventlog.Store, operation string, effect func() (api.Incarnation, error)) (api.Incarnation, error) {
	recorder, tracked := log.(allocationRecorder)
	if !tracked {
		return effect()
	}
	key := ""
	if cleanup, ok := p.backend.(api.CleanupRuntime); ok {
		key = cleanup.CleanupKey()
	}
	// Unsupported backends still run, but their attempts remain unreleasable rather than
	// falling back to a potentially destructive name-only Stop.
	attempt, err := recorder.BeginAllocation(ctx, key, operation)
	if err != nil {
		return api.Incarnation{}, fmt.Errorf("placement: begin %s allocation: %w", operation, err)
	}
	in, err := effect()
	if err != nil {
		return in, err
	}
	if err := recorder.ConfirmAllocation(ctx, attempt, in); err != nil {
		return in, fmt.Errorf("placement: confirm %s allocation: %w", operation, err)
	}
	return in, nil
}
