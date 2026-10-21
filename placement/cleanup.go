package placement

import (
	"context"
	"fmt"

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
