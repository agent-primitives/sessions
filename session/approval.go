package session

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/aramase/agentsessions/api"
	v1 "github.com/aramase/agentsessions/api/genpb"
	"github.com/aramase/agentsessions/controller"
	"github.com/aramase/agentsessions/eventlog"
	"github.com/aramase/agentsessions/observability"
	"github.com/aramase/agentsessions/placement"
	"github.com/aramase/agentsessions/wire"
)

// Approve commits only the supplied decision. It deliberately does not resolve a harness:
// decisions remain available while compute is unavailable or the recorded route is unserved.
func (s *Service) Approve(ctx context.Context, req *v1.ApproveRequest) (response *v1.ApproveResponse, err error) {
	ctx = observability.EnsureRequestID(ctx)
	finish := observability.Start(ctx, s.logger, "session", "approve", "session_uid", req.GetSession())
	defer func() { finish(err, "error_kind", serviceErrorKind(err)) }()
	if req.GetSession() == "" || req.GetExecutionId() == "" || req.GetToolCallId() == "" || req.GetRequestSeq() <= 0 || req.Approved == nil {
		return nil, status.Error(codes.InvalidArgument, "session, execution_id, tool_call_id, positive request_seq and approved presence are required")
	}
	uid := req.GetSession()
	// Session(uid) can create a journal on mutation; lookup must precede the decision primitive.
	if _, err := s.store.SessionInfo(uid); err != nil {
		return nil, sessionStoreError(err, uid)
	}
	decision, err := s.registry.Approve(ctx, s.store.Session(uid), uid, api.ApprovalDecision{
		ExecutionID: req.GetExecutionId(), ToolCallID: req.GetToolCallId(), RequestSeq: req.GetRequestSeq(),
		Approved: req.GetApproved(), Reason: req.GetReason(), Identity: wire.EventFromProto(&v1.Event{Actor: req.GetIdentity()}).Actor,
	})
	if err != nil {
		return nil, approvalError(err)
	}
	info, err := s.store.SessionInfo(uid)
	if err != nil {
		return nil, sessionStoreError(err, uid)
	}
	sess, err := sessionProto(info)
	if err != nil {
		return nil, err
	}
	return &v1.ApproveResponse{Session: sess, Decision: eventlog.RecordToProto(decision)}, nil
}

func approvalError(err error) error {
	switch {
	case errors.Is(err, controller.ErrInvalidApprovalDecision), errors.Is(err, controller.ErrInvalidExecutionLog), errors.Is(err, controller.ErrReplayDiverged), errors.Is(err, controller.ErrApprovalUnavailable):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, placement.ErrSessionBusy), errors.Is(err, placement.ErrCheckpointing), errors.Is(err, eventlog.ErrConflict), errors.Is(err, eventlog.ErrFenced):
		return status.Error(codes.Aborted, err.Error())
	default:
		return status.Errorf(codes.Internal, "approve: %v", err)
	}
}
