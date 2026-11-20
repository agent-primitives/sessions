package substrate

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

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
// it, and one with WithEgressPolicy set refuses (ErrEgressPolicyUnsupported) to resume an existing
// actor, or to create one when the function returns a policy, instead of running compute on a
// policy it cannot converge.
//
// With WithEgressPolicy set, the backend calls GetActorEgressPolicy, CreateActorEgressPolicy and
// UpdateActorEgressPolicy to converge an actor's stored policy on the configured one, so a client
// used with that option must implement all three; it never calls DeleteActorEgressPolicy, which is
// for embedders. Without the option the backend makes no egress call.
type EgressPolicyClient interface {
	// GetActorEgressPolicy reads the actor's policy. No policy is ErrEgressPolicyNotFound, which
	// substrate also reports for a missing actor. A stored policy the client cannot convert is
	// ErrEgressPolicyInvalid. A policy it returns carries the UID and Version that
	// UpdateActorEgressPolicy takes as its precondition.
	GetActorEgressPolicy(ctx context.Context, actor ActorRef) (EgressPolicyInfo, error)
	// CreateActorEgressPolicy creates the actor's policy. An actor that already has one must be
	// reported as (or wrap) ErrEgressPolicyExists, and a missing actor as ErrActorNotFound.
	CreateActorEgressPolicy(ctx context.Context, actor ActorRef, policy EgressPolicy) error
	// UpdateActorEgressPolicy replaces the actor's policy if its UID and Version still match
	// precond (read with GetActorEgressPolicy), and returns the stored result. A mismatch is
	// ErrActorBusy (retryable: read again), no policy is ErrEgressPolicyNotFound, and a missing
	// actor is ErrActorNotFound.
	UpdateActorEgressPolicy(ctx context.Context, actor ActorRef, policy EgressPolicy, precond EgressPolicyInfo) (EgressPolicyInfo, error)
	// DeleteActorEgressPolicy removes the actor's policy, leaving it deny-all. No policy is
	// ErrEgressPolicyNotFound.
	DeleteActorEgressPolicy(ctx context.Context, actor ActorRef) error
}

// ErrEgressPolicyExists is what an EgressPolicyClient must return (or wrap) from
// CreateActorEgressPolicy when the actor already has a policy. The backend then reads that policy
// and converges it as it does any existing actor's.
var ErrEgressPolicyExists = errors.New("substrate: actor egress policy already exists")

// ErrEgressPolicyNotFound is what an EgressPolicyClient must return (or wrap) when the actor has no
// policy.
var ErrEgressPolicyNotFound = errors.New("substrate: actor egress policy not found")

// ErrEgressPolicyInvalid is what an EgressPolicyClient must return (or wrap) from
// CreateActorEgressPolicy and UpdateActorEgressPolicy when the policy itself is refused:
// substrate's INVALID_ARGUMENT, or a rule the client cannot express; and from
// GetActorEgressPolicy when the stored policy cannot be converted. The backend reports either as
// api.ErrComputeConfigInvalid, since it repeats until the configured policy (or the client)
// changes, and leaves the client's message out of the error text, since it can name a rejected
// hostname or credential reference. A stored policy that cannot be read is never overwritten:
// without it the backend has nothing to compare with and no precondition to write under. Go
// callers reach the client's error through Unwrap; logs record only the error_kind
// egress_policy_invalid, as for every runtime error.
var ErrEgressPolicyInvalid = errors.New("substrate: actor egress policy is invalid")

// ErrEgressPolicyUnsupported is returned when WithEgressPolicy is set but the backend's
// ControlClient does not implement EgressPolicyClient, and the backend would have to read or
// write a policy: before it resumes an existing SUSPENDED actor, whatever the function returns (a
// nil result must clear a stored policy, which takes a read and an update), and before Create or
// Fork creates an actor when the function returns a policy. A
// function that returns nil for a new actor needs no call, since a new actor has no policy. No
// actor is resumed: starting compute on a policy the backend cannot converge is refused. The
// backend's error also wraps api.ErrComputeConfigInvalid.
var ErrEgressPolicyUnsupported = errors.New("substrate: egress policy configured but the control client does not implement EgressPolicyClient")

// errEgressPolicyFunc marks an error returned by the WithEgressPolicy function.
var errEgressPolicyFunc = errors.New("substrate: egress policy function")

