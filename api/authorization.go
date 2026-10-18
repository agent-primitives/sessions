package api

import (
	"context"
	"errors"
)

// CallerIdentity is the exact issuer/subject pair of a caller verified by a trusted
// host. It is not IdentityRef (agent provenance), and constructing it performs no
// cryptographic verification. Do not populate it from caller-supplied metadata.
type CallerIdentity struct {
	Issuer  string
	Subject string
}

// Valid reports whether both parts of the authority key are present. No trimming,
// case folding or issuer-prefix mapping is performed.
func (c CallerIdentity) Valid() bool { return c.Issuer != "" && c.Subject != "" }

// ErrInvalidCallerIdentity reports an incomplete owner passed to owned creation
// or an empty/incomplete owner filter passed to a session listing.
var ErrInvalidCallerIdentity = errors.New("api: caller identity requires issuer and subject")

// Action identifies a permission independently of the RPC or transport.
type Action string

const (
	ActionRead    Action = "read"
	ActionExec    Action = "exec"
	ActionFork    Action = "fork"
	ActionSuspend Action = "suspend"
	ActionResume  Action = "resume"
	ActionDelete  Action = "delete"
	ActionCancel  Action = "cancel"
	ActionCreate  Action = "create"
	ActionApprove Action = "approve"
	ActionMint    Action = "mint"
)

// AuthorizationRequest carries the verified caller and trusted stored owner of
// one session. It is not a request-supplied ownership assignment. Stateful Fork
// requires a separate suspend check; Resume that drives effects also requires exec.
type AuthorizationRequest struct {
	Caller     CallerIdentity
	Action     Action
	SessionUID string
	Owner      CallerIdentity
}

// Authorizer decides one resource permission. A denial must precede disclosure or
// effects. Listing needs an authorized storage query, not post-filtered pages.
// This contract is host-side only; Sessions RPCs do not yet invoke it.
type Authorizer interface {
	Authorize(context.Context, AuthorizationRequest) error
}

// ErrPermissionDenied is returned by OwnerAuthorizer when the caller does not own
// the session, either identity is incomplete, the session UID is missing, or the
// action is not an ordinary owner action (including create, approve and mint).
var ErrPermissionDenied = errors.New("api: permission denied")

// OwnerAuthorizer is the first owner/grants policy implementation. It supports
// owner checks only; no grants are interpreted. Approval decisions, audience
// minting and creation require separate policy and are denied even to owners.
// ActionFork authorizes the parent only; it does not assign child ownership.
// The current fork path creates unbound children. Enforcing hosts need a separate
// owned-child reservation/publication primitive before exposing owned forks.
type OwnerAuthorizer struct{}

var _ Authorizer = OwnerAuthorizer{}

func (OwnerAuthorizer) Authorize(ctx context.Context, r AuthorizationRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.SessionUID == "" || !r.Caller.Valid() || !r.Owner.Valid() || r.Caller != r.Owner {
		return ErrPermissionDenied
	}
	switch r.Action {
	case ActionRead, ActionExec, ActionFork, ActionSuspend, ActionResume, ActionDelete, ActionCancel:
		return nil
	default:
		return ErrPermissionDenied
	}
}
