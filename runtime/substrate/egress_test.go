package substrate_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/runtime/substrate"
)

// egressFake is the EgressPolicyClient half of a fake control client. It records its calls in the
// same list as the mockControl it shares, so a test asserts one ordered sequence, and models
// substrate's store: one policy per actor, AlreadyExists for a second create, and a missing actor
// reported as not found (substrate's FAILED_PRECONDITION).
type egressFake struct {
	m        *mockControl
	policies map[string]substrate.EgressPolicy
	// createErr fails CreateActorEgressPolicy; beforeCreate runs first, so a test can delete or
	// replace the actor, or cancel the caller, in the window before the install answers.
	createErr    error
	beforeCreate func(name string)
}

func (e *egressFake) GetActorEgressPolicy(ctx context.Context, a substrate.ActorRef) (substrate.EgressPolicyInfo, error) {
	e.m.calls = append(e.m.calls, "egress-get:"+a.Name)
	p, ok := e.policies[a.Name]
	if !ok {
		return substrate.EgressPolicyInfo{}, substrate.ErrEgressPolicyNotFound
	}
	return substrate.EgressPolicyInfo{Policy: p, UID: "policy-" + a.Name, Version: 1}, nil
}

func (e *egressFake) CreateActorEgressPolicy(ctx context.Context, a substrate.ActorRef, p substrate.EgressPolicy) error {
	e.m.calls = append(e.m.calls, "egress:"+a.Name)
	if e.beforeCreate != nil {
		e.beforeCreate(a.Name)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.createErr != nil {
		return e.createErr
	}
	if _, ok := e.m.actors[a.Name]; !ok {
		return substrate.ErrActorNotFound
	}
	if _, ok := e.policies[a.Name]; ok {
		return substrate.ErrEgressPolicyExists
	}
	if e.policies == nil {
		e.policies = map[string]substrate.EgressPolicy{}
	}
	e.policies[a.Name] = p
	return nil
}

func (e *egressFake) UpdateActorEgressPolicy(ctx context.Context, a substrate.ActorRef, p substrate.EgressPolicy, _ substrate.EgressPolicyInfo) (substrate.EgressPolicyInfo, error) {
	e.m.calls = append(e.m.calls, "egress-update:"+a.Name)
	return substrate.EgressPolicyInfo{}, errors.New("not used by the backend")
}

func (e *egressFake) DeleteActorEgressPolicy(ctx context.Context, a substrate.ActorRef) error {
	e.m.calls = append(e.m.calls, "egress-delete:"+a.Name)
	return errors.New("not used by the backend")
}

// egressControl is a ControlClient that implements EgressPolicyClient; egressCloner also
// implements SnapshotCloner.
type egressControl struct {
	*mockControl
	*egressFake
}

type egressCloner struct {
	*mockCloner
	*egressFake
}

func newEgressControl() egressControl {
	m := &mockControl{info: running("worker-a")}
	// Substrate creates an actor SUSPENDED; the backend's resume is what starts it.
	m.afterCreate = func(name string) { m.actors[name] = substrate.StatusSuspended }
	return egressControl{mockControl: m, egressFake: &egressFake{m: m}}
}

func newEgressCloner() egressCloner {
	c := newClonerWithSuspendedParent()
	return egressCloner{mockCloner: c, egressFake: &egressFake{m: c.mockControl}}
}

var testPolicy = substrate.EgressPolicy{Rules: []substrate.EgressRule{{
	Protocol:  substrate.EgressHTTPS,
	Hostnames: []string{"api.example.com"},
	ReplaceHeaders: []substrate.CredentialHeader{{
		Header: "Authorization", Prefix: "Bearer ", CredentialURI: "ate-secret://kv/vault/model-key",
	}},
}}}

// policyFunc is a WithEgressPolicy function that returns policy for every session and records the
// session UIDs it was asked for.
type policyFunc struct {
	policy *substrate.EgressPolicy
	err    error
	seen   []string
}

func (f *policyFunc) option() substrate.Option {
	return substrate.WithEgressPolicy(func(_ context.Context, s *api.SessionSpec) (*substrate.EgressPolicy, error) {
		f.seen = append(f.seen, s.SessionUID)
		return f.policy, f.err
	})
}

func staticPolicy() *policyFunc {
	p := testPolicy
	return &policyFunc{policy: &p}
}

func wantCalls(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("calls=%v\nwant  %v", got, want)
	}
}