// errEgressPolicyInstall marks a failed egress policy read or write.
var errEgressPolicyInstall = errors.New("substrate: install egress policy")

// WithEgressPolicy converges each actor's egress policy on the one policy returns before the
// backend resumes a SUSPENDED actor (one it created or found suspended), so compute
// the backend resumes never runs without it, and never runs with a policy the function no longer
// returns. An actor already RUNNING when the backend reaches it, or one the substrate router wakes,
// keeps its stored policy until it is suspended and the backend resumes it. policy returns the
// policy for one session; nil, with no error, is deny-all: an actor without a policy is left without
// one, and one that has a policy is cleared to an empty rule set, which substrate also enforces as
// deny-all.
//
// policy receives a spec whose SessionUID is set (the child's on Fork). No other field is set
// today, on any path; later releases may set more, so key per-session policies on SessionUID and do
// not rely on other fields being zero. It must return the current policy each time it is called,
// read after any change made before the call: the backend calls it after reading the actor's
// stored policy, so that a write it bases on the result cannot undo a newer one, and a function
// that caches converges only as fresh as its cache. It may run several times in one backend call:
// once before Create creates an actor and before Fork tags the parent (so an error, or a policy
// with a ControlClient without EgressPolicyClient, refuses before any actor exists; that result is
// never written), and once for each attempt to converge, up to three. An error from any call
// fails the backend call without resuming the actor, and is not retried. Return rules in a stable
// order: rule order is compared, so a reordered policy is written again.
//
// Converging reads the stored policy (GetActorEgressPolicy) and compares it with the configured
// one by meaning: a rule that leaves its ports unset equals one with substrate's default port
// stored, and hostnames and ports compare as sets. An equal policy is left as it is, so an
// unchanged policy costs one read and no write. A different one is replaced with
// UpdateActorEgressPolicy, preconditioned on the UID and version just read, and a missing one is
// created. An actor the backend has just created (Create, or a fork's create from tag) has no
// policy, so it is created without a read. A precondition that no longer holds (another writer
// changed the policy) starts the read, the call to policy and the write over, up to three attempts
// in all, then fails with ErrActorBusy, which is retryable. The backend never deletes a policy.
//
// The converge runs between the backend's CreateActor (or a fork's create from tag) and its
// ResumeActor, and before every later ResumeActor the backend issues on a SUSPENDED actor: Create
// attaching to one, and Restore. A failed converge fails the call without resuming the actor, which
// stays SUSPENDED (a fork's clone is torn down). While the option is set, a policy edited in
// substrate by anyone else is overwritten at the actor's next backend resume. Without it, the
// backend makes no egress call, and policies already stored are kept.
//
// Refusals that repeat until the configuration changes wrap api.ErrComputeConfigInvalid
// (FAILED_PRECONDITION at the Sessions API): a ControlClient without EgressPolicyClient
// (ErrEgressPolicyUnsupported, which refuses every resume of an existing actor whatever the
// function returns), a policy the client refuses as invalid (ErrEgressPolicyInvalid), whose
// message the error text leaves out, and a stored policy the client cannot read. An error from
// policy itself, and any other egress call failure, is not classified.
//
// The backend issues no egress call for an actor it finds RUNNING (Create attaching to it, or
// Restore of it): a turn does not pay for a policy read. A running actor keeps the policy it has
// until it is suspended and the backend resumes it.
func WithEgressPolicy(policy func(ctx context.Context, s *api.SessionSpec) (*EgressPolicy, error)) Option {
	return func(b *Backend) { b.egressPolicy = policy }
}

// egressConvergeAttempts bounds the read, evaluate and write attempts of one converge. Each
// precondition another writer moved starts a new attempt.
const egressConvergeAttempts = 3

// egressConverge converges one session's egress policy on its actor within one backend call.
type egressConverge struct {
	b          *Backend
	sessionUID string
}

// egressFor returns the egress converge of one session for one backend call.
func (b *Backend) egressFor(sessionUID string) *egressConverge {
	return &egressConverge{b: b, sessionUID: sessionUID}
}

// desired evaluates the WithEgressPolicy function for the session.
func (e *egressConverge) desired(ctx context.Context) (*EgressPolicy, error) {
	policy, err := e.b.egressPolicy(ctx, &api.SessionSpec{SessionUID: e.sessionUID})
	if err != nil {
		return nil, fmt.Errorf("%w for session %q: %w", errEgressPolicyFunc, e.sessionUID, err)
	}
	return policy, nil
}

