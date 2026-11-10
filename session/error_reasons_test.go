package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/placement"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func assertErrorInfo(t *testing.T, err error, code codes.Code, reason string) {
	t.Helper()
	st := status.Convert(err)
	if st.Code() != code {
		t.Fatalf("status=%v, want %v", err, code)
	}
	details := st.Details()
	if reason == "" {
		if len(details) != 0 {
			t.Fatalf("unclassified error has details: %v", details)
		}
		return
	}
	if len(details) != 1 {
		t.Fatalf("details=%v, want one ErrorInfo (%s)", details, reason)
	}
	info, ok := details[0].(*errdetails.ErrorInfo)
	if !ok || info.Reason != reason || info.Domain != "agentsessions" || len(info.Metadata) != 0 {
		t.Fatalf("detail=%v, want agentsessions/%s without metadata", details[0], reason)
	}
}

// A fresh status must select a host-owned code/reason, retain the Go cause, and never trust
// details supplied by a backend. These assertions are independent of human-readable messages.
func TestOperationErrorReasons(t *testing.T) {
	foreign, err := status.New(codes.Aborted, "backend busy").WithDetails(&errdetails.ErrorInfo{Domain: "agentsessions", Reason: "SESSION_BUSY"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		cause  error
		code   codes.Code
		reason string
	}{
		{"compute unavailable", api.ErrComputeUnavailable, codes.Unavailable, "COMPUTE_UNAVAILABLE"},
		{"compute exhausted", api.ErrComputeResourceExhausted, codes.ResourceExhausted, "COMPUTE_RESOURCE_EXHAUSTED"},
		{"compute config", api.ErrComputeConfigInvalid, codes.FailedPrecondition, "COMPUTE_CONFIG_INVALID"},
		{"conflict", eventlog.ErrConflict, codes.Aborted, "STALE_CURSOR"},
		{"session busy", placement.ErrSessionBusy, codes.Aborted, "SESSION_BUSY"},
		{"compute busy", api.ErrComputeBusy, codes.Aborted, "COMPUTE_BUSY"},
		{"fenced", eventlog.ErrFenced, codes.Aborted, "FENCED"},
		{"checkpoint", placement.ErrCheckpointing, codes.Aborted, "CHECKPOINT_IN_PROGRESS"},
		{"checkpoint supersedes fence", errors.Join(placement.ErrCheckpointing, eventlog.ErrFenced, context.Canceled), codes.Aborted, "CHECKPOINT_IN_PROGRESS"},
		{"deleted", eventlog.ErrDeleted, codes.FailedPrecondition, "SESSION_DELETED"},
		{"memory lost", api.ErrMemoryStateLost, codes.FailedPrecondition, "MEMORY_STATE_LOST"},
		{"invalid checkpoint", api.ErrInvalidCheckpoint, codes.FailedPrecondition, "CHECKPOINT_INVALID"},
		{"pending checkpoint", api.ErrCheckpointPending, codes.Unavailable, "CHECKPOINT_PENDING"},
		{"caller canceled", errors.Join(placement.ErrAdmissionInterrupted, context.Canceled), codes.Canceled, ""},
		{"caller deadline", errors.Join(placement.ErrAdmissionInterrupted, context.DeadlineExceeded), codes.DeadlineExceeded, ""},
		{"unknown", errors.New("unknown failure"), codes.Internal, ""},
		{"foreign status", foreign.Err(), codes.Internal, ""},
	} {
		for name, mapper := range map[string]func(string, error) error{"exec": execError, "resume": resumeError, "suspend": suspendError, "fork": forkError} {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				cause := fmt.Errorf("phase: %w", tc.cause)
				got := mapper("uid", cause)
				assertErrorInfo(t, got, tc.code, tc.reason)
				if !errors.Is(got, tc.cause) || !strings.Contains(status.Convert(got).Message(), cause.Error()) {
					t.Fatalf("lost cause/message: %v", got)
				}
			})
		}
	}
}

func TestRuntimeContextClassification(t *testing.T) {
	live := context.Background()
	ended, cancel := context.WithCancel(live)
	cancel()
	for _, mapper := range []func(string, error) error{suspendError, forkError} {
		peer := fmt.Errorf("peer: %w", context.DeadlineExceeded)
		assertErrorInfo(t, mapper("uid", runtimeContextError(live, peer)), codes.Internal, "")
		assertErrorInfo(t, mapper("uid", runtimeContextError(ended, context.Canceled)), codes.Canceled, "")
	}
}

func TestInternalCauseDoesNotBecomeTrustedDiagnostic(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	cause := errors.Join(hostInternalError("private runtime text"), context.DeadlineExceeded)
	err := internalStatus(cause, cause.Error())
	if !errors.Is(err, cause) || serviceErrorKind(err) != "internal" {
		t.Fatalf("cause or boundary kind lost: %v", err)
	}
	(&authorization{}).internalError(context.Background(), logger, "Exec", &err)
	if status.Convert(err).Message() != "internal error" || strings.Contains(output.String(), "private runtime text") || errors.Is(err, cause) {
		t.Fatalf("untrusted cause escaped authorization sanitization: %v; logs=%s", err, output.String())
	}
}

func TestDeleteAndEarlyAdmissionReasons(t *testing.T) {
	for _, tc := range []struct {
		cause  error
		code   codes.Code
		reason string
	}{
		{eventlog.ErrDeleted, codes.FailedPrecondition, "SESSION_DELETED"},
		{placement.ErrSessionBusy, codes.Aborted, "SESSION_BUSY"},
	} {
		assertErrorInfo(t, deleteError(tc.cause, "uid"), tc.code, tc.reason)
		assertErrorInfo(t, withStatusCause(tc.code, tc.cause), tc.code, tc.reason)
	}
	assertErrorInfo(t, forkError("uid", api.ErrSnapshotSuperseded), codes.Aborted, "SNAPSHOT_SUPERSEDED")
	for _, mapper := range []func(string, error) error{execError, resumeError, forkError} {
		// A live caller's peer timeout stays an outage, not the caller's deadline.
		cause := errors.Join(placement.ErrHarnessUnavailable, context.DeadlineExceeded)
		assertErrorInfo(t, mapper("uid", cause), codes.Unavailable, "HARNESS_UNAVAILABLE")
	}
}

func TestCleanupFailureDoesNotReplacePrimaryReason(t *testing.T) {
	cause := errors.Join(placement.ErrSessionBusy, errors.New("primary"))
	left := &placement.ComputeLeftRunningError{Instance: "instance", ComputeInstance: "compute-uid", Cause: cause, Cleanup: context.DeadlineExceeded}
	for _, mapper := range []func(string, error) error{execError, resumeError} {
		got := mapper("uid", left)
		assertErrorInfo(t, got, codes.Aborted, "SESSION_BUSY")
		if !errors.Is(got, cause) || errors.Is(got, context.DeadlineExceeded) || !strings.Contains(got.Error(), "may still be running") {
			t.Fatalf("cleanup replaced primary cause or lost warning: %v", got)
		}
	}
}
