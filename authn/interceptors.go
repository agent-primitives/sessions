// Package authn provides opt-in gRPC caller authentication, not authorization.
// The reference daemon does not install these interceptors.
package authn

import (
	"context"
	"errors"
	"time"

	"github.com/agent-primitives/sessions/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// VerifiedCaller contains a verified identity and its credential expiry, never a
// bearer token, assertion or refresh credential.
type VerifiedCaller struct {
	Identity   api.CallerIdentity
	ValidUntil time.Time
}

// CallerVerifier verifies incoming credentials from metadata or a verified TLS
// peer. Implementations must reject missing, malformed, duplicate and expired
// credentials, validate issuer/audience and signatures, and honor cancellation.
// Never discover trust configuration from untrusted issuer URLs. Adapters own
// failure logging and metrics, without credential material; interceptors return
// generic authentication errors and do not log or count verifier failures.
type CallerVerifier interface {
	Verify(context.Context) (VerifiedCaller, error)
}

// Mode selects whether the interceptors authenticate callers.
type Mode string

const (
	// Off skips verification and installs no caller identity.
	Off Mode = "off"
	// Required rejects calls without a valid verified caller.
	Required Mode = "required"
)

type callerKey struct{}

// Caller returns the exact verified issuer/subject pair installed by an
// interceptor. Request identity fields and forwarded headers do not populate it.
func Caller(ctx context.Context) (api.CallerIdentity, bool) {
	c, ok := ctx.Value(callerKey{}).(api.CallerIdentity)
	return c, ok && c.Valid()
}

var (
	unauthenticated  = status.Error(codes.Unauthenticated, "authentication required")
	errCallerExpired = errors.New("authn: caller expired")
)

// NewInterceptors constructs authentication for every RPC on a gRPC server,
// without method exemptions. Install them before interceptors that disclose data
// or perform effects; a panic-recovery interceptor may wrap them, since grpc-go
// does not recover verifier panics. Required needs a verifier; invalid modes fail here.
// Authentication alone is not isolation: handlers still need authorization and
// operators must assemble a trusted transport. Unary authentication is admission-only:
// credential expiry does not end an admitted call or remove its caller identity.
// Only stream contexts are bounded by credential expiry. Stream expiry is cooperative;
// it cannot undo effects or interrupt transport I/O already in progress.
func NewInterceptors(mode Mode, verifier CallerVerifier) (grpc.UnaryServerInterceptor, grpc.StreamServerInterceptor, error) {
	if mode != Off && mode != Required {
		return nil, nil, errors.New("authn: invalid mode")
	}
	if mode == Required && verifier == nil {
		return nil, nil, errors.New("authn: required mode needs a verifier")
	}
	verify := func(ctx context.Context) (context.Context, time.Time, error) {
		if mode == Off {
			return context.WithValue(ctx, callerKey{}, api.CallerIdentity{}), time.Time{}, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, time.Time{}, status.FromContextError(err).Err()
		}
		c, err := verifier.Verify(ctx)
		if ctx.Err() != nil {
			return nil, time.Time{}, status.FromContextError(ctx.Err()).Err()
		}
		if err != nil || !c.Identity.Valid() || !c.ValidUntil.After(time.Now()) {
			return nil, time.Time{}, unauthenticated
		}
		return context.WithValue(ctx, callerKey{}, c.Identity), c.ValidUntil, nil
	}
	unary := func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, _, err := verify(ctx)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
	stream := func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, expiry, err := verify(ss.Context())
		if err != nil {
			return err
		}
		if mode == Off {
			return handler(srv, &callerStream{ServerStream: ss, ctx: ctx})
		}
		ctx, cancel := context.WithDeadlineCause(ctx, expiry, errCallerExpired)
		defer cancel()
		wrapped := &callerStream{ServerStream: ss, ctx: ctx}
		err = handler(srv, wrapped)
		if ended := wrapped.contextError(); ended != nil {
			return ended
		}
		return err
	}
	return unary, stream, nil
}

type callerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *callerStream) Context() context.Context { return s.ctx }

func (s *callerStream) contextError() error {
	if s.ctx.Err() == nil {
		return nil
	}
	if errors.Is(context.Cause(s.ctx), errCallerExpired) {
		return unauthenticated
	}
	return status.FromContextError(s.ctx.Err()).Err()
}

func (s *callerStream) SendMsg(m any) error {
	if err := s.contextError(); err != nil {
		return err
	}
	return s.ServerStream.SendMsg(m)
}

func (s *callerStream) RecvMsg(m any) error {
	if err := s.contextError(); err != nil {
		return err
	}
	err := s.ServerStream.RecvMsg(m)
	if ended := s.contextError(); ended != nil {
		return ended
	}
	return err
}