// errEgressUnsupported is the refusal of a policy the ControlClient cannot install.
func errEgressUnsupported() error {
	return fmt.Errorf("%w: %w", api.ErrComputeConfigInvalid, ErrEgressPolicyUnsupported)
}

// check refuses, before any actor exists, a function that fails or a policy the ControlClient
// cannot install. Its result is never written: converge evaluates the function again after it has
// read the state its write is preconditioned on.
func (e *egressConverge) check(ctx context.Context) error {
	if e.b.egressPolicy == nil {
		return nil
	}
	policy, err := e.desired(ctx)
	if err != nil {
		return err
	}
	if _, ok := e.b.ctl.(EgressPolicyClient); policy != nil && !ok {
		return errEgressUnsupported()
	}
	return nil
}

// Outcomes of a converge, logged as policy_outcome.
const (
	egressAbsent    = "absent"    // no policy wanted and none stored
	egressCreated   = "created"   // a policy created on an actor that had none
	egressUnchanged = "unchanged" // the stored policy already equals the configured one
	egressUpdated   = "updated"   // the stored policy replaced with the configured one
	egressCleared   = "cleared"   // the stored policy replaced with an empty (deny-all) rule set
)

// converge makes actor's stored policy the session's configured one. fresh says the actor was just
// created by this backend call, so it has no policy and the first attempt creates without a read.
// The error wraps the client's, so ErrActorNotFound and ErrActorBusy still classify it; a busy
// refusal (api.ErrComputeBusy) keeps the client's error in its chain but not in its text
// (busyRefusal); a policy the client refuses or cannot read (ErrEgressPolicyInvalid) is
// egressPolicyRefusedError. Without the option it does nothing. Without EgressPolicyClient it
// refuses an existing actor whatever the function returns, and a fresh one only when the function
// returns a policy.
func (e *egressConverge) converge(ctx context.Context, actor ActorRef, fresh bool) error {
	if e.b.egressPolicy == nil {
		return nil
	}
	client, ok := e.b.ctl.(EgressPolicyClient)
	if !ok {
		if !fresh {
			// An existing actor can hold a policy that only a read finds and only an update
			// clears, so even a nil result cannot be converged: resuming would keep that policy.
			return errEgressUnsupported()
		}
		// A fresh actor has no policy: none wanted needs no call, any other is refused.
		policy, err := e.desired(ctx)
		if err == nil && policy != nil {
			err = errEgressUnsupported()
		}
		return err
	}
	callCtx, finish := observability.BeginDebug(ctx, e.b.logger, "runtime.substrate", "install_egress_policy",
		"actor", actor.Name,
		"atespace", actor.Atespace,
		"fresh", fresh,
	)
	res, err := e.attempts(callCtx, client, actor, fresh)
	attrs := []any{"policy_outcome", res.outcome, "rules", res.rules, "attempts", res.attempts}
	switch {
	case err == nil:
		finish(nil, attrs...)
	case errors.Is(err, ErrEgressPolicyInvalid):
		finish(err, append(attrs, "error_kind", "egress_policy_invalid")...)
	default:
		finish(err, append(attrs, "error_kind", substrateErrorKind(err))...)
	}
	return err
}

// egressResult is what a converge did: its outcome, the configured policy's rule count (-1 before
// the function answered) and the attempts it made. It carries no policy contents.
type egressResult struct {
	outcome  string
	rules    int
	attempts int
}

// attempts runs up to egressConvergeAttempts attempts, each of which reads (unless fresh), asks the
// function and writes, and starts over only on a conflict.
func (e *egressConverge) attempts(ctx context.Context, client EgressPolicyClient, actor ActorRef, fresh bool) (egressResult, error) {
	res := egressResult{rules: -1}
	var conflict error
	for res.attempts = 1; ; res.attempts++ {
		var err error
		if fresh && res.attempts == 1 {
			conflict, err = e.createFresh(ctx, client, actor, &res)
		} else {
			conflict, err = e.convergeExisting(ctx, client, actor, &res)
		}
		if err != nil || conflict == nil {
			return res, err
		}
		if res.attempts == egressConvergeAttempts {
			break
		}
		if err := ctx.Err(); err != nil {
			return res, fmt.Errorf("%w on actor %s/%s: %w", errEgressPolicyInstall, actor.Atespace, actor.Name, err)
		}
	}
	res.outcome = ""
	// The conflict's text can carry substrate's own message, so it stays in the chain only.
	return res, &hiddenCause{
		err: fmt.Errorf("%w on actor %s/%s: its policy changed during each of %d attempts to converge it: %w",
			errEgressPolicyInstall, actor.Atespace, actor.Name, egressConvergeAttempts, ErrActorBusy),
		cause: conflict,
	}
}

