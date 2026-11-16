package ateadapter

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/agent-primitives/sessions/runtime/substrate"
	atepb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var _ substrate.EgressPolicyClient = (*Adapter)(nil)

// egressPolicyName is the name of an actor's singleton EgressPolicy resource.
const egressPolicyName = "default"

// GetActorEgressPolicy reads the actor's EgressPolicy. NOT_FOUND, which substrate answers both for
// an actor without a policy and for a missing actor, is substrate.ErrEgressPolicyNotFound.
func (ad *Adapter) GetActorEgressPolicy(ctx context.Context, actor substrate.ActorRef) (substrate.EgressPolicyInfo, error) {
	got, err := ad.ctl.GetActorEgressPolicy(ctx, &atepb.GetActorEgressPolicyRequest{Actor: objectRef(actor), Name: egressPolicyName})
	if err != nil {
		return substrate.EgressPolicyInfo{}, egressError(actor, err)
	}
	return egressPolicyInfo(got)
}

// CreateActorEgressPolicy creates the actor's EgressPolicy. ALREADY_EXISTS is
// substrate.ErrEgressPolicyExists, FAILED_PRECONDITION, which substrate answers when the parent
// actor does not exist, is substrate.ErrActorNotFound, and INVALID_ARGUMENT, or a rule the message
// cannot express, is substrate.ErrEgressPolicyInvalid.
func (ad *Adapter) CreateActorEgressPolicy(ctx context.Context, actor substrate.ActorRef, policy substrate.EgressPolicy) error {
	p, err := egressPolicyToProto(actor, policy)
	if err != nil {
		return fmt.Errorf("%w: %w", substrate.ErrEgressPolicyInvalid, err)
	}
	_, err = ad.ctl.CreateActorEgressPolicy(ctx, &atepb.CreateActorEgressPolicyRequest{Actor: objectRef(actor), EgressPolicy: p})
	return egressWriteError(actor, err)
}

// UpdateActorEgressPolicy replaces the actor's EgressPolicy with policy, preconditioned on the UID
// and version in precond. ABORTED, a precondition that no longer matches, is substrate.ErrActorBusy,
// and INVALID_ARGUMENT, or a rule the message cannot express, is substrate.ErrEgressPolicyInvalid.
func (ad *Adapter) UpdateActorEgressPolicy(ctx context.Context, actor substrate.ActorRef, policy substrate.EgressPolicy, precond substrate.EgressPolicyInfo) (substrate.EgressPolicyInfo, error) {
	p, err := egressPolicyToProto(actor, policy)
	if err != nil {
		return substrate.EgressPolicyInfo{}, fmt.Errorf("%w: %w", substrate.ErrEgressPolicyInvalid, err)
	}
	p.Metadata.Uid, p.Metadata.Version = precond.UID, precond.Version
	got, err := ad.ctl.UpdateActorEgressPolicy(ctx, &atepb.UpdateActorEgressPolicyRequest{Actor: objectRef(actor), EgressPolicy: p})
	if err != nil {
		return substrate.EgressPolicyInfo{}, egressWriteError(actor, err)
	}
	return egressPolicyInfo(got)
}

// DeleteActorEgressPolicy deletes the actor's EgressPolicy, which leaves the actor deny-all.
func (ad *Adapter) DeleteActorEgressPolicy(ctx context.Context, actor substrate.ActorRef) error {
	_, err := ad.ctl.DeleteActorEgressPolicy(ctx, &atepb.DeleteActorEgressPolicyRequest{Actor: objectRef(actor), Name: egressPolicyName})
	return egressError(actor, err)
}

