package session

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/authn"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/observability"
	"github.com/agent-primitives/sessions/placement"
)

// CloseApproval writes the host records that close one approval gate, under the session guard
// shared with Exec, Resume, Approve and stateful Fork. Like Approve it resolves no harness and runs
// no compute; unlike Approve it needs approval.close rather than approve, so it can deny or revoke
// but never approve. The closer is the authenticated caller, never a request field.
func (s *Service) CloseApproval(ctx context.Context, req *v1.CloseApprovalRequest) (response *v1.CloseApprovalResponse, err error) {
	ctx = observability.EnsureRequestID(ctx)
	defer s.auth.internalError(ctx, s.logger, v1.Sessions_CloseApproval_FullMethodName, &err)
	if err := s.authorizeMutation(ctx, v1.Sessions_CloseApproval_FullMethodName, req.GetSession(), api.ActionRead, api.ActionApprovalClose); err != nil {
		return nil, err
	}
	ctx, finish := observability.Begin(ctx, s.logger, "session", "close_approval", "session_uid", req.GetSession())
	defer func() { finish(err, "error_kind", serviceErrorKind(err)) }()
	if req.GetSession() == "" || req.GetExecutionId() == "" || req.GetToolCallId() == "" || req.GetRequestSeq() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "session, execution_id, tool_call_id and positive request_seq are required")
	}
	uid := req.GetSession()
	// Session(uid) can create a journal on mutation; lookup must precede the close primitive.
	if err := s.registry.WithSessionRead(uid, func() error {
		info, err := s.store.SessionInfo(uid)
		if err != nil {
			return sessionStoreError(err, uid)
		}
		return deletedSessionError(info)
	}); err != nil {
		if errors.Is(err, placement.ErrSessionBusy) {
			return nil, closeApprovalError(err)
		}
		return nil, err
	}
	s.chainTrace(ctx, uid)
	// Only the authentication interceptors install a caller. Without authentication an undecided
	// gate's denial records no approver, as Approve does.
	closer, _ := authn.Caller(ctx)
	decision, receipt, err := s.registry.CloseApproval(ctx, s.store.SessionLog(uid), uid, api.ApprovalRef{
		ExecutionID: req.GetExecutionId(), ToolCallID: req.GetToolCallId(), RequestSeq: req.GetRequestSeq(),
	}, closer)
	if err != nil {
		return nil, closeApprovalError(err)
	}
	info, err := s.store.SessionInfo(uid)
	if err != nil {
		return nil, sessionStoreError(err, uid)
	}
	sess, err := sessionProto(info)
	if err != nil {
		return nil, err
	}
	return &v1.CloseApprovalResponse{
		Session: sess, Decision: eventlog.RecordToProto(decision), Receipt: eventlog.RecordToProto(receipt),
	}, nil
}

// closeApprovalError maps errors as approvalError does, plus DATA_LOSS for a journal that fails
// chain verification, which CloseApproval checks before it chooses a record to write.
func closeApprovalError(err error) error {
	if errors.Is(err, eventlog.ErrTampered) {
		// The journal failed hash-chain verification (CHAIN-12); retrying cannot help.
		return status.Error(codes.DataLoss, err.Error())
	}
	if mapped := approvalError(err); status.Code(mapped) != codes.Internal {
		return mapped
	}
	return hostInternalError("close approval: %v", err)
}