// createFresh is the first attempt on an actor this call just created. The function is asked only
// now, after the actor exists, so what it returns reflects any change made before the actor did; a
// policy created in between (ErrEgressPolicyExists) is a conflict, and the next attempt reads it.
func (e *egressConverge) createFresh(ctx context.Context, client EgressPolicyClient, actor ActorRef, res *egressResult) (conflict, err error) {
	policy, err := e.desired(ctx)
	if err != nil {
		return nil, err
	}
	if policy == nil {
		res.rules, res.outcome = 0, egressAbsent
		return nil, nil
	}
	res.rules = len(policy.Rules)
	return e.create(ctx, client, actor, *policy, res)
}

// convergeExisting is one attempt on an actor that may already have a policy: read it, ask the
// function, and write only what differs, preconditioned on what was read.
func (e *egressConverge) convergeExisting(ctx context.Context, client EgressPolicyClient, actor ActorRef, res *egressResult) (conflict, err error) {
	stored, err := client.GetActorEgressPolicy(ctx, actor)
	found := err == nil
	switch {
	case errors.Is(err, ErrEgressPolicyNotFound):
	case errors.Is(err, ErrEgressPolicyInvalid):
		return nil, &egressPolicyRefusedError{actor: actor, cause: err, stored: true}
	case errors.Is(err, api.ErrComputeBusy):
		// Not a conflict: nothing was read to precondition a write on.
		return nil, busyRefusal(fmt.Sprintf("%v on actor %s/%s: read the stored policy", errEgressPolicyInstall, actor.Atespace, actor.Name), err)
	case err != nil:
		return nil, fmt.Errorf("%w on actor %s/%s: read the stored policy: %w", errEgressPolicyInstall, actor.Atespace, actor.Name, err)
	}
	policy, err := e.desired(ctx)
	if err != nil {
		return nil, err
	}
	res.rules = 0
	if policy != nil {
		res.rules = len(policy.Rules)
	}
	want, outcome := EgressPolicy{}, egressCleared
	switch {
	case !found && policy == nil:
		res.outcome = egressAbsent
		return nil, nil
	case !found:
		return e.create(ctx, client, actor, *policy, res)
	case policy == nil && len(stored.Policy.Rules) == 0:
		res.outcome = egressUnchanged
		return nil, nil
	case policy != nil && egressPolicyEqual(*policy, stored.Policy):
		res.outcome = egressUnchanged
		return nil, nil
	case policy != nil:
		want, outcome = *policy, egressUpdated
	}
	if stored.UID == "" || stored.Version == 0 {
		// Substrate refuses an update without both, as INVALID_ARGUMENT, which the client would
		// report as an invalid policy. This is the client's fault, not the configuration's.
		return nil, fmt.Errorf("%w on actor %s/%s: GetActorEgressPolicy returned a policy without the UID and version an update needs",
			errEgressPolicyInstall, actor.Atespace, actor.Name)
	}
	_, err = client.UpdateActorEgressPolicy(ctx, actor, want, stored)
	switch {
	case err == nil:
		res.outcome = outcome
		return nil, nil
	case errors.Is(err, ErrActorBusy), errors.Is(err, ErrEgressPolicyNotFound):
		// Another writer changed or deleted the policy since the read.
		return err, nil
	}
	return nil, egressWriteError(actor, err)
}

// create creates policy on an actor that had none when this attempt looked.
func (e *egressConverge) create(ctx context.Context, client EgressPolicyClient, actor ActorRef, policy EgressPolicy, res *egressResult) (conflict, err error) {
	err = client.CreateActorEgressPolicy(ctx, actor, policy)
	switch {
	case err == nil:
		res.outcome = egressCreated
		return nil, nil
	case errors.Is(err, ErrEgressPolicyExists):
		return err, nil
	}
	return nil, egressWriteError(actor, err)
}