// Create installs the policy on the actor it creates before resuming it, so the actor's compute
// never runs without its policy. The function is asked once, for the session's UID.
func TestCreateInstallsEgressPolicyBeforeResume(t *testing.T) {
	c := newEgressControl()
	f := staticPolicy()
	if _, err := newEgressBackend(c, f.option()).Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x"}); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, c.calls, []string{"get:sess-x", "create:sess-x", "egress:sess-x", "resume:sess-x"})
	if !reflect.DeepEqual(c.policies["sess-x"], testPolicy) {
		t.Fatalf("installed policy=%+v want %+v", c.policies["sess-x"], testPolicy)
	}
	if !reflect.DeepEqual(f.seen, []string{"sess-x"}) {
		t.Fatalf("policy function saw %v, want one call for sess-x", f.seen)
	}
}

func newEgressBackend(ctl substrate.ControlClient, opts ...substrate.Option) *substrate.Backend {
	return substrate.New(ctl, "space", substrate.ObjectRef{Atespace: "tmpl", Name: "echo"},
		api.Descriptor{ID: "echo", Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}}, opts...)
}

// With no option, or a function that returns no policy, the backend issues exactly the calls it
// did before egress policies existed.
func TestNoEgressPolicyKeepsTheCallSequence(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []substrate.Option
	}{
		{name: "no option"},
		{name: "nil policy", opts: []substrate.Option{(&policyFunc{}).option()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newEgressControl()
			b := newEgressBackend(c, tc.opts...)
			if _, err := b.Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x"}); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Snapshot(context.Background(), api.Incarnation{ID: "sess-x"}, api.SnapshotExternal); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Restore(context.Background(), api.SnapshotRef{Local: "sess-x", ExternalURI: "gs://snap/sess-x"}); err != nil {
				t.Fatal(err)
			}
			wantCalls(t, c.calls, []string{"get:sess-x", "create:sess-x", "resume:sess-x", "suspend:sess-x", "get:sess-x", "resume:sess-x"})
		})
	}
	// A client without EgressPolicyClient is fine when there is nothing to install.
	m := &mockControl{info: running("worker-a")}
	if _, err := newBackend(m, (&policyFunc{}).option()).Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x"}); err != nil {
		t.Fatalf("a nil policy needs no EgressPolicyClient: %v", err)
	}
}

// A policy the client cannot install, or a function that fails, refuses Create before any actor
// exists.
func TestEgressPolicyRefusedBeforeCreateActor(t *testing.T) {
	m := &mockControl{info: running("worker-a")}
	_, err := newBackend(m, staticPolicy().option()).Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x"})
	if !errors.Is(err, substrate.ErrEgressPolicyUnsupported) || !errors.Is(err, api.ErrComputeConfigInvalid) || !strings.Contains(err.Error(), "EgressPolicyClient") {
		t.Fatalf("err=%v want ErrEgressPolicyUnsupported and api.ErrComputeConfigInvalid naming EgressPolicyClient", err)
	}
	wantCalls(t, m.calls, []string{"get:sess-x"})

	c := newEgressControl()
	boom := errors.New("policy store unavailable")
	_, err = newEgressBackend(c, (&policyFunc{err: boom}).option()).Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x"})
	if !errors.Is(err, boom) || errors.Is(err, api.ErrComputeConfigInvalid) {
		t.Fatalf("err=%v want the function's error, unclassified", err)
	}
	wantCalls(t, c.calls, []string{"get:sess-x"})
}

