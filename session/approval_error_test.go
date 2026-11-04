package session

import (
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/placement"
)

func TestApprovalCheckpointRefusalIsAborted(t *testing.T) {
	err := approvalError(fmt.Errorf("checkpoint: %w", placement.ErrCheckpointing))
	if status.Code(err) != codes.Aborted {
		t.Fatalf("Approve checkpoint refusal=%v, want ABORTED", err)
	}
}

func TestOrdinaryLifecycleJournalContentionIsAborted(t *testing.T) {
	for _, cause := range []error{eventlog.ErrConflict, eventlog.ErrFenced} {
		for name, mapError := range map[string]func(string, error) error{"Suspend": suspendError, "Resume": resumeError} {
			t.Run(name+"/"+cause.Error(), func(t *testing.T) {
				if err := mapError("sess-x", fmt.Errorf("journal: %w", cause)); status.Code(err) != codes.Aborted {
					t.Fatalf("%s=%v, want ABORTED", name, err)
				}
			})
		}
	}
}

func TestCloseApprovalErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		cause error
		want  codes.Code
	}{
		{placement.ErrSessionBusy, codes.Aborted},
		{placement.ErrCheckpointing, codes.Aborted},
		{eventlog.ErrConflict, codes.Aborted},
		{eventlog.ErrFenced, codes.Aborted},
		{eventlog.ErrTampered, codes.DataLoss},
		{controller.ErrInvalidApprovalDecision, codes.FailedPrecondition},
		{controller.ErrInvalidExecutionLog, codes.FailedPrecondition},
		{eventlog.ErrDeleted, codes.FailedPrecondition},
		{api.ErrInvalidCallerIdentity, codes.Internal},
		{errors.New("storage"), codes.Internal},
	} {
		t.Run(tc.cause.Error(), func(t *testing.T) {
			if err := closeApprovalError(fmt.Errorf("close: %w", tc.cause)); status.Code(err) != tc.want {
				t.Fatalf("closeApprovalError = %v, want %v", err, tc.want)
			}
		})
	}
}
