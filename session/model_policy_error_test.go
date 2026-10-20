package session

import (
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/agent-primitives/sessions/api"
)

func TestResumeModelPolicyErrorPreservesCause(t *testing.T) {
	for _, cause := range []error{api.ErrModelNotAllowed, api.ErrInvalidModelPolicy} {
		err := resumeError(fmt.Errorf("model redrive: %w", cause))
		if status.Code(err) != codes.FailedPrecondition || !errors.Is(err, cause) {
			t.Errorf("resume model admission lost code/cause: %v", err)
		}
	}
}

func TestExecModelPolicyErrorPreservesCause(t *testing.T) {
	for _, cause := range []error{api.ErrModelNotAllowed, api.ErrInvalidModelPolicy} {
		err := execError(fmt.Errorf("selected route: %w", cause))
		if status.Code(err) != codes.FailedPrecondition || !errors.Is(err, cause) {
			t.Fatalf("exec model admission lost code/cause: %v", err)
		}
	}
}