// A failed install fails Create without resuming the actor it created, which stays SUSPENDED. A
// retry finds it and installs before resuming it.
func TestFailedEgressInstallLeavesTheActorSuspended(t *testing.T) {
	c := newEgressControl()
	denied := errors.New("rpc error: code = PermissionDenied")
	c.createErr = denied
	b := newEgressBackend(c, staticPolicy().option())
	_, err := b.Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x"})
	if !errors.Is(err, denied) || !strings.Contains(err.Error(), "install egress policy") || errors.Is(err, api.ErrComputeConfigInvalid) {
		t.Fatalf("err=%v want the install error, unclassified", err)
	}
	wantCalls(t, c.calls, []string{"get:sess-x", "create:sess-x", "egress:sess-x"})
	if st := c.statusOf("sess-x"); st != substrate.StatusSuspended {
		t.Fatalf("actor status=%v after a failed install, want SUSPENDED", st)
	}

	c.createErr, c.calls = nil, nil
	if _, err := b.Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x"}); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, c.calls, []string{"get:sess-x", "egress:sess-x", "resume:sess-x"})
}

// Every resume the backend issues on a SUSPENDED actor is preceded by the install: attaching to it,
// attaching after another host created it first, and restoring it. An actor that already has a
// policy keeps it.
func TestEgressPolicyBeforeEveryResume(t *testing.T) {
	t.Run("attach to a suspended actor keeps its existing policy", func(t *testing.T) {
		c := newEgressControl()
		c.exists("sess-x", substrate.StatusSuspended)
		old := substrate.EgressPolicy{Rules: []substrate.EgressRule{{Protocol: substrate.EgressHTTP, Hostnames: []string{"old.example.com"}}}}
		c.policies = map[string]substrate.EgressPolicy{"sess-x": old}
		if _, err := newEgressBackend(c, staticPolicy().option()).Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x"}); err != nil {
			t.Fatal(err)
		}
		wantCalls(t, c.calls, []string{"get:sess-x", "egress:sess-x", "resume:sess-x"})
		if !reflect.DeepEqual(c.policies["sess-x"], old) {
			t.Fatalf("policy=%+v, want the existing one kept", c.policies["sess-x"])
		}
	})
	t.Run("another host created the actor first", func(t *testing.T) {
		c := newEgressControl()
		c.afterGet = func(name string) {
			c.exists(name, substrate.StatusSuspended)
			c.afterGet = nil
		}
		f := staticPolicy()
		if _, err := newEgressBackend(c, f.option()).Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x"}); err != nil {
			t.Fatal(err)
		}
		wantCalls(t, c.calls, []string{"get:sess-x", "create:sess-x", "get:sess-x", "egress:sess-x", "resume:sess-x"})
		if len(f.seen) != 1 {
			t.Fatalf("policy function ran %d times in one call, want 1", len(f.seen))
		}
	})
	t.Run("restore of a suspended actor", func(t *testing.T) {
		c := newEgressControl()
		c.exists("sess-x", substrate.StatusSuspended)
		c.holds("sess-x", "gs://snap/sess-x")
		f := staticPolicy()
		if _, err := newEgressBackend(c, f.option()).Restore(context.Background(), api.SnapshotRef{Local: "sess-x", ExternalURI: "gs://snap/sess-x"}); err != nil {
			t.Fatal(err)
		}
		wantCalls(t, c.calls, []string{"get:sess-x", "egress:sess-x", "resume:sess-x"})
		if !reflect.DeepEqual(f.seen, []string{"sess-x"}) {
			t.Fatalf("policy function saw %v, want sess-x", f.seen)
		}
	})
}

// The backend does not touch the policy of running compute: attaching to or restoring a RUNNING
// actor issues no egress call and does not ask the function, so neither can fail a call that
// needs no resume.
func TestNoEgressCallForARunningActor(t *testing.T) {
	c := newEgressControl()
	c.exists("sess-x", substrate.StatusRunning)
	f := &policyFunc{err: errors.New("must not be asked")}
	b := newEgressBackend(c, f.option())
	if _, err := b.Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Restore(context.Background(), api.SnapshotRef{Local: "sess-x"}); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, c.calls, []string{"get:sess-x", "get:sess-x", "resume:sess-x"})
	if len(f.seen) != 0 {
		t.Fatalf("policy function asked for %v on a running actor", f.seen)
	}
}

