package placement

import (
	"context"
	"errors"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
)

type contextAllocationLog struct {
	eventlog.Store
	confirmErr error
}

func (*contextAllocationLog) BeginAllocation(context.Context, string, string) (string, error) {
	return "attempt", nil
}
func (l *contextAllocationLog) ConfirmAllocation(context.Context, string, api.Incarnation) error {
	return l.confirmErr
}

// Treating every wrapped deadline as caller cancellation would hide independent confirmation
// failures; treating checkpoint cancellation as caller cancellation would lose the retry path.
func TestAllocationContextFailureBoundaries(t *testing.T) {
	p := &Placer{}
	for _, tc := range []struct {
		name        string
		tracked     bool
		cancelCause error
		effectErr   error
		confirmErr  error
		interrupted bool
		superseded  bool
	}{
		{name: "caller during start", cancelCause: context.Canceled, effectErr: context.Canceled, interrupted: true},
		{name: "tracked caller during start", tracked: true, cancelCause: context.Canceled, effectErr: context.Canceled, interrupted: true},
		{name: "peer deadline", effectErr: context.DeadlineExceeded},
		{name: "independent confirmation deadline", tracked: true, confirmErr: context.DeadlineExceeded},
		{name: "caller ended but confirmation failed", tracked: true, cancelCause: context.Canceled, confirmErr: context.DeadlineExceeded},
		{name: "checkpoint superseded start", cancelCause: errSuperseded, effectErr: context.Canceled, superseded: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			var log eventlog.Store
			if tc.tracked {
				log = &contextAllocationLog{confirmErr: tc.confirmErr}
			}
			_, err := p.allocate(ctx, log, "create", func() (api.Incarnation, error) {
				if tc.cancelCause != nil {
					cancel(tc.cancelCause)
				}
				return api.Incarnation{ID: "instance"}, tc.effectErr
			})
			if errors.Is(err, ErrAdmissionInterrupted) != tc.interrupted || errors.Is(err, ErrCheckpointing) != tc.superseded {
				t.Fatalf("allocation=%v: interrupted=%v superseded=%v", err, tc.interrupted, tc.superseded)
			}
			if tc.effectErr != nil && !tc.superseded && !errors.Is(err, tc.effectErr) {
				t.Fatalf("lost start cause: %v", err)
			}
			if tc.confirmErr != nil && !errors.Is(err, tc.confirmErr) {
				t.Fatalf("lost confirmation cause: %v", err)
			}
		})
	}
}
