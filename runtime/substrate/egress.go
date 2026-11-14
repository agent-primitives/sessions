package substrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/observability"
)

// EgressPolicy is the network egress an actor is allowed: a set of allow rules, mirroring
// substrate's EgressPolicy resource without its metadata. Substrate denies all egress to an actor
// that has no policy, and denies traffic that matches no rule. The types are data only: substrate
// validates them (hostname syntax, rule and port limits, rules that tie) when the policy is created.
type EgressPolicy struct {
	Rules []EgressRule
}

// EgressProtocol selects what an EgressRule matches on, as substrate's EgressRule protocols do.
type EgressProtocol int

const (
	// EgressProtocolUnspecified is the zero value. A control client refuses a rule that leaves it.
	EgressProtocolUnspecified EgressProtocol = iota
	// EgressHTTP allows cleartext HTTP (HTTP/1.1 and h2c), decided per request on its authority.
	EgressHTTP
	// EgressHTTPS allows HTTPS that the gateway intercepts, decided per request inside the TLS
	// connection. The actor must trust the gateway CA.
	EgressHTTPS
	// EgressTLSPassthrough allows TLS the gateway forwards without decrypting, decided once per
	// connection on the SNI. It carries no header effects.
	EgressTLSPassthrough
)

// String returns the protocol name as substrate's EgressRule spells the field.
func (p EgressProtocol) String() string {
	switch p {
	case EgressHTTP:
		return "http"
	case EgressHTTPS:
		return "https"
	case EgressTLSPassthrough:
		return "tls_passthrough"
	default:
		return fmt.Sprintf("EgressProtocol(%d)", int(p))
	}
}

// EgressRule allows traffic of one protocol to a set of hostnames and ports.
type EgressRule struct {
	Protocol EgressProtocol
	// Hostnames are DNS names, or DNS names whose leftmost label is "*", matched against the
	// request authority (EgressHTTP) or the TLS SNI.
	Hostnames []string
	// Ports are destination port numbers. Leave both Ports and AllPorts unset for substrate's
	// default (80 for EgressHTTP, 443 for EgressHTTPS), which substrate stores, so
	// GetActorEgressPolicy returns it in Ports; EgressTLSPassthrough requires one. Setting both is
	// refused.
	Ports    []int32
	AllPorts bool
	// ReplaceHeaders are substrate egress credential effects: request headers the gateway replaces
	// with a credential when the request carries them. Only EgressHTTP and EgressHTTPS rules may
	// carry them.
	ReplaceHeaders []CredentialHeader
}

// CredentialHeader is a substrate egress credential effect: the gateway replaces the value of
// Header, when a request carries it, with Prefix followed by a credential from exactly one source,
// CredentialURI or ActorJWT.
type CredentialHeader struct {
	Header string
	Prefix string
	// CredentialURI references a credential substrate's registered provider resolves
	// (ate-secret://<provider-class>/<provider-name>/<tail>). It is a reference, not the secret.
	CredentialURI string
	// ActorJWT has the gateway mint a substrate-issued JWT for the actor that sent the request.
	ActorJWT *ActorJWTSource
}

// ActorJWTSource is the substrate-issued actor JWT a CredentialHeader injects.
type ActorJWTSource struct {
	Audiences         []string
	ExpirationSeconds int64
}

// EgressPolicyInfo is an actor's stored egress policy with the server-assigned identity and
// version that UpdateActorEgressPolicy takes as its precondition.
type EgressPolicyInfo struct {
	Policy  EgressPolicy
	UID     string
	Version int64
}

