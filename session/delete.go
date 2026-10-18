package session

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/observability"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/sqlitelog"
)

// DeleteSession acknowledges permanent durable intent only. It does not resolve a harness or
// contact runtime resources; the retained compute projection is not a cleanup claim.
func (s *Service) DeleteSession(ctx context.Context, req *v1.DeleteSessionRequest) (session *v1.Session, err error) {
	defer s.auth.internalError(&err)
	if err := s.authorize(ctx, req.GetUid(), api.ActionDelete); err != nil {
		return nil, err
	}
	ctx = observability.EnsureRequestID(ctx)
	uid := req.GetUid()
	finish := observability.Start(ctx, s.logger, "session", "delete", "session_uid", uid)
	defer func() { finish(err, "error_kind", serviceErrorKind(err)) }()
	if uid == "" {
		return nil, status.Error(codes.InvalidArgument, "uid is required")
	}
	var info sqlitelog.SessionInfo
	// The callback calls only the atomic store primitive, never a guarded Placer method.
	err = s.registry.WithSessionGuard(uid, func() error {
		var err error
		info, err = s.store.DeleteSession(ctx, uid)
		return err
	})
	if err != nil {
		return nil, deleteError(err, uid)
	}
	return sessionProto(info)
}

// statusCause chooses the boundary code without accepting an unrelated backend transport code,
// while retaining the original store/context/sentinel cause for direct Go callers.
type statusCause struct {
	cause  error
	status *status.Status
}

func (e *statusCause) Error() string              { return e.status.Err().Error() }
func (e *statusCause) Unwrap() error              { return e.cause }
func (e *statusCause) GRPCStatus() *status.Status { return e.status }
func withStatusCause(code codes.Code, err error) error {
	return &statusCause{cause: err, status: status.New(code, err.Error())}
}
func deleteError(err error, uid string) error {
	code := codes.Internal
	switch {
	case errors.Is(err, placement.ErrSessionBusy):
		code = codes.Aborted
	case errors.Is(err, sqlitelog.ErrSessionNotFound):
		code = codes.NotFound
	case errors.Is(err, eventlog.ErrDeleted):
		code = codes.FailedPrecondition
	case errors.Is(err, context.Canceled):
		code = codes.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		code = codes.DeadlineExceeded
	}
	return withStatusCause(code, fmt.Errorf("delete session %q: %w", uid, err))
}
func deletedSessionError(info sqlitelog.SessionInfo) error {
	if info.DeletionState == api.DeletionActive {
		return nil
	}
	return withStatusCause(codes.FailedPrecondition, fmt.Errorf("session %q: %w", info.UID, eventlog.ErrDeleted))
}