// egressWriteError classifies a failed create or update: a policy the client refuses
// (ErrEgressPolicyInvalid) is egressPolicyRefusedError, a busy refusal is busyRefusal, and anything
// else keeps the client's error.
func egressWriteError(actor ActorRef, err error) error {
	switch {
	case errors.Is(err, ErrEgressPolicyInvalid):
		return &egressPolicyRefusedError{actor: actor, cause: err}
	case errors.Is(err, api.ErrComputeBusy):
		return busyRefusal(fmt.Sprintf("%v on actor %s/%s", errEgressPolicyInstall, actor.Atespace, actor.Name), err)
	}
	return fmt.Errorf("%w on actor %s/%s: %w", errEgressPolicyInstall, actor.Atespace, actor.Name, err)
}

// egressPolicyEqual reports whether desired and stored allow the same traffic with the same
// effects, as substrate stores them: a rule with neither Ports nor AllPorts equals one holding
// substrate's default port, nil and empty lists are equal, hostnames and ports compare as sets
// (sorted, not deduplicated, so a duplicate only costs a write), and rules, replaced headers and
// JWT audiences compare in order.
func egressPolicyEqual(desired, stored EgressPolicy) bool {
	return slices.EqualFunc(desired.Rules, stored.Rules, egressRuleEqual)
}

func egressRuleEqual(a, b EgressRule) bool {
	return a.Protocol == b.Protocol &&
		a.AllPorts == b.AllPorts &&
		sameSorted(a.Hostnames, b.Hostnames) &&
		sameSorted(egressPorts(a), egressPorts(b)) &&
		slices.EqualFunc(a.ReplaceHeaders, b.ReplaceHeaders, credentialHeaderEqual)
}

// egressPorts is the rule's ports as substrate stores them: its default port (Substrate v1.0.0
// cmd/ateapi/internal/defaults) for an http or https rule that sets neither Ports nor AllPorts.
func egressPorts(r EgressRule) []int32 {
	if r.AllPorts || len(r.Ports) > 0 {
		return r.Ports
	}
	switch r.Protocol {
	case EgressHTTP:
		return []int32{80}
	case EgressHTTPS:
		return []int32{443}
	}
	return nil
}

func credentialHeaderEqual(a, b CredentialHeader) bool {
	if a.Header != b.Header || a.Prefix != b.Prefix || a.CredentialURI != b.CredentialURI || (a.ActorJWT == nil) != (b.ActorJWT == nil) {
		return false
	}
	return a.ActorJWT == nil ||
		(a.ActorJWT.ExpirationSeconds == b.ActorJWT.ExpirationSeconds && slices.Equal(a.ActorJWT.Audiences, b.ActorJWT.Audiences))
}

// sameSorted reports whether a and b hold the same elements with the same multiplicity, without
// changing either.
func sameSorted[T cmp.Ordered](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	if slices.Equal(a, b) {
		return true
	}
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// egressPolicyRefusedError reports a policy the control client refused as invalid, or a stored one
// it cannot read (ErrEgressPolicyInvalid). It is api.ErrComputeConfigInvalid: the same refusal
// repeats on every call until the configuration or the client changes. Its text is fixed, because
// it reaches Sessions callers even with authorization on and the client's message can name a
// rejected hostname or credential reference. Go callers still reach the cause through Unwrap.
type egressPolicyRefusedError struct {
	actor  ActorRef
	cause  error
	stored bool // the stored policy could not be read, rather than the configured one refused
}

func (e *egressPolicyRefusedError) Error() string {
	hint := "check the configured policy against substrate's EgressPolicy validation"
	if e.stored {
		hint = "the control client cannot read the actor's stored policy, so it was left unchanged; " +
			"upgrade the client or replace the policy in substrate"
	}
	return fmt.Sprintf("%v: %v on actor %s/%s: %v; %s",
		api.ErrComputeConfigInvalid, errEgressPolicyInstall, e.actor.Atespace, e.actor.Name,
		ErrEgressPolicyInvalid, hint)
}

func (e *egressPolicyRefusedError) Unwrap() []error {
	return []error{api.ErrComputeConfigInvalid, errEgressPolicyInstall, e.cause}
}
