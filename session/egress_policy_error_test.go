package session_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/substrate"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
	"github.com/agent-primitives/sessions/wire"
)

// egressRefusal is what substrate's validation answers for a policy it refuses. It names the
// rejected hostname and credential reference, which must not reach a Sessions caller.
const egressRefusal = "rpc error: code = InvalidArgument desc = rules[0].https.hostnames[0]: " +
	"secret-host.example.com; credential_uri ate-secret://kv/vault/model-key"

// egressActorControl is actorControl with substrate's actor egress policies: one per actor,
// AlreadyExists for a second create, and an update preconditioned on the stored UID and version.
// While refuse is set, a create or update refuses the policy as substrate's validation does,
// before it looks at the store; while unreadable is set, a read reports a stored policy the client
// cannot convert; while conflict is set, another writer wins every write: a create finds a policy,
// a read returns one at a version an update then finds moved; while busy is set, another operation
// holds the actor, so a read or create is refused as the adapter reports substrate's ABORTED.
type egressActorControl struct {
	*actorControl
	refuse, unreadable, conflict, busy atomic.Bool
	mu                                 sync.Mutex
	policies                           map[string]substrate.EgressPolicyInfo
}

// egressBusy is the adapter's error for a call substrate refuses as ABORTED, with substrate's text.
func egressBusy(a substrate.ActorRef) error {
	return fmt.Errorf("%w: %s/%s: rpc error: code = Aborted desc = %s", substrate.ErrActorBusy, a.Atespace, a.Name, egressRefusal)
}