// egressError maps the ate-api codes of the EgressPolicy RPCs onto the backend's sentinels, keeping
// the status error wrapped. Other codes, PERMISSION_DENIED and INVALID_ARGUMENT included, pass
// through unchanged; egressWriteError classifies INVALID_ARGUMENT for the calls that carry a
// policy.
func egressError(actor substrate.ActorRef, err error) error {
	var sentinel error
	switch status.Code(err) {
	case codes.OK:
		return nil
	case codes.AlreadyExists:
		sentinel = substrate.ErrEgressPolicyExists
	case codes.NotFound:
		sentinel = substrate.ErrEgressPolicyNotFound
	case codes.FailedPrecondition:
		sentinel = substrate.ErrActorNotFound
	case codes.Aborted:
		sentinel = substrate.ErrActorBusy
	default:
		return err
	}
	return fmt.Errorf("%w: %s/%s: %w", sentinel, actor.Atespace, actor.Name, err)
}

// egressWriteError is egressError for Create and Update, which carry a policy. Their
// INVALID_ARGUMENT is substrate's validation refusing the request, whose inputs are the policy and
// the actor reference the backend builds from its configured atespace, so it repeats until the
// configuration changes: substrate.ErrEgressPolicyInvalid. The status message, which can name a
// rejected hostname or credential reference, stays wrapped for the log; the backend leaves it out
// of what it returns to Sessions callers.
func egressWriteError(actor substrate.ActorRef, err error) error {
	if status.Code(err) == codes.InvalidArgument {
		return fmt.Errorf("%w: %s/%s: %w", substrate.ErrEgressPolicyInvalid, actor.Atespace, actor.Name, err)
	}
	return egressError(actor, err)
}

func egressPolicyInfo(p *atepb.EgressPolicy) (substrate.EgressPolicyInfo, error) {
	policy, err := EgressPolicyFromProto(p)
	if err != nil {
		return substrate.EgressPolicyInfo{}, err
	}
	return substrate.EgressPolicyInfo{Policy: policy, UID: p.GetMetadata().GetUid(), Version: p.GetMetadata().GetVersion()}, nil
}

// errEgressRule reports a rule the conversion cannot express. It names the rule by index only, so
// an error that reaches a log carries no hostname or credential reference.
var errEgressRule = errors.New("ateadapter: invalid egress rule")

// egressPolicyToProto converts policy into the actor's EgressPolicy resource. It refuses what
// substrate's message cannot express (an unspecified protocol, AllPorts with Ports, header effects
// on tls_passthrough, a header with both or neither credential source); substrate validates the
// rest.
func egressPolicyToProto(actor substrate.ActorRef, policy substrate.EgressPolicy) (*atepb.EgressPolicy, error) {
	out := &atepb.EgressPolicy{
		Metadata: &atepb.ResourceMetadata{Atespace: actor.Atespace, Name: egressPolicyName},
		Rules:    make([]*atepb.EgressRule, 0, len(policy.Rules)),
	}
	for i, r := range policy.Rules {
		if r.AllPorts && len(r.Ports) > 0 {
			return nil, fmt.Errorf("%w %d: AllPorts and Ports are both set", errEgressRule, i)
		}
		var ports *atepb.Ports
		switch {
		case r.AllPorts:
			ports = &atepb.Ports{All: &atepb.AllPorts{}}
		case len(r.Ports) > 0:
			ports = &atepb.Ports{Numbers: slices.Clone(r.Ports)}
		}
		effects, err := effectsToProto(i, r.ReplaceHeaders)
		if err != nil {
			return nil, err
		}
		hostnames := slices.Clone(r.Hostnames)
		rule := &atepb.EgressRule{}
		switch r.Protocol {
		case substrate.EgressHTTP:
			rule.Http = &atepb.HttpRule{Hostnames: hostnames, Ports: ports, Effects: effects}
		case substrate.EgressHTTPS:
			rule.Https = &atepb.HttpsRule{Hostnames: hostnames, Ports: ports, Effects: effects}
		case substrate.EgressTLSPassthrough:
			if effects != nil {
				return nil, fmt.Errorf("%w %d: a tls_passthrough rule cannot replace headers", errEgressRule, i)
			}
			rule.TlsPassthrough = &atepb.TlsPassthroughRule{Hostnames: hostnames, Ports: ports}
		default:
			return nil, fmt.Errorf("%w %d: protocol %v", errEgressRule, i, r.Protocol)
		}
		out.Rules = append(out.Rules, rule)
	}
	return out, nil
}

