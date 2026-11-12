package session_test

import (
	"context"
	"errors"
	"testing"

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

type failedForkWithRollback struct {
	*local.Backend
	cause             error
	cancel            context.CancelFunc
	cancelAtFailure   bool
	forks, stops      int
	cleanupContextErr error
}

func (b *failedForkWithRollback) Fork(ctx context.Context, ref api.SnapshotRef, opts api.ForkOpts) (api.Incarnation, error) {
	b.forks++
	if b.forks == 2 {
		if b.cancelAtFailure {
			b.cancel()
		}
		return api.Incarnation{}, b.cause
	}
	return b.Backend.Fork(ctx, ref, opts)
}

func (b *failedForkWithRollback) Stop(ctx context.Context, in api.Incarnation) error {
	b.stops++
	b.cancel()
	b.cleanupContextErr = ctx.Err()
	return b.Backend.Stop(ctx, in)
}

// The first child exists before the second fails. Cancellation during its real rollback
// must not change the failure code/reason sampled at the child, or cancel cleanup.
func TestForkFailureClassificationPrecedesRollback(t *testing.T) {
	for _, tc := range []struct {
		name            string
		cause           error
		cancelAtFailure bool
		code            codes.Code
		reason          string
	}{
		{"caller cancels at failing child", context.DeadlineExceeded, true, codes.Canceled, ""},
		{"caller cancels during rollback", api.ErrComputeUnavailable, false, codes.Unavailable, "COMPUTE_UNAVAILABLE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openStore(t, ":memory:")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			backend := &failedForkWithRollback{Backend: local.New(echoagent.Harness{}), cause: tc.cause, cancel: cancel, cancelAtFailure: tc.cancelAtFailure}
			t.Cleanup(func() { _ = backend.Close() })
			if err := store.PutSession(sqlitelog.SessionMeta{UID: "parent", Harness: "echo"}); err != nil {
				t.Fatal(err)
			}
			svc := session.NewService(store, echoRegistry(t, backend))
			_, err := svc.Fork(ctx, &v1.ForkRequest{Session: "parent", Count: 2})
			if status.Code(err) != tc.code || !errors.Is(err, tc.cause) {
				t.Fatalf("fork=%v, want %s retaining %v", err, tc.code, tc.cause)
			}
			details := status.Convert(err).Details()
			if tc.reason == "" {
				if len(details) != 0 {
					t.Fatalf("caller failure got invented reason: %v", details)
				}
			} else {
				if len(details) != 1 {
					t.Fatalf("lost primary reason: %v", details)
				}
				info, ok := details[0].(*errdetails.ErrorInfo)
				if !ok || info.Domain != "agentsessions" || info.Reason != tc.reason {
					t.Fatalf("primary details=%v", details)
				}
			}
			if backend.forks != 2 || backend.stops != 1 || backend.cleanupContextErr != nil {
				t.Fatalf("rollback: forks=%d stops=%d context=%v", backend.forks, backend.stops, backend.cleanupContextErr)
			}
			if head, err := store.Session("parent").Head(); err != nil || head != 0 {
				t.Fatalf("failed stateless fork changed parent: head=%d err=%v", head, err)
			}
		})
	}
}