func (c *egressActorControl) GetActorEgressPolicy(_ context.Context, a substrate.ActorRef) (substrate.EgressPolicyInfo, error) {
	if c.unreadable.Load() {
		return substrate.EgressPolicyInfo{}, fmt.Errorf("%w: stored policy: %s", substrate.ErrEgressPolicyInvalid, egressRefusal)
	}
	if c.busy.Load() {
		return substrate.EgressPolicyInfo{}, egressBusy(a)
	}
	if c.conflict.Load() {
		return substrate.EgressPolicyInfo{UID: "policy-" + a.Name, Version: 1}, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	info, ok := c.policies[a.Name]
	if !ok {
		return substrate.EgressPolicyInfo{}, substrate.ErrEgressPolicyNotFound
	}
	return info, nil
}

func (c *egressActorControl) CreateActorEgressPolicy(_ context.Context, a substrate.ActorRef, p substrate.EgressPolicy) error {
	if c.refuse.Load() {
		return fmt.Errorf("%w: %s/%s: %s", substrate.ErrEgressPolicyInvalid, a.Atespace, a.Name, egressRefusal)
	}
	if c.busy.Load() {
		return egressBusy(a)
	}
	if c.conflict.Load() {
		return substrate.ErrEgressPolicyExists
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.policies[a.Name]; ok {
		return substrate.ErrEgressPolicyExists
	}
	if c.policies == nil {
		c.policies = map[string]substrate.EgressPolicyInfo{}
	}
	c.policies[a.Name] = substrate.EgressPolicyInfo{Policy: p, UID: "policy-" + a.Name, Version: 1}
	return nil
}

func (c *egressActorControl) UpdateActorEgressPolicy(_ context.Context, a substrate.ActorRef, p substrate.EgressPolicy, precond substrate.EgressPolicyInfo) (substrate.EgressPolicyInfo, error) {
	if c.refuse.Load() {
		return substrate.EgressPolicyInfo{}, fmt.Errorf("%w: %s/%s: %s", substrate.ErrEgressPolicyInvalid, a.Atespace, a.Name, egressRefusal)
	}
	busy := egressBusy(a)
	if c.conflict.Load() {
		return substrate.EgressPolicyInfo{}, busy
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	info, ok := c.policies[a.Name]
	switch {
	case !ok:
		return substrate.EgressPolicyInfo{}, substrate.ErrEgressPolicyNotFound
	case info.UID != precond.UID || info.Version != precond.Version:
		return substrate.EgressPolicyInfo{}, busy
	}
	info.Policy, info.Version = p, info.Version+1
	c.policies[a.Name] = info
	return info, nil
}

func (c *egressActorControl) DeleteActorEgressPolicy(context.Context, substrate.ActorRef) error {
	return errors.New("not used by the backend")
}

// secretPolicy is the egress policy newEgressHost configures for every session unless a test
// changes it.
var secretPolicy = substrate.EgressPolicy{Rules: []substrate.EgressRule{{Protocol: substrate.EgressHTTPS, Hostnames: []string{"secret-host.example.com"}}}}

// newEgressHost serves one REQUIRES_MEMORY_SNAPSHOT substrate harness on ctl with the egress policy
// configured for every session that policy holds (secretPolicy when nil).
func newEgressHost(t *testing.T, ctl substrate.ControlClient, policy *atomic.Pointer[substrate.EgressPolicy]) v1.SessionsClient {
	t.Helper()
	store, r := newEgressRegistry(t, ctl, policy)
	return serveRegistry(t, store, r)
}

// newEgressRegistry is the store and registry newEgressHost serves.
func newEgressRegistry(t *testing.T, ctl substrate.ControlClient, policy *atomic.Pointer[substrate.EgressPolicy]) (*sqlitelog.Store, *placement.Registry) {
	t.Helper()
	desc, err := echoagent.Harness{}.Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	desc.ID = "mem"
	desc.Capabilities.Resumability = api.ResumabilityRequiresMemorySnapshot
	if policy == nil {
		policy = &atomic.Pointer[substrate.EgressPolicy]{}
		policy.Store(&secretPolicy)
	}
	opt := substrate.WithEgressPolicy(func(context.Context, *api.SessionSpec) (*substrate.EgressPolicy, error) {
		return policy.Load(), nil
	})
	backend := substrate.New(ctl, "space", substrate.ObjectRef{Atespace: "space", Name: "mem"}, desc, opt)
	dial := placement.WithDialer(func(api.Incarnation) (api.Harness, func() error, error) {
		return echoagent.Harness{}, func() error { return nil }, nil
	})
	r, err := placement.NewRegistry("mem", map[string]*placement.Placer{"mem": placement.New(backend, echoagent.Model, dial)})
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, r
}

// wantComputeConfigInvalid fails unless err is FAILED_PRECONDITION with reason
// COMPUTE_CONFIG_INVALID, and its message names want and none of the policy's contents.
func wantComputeConfigInvalid(t *testing.T, op string, err error, want string) {
	t.Helper()
	st := status.Convert(err)
	if st.Code() != codes.FailedPrecondition {
		t.Fatalf("%s: err=%v, want FailedPrecondition", op, err)
	}
	var reasons []string
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			reasons = append(reasons, info.GetReason())
		}
	}
	if len(reasons) != 1 || reasons[0] != "COMPUTE_CONFIG_INVALID" {
		t.Fatalf("%s: reasons=%v, want [COMPUTE_CONFIG_INVALID]", op, reasons)
	}
	if !strings.Contains(st.Message(), want) {
		t.Fatalf("%s: message %q does not say %q", op, st.Message(), want)
	}
	for _, leaked := range []string{"secret-host", "ate-secret", "InvalidArgument"} {
		if strings.Contains(st.Message(), leaked) {
			t.Fatalf("%s: message carries the policy or substrate's refusal (%q): %q", op, leaked, st.Message())
		}
	}
}

// A configured egress policy that substrate refuses as invalid, or a stored one the client cannot
// read, is refused on every call until the configuration changes, so Exec, Resume and Fork report
// FAILED_PRECONDITION with COMPUTE_CONFIG_INVALID, not INTERNAL. A policy another writer changes on
// every attempt to converge it, or a call substrate refuses because another operation holds the
// actor, is ABORTED, which a retry may clear. The message leaves out substrate's own text, which
// names the rejected hostname and credential reference.
func TestRefusedEgressPolicyIsComputeConfigInvalid(t *testing.T) {
	ctx := context.Background()
	changed := substrate.EgressPolicy{Rules: []substrate.EgressRule{{Protocol: substrate.EgressHTTPS, Hostnames: []string{"secret-host.example.com", "other.example.com"}}}}
	for _, tc := range []struct {
		op, name string
		fail     func(*egressActorControl)
		check    func(t *testing.T, op string, err error)
	}{
		{op: "exec", name: "create refused", fail: refuse, check: configInvalid("egress policy is invalid")},
		{op: "resume", name: "update refused", fail: refuse, check: configInvalid("egress policy is invalid")},
		{op: "fork", name: "create refused", fail: refuse, check: configInvalid("egress policy is invalid")},
		{op: "resume", name: "stored unreadable", fail: func(c *egressActorControl) { c.unreadable.Store(true) },
			check: configInvalid("cannot read the actor's stored policy")},
		{op: "exec", name: "conflicts exhausted", fail: conflict, check: aborted},
		{op: "resume", name: "conflicts exhausted", fail: conflict, check: aborted},
		{op: "fork", name: "conflicts exhausted", fail: conflict, check: aborted},
		{op: "exec", name: "create busy", fail: busy, check: computeBusy},
		{op: "exec suspended", name: "read busy", fail: busy, check: computeBusy},
		{op: "resume", name: "read busy", fail: busy, check: computeBusy},
		{op: "fork", name: "create busy", fail: busy, check: computeBusy},
	} {
		t.Run(tc.op+" "+tc.name, func(t *testing.T) {
			ctl := &egressActorControl{actorControl: &actorControl{status: map[string]substrate.ActorStatus{}, snapshots: map[string]string{}}}
			policy := &atomic.Pointer[substrate.EgressPolicy]{}
			policy.Store(&secretPolicy)
			c := newEgressHost(t, ctl, policy)
			sess := mustCreate(t, c)
			if tc.op != "exec" {
				if err := execHi(c, sess); err != nil {
					t.Fatalf("first turn: %v", err)
				}
			}
			if tc.op == "resume" || tc.op == "exec suspended" {
				if _, err := c.Suspend(ctx, &v1.SuspendRequest{Session: sess}); err != nil {
					t.Fatalf("suspend: %v", err)
				}
				// A changed policy, so the resume writes it.
				policy.Store(&changed)
			}
			tc.fail(ctl)
			var err error
			switch tc.op {
			case "exec", "exec suspended":
				err = execHi(c, sess)
			case "resume":
				_, err = c.Resume(ctx, &v1.ResumeRequest{Session: sess})
			case "fork":
				_, err = c.Fork(ctx, &v1.ForkRequest{Session: sess, Count: 1})
			}
			tc.check(t, tc.op, err)
		})
	}
}

func refuse(c *egressActorControl) { c.refuse.Store(true) }

func conflict(c *egressActorControl) { c.conflict.Store(true) }

func busy(c *egressActorControl) { c.busy.Store(true) }

func configInvalid(want string) func(*testing.T, string, error) {
	return func(t *testing.T, op string, err error) {
		t.Helper()
		wantComputeConfigInvalid(t, op, err, want)
	}
}

// aborted fails unless err is ABORTED and its message leaves out substrate's own text.
func aborted(t *testing.T, op string, err error) {
	t.Helper()
	st := status.Convert(err)
	if st.Code() != codes.Aborted {
		t.Fatalf("%s: err=%v, want Aborted", op, err)
	}
	for _, leaked := range []string{"secret-host", "ate-secret", "InvalidArgument"} {
		if strings.Contains(st.Message(), leaked) {
			t.Fatalf("%s: message carries substrate's text (%q): %q", op, leaked, st.Message())
		}
	}
}

// computeBusy fails unless err is aborted with reason COMPUTE_BUSY.
func computeBusy(t *testing.T, op string, err error) {
	t.Helper()
	aborted(t, op, err)
	var reasons []string
	for _, d := range status.Convert(err).Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			reasons = append(reasons, info.GetReason())
		}
	}
	if len(reasons) != 1 || reasons[0] != "COMPUTE_BUSY" {
		t.Fatalf("%s: reasons=%v, want [COMPUTE_BUSY]", op, reasons)
	}
}

