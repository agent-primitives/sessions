package session_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type failedSuspendSnapshot struct {
	*local.Backend
	cancel context.CancelFunc
	cause  error
}

func (b *failedSuspendSnapshot) Snapshot(context.Context, api.Incarnation, api.SnapshotKind) (api.SnapshotRef, error) {
	b.cancel()
	return api.SnapshotRef{}, b.cause
}

type failedSuspendAppendStore struct {
	*sqlitelog.Store
	cancel context.CancelFunc
	cause  error
}

func (s *failedSuspendAppendStore) SessionLog(uid string) eventlog.SessionJournal {
	return failedSuspendAppend{SessionJournal: s.Store.SessionLog(uid), cancel: s.cancel, cause: s.cause}
}

type failedSuspendAppend struct {
	eventlog.SessionJournal
	cancel context.CancelFunc
	cause  error
}

func (l failedSuspendAppend) Append(int64, int64, api.Event) (eventlog.Record, error) {
	l.cancel()
	return eventlog.Record{}, l.cause
}

// Cancellation is classified where Runtime.Snapshot returns. A later persistence failure owns the
// result even if the caller context ends while the SUSPEND record is being appended.
func TestSuspendCancellationFailureBoundary(t *testing.T) {
	for _, tc := range []struct {
		name        string
		failAppend  bool
		deadline    bool
		wantCode    codes.Code
		callerErr   error
		interrupted bool
	}{
		{name: "snapshot failure after cancellation", wantCode: codes.Canceled, callerErr: context.Canceled, interrupted: true},
		{name: "snapshot failure after deadline", deadline: true, wantCode: codes.DeadlineExceeded, callerErr: context.DeadlineExceeded, interrupted: true},
		{name: "journal failure after cancellation", failAppend: true, wantCode: codes.Internal, callerErr: context.Canceled},
		{name: "journal failure after deadline", failAppend: true, deadline: true, wantCode: codes.Internal, callerErr: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			endAtFailure := func() {}
			if tc.deadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Time{})
				defer cancel()
			} else {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				endAtFailure = cancel
			}
			cause := errors.New("injected " + tc.name)
			backing := openStore(t, ":memory:")
			var store session.SessionStore = backing
			inner := local.New(echoagent.Harness{})
			t.Cleanup(func() { _ = inner.Close() })
			var backend placement.Backend = &failedSuspendSnapshot{Backend: inner, cancel: endAtFailure, cause: cause}
			if tc.failAppend {
				backend = inner
				store = &failedSuspendAppendStore{Store: backing, cancel: endAtFailure, cause: cause}
			}
			if err := backing.PutSession(sqlitelog.SessionMeta{UID: "uid", Harness: "echo"}); err != nil {
				t.Fatal(err)
			}

			_, err := session.NewService(store, echoRegistry(t, backend)).Suspend(ctx, &v1.SuspendRequest{Session: "uid"})
			if status.Code(err) != tc.wantCode || !errors.Is(err, cause) {
				t.Fatalf("Suspend=%v, want %s retaining %v", err, tc.wantCode, cause)
			}
			if got := errors.Is(err, placement.ErrAdmissionInterrupted); got != tc.interrupted {
				t.Fatalf("Suspend=%v: ErrAdmissionInterrupted=%v, want %v", err, got, tc.interrupted)
			}
			if got := errors.Is(err, tc.callerErr); got != tc.interrupted {
				t.Fatalf("Suspend=%v: caller error %v=%v, want %v", err, tc.callerErr, got, tc.interrupted)
			}
			if details := status.Convert(err).Details(); len(details) != 0 {
				t.Fatalf("Suspend failure acquired ErrorInfo: %v", details)
			}
			if head, headErr := backing.Session("uid").Head(); headErr != nil || head != 0 {
				t.Fatalf("failed Suspend changed journal: head=%d err=%v", head, headErr)
			}
		})
	}
}