// A failed install on a SUSPENDED actor fails the call and resumes nothing.
func TestFailedEgressInstallResumesNothing(t *testing.T) {
	denied := errors.New("rpc error: code = PermissionDenied")
	for _, tc := range []struct {
		name string
		call func(*substrate.Backend) error
	}{
		{name: "attach", call: func(b *substrate.Backend) error {
			_, err := b.Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x"})
			return err
		}},
		{name: "restore", call: func(b *substrate.Backend) error {
			_, err := b.Restore(context.Background(), api.SnapshotRef{Local: "sess-x", ExternalURI: "gs://snap/sess-x"})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newEgressControl()
			c.exists("sess-x", substrate.StatusSuspended)
			c.holds("sess-x", "gs://snap/sess-x")
			c.createErr = denied
			if err := tc.call(newEgressBackend(c, staticPolicy().option())); !errors.Is(err, denied) {
				t.Fatalf("err=%v want the install error", err)
			}
			wantCalls(t, c.calls, []string{"get:sess-x", "egress:sess-x"})
			if st := c.statusOf("sess-x"); st != substrate.StatusSuspended {
				t.Fatalf("actor status=%v, want SUSPENDED", st)
			}
		})
	}
}

// An install that reports the actor missing is classified as a failed resume would be: gone, Create
// places a new actor; replaced, it is retryable ErrActorBusy; gone again after another host created
// it, ErrActorBusy; a gone memory-tier actor on Restore is ErrMemoryStateLost.
func TestEgressInstallOnAMissingActor(t *testing.T) {
	t.Run("gone during attach creates a new actor", func(t *testing.T) {
		c := newEgressControl()
		c.exists("sess-x", substrate.StatusSuspended)
		c.beforeCreate = func(name string) {
			c.delete(name)
			c.beforeCreate = nil
		}
		if _, err := newEgressBackend(c, staticPolicy().option()).Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x"}); err != nil {
			t.Fatal(err)
		}
		wantCalls(t, c.calls, []string{"get:sess-x", "egress:sess-x", "get:sess-x", "create:sess-x", "egress:sess-x", "resume:sess-x"})
	})
	t.Run("replaced during attach is busy", func(t *testing.T) {
		c := newEgressControl()
		c.exists("sess-x", substrate.StatusSuspended)
		c.createErr = substrate.ErrActorNotFound
		c.beforeCreate = func(name string) {
			c.delete(name)
			c.exists(name, substrate.StatusSuspended)
		}
		_, err := newEgressBackend(c, staticPolicy().option()).Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x"})
		if !errors.Is(err, substrate.ErrActorBusy) || !strings.Contains(err.Error(), "egress policy was being installed") {
			t.Fatalf("err=%v want ErrActorBusy for a replaced actor", err)
		}
		wantCalls(t, c.calls, []string{"get:sess-x", "egress:sess-x", "get:sess-x"})
	})
	t.Run("gone again after another host created it is busy", func(t *testing.T) {
		c := newEgressControl()
		c.afterGet = func(name string) {
			c.exists(name, substrate.StatusSuspended)
			c.afterGet = nil
		}
		c.beforeCreate = func(name string) { c.delete(name) }
		_, err := newEgressBackend(c, staticPolicy().option()).Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x"})
		if !errors.Is(err, substrate.ErrActorBusy) {
			t.Fatalf("err=%v want ErrActorBusy", err)
		}
		wantCalls(t, c.calls, []string{"get:sess-x", "create:sess-x", "get:sess-x", "egress:sess-x", "get:sess-x"})
	})
	t.Run("memory-tier restore of a gone actor is lost state", func(t *testing.T) {
		c := newEgressControl()
		c.exists("sess-x", substrate.StatusSuspended)
		c.holds("sess-x", "gs://snap/sess-x")
		c.beforeCreate = func(name string) { c.delete(name) }
		_, err := newMemoryBackend(c, staticPolicy().option()).Restore(context.Background(),
			api.SnapshotRef{Local: "sess-x", ExternalURI: "gs://snap/sess-x", Memory: true})
		if !errors.Is(err, api.ErrMemoryStateLost) {
			t.Fatalf("err=%v want ErrMemoryStateLost", err)
		}
		if slices.Contains(c.calls, "resume:sess-x") {
			t.Fatalf("calls=%v resumed after a failed install", c.calls)
		}
	})
}

