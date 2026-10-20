package session

import (
	"context"
	"errors"
	"log/slog"

	"github.com/agent-primitives/sessions/api"
	pb "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/authn"
	"github.com/agent-primitives/sessions/observability"
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
		return &authorizationFailure{status.New(codes.Internal, "internal error")}
	}
}

// Mark already-sanitized authorization failures by origin, not by their text.
// In particular, the deferred host diagnostic must never log a raw policy error.
type authorizationFailure struct{ status *status.Status }

func (e *authorizationFailure) Error() string              { return e.status.Err().Error() }
func (e *authorizationFailure) GRPCStatus() *status.Status { return e.status }

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
func (a *authorization) global(ctx context.Context, method string, action api.Action) error {
	if a == nil {
		return nil
	}
	caller, err := a.caller(ctx)
	if err != nil {
		return err
	}
	return a.check(ctx, api.AuthorizationRequest{Caller: caller, Method: method, Action: action})
}
func (s *Service) authorize(ctx context.Context, method, uid string, actions ...api.Action) error {
	return s.authorizeWith(ctx, method, uid, s.store.SessionInfo, actions...)
}

// Probe deletion admission before owner SQL, but do not hold it across the query:
// callers that policy will deny must not block the owner's Delete while waiting
// for the Store connection. The mutation's later guarded phases remain authoritative.
func (s *Service) authorizeMutation(ctx context.Context, method, uid string, actions ...api.Action) error {
	return s.authorizeWith(ctx, method, uid, func(uid string) (sqlitelog.SessionInfo, error) {
		if err := s.mutationRead(uid, func() error { return nil }); err != nil {
			return sqlitelog.SessionInfo{}, err
		}
		return s.store.SessionInfo(uid)
	}, actions...)
}

func (s *Service) authorizeWith(ctx context.Context, method, uid string, lookup func(string) (sqlitelog.SessionInfo, error), actions ...api.Action) error {
	if s.auth == nil {
		return nil
	}
	caller, err := s.auth.caller(ctx)
	if err != nil {
		return err
	}
	info, err := lookup(uid)
	if errors.Is(err, placement.ErrSessionBusy) {
		return err // mutationRead already maps deletion contention to ABORTED.
	}
	if errors.Is(err, sqlitelog.ErrSessionNotFound) {
		return authorizationError(ctx, api.ErrPermissionDenied)
	}
	if err != nil {
		logHostError(ctx, s.logger, method, err)
		return authorizationError(ctx, err)
	}
	return s.auth.checkActions(ctx, api.AuthorizationRequest{Caller: caller, Owner: info.Owner, SessionUID: uid, Method: method}, actions...)
}

// Ownership is immutable; recovery can reuse the owner captured by its read phase.
func (s *Service) authorizeOwner(ctx context.Context, method, uid string, owner api.CallerIdentity, actions ...api.Action) error {
	if s.auth == nil {
		return nil
	}
	caller, err := s.auth.caller(ctx)
	if err != nil {
		return err
	}
	return s.auth.checkActions(ctx, api.AuthorizationRequest{Caller: caller, Owner: owner, SessionUID: uid, Method: method}, actions...)
}

func (a *authorization) checkActions(ctx context.Context, request api.AuthorizationRequest, actions ...api.Action) error {
	for _, action := range actions {
		request.Action = action
		if err := a.check(ctx, request); err != nil {
			return err
		}
	}
	return nil
}

// hostFailure marks errors constructed at trusted host/store boundaries. Unknown
// origins fail closed for logging: execution/runtime failures may carry provider
// bodies, harness text or credential-bearing URLs, even after status mapping.
type hostFailure struct{ error }

func (e *hostFailure) GRPCStatus() *status.Status { return status.Convert(e.error) }

func hostInternalError(format string, args ...any) error {
	return &hostFailure{status.Errorf(codes.Internal, format, args...)}
}

// internalError keeps diagnostics off enforced responses without changing off-mode
// errors. Policies own their diagnostics; untrusted messages are never logged.
func (a *authorization) internalError(ctx context.Context, logger *slog.Logger, method string, err *error) {
	if a != nil && status.Code(*err) == codes.Internal {
		var sanitized *authorizationFailure
		var host *hostFailure
		switch {
		case errors.As(*err, &sanitized):
			// Policy failures or an owner lookup already logged at its source.
		case errors.As(*err, &host):
			logHostError(ctx, logger, method, *err)
		default:
			category := "external"
			switch method {
			case pb.Sessions_Exec_FullMethodName, pb.Sessions_Resume_FullMethodName:
				category = "execution"
			case pb.Sessions_Suspend_FullMethodName, pb.Sessions_Fork_FullMethodName:
				category = "runtime"
			}
			observability.Logger(ctx, logger).ErrorContext(ctx, "external operation failed",
				"method", method, "error_kind", "internal", "error_category", category)
		}
		*err = status.Error(codes.Internal, "internal error")
	}
}

// No request data or unknown-origin error text belongs in this diagnostic.
func logHostError(ctx context.Context, logger *slog.Logger, method string, err error) {
	observability.Logger(ctx, logger).ErrorContext(ctx, "host operation failed",
		"method", method, "error_kind", "internal", "error_category", "host", "error", err)
}

// Cancel remains unimplemented after its independent permission check.
func (s *Service) Cancel(ctx context.Context, req *pb.CancelRequest) (*pb.Session, error) {
	if err := s.authorize(observability.EnsureRequestID(ctx), pb.Sessions_Cancel_FullMethodName, req.GetSession(), api.ActionCancel); err != nil {
		return nil, err
	}
	return s.UnimplementedSessionsServer.Cancel(ctx, req)
}
