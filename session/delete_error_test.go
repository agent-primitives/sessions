package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"modernc.org/sqlite"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/sqlitelog"
)

// Mapping coincidental backend statuses instead of typed local causes, or dropping Unwrap,
// would break the chosen public code and the original Go cause independently.
func TestDeleteErrorChoosesCodeAndPreservesRootCause(t *testing.T) {
	fault := errors.New("disk fault")
	for _, tc := range []struct {
		name  string
		cause error
		code  codes.Code
	}{
		{"unknown", sqlitelog.ErrSessionNotFound, codes.NotFound},
		{"deleted", eventlog.ErrDeleted, codes.FailedPrecondition},
		{"busy", placement.ErrSessionBusy, codes.Aborted},
		{"canceled", context.Canceled, codes.Canceled},
		{"deadline", context.DeadlineExceeded, codes.DeadlineExceeded},
		{"storage", fault, codes.Internal},
		{"backend Aborted is not busy", status.Error(codes.Aborted, "backend conflict"), codes.Internal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := deleteError(fmt.Errorf("store phase: %w", tc.cause), "uid")
			if status.Code(err) != tc.code || !errors.Is(err, tc.cause) {
				t.Errorf("mapped=%v, want %v retaining %v", err, tc.code, tc.cause)
			}
		})
	}
}

func TestDeleteDirectServiceContextsAndSQLFaultReleaseGuard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	store, err := sqlitelog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	p := placement.New(nil, nil)
	r, err := placement.NewRegistry("unserved-runtime", map[string]*placement.Placer{"unserved-runtime": p})
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(store, r)
	uid := "context-test"
	if err := store.PutSession(sqlitelog.SessionMeta{UID: uid, Harness: "retired"}); err != nil {
		t.Fatal(err)
	}
	before, err := store.SessionInfo(uid)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	deadline, cancelDeadline := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancelDeadline()
	for _, tc := range []struct {
		ctx   context.Context
		cause error
		code  codes.Code
	}{{canceled, context.Canceled, codes.Canceled}, {deadline, context.DeadlineExceeded, codes.DeadlineExceeded}} {
		if _, err := svc.DeleteSession(tc.ctx, &v1.DeleteSessionRequest{Uid: uid}); status.Code(err) != tc.code || !errors.Is(err, tc.cause) {
			t.Errorf("canceled Delete=%v, want %v and exact cause %v", err, tc.code, tc.cause)
		}
		info, err := store.SessionInfo(uid)
		if err != nil || info != before {
			t.Errorf("canceled Delete changed metadata: %v %v", info, err)
		}
		if err := r.WithSessionGuard(uid, func() error { return nil }); err != nil {
			t.Fatalf("canceled Delete retained guard: %v", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("CREATE TRIGGER fail_intent BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT, 'intent failure'); END"); err != nil {
		t.Fatal(err)
	}
	_, err = svc.DeleteSession(t.Context(), &v1.DeleteSessionRequest{Uid: uid})
	var sqlCause *sqlite.Error
	if status.Code(err) != codes.Internal || !errors.As(err, &sqlCause) || !strings.Contains(err.Error(), uid) || !strings.Contains(err.Error(), "append intent") {
		t.Errorf("SQL failure lost code/root cause/phase: %v", err)
	}
	info, lookupErr := store.SessionInfo(uid)
	if lookupErr != nil || info != before {
		t.Errorf("SQL fault changed state: %v %v", info, lookupErr)
	}
	if _, err := db.Exec("DROP TRIGGER fail_intent"); err != nil {
		t.Fatal(err)
	}
	deleted, err := svc.DeleteSession(t.Context(), &v1.DeleteSessionRequest{Uid: uid})
	if err != nil || deleted.GetDeletionState() != v1.DeletionState_DELETION_PENDING {
		t.Fatalf("fault retained guard or failed retry: %v %v", deleted, err)
	}
	if err := store.Session(uid).Verify(); err != nil {
		t.Fatal(err)
	}
}

// DELETE changes neither projection axis, including for unrecognized stored states.
// Unrecognized states render as UNSPECIFIED and fail closed at admission, not as ACTIVE.
func TestDeletionRenderingIndependentAxesAndUnknownState(t *testing.T) {
	requested := time.Unix(12, 34)
	for _, tc := range []struct {
		state   api.DeletionState
		wire    v1.DeletionState
		request time.Time
	}{{api.DeletionActive, v1.DeletionState_DELETION_ACTIVE, time.Time{}}, {api.DeletionPending, v1.DeletionState_DELETION_PENDING, requested}, {api.DeletionState("UNRECOGNIZED"), v1.DeletionState_DELETION_STATE_UNSPECIFIED, requested}, {api.DeletionState("unknown"), v1.DeletionState_DELETION_STATE_UNSPECIFIED, time.Time{}}} {
		info := sqlitelog.SessionInfo{SessionMeta: sqlitelog.SessionMeta{UID: "uid"}, ComputeState: api.ComputeCold, LastSeq: 1, DeletionSeq: 1, DeletionState: tc.state, DeletionRequestedAt: tc.request}
		got, err := sessionProto(info)
		if err != nil {
			t.Fatal(err)
		}
		if got.GetDeletionState() != tc.wire || got.GetExecState() != v1.ExecState_EXEC_PENDING || got.GetComputeState() != v1.ComputeState_COMPUTE_COLD || got.GetLastSeq() != 1 {
			t.Errorf("rendered axes=%v", got)
		}
		if (got.GetDeletionRequestedTime() == nil) != tc.request.IsZero() {
			t.Errorf("absence rendered as epoch: %v", got)
		}
		if !tc.request.IsZero() && !got.GetDeletionRequestedTime().AsTime().Equal(tc.request) {
			t.Error("request timestamp changed")
		}
		err = deletedSessionError(info)
		if tc.state == api.DeletionActive {
			if err != nil {
				t.Error(err)
			}
		} else if status.Code(err) != codes.FailedPrecondition || !errors.Is(err, eventlog.ErrDeleted) {
			t.Errorf("non-ACTIVE admission=%v", err)
		}
	}
}