func forkMemory(b *substrate.Backend, ctx context.Context) error {
	_, err := b.Fork(ctx, api.SnapshotRef{Local: "parent", ExternalURI: "snap-parent-1", Memory: true},
		api.ForkOpts{ChildSessionUID: "child"})
	return err
}

// A stateful fork installs the child's policy on the clone between its create and its resume.
func TestForkCloneInstallsEgressPolicyBeforeResume(t *testing.T) {
	c := newEgressCloner()
	f := staticPolicy()
	if err := forkMemory(newMemoryBackend(c, f.option()), context.Background()); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, c.calls, []string{"get:parent", "tag:parent->fork-child", "get:parent", "clone:child:from=fork-child", "egress:child", "resume:child"})
	if !reflect.DeepEqual(f.seen, []string{"child"}) {
		t.Fatalf("policy function saw %v, want the child's UID", f.seen)
	}
}

// A stateless fork creates its child through Create, which installs the child's policy.
func TestStatelessForkChildGetsEgressPolicy(t *testing.T) {
	c := newEgressControl()
	f := staticPolicy()
	if _, err := newEgressBackend(c, f.option()).Fork(context.Background(), api.SnapshotRef{Local: "parent"}, api.ForkOpts{ChildSessionUID: "child"}); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, c.calls, []string{"get:child", "create:child", "egress:child", "resume:child"})
	if !reflect.DeepEqual(f.seen, []string{"child"}) {
		t.Fatalf("policy function saw %v, want the child's UID", f.seen)
	}
}

// A fork whose child policy cannot be resolved is refused before it tags the parent.
func TestForkRefusesAnUninstallablePolicyBeforeTagging(t *testing.T) {
	c := newClonerWithSuspendedParent()
	if err := forkMemory(newMemoryBackend(c, staticPolicy().option()), context.Background()); !errors.Is(err, substrate.ErrEgressPolicyUnsupported) {
		t.Fatalf("err=%v want ErrEgressPolicyUnsupported", err)
	}
	wantCalls(t, c.calls, nil)

	ec := newEgressCloner()
	boom := errors.New("no policy for child")
	if err := forkMemory(newMemoryBackend(ec, (&policyFunc{err: boom}).option()), context.Background()); !errors.Is(err, boom) {
		t.Fatalf("err=%v want the function's error", err)
	}
	wantCalls(t, ec.calls, nil)
}

// A failed install on a fork's clone tears the clone down without resuming it: the clone is deleted
// before its tag is released, since it borrows the tag's snapshot. When the clone cannot be read
// for cleanup both are left in place; when it is gone only the tag is released and the fork is
// retryable.
func TestForkCloneEgressInstallFailure(t *testing.T) {
	head := []string{"get:parent", "tag:parent->fork-child", "get:parent", "clone:child:from=fork-child", "egress:child"}
	denied := errors.New("rpc error: code = PermissionDenied")
	t.Run("clone torn down", func(t *testing.T) {
		c := newEgressCloner()
		c.createErr = denied
		err := forkMemory(newMemoryBackend(c, staticPolicy().option()), context.Background())
		if !errors.Is(err, denied) {
			t.Fatalf("err=%v want the install error", err)
		}
		wantCalls(t, c.calls, append(slices.Clone(head), "get:child", "suspend:child", "delete:child", "untag:fork-child"))
	})
	t.Run("clone unreadable is left in place", func(t *testing.T) {
		c := newEgressCloner()
		c.createErr = denied
		c.beforeCreate = func(string) { c.getErr = errors.New("unavailable") }
		err := forkMemory(newMemoryBackend(c, staticPolicy().option()), context.Background())
		if !errors.Is(err, denied) || !strings.Contains(err.Error(), "were left in place") {
			t.Fatalf("err=%v want the install error saying the clone and tag were left", err)
		}
		wantCalls(t, c.calls, append(slices.Clone(head), "get:child"))
	})
	t.Run("clone gone releases the tag", func(t *testing.T) {
		c := newEgressCloner()
		c.beforeCreate = func(name string) { c.delete(name) }
		err := forkMemory(newMemoryBackend(c, staticPolicy().option()), context.Background())
		if !errors.Is(err, substrate.ErrActorBusy) || !errors.Is(err, substrate.ErrActorNotFound) {
			t.Fatalf("err=%v want ErrActorBusy wrapping the install's not found", err)
		}
		wantCalls(t, c.calls, append(slices.Clone(head), "get:child", "untag:fork-child"))
	})
	t.Run("caller cancelled during the install", func(t *testing.T) {
		c := newEgressCloner()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		c.beforeCreate = func(string) { cancel() }
		err := forkMemory(newMemoryBackend(c, staticPolicy().option()), ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v want context.Canceled", err)
		}
		// Suspend, delete and untag refuse a cancelled context, so seeing them proves the cleanup
		// ran detached from the caller.
		wantCalls(t, c.calls, append(slices.Clone(head), "get:child", "suspend:child", "delete:child", "untag:fork-child"))
	})
}