func effectsToProto(rule int, headers []substrate.CredentialHeader) (*atepb.HttpRuleEffects, error) {
	if len(headers) == 0 {
		return nil, nil
	}
	out := &atepb.HttpRuleEffects{ReplaceHeaders: make([]*atepb.CredentialHeader, 0, len(headers))}
	for j, h := range headers {
		if (h.CredentialURI == "") == (h.ActorJWT == nil) {
			return nil, fmt.Errorf("%w %d: replaced header %d needs exactly one of CredentialURI and ActorJWT", errEgressRule, rule, j)
		}
		ch := &atepb.CredentialHeader{Header: h.Header, Prefix: h.Prefix, CredentialUri: h.CredentialURI}
		if h.ActorJWT != nil {
			ch.ActorJwt = &atepb.ActorJwtSource{Audiences: slices.Clone(h.ActorJWT.Audiences), ExpirationSeconds: h.ActorJWT.ExpirationSeconds}
		}
		out.ReplaceHeaders = append(out.ReplaceHeaders, ch)
	}
	return out, nil
}

// EgressPolicyFromProto converts substrate's EgressPolicy message into the backend's neutral
// policy, dropping its metadata. It refuses a rule that sets no protocol or more than one, ports
// that set both all and numbers, and a replaced header with both or neither credential source.
func EgressPolicyFromProto(p *atepb.EgressPolicy) (substrate.EgressPolicy, error) {
	out := substrate.EgressPolicy{Rules: make([]substrate.EgressRule, 0, len(p.GetRules()))}
	for i, r := range p.GetRules() {
		var (
			rule    substrate.EgressRule
			ports   *atepb.Ports
			effects *atepb.HttpRuleEffects
			set     int
		)
		if h := r.GetHttp(); h != nil {
			set++
			rule.Protocol, rule.Hostnames, ports, effects = substrate.EgressHTTP, h.GetHostnames(), h.GetPorts(), h.GetEffects()
		}
		if h := r.GetHttps(); h != nil {
			set++
			rule.Protocol, rule.Hostnames, ports, effects = substrate.EgressHTTPS, h.GetHostnames(), h.GetPorts(), h.GetEffects()
		}
		if t := r.GetTlsPassthrough(); t != nil {
			set++
			rule.Protocol, rule.Hostnames, ports = substrate.EgressTLSPassthrough, t.GetHostnames(), t.GetPorts()
		}
		if set != 1 {
			return substrate.EgressPolicy{}, fmt.Errorf("%w %d: sets %d of http, https and tls_passthrough, want exactly one", errEgressRule, i, set)
		}
		rule.Hostnames = slices.Clone(rule.Hostnames)
		if ports.GetAll() != nil && len(ports.GetNumbers()) > 0 {
			return substrate.EgressPolicy{}, fmt.Errorf("%w %d: ports sets both all and numbers", errEgressRule, i)
		}
		rule.AllPorts = ports.GetAll() != nil
		rule.Ports = slices.Clone(ports.GetNumbers())
		for j, h := range effects.GetReplaceHeaders() {
			if (h.GetCredentialUri() == "") == (h.GetActorJwt() == nil) {
				return substrate.EgressPolicy{}, fmt.Errorf("%w %d: replaced header %d needs exactly one of credential_uri and actor_jwt", errEgressRule, i, j)
			}
			ch := substrate.CredentialHeader{Header: h.GetHeader(), Prefix: h.GetPrefix(), CredentialURI: h.GetCredentialUri()}
			if jwt := h.GetActorJwt(); jwt != nil {
				ch.ActorJWT = &substrate.ActorJWTSource{Audiences: slices.Clone(jwt.GetAudiences()), ExpirationSeconds: jwt.GetExpirationSeconds()}
			}
			rule.ReplaceHeaders = append(rule.ReplaceHeaders, ch)
		}
		out.Rules = append(out.Rules, rule)
	}
	return out, nil
}
