package session

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/placement"
)

// A ComputeLeftRunningError maps to the code of the failure it wraps, never of its cleanup.
func TestComputeLeftRunningKeepsCauseCode(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
		want  codes.Code
	}{
		{"harness unreachable", fmt.Errorf("%w: describe harness", placement.ErrHarnessUnavailable), codes.Unavailable},
		{"resume marker append failed", errors.New("sqlitelog: append: disk I/O error"), codes.Internal},
		{"fenced", eventlog.ErrFenced, codes.Aborted},
		{"conflict", eventlog.ErrConflict, codes.Aborted},
		{"checkpointing", placement.ErrCheckpointing, codes.Aborted},
		{"deleted", eventlog.ErrDeleted, codes.FailedPrecondition},
	} {
		left := &placement.ComputeLeftRunningError{Instance: "i", Cause: tc.cause,
			// A cleanup error the mapping would classify differently must not leak into the code.
			Cleanup: fmt.Errorf("release failed: %w", context.DeadlineExceeded)}
		for name, mapError := range map[string]func(string, error) error{"Exec": execError, "Resume": resumeError} {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				err := mapError("sess-x", left)
				if got := status.Code(err); got != tc.want || got != status.Code(mapError("sess-x", tc.cause)) {
					t.Fatalf("%s = %v, want %v as for the bare cause", name, err, tc.want)
				}
			})
		}
	}
}