// EgressPolicyClient is the OPTIONAL egress-policy extension of ControlClient, backed by substrate's
// actor EgressPolicy APIs (one policy per actor). Like SnapshotCloner it is separate so a control
// client without it still satisfies ControlClient: a backend with no WithEgressPolicy never needs
// it, and one with a policy configured refuses to start compute instead of starting it without the
// policy (ErrEgressPolicyUnsupported).
//
// The backend calls only CreateActorEgressPolicy. The others let an embedder read, replace or
// remove a policy; the backend never changes the policy of an existing actor.
type EgressPolicyClient interface {
	// GetActorEgressPolicy reads the actor's policy. No policy is ErrEgressPolicyNotFound, which
	// substrate also reports for a missing actor.
	GetActorEgressPolicy(ctx context.Context, actor ActorRef) (EgressPolicyInfo, error)
	// CreateActorEgressPolicy creates the actor's policy. An actor that already has one must be
	// reported as (or wrap) ErrEgressPolicyExists, and a missing actor as ErrActorNotFound.
	CreateActorEgressPolicy(ctx context.Context, actor ActorRef, policy EgressPolicy) error
	// UpdateActorEgressPolicy replaces the actor's policy if its UID and Version still match
	// precond (read with GetActorEgressPolicy), and returns the stored result. A mismatch is
	// ErrActorBusy (retryable: read again), no policy is ErrEgressPolicyNotFound.
	UpdateActorEgressPolicy(ctx context.Context, actor ActorRef, policy EgressPolicy, precond EgressPolicyInfo) (EgressPolicyInfo, error)
	// DeleteActorEgressPolicy removes the actor's policy, leaving it deny-all. No policy is
	// ErrEgressPolicyNotFound.
	DeleteActorEgressPolicy(ctx context.Context, actor ActorRef) error
}

// ErrEgressPolicyExists is what an EgressPolicyClient must return (or wrap) from
// CreateActorEgressPolicy when the actor already has a policy. The backend keeps that policy.
var ErrEgressPolicyExists = errors.New("substrate: actor egress policy already exists")

// ErrEgressPolicyNotFound is what an EgressPolicyClient must return (or wrap) when the actor has no
// policy.
var ErrEgressPolicyNotFound = errors.New("substrate: actor egress policy not found")

// ErrEgressPolicyInvalid is what an EgressPolicyClient must return (or wrap) from
// CreateActorEgressPolicy and UpdateActorEgressPolicy when the policy itself is refused:
// substrate's INVALID_ARGUMENT, or a rule the client cannot express. The backend reports a refused
// install as api.ErrComputeConfigInvalid, since it repeats until the configured policy changes,
// and leaves the client's message out of the error text, since it can name a rejected hostname or
// credential reference. Go callers reach the client's error through Unwrap; logs record only the
// error_kind egress_policy_invalid, as for every runtime error.
var ErrEgressPolicyInvalid = errors.New("substrate: actor egress policy is invalid")

// ErrEgressPolicyUnsupported is returned when WithEgressPolicy yields a policy but the backend's
// ControlClient does not implement EgressPolicyClient. No actor is resumed: starting compute
// without the policy it was configured with is refused. The backend's error also wraps
// api.ErrComputeConfigInvalid.
var ErrEgressPolicyUnsupported = errors.New("substrate: egress policy configured but the control client does not implement EgressPolicyClient")

// errEgressPolicyFunc marks an error returned by the WithEgressPolicy function.
var errEgressPolicyFunc = errors.New("substrate: egress policy function")

// errEgressPolicyInstall marks a failed CreateActorEgressPolicy.
var errEgressPolicyInstall = errors.New("substrate: install egress policy")

// WithEgressPolicy installs an egress policy on each actor before the backend first resumes it, so
// the actor's compute never runs without it. policy returns the policy for one session; nil, with
// no error, installs none, and the actor stays deny-all as substrate leaves an actor without a
// policy.
//
// policy receives a spec whose SessionUID is set (the child's on Fork). No other field is set
// today, on any path; later releases may set more, so key per-session policies on SessionUID and do
// not rely on other fields being zero. It runs at most once per backend call, and only when the
// call is about to start an actor's compute: Create before it creates an actor (so an error, or a
// ControlClient without EgressPolicyClient, refuses before any actor exists) or before it resumes a
// SUSPENDED one, Restore before it resumes, and Fork before it tags the parent.
//
// The policy is installed with CreateActorEgressPolicy between the backend's CreateActor (or a
// fork's create from tag) and its ResumeActor, and again before every later ResumeActor the
// backend issues. An actor that already has a policy keeps it, so a changed policy reaches only
// actors created after the change; nothing converges existing actors. A failed install fails the
// call without resuming the actor, which stays SUSPENDED (a fork's clone is torn down).
//
// Refusals that repeat until the configuration changes wrap api.ErrComputeConfigInvalid
// (FAILED_PRECONDITION at the Sessions API): a ControlClient without EgressPolicyClient, and a
// policy the client refuses as invalid (ErrEgressPolicyInvalid), whose message the error text
// leaves out. An error from policy itself, and any other install failure, is not classified.
//
// The backend issues no egress call for an actor it finds RUNNING (Create attaching to it, or
// Restore of it): it does not change the policy of running compute. Such an actor without a policy,
// one running before the option was set or woken by a routed request after a failed install, stays
// deny-all until it is suspended and the backend resumes it.
func WithEgressPolicy(policy func(ctx context.Context, s *api.SessionSpec) (*EgressPolicy, error)) Option {
	return func(b *Backend) { b.egressPolicy = policy }
}

