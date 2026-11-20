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
	"github.com/agent-primitives/sessions/runtime/substrate"
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

// Substrate tears a failed fork child down on detached contexts before Fork returns, so the
// caller's deadline can lapse during that teardown. That must not turn the child's primary failure
// into DEADLINE_EXCEEDED; a deadline that lapsed during the failing call itself still does.
func TestForkKeepsPrimaryFailureWhenDeadlineLapsesInTeardown(t *testing.T) {
	primary := fmt.Errorf("%w: worker pool drained", api.ErrComputeUnavailable)
	for _, tc := range []struct {
		name           string
		lapseInResume  bool
		code           codes.Code
		reason         string
		retainsPrimary bool
	}{
		{name: "deadline lapses during teardown", code: codes.Unavailable, reason: "COMPUTE_UNAVAILABLE", retainsPrimary: true},
		{name: "deadline lapses during the child's resume", lapseInResume: true, code: codes.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
			defer cancel()
			ctl := &actorControl{status: map[string]substrate.ActorStatus{}, snapshots: map[string]string{}}
			var liveAtFailure bool
			ctl.resumeErr = func(string) error {
				if tc.lapseInResume {
					<-ctx.Done()
					// A gRPC ControlClient reports the caller's deadline as a status, not context.DeadlineExceeded.
					return status.Error(codes.DeadlineExceeded, "context deadline exceeded")
				}
				liveAtFailure = ctx.Err() == nil
				return primary
			}
			// The child's teardown suspends it first, and outlasts the caller's deadline.
			ctl.onSuspend = func(string) { <-ctx.Done() }
			desc, err := echoagent.Harness{}.Describe(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			desc.Capabilities.Resumability = api.ResumabilityStatelessReplay
			backend := substrate.New(ctl, "space", substrate.ObjectRef{Atespace: "space", Name: "echo"}, desc)
			store := openStore(t, ":memory:")
			if err := store.PutSession(sqlitelog.SessionMeta{UID: "parent", Harness: "echo"}); err != nil {
				t.Fatal(err)
			}

			_, err = session.NewService(store, echoRegistry(t, backend)).Fork(ctx, &v1.ForkRequest{Session: "parent", Count: 1})
			if !tc.lapseInResume && !liveAtFailure {
				t.Fatal("the child's resume ran after the caller's deadline; the test needs a longer one")
			}
			if ctx.Err() == nil {
				t.Fatal("the caller's deadline did not lapse during the fork")
			}
			if status.Code(err) != tc.code || errors.Is(err, primary) != tc.retainsPrimary {
				t.Fatalf("fork=%v, want %s (retaining the primary failure: %v)", err, tc.code, tc.retainsPrimary)
			}
			details := status.Convert(err).Details()
			if tc.reason == "" {
				if len(details) != 0 {
					t.Fatalf("caller failure got a reason: %v", details)
				}
				return
			}
			if len(details) != 1 {
				t.Fatalf("lost primary reason: %v", details)
			}
			if info, ok := details[0].(*errdetails.ErrorInfo); !ok || info.Domain != "agentsessions" || info.Reason != tc.reason {
				t.Fatalf("details=%v, want agentsessions/%s", details, tc.reason)
			}
		})
	}
}