// Authorization sanitizes only INTERNAL errors, so an ABORTED busy egress refusal reaches an
// authorized caller as it is: its text must leave out substrate's own.
func TestBusyEgressPolicyTextWithAuthorization(t *testing.T) {
	ctl := &egressActorControl{actorControl: &actorControl{status: map[string]substrate.ActorStatus{}, snapshots: map[string]string{}}}
	store, r := newEgressRegistry(t, ctl, nil)
	svc := session.NewService(store, r, session.WithAuthorization(policyFunc(func(context.Context, api.AuthorizationRequest) error { return nil })))
	ctx := callerContext(t, authOwner)
	created, err := svc.CreateSession(ctx, &v1.CreateSessionRequest{Session: &v1.Session{Harness: "mem"}})
	if err != nil {
		t.Fatal(err)
	}
	sess := created.GetMetadata().GetUid()
	exec := &v1.ExecRequest{Session: sess, Inputs: []*v1.Message{wire.MessageToProto(api.TextMessage("user", "hi"))}}
	if err := svc.Exec(exec, &rpcStream{ctx: ctx}); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if _, err := svc.Suspend(ctx, &v1.SuspendRequest{Session: sess}); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	busy(ctl)
	_, err = svc.Resume(ctx, &v1.ResumeRequest{Session: sess})
	computeBusy(t, "resume", err)
}

// A configured egress policy with a control client that cannot install it is refused before any
// actor is created, as FAILED_PRECONDITION with COMPUTE_CONFIG_INVALID.
func TestUninstallableEgressPolicyIsComputeConfigInvalid(t *testing.T) {
	ctl := &actorControl{status: map[string]substrate.ActorStatus{}, snapshots: map[string]string{}}
	c := newEgressHost(t, ctl, nil)
	sess := mustCreate(t, c)
	wantComputeConfigInvalid(t, "exec", execHi(c, sess), "EgressPolicyClient")
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	if ctl.created != 0 {
		t.Fatalf("created %d actors, want none", ctl.created)
	}
}
