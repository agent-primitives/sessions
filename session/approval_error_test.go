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

func TestApprovalErrorReasons(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cause  error
		code   codes.Code
		reason string
	}{
		{"busy", placement.ErrSessionBusy, codes.Aborted, "SESSION_BUSY"},
		{"checkpoint", placement.ErrCheckpointing, codes.Aborted, "CHECKPOINT_IN_PROGRESS"},
		{"conflict", eventlog.ErrConflict, codes.Aborted, "STALE_CURSOR"},
		{"fence", eventlog.ErrFenced, codes.Aborted, "FENCED"},
		{"checkpoint before fence", errors.Join(placement.ErrCheckpointing, eventlog.ErrFenced), codes.Aborted, "CHECKPOINT_IN_PROGRESS"},
		{"conflict before busy", errors.Join(eventlog.ErrConflict, placement.ErrSessionBusy), codes.Aborted, "STALE_CURSOR"},
		{"deleted", eventlog.ErrDeleted, codes.FailedPrecondition, "SESSION_DELETED"},
	} {
		for name, mapper := range map[string]func(error) error{"Approve": approvalError, "CloseApproval": closeApprovalError} {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				cause := fmt.Errorf("approval: %w", tc.cause)
				got := mapper(cause)
				assertErrorInfo(t, got, tc.code, tc.reason)
				if !errors.Is(got, cause) || status.Convert(got).Message() != cause.Error() {
					t.Fatalf("lost cause or changed message: %v", got)
				}
			})
		}
	}
}

func TestApprovalReasonlessErrors(t *testing.T) {
	for _, tc := range []struct {
		cause error
		code  codes.Code
	}{
		{controller.ErrInvalidApprovalDecision, codes.FailedPrecondition},
		{controller.ErrInvalidExecutionLog, codes.FailedPrecondition},
		{controller.ErrReplayDiverged, codes.FailedPrecondition},
		{controller.ErrApprovalUnavailable, codes.FailedPrecondition},
		{errors.New("storage"), codes.Internal},
		{status.Error(codes.Aborted, "foreign status"), codes.Internal},
	} {
		for name, mapper := range map[string]func(error) error{"Approve": approvalError, "CloseApproval": closeApprovalError} {
			t.Run(name+"/"+tc.cause.Error(), func(t *testing.T) {
				assertErrorInfo(t, mapper(fmt.Errorf("approval: %w", tc.cause)), tc.code, "")
			})
		}
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
