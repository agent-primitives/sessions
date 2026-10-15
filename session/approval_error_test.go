package session

import (
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/aramase/agentsessions/eventlog"
	"github.com/aramase/agentsessions/placement"
)

func TestApprovalCheckpointRefusalIsAborted(t *testing.T) {
	err := approvalError(fmt.Errorf("checkpoint: %w", placement.ErrCheckpointing))
	if status.Code(err) != codes.Aborted {
		t.Fatalf("Approve checkpoint refusal=%v, want ABORTED", err)
	}
}

func TestOrdinaryLifecycleJournalContentionIsAborted(t *testing.T) {
	for _, cause := range []error{eventlog.ErrConflict, eventlog.ErrFenced} {
		for name, mapError := range map[string]func(error) error{"Suspend": suspendError, "Resume": resumeError} {
			t.Run(name+"/"+cause.Error(), func(t *testing.T) {
				if err := mapError(fmt.Errorf("journal: %w", cause)); status.Code(err) != codes.Aborted {
					t.Fatalf("%s=%v, want ABORTED", name, err)
				}
			})
		}
	}
}