// The install is logged by actor and rule count only: no hostname or credential reference.
func TestEgressInstallLogsNoPolicyContents(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := newEgressControl()
	if _, err := newEgressBackend(c, staticPolicy().option(), substrate.WithLogger(logger)).Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x"}); err != nil {
		t.Fatal(err)
	}
	logs := out.String()
	if !strings.Contains(logs, `"operation":"install_egress_policy"`) || !strings.Contains(logs, `"rules":1`) {
		t.Fatalf("install not logged with its rule count: %s", logs)
	}
	for _, secret := range []string{"api.example.com", "ate-secret://", "Bearer"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("logs carry policy contents %q: %s", secret, logs)
		}
	}
}

// A policy the client refuses as invalid is refused the same way on every call until the
// configuration changes, so it is api.ErrComputeConfigInvalid. Neither its error text nor the log
// carries the client's message, which can name a rejected hostname or credential reference; Go
// callers reach it through Unwrap, and the log records the error_kind. The actor stays SUSPENDED.
func TestRefusedEgressPolicyIsComputeConfigInvalid(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := newEgressControl()
	refusal := "rpc error: code = InvalidArgument desc = rules[0].https.hostnames[0]: secret-host.example.com; credential_uri ate-secret://kv/vault/model-key"
	c.createErr = fmt.Errorf("%w: space/sess-x: %s", substrate.ErrEgressPolicyInvalid, refusal)
	_, err := newEgressBackend(c, staticPolicy().option(), substrate.WithLogger(logger)).Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x"})
	if !errors.Is(err, api.ErrComputeConfigInvalid) || !errors.Is(err, substrate.ErrEgressPolicyInvalid) || !errors.Is(err, c.createErr) {
		t.Fatalf("err=%v want api.ErrComputeConfigInvalid wrapping the client's ErrEgressPolicyInvalid", err)
	}
	for _, leaked := range []string{"secret-host", "ate-secret", "InvalidArgument"} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("error text carries the client's message (%q): %v", leaked, err)
		}
	}
	if !strings.Contains(err.Error(), "space/sess-x") {
		t.Fatalf("error %q does not name the actor", err)
	}
	wantCalls(t, c.calls, []string{"get:sess-x", "create:sess-x", "egress:sess-x"})
	if st := c.statusOf("sess-x"); st != substrate.StatusSuspended {
		t.Fatalf("actor status=%v after a refused policy, want SUSPENDED", st)
	}
	logs := out.String()
	if !strings.Contains(logs, `"error_kind":"egress_policy_invalid"`) {
		t.Fatalf("install refusal not logged with its error_kind: %s", logs)
	}
	for _, leaked := range []string{"secret-host", "ate-secret"} {
		if strings.Contains(logs, leaked) {
			t.Fatalf("logs carry the client's message (%q): %s", leaked, logs)
		}
	}
}
