package session_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/runtime/local"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type failedStart struct {
	*local.Backend
	cause  error
	cancel context.CancelFunc
	wait   bool
}

func (b *failedStart) Create(ctx context.Context, _ *api.SessionSpec) (api.Incarnation, error) {
	if b.cancel != nil {
		b.cancel()
	}
	if b.wait {
		<-ctx.Done()
	}
	if ctx.Err() != nil {
		return api.Incarnation{}, fmt.Errorf("start: %w", ctx.Err())
	}
	return api.Incarnation{}, fmt.Errorf("start: %w", b.cause)
}

// A start failure precedes EXECUTION_START. Returning INTERNAL or appending a turn would make
// a caller choose the wrong recovery path; model/Run failures are tested separately.
func TestExecComputeStartReasonsAndCallerContexts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cause  error
		code   codes.Code
		reason string
	}{
		{"unavailable", api.ErrComputeUnavailable, codes.Unavailable, "COMPUTE_UNAVAILABLE"},
		{"capacity", api.ErrComputeResourceExhausted, codes.ResourceExhausted, "COMPUTE_RESOURCE_EXHAUSTED"},
		{"configuration", api.ErrComputeConfigInvalid, codes.FailedPrecondition, "COMPUTE_CONFIG_INVALID"},
		{"canceled", context.Canceled, codes.Canceled, ""},
		{"deadline", context.DeadlineExceeded, codes.DeadlineExceeded, ""},
		{"peer deadline", context.DeadlineExceeded, codes.Internal, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openStore(t, ":memory:")
			inner := local.New(echoagent.Harness{})
			t.Cleanup(func() { _ = inner.Close() })
			backend := &failedStart{Backend: inner, cause: tc.cause}
			ctx := t.Context()
			if tc.name == "canceled" {
				ctx, backend.cancel = context.WithCancel(ctx)
				defer backend.cancel()
			}
			if tc.name == "deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
				defer cancel()
				backend.wait = true
			}
			if err := store.PutSession(sqlitelog.SessionMeta{UID: "start", Harness: "echo"}); err != nil {
				t.Fatal(err)
			}
			svc := session.NewService(store, echoRegistry(t, backend))
			err := svc.Exec(&v1.ExecRequest{Session: "start"}, &rpcStream{ctx: ctx})
			if status.Code(err) != tc.code || !errors.Is(err, tc.cause) {
				t.Fatalf("start=%v, want %s retaining %v", err, tc.code, tc.cause)
			}
			details := status.Convert(err).Details()
			if tc.reason != "" {
				if len(details) != 1 {
					t.Fatalf("missing reason: %v", details)
				}
				info, ok := details[0].(*errdetails.ErrorInfo)
				if !ok || info.Domain != "agentsessions" || info.Reason != tc.reason {
					t.Fatalf("unexpected details: %v", details)
				}
			} else if len(details) != 0 {
				t.Fatalf("unknown/context start acquired a reason: %v", details)
			}
			if head, err := store.Session("start").Head(); err != nil || head != 0 {
				t.Fatalf("failed start journaled a turn: head=%d err=%v", head, err)
			}
		})
	}
}
