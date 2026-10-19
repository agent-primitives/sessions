package session

import (
	"context"
	"errors"

	"github.com/agent-primitives/sessions/api"
	pb "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/authn"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/sqlitelog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// WithAuthorization requires a verified authn.Caller and authorizes each Sessions
// RPC before disclosure or effects. A nil policy fails closed, not back to off.
// This is an embedded-host seam, not a complete deployment mode: journals remain
// marked off, owned Fork is refused, and verified approval audit is not available.
// Do not share its journal with unenforced writers. The reference daemon does not
// install this option; activation needs journal policy/startup and TLS assembly.
func WithAuthorization(policy api.Authorizer) Option {
	return func(s *Service) { s.auth = &authorization{policy: policy} }
}

// HarnessRegistryOption configures authorization on the separate operator service.
type HarnessRegistryOption func(*HarnessRegistry)

// WithRegistryAuthorization requires the same verified-caller boundary as
// WithAuthorization, with distinct global registry.* permissions. It does not
// authenticate the registry's outbound clients or enable its listener.
func WithRegistryAuthorization(policy api.Authorizer) HarnessRegistryOption {
	return func(h *HarnessRegistry) { h.auth = &authorization{policy: policy} }
}

type authorization struct{ policy api.Authorizer }

func (a *authorization) caller(ctx context.Context) (api.CallerIdentity, error) {
	if err := ctx.Err(); err != nil {
		return api.CallerIdentity{}, status.FromContextError(err).Err()
	}
	caller, ok := authn.Caller(ctx)
	if !ok {
		return api.CallerIdentity{}, status.Error(codes.Unauthenticated, "authentication required")
	}
	return caller, nil
}
func authorizationError(ctx context.Context, err error) error {
	switch {
	case ctx.Err() != nil:
		return status.FromContextError(ctx.Err()).Err()
	case errors.Is(err, api.ErrPermissionDenied):
		return status.Error(codes.PermissionDenied, "permission denied")
	default:
		return status.Error(codes.Internal, "internal error")
	}
}
func (a *authorization) check(ctx context.Context, r api.AuthorizationRequest) error {
	if a.policy == nil {
		return authorizationError(ctx, errors.New("missing authorizer"))
	}
	if err := a.policy.Authorize(ctx, r); err != nil {
		return authorizationError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return authorizationError(ctx, err)
	}
	return nil
}
func (a *authorization) global(ctx context.Context, action api.Action) error {
	if a == nil {
		return nil
	}
	caller, err := a.caller(ctx)
	if err != nil {
		return err
	}
	return a.check(ctx, api.AuthorizationRequest{Caller: caller, Action: action})
}
func (s *Service) authorize(ctx context.Context, uid string, actions ...api.Action) error {
	return s.authorizeWith(ctx, uid, s.store.SessionInfo, actions...)
}

// Mutation owner lookups obey deletion admission before waiting for SQL. The read
// hold ends before policy evaluation and before the mutation's own guarded phase.
func (s *Service) authorizeMutation(ctx context.Context, uid string, actions ...api.Action) error {
	return s.authorizeWith(ctx, uid, s.mutationSessionInfo, actions...)
}

func (s *Service) authorizeWith(ctx context.Context, uid string, lookup func(string) (sqlitelog.SessionInfo, error), actions ...api.Action) error {
	if s.auth == nil {
		return nil
	}
	caller, err := s.auth.caller(ctx)
	if err != nil {
		return err
	}
	info, err := lookup(uid)
	if errors.Is(err, placement.ErrSessionBusy) {
		return err // mutationSessionInfo already maps deletion contention to ABORTED.
	}
	if errors.Is(err, sqlitelog.ErrSessionNotFound) {
		return authorizationError(ctx, api.ErrPermissionDenied)
	}
	if err != nil {
		return authorizationError(ctx, err)
	}
	if !info.Owner.Valid() {
		return authorizationError(ctx, api.ErrPermissionDenied)
	}
	for _, action := range actions {
		if err := s.auth.check(ctx, api.AuthorizationRequest{Caller: caller, Owner: info.Owner, SessionUID: uid, Action: action}); err != nil {
			return err
		}
	}
	return nil
}

// internalError keeps policy, store and backend diagnostics off required-mode
// responses without changing the trusted-network/off service's existing errors.
func (a *authorization) internalError(err *error) {
	if a != nil && status.Code(*err) == codes.Internal {
		*err = status.Error(codes.Internal, "internal error")
	}
}

// Cancel remains unimplemented after its independent permission check.
func (s *Service) Cancel(ctx context.Context, req *pb.CancelRequest) (*pb.Session, error) {
	if err := s.authorize(ctx, req.GetSession(), api.ActionCancel); err != nil {
		return nil, err
	}
	return s.UnimplementedSessionsServer.Cancel(ctx, req)
}