// egressInstall resolves and installs the egress policy for one session within one backend call.
// It evaluates the WithEgressPolicy function at most once, on first use.
type egressInstall struct {
	b          *Backend
	sessionUID string
	resolved   bool
	policy     *EgressPolicy
	client     EgressPolicyClient
	err        error
}

// egressFor returns the egress install of one session for one backend call.
func (b *Backend) egressFor(sessionUID string) *egressInstall {
	return &egressInstall{b: b, sessionUID: sessionUID}
}

// resolve evaluates the WithEgressPolicy function and checks that the ControlClient can install
// what it returns. Without the option, or with a nil policy, there is nothing to install.
func (e *egressInstall) resolve(ctx context.Context) error {
	if e.resolved || e.b.egressPolicy == nil {
		return e.err
	}
	e.resolved = true
	policy, err := e.b.egressPolicy(ctx, &api.SessionSpec{SessionUID: e.sessionUID})
	switch {
	case err != nil:
		e.err = fmt.Errorf("%w for session %q: %w", errEgressPolicyFunc, e.sessionUID, err)
	case policy == nil:
	default:
		client, ok := e.b.ctl.(EgressPolicyClient)
		if !ok {
			e.err = fmt.Errorf("%w: %w", api.ErrComputeConfigInvalid, ErrEgressPolicyUnsupported)
			break
		}
		e.policy, e.client = policy, client
	}
	return e.err
}

// install creates the session's policy on actor, resolving it first if needed. An actor that
// already has a policy keeps it. The error wraps the client's, so ErrActorNotFound and ErrActorBusy
// still classify it; a policy the client refuses (ErrEgressPolicyInvalid) is
// egressPolicyRefusedError.
func (e *egressInstall) install(ctx context.Context, actor ActorRef) error {
	if err := e.resolve(ctx); err != nil || e.policy == nil {
		return err
	}
	callCtx, finish := observability.BeginDebug(ctx, e.b.logger, "runtime.substrate", "install_egress_policy",
		"actor", actor.Name,
		"atespace", actor.Atespace,
		"rules", len(e.policy.Rules),
	)
	err := e.client.CreateActorEgressPolicy(callCtx, actor, *e.policy)
	switch {
	case err == nil:
		finish(nil)
	case errors.Is(err, ErrEgressPolicyExists):
		finish(nil, "policy_exists", true)
	case errors.Is(err, ErrEgressPolicyInvalid):
		finish(err, "error_kind", "egress_policy_invalid")
		return &egressPolicyRefusedError{actor: actor, cause: err}
	default:
		finish(err, "error_kind", "egress_policy_failed")
		return fmt.Errorf("%w on actor %s/%s: %w", errEgressPolicyInstall, actor.Atespace, actor.Name, err)
	}
	return nil
}

// egressPolicyRefusedError reports a policy the control client refused as invalid
// (ErrEgressPolicyInvalid). It is api.ErrComputeConfigInvalid: the same policy is refused on every
// call until the configuration changes. Its text is fixed, because it reaches Sessions callers
// even with authorization on and the client's message can name a rejected hostname or credential
// reference. Go callers still reach the cause through Unwrap.
type egressPolicyRefusedError struct {
	actor ActorRef
	cause error
}

func (e *egressPolicyRefusedError) Error() string {
	return fmt.Sprintf("%v: %v on actor %s/%s: %v; "+
		"check the configured policy against substrate's EgressPolicy validation",
		api.ErrComputeConfigInvalid, errEgressPolicyInstall, e.actor.Atespace, e.actor.Name,
		ErrEgressPolicyInvalid)
}

func (e *egressPolicyRefusedError) Unwrap() []error {
	return []error{api.ErrComputeConfigInvalid, errEgressPolicyInstall, e.cause}
}
