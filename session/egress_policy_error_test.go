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
	"github.com/agent-primitives/sessions/sqlitelog"
)

// egressRefusal is what substrate's validation answers for a policy it refuses. It names the
// rejected hostname and credential reference, which must not reach a Sessions caller.
const egressRefusal = "rpc error: code = InvalidArgument desc = rules[0].https.hostnames[0]: " +
	"secret-host.example.com; credential_uri ate-secret://kv/vault/model-key"

// egressActorControl is actorControl with substrate's actor egress policies: one per actor,
// AlreadyExists for a second create. While refuse is set, CreateActorEgressPolicy refuses the
// policy as substrate's validation does, before it looks at the store.
type egressActorControl struct {
	*actorControl
	refuse   atomic.Bool
	mu       sync.Mutex
	policies map[string]bool
}

func (c *egressActorControl) GetActorEgressPolicy(context.Context, substrate.ActorRef) (substrate.EgressPolicyInfo, error) {
	return substrate.EgressPolicyInfo{}, errors.New("not used by the backend")
}

func (c *egressActorControl) CreateActorEgressPolicy(_ context.Context, a substrate.ActorRef, _ substrate.EgressPolicy) error {
	if c.refuse.Load() {
		return fmt.Errorf("%w: %s/%s: %s", substrate.ErrEgressPolicyInvalid, a.Atespace, a.Name, egressRefusal)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.policies[a.Name] {
		return substrate.ErrEgressPolicyExists
	}
	if c.policies == nil {
		c.policies = map[string]bool{}
	}
	c.policies[a.Name] = true
	return nil
}

func (c *egressActorControl) UpdateActorEgressPolicy(context.Context, substrate.ActorRef, substrate.EgressPolicy, substrate.EgressPolicyInfo) (substrate.EgressPolicyInfo, error) {
	return substrate.EgressPolicyInfo{}, errors.New("not used by the backend")
}

func (c *egressActorControl) DeleteActorEgressPolicy(context.Context, substrate.ActorRef) error {
	return errors.New("not used by the backend")
}

// newEgressHost serves one REQUIRES_MEMORY_SNAPSHOT substrate harness on ctl with an egress policy
// configured for every session.
func newEgressHost(t *testing.T, ctl substrate.ControlClient) v1.SessionsClient {
	t.Helper()
	desc, err := echoagent.Harness{}.Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	desc.ID = "mem"
	desc.Capabilities.Resumability = api.ResumabilityRequiresMemorySnapshot
	policy := substrate.WithEgressPolicy(func(context.Context, *api.SessionSpec) (*substrate.EgressPolicy, error) {
		return &substrate.EgressPolicy{Rules: []substrate.EgressRule{{Protocol: substrate.EgressHTTPS, Hostnames: []string{"secret-host.example.com"}}}}, nil
	})
	backend := substrate.New(ctl, "space", substrate.ObjectRef{Atespace: "space", Name: "mem"}, desc, policy)
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
	return serveRegistry(t, store, r)
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

// A configured egress policy that substrate refuses as invalid is refused on every call until the
// configuration changes, so Exec, Resume and Fork report FAILED_PRECONDITION with
// COMPUTE_CONFIG_INVALID, not INTERNAL. The message leaves out substrate's refusal, which names the
// rejected hostname and credential reference.
func TestRefusedEgressPolicyIsComputeConfigInvalid(t *testing.T) {
	ctx := context.Background()
	for _, op := range []string{"exec", "resume", "fork"} {
		t.Run(op, func(t *testing.T) {
			ctl := &egressActorControl{actorControl: &actorControl{status: map[string]substrate.ActorStatus{}, snapshots: map[string]string{}}}
			c := newEgressHost(t, ctl)
			sess := mustCreate(t, c)
			var err error
			switch op {
			case "exec":
				ctl.refuse.Store(true)
				err = execHi(c, sess)
			case "resume":
				if err := execHi(c, sess); err != nil {
					t.Fatalf("first turn: %v", err)
				}
				if _, err := c.Suspend(ctx, &v1.SuspendRequest{Session: sess}); err != nil {
					t.Fatalf("suspend: %v", err)
				}
				ctl.refuse.Store(true)
				_, err = c.Resume(ctx, &v1.ResumeRequest{Session: sess})
			case "fork":
				if err := execHi(c, sess); err != nil {
					t.Fatalf("first turn: %v", err)
				}
				ctl.refuse.Store(true)
				_, err = c.Fork(ctx, &v1.ForkRequest{Session: sess, Count: 1})
			}
			wantComputeConfigInvalid(t, op, err, "egress policy is invalid")
		})
	}
}

// A configured egress policy with a control client that cannot install it is refused before any
// actor is created, as FAILED_PRECONDITION with COMPUTE_CONFIG_INVALID.
func TestUninstallableEgressPolicyIsComputeConfigInvalid(t *testing.T) {
	ctl := &actorControl{status: map[string]substrate.ActorStatus{}, snapshots: map[string]string{}}
	c := newEgressHost(t, ctl)
	sess := mustCreate(t, c)
	wantComputeConfigInvalid(t, "exec", execHi(c, sess), "EgressPolicyClient")
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	if ctl.created != 0 {
		t.Fatalf("created %d actors, want none", ctl.created)
	}
}
