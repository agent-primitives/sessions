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
// substrate's store: one policy per actor, dropped with the actor; AlreadyExists for a second
// create; a missing actor reported as not found (substrate's NOT_FOUND on a read and
// FAILED_PRECONDITION on a write); the default port stored for an http or https rule that names
// none; and an update that must name the stored UID and version (ABORTED otherwise), and bumps the
// version.
type egressFake struct {
	m        *mockControl
	policies map[string]storedEgress
	minted   int
	// readErr, createErr and updateErr fail the call; before* hooks run first, so a test can delete or
	// replace the actor, change the policy, or cancel the caller in the window before the call
	// answers. noPrecondition makes GetActorEgressPolicy report no UID or version, as a client that
	// breaks its contract would.
	readErr, createErr, updateErr         error
	beforeGet, beforeCreate, beforeUpdate func(name string)
	noPrecondition                        bool
}

// storedEgress is one stored policy and the UID of the actor it belongs to.
type storedEgress struct {
	actorUID string
	info     substrate.EgressPolicyInfo
}

// current returns the actor's stored policy: none for a missing actor, or one stored for an earlier
// actor of the same name.
func (e *egressFake) current(name string) (substrate.EgressPolicyInfo, bool) {
	s, ok := e.policies[name]
	if _, exists := e.m.actors[name]; !ok || !exists || s.actorUID != e.m.uids[name] {
		return substrate.EgressPolicyInfo{}, false
	}
	return s.info, true
}

// store stores p as the actor's policy, as substrate stores it, with the given version.
func (e *egressFake) store(name string, p substrate.EgressPolicy, version int64) {
	if e.policies == nil {
		e.policies = map[string]storedEgress{}
	}
	uid := e.policies[name].info.UID
	if version == 1 {
		e.minted++
		uid = fmt.Sprintf("policy-%s-%d", name, e.minted)
	}
	e.policies[name] = storedEgress{actorUID: e.m.uid(name), info: substrate.EgressPolicyInfo{Policy: storedForm(p), UID: uid, Version: version}}
}

// setPolicy stores p on an existing actor, as another writer would.
func (e *egressFake) setPolicy(name string, p substrate.EgressPolicy) { e.store(name, p, 1) }

// bump replaces the actor's policy with p, as another writer's update would.
func (e *egressFake) bump(name string, p substrate.EgressPolicy) {
	info, _ := e.current(name)
	e.store(name, p, info.Version+1)
}

// policy is the actor's stored policy; a test fails if there is none.
func (e *egressFake) policy(t *testing.T, name string) substrate.EgressPolicyInfo {
	t.Helper()
	info, ok := e.current(name)
	if !ok {
		t.Fatalf("actor %q has no stored egress policy", name)
	}
	return info
}

// storedForm is p as substrate stores it: its default port on an http or https rule that names no
// port, and an empty rule list for none.
func storedForm(p substrate.EgressPolicy) substrate.EgressPolicy {
	out := substrate.EgressPolicy{Rules: []substrate.EgressRule{}}
	for _, r := range p.Rules {
		if !r.AllPorts && len(r.Ports) == 0 {
			switch r.Protocol {
			case substrate.EgressHTTP:
				r.Ports = []int32{80}
			case substrate.EgressHTTPS:
				r.Ports = []int32{443}
			}
		}
		out.Rules = append(out.Rules, r)
	}
	return out
}

func (e *egressFake) GetActorEgressPolicy(ctx context.Context, a substrate.ActorRef) (substrate.EgressPolicyInfo, error) {
	e.m.calls = append(e.m.calls, "egress-get:"+a.Name)
	if e.beforeGet != nil {
		e.beforeGet(a.Name)
	}
	if err := ctx.Err(); err != nil {
		return substrate.EgressPolicyInfo{}, err
	}
	if e.readErr != nil {
		return substrate.EgressPolicyInfo{}, e.readErr
	}
	info, ok := e.current(a.Name)
	if !ok {
		return substrate.EgressPolicyInfo{}, substrate.ErrEgressPolicyNotFound
	}
	if e.noPrecondition {
		info.UID, info.Version = "", 0
	}
	return info, nil
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
	if _, ok := e.current(a.Name); ok {
		return substrate.ErrEgressPolicyExists
	}
	e.store(a.Name, p, 1)
	return nil
}

func (e *egressFake) UpdateActorEgressPolicy(ctx context.Context, a substrate.ActorRef, p substrate.EgressPolicy, precond substrate.EgressPolicyInfo) (substrate.EgressPolicyInfo, error) {
	e.m.calls = append(e.m.calls, "egress-update:"+a.Name)
	if e.beforeUpdate != nil {
		e.beforeUpdate(a.Name)
	}
	if e.updateErr != nil { // answered before the caller went away
		return substrate.EgressPolicyInfo{}, e.updateErr
	}
	if err := ctx.Err(); err != nil {
		return substrate.EgressPolicyInfo{}, err
	}
	if precond.UID == "" || precond.Version == 0 {
		return substrate.EgressPolicyInfo{}, fmt.Errorf("%w: rpc error: code = InvalidArgument desc = precondition required", substrate.ErrEgressPolicyInvalid)
	}
	if _, ok := e.m.actors[a.Name]; !ok {
		return substrate.EgressPolicyInfo{}, substrate.ErrActorNotFound
	}
	info, ok := e.current(a.Name)
	if !ok {
		return substrate.EgressPolicyInfo{}, substrate.ErrEgressPolicyNotFound
	}
	if info.UID != precond.UID || info.Version != precond.Version {
		return substrate.EgressPolicyInfo{}, fmt.Errorf("%w: %s: rpc error: code = Aborted desc = %s", substrate.ErrActorBusy, a.Name, substrateText)
	}
	e.store(a.Name, p, info.Version+1)
	info, _ = e.current(a.Name)
	return info, nil
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

// newSuspendedEgressControl is an egressControl holding a SUSPENDED sess-x, with a snapshot to
// restore, that Create attaches to and Restore resumes.
func newSuspendedEgressControl() egressControl {
	c := newEgressControl()
	c.exists("sess-x", substrate.StatusSuspended)
	c.holds("sess-x", "gs://snap/sess-x")
	return c
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

var otherPolicy = substrate.EgressPolicy{Rules: []substrate.EgressRule{{Protocol: substrate.EgressHTTP, Hostnames: []string{"old.example.com"}}}}

// policyFunc is a WithEgressPolicy function that returns policy for every session and records the
// session UIDs it was asked for. onCall, when set, runs first, so a test can change the answer
// from one call to the next.
type policyFunc struct {
	policy *substrate.EgressPolicy
	err    error
	seen   []string
	onCall func(f *policyFunc)
}

func (f *policyFunc) option() substrate.Option {
	return substrate.WithEgressPolicy(func(_ context.Context, s *api.SessionSpec) (*substrate.EgressPolicy, error) {
		f.seen = append(f.seen, s.SessionUID)
		if f.onCall != nil {
			f.onCall(f)
		}
		return f.policy, f.err
	})
}

func staticPolicy() *policyFunc { return returning(testPolicy) }

func returning(p substrate.EgressPolicy) *policyFunc { return &policyFunc{policy: &p} }

func wantCalls(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("calls=%v\nwant  %v", got, want)
	}
}

func wantPolicy(t *testing.T, got substrate.EgressPolicyInfo, want substrate.EgressPolicy, version int64) {
	t.Helper()
	if !reflect.DeepEqual(got.Policy, storedForm(want)) || got.Version != version {
		t.Fatalf("stored policy=%+v version %d, want %+v version %d", got.Policy, got.Version, storedForm(want), version)
	}
}

func newEgressBackend(ctl substrate.ControlClient, opts ...substrate.Option) *substrate.Backend {
	return substrate.New(ctl, "space", substrate.ObjectRef{Atespace: "tmpl", Name: "echo"},
		api.Descriptor{ID: "echo", Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}}, opts...)
}

func createSess(b *substrate.Backend) error {
	_, err := b.Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x"})
	return err
}

func restoreSess(b *substrate.Backend) error {
	_, err := b.Restore(context.Background(), api.SnapshotRef{Local: "sess-x", ExternalURI: "gs://snap/sess-x"})
	return err
}

// debugLogger returns a logger that writes JSON at debug level to out.
func debugLogger(out *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// Create creates the policy on the actor it creates before resuming it, so the actor's compute
// never runs without its policy. The actor has no policy yet, so nothing is read first. The
// function is asked before the create, to refuse early, and again after it, and only that answer
// is written.
func TestCreateInstallsEgressPolicyBeforeResume(t *testing.T) {
	c := newEgressControl()
	f := staticPolicy()
	f.onCall = func(f *policyFunc) {
		if len(f.seen) == 1 {
			f.policy = &otherPolicy // answered before the create: never written
		} else {
			f.policy = &testPolicy
		}
	}
	if err := createSess(newEgressBackend(c, f.option())); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, c.calls, []string{"get:sess-x", "create:sess-x", "egress:sess-x", "resume:sess-x"})
	wantPolicy(t, c.policy(t, "sess-x"), testPolicy, 1)
	if !reflect.DeepEqual(f.seen, []string{"sess-x", "sess-x"}) {
		t.Fatalf("policy function saw %v, want sess-x before and after the create", f.seen)
	}
}

// With no option the backend issues exactly the calls it did before egress policies existed. A
// function that returns no policy adds the create of an empty policy on the actor Create makes, and
// the read before it resumes an existing SUSPENDED actor, which finds nothing to clear.
func TestNoEgressPolicyCalls(t *testing.T) {
	for _, tc := range []struct {
		name            string
		opts            []substrate.Option
		create, restore []string
	}{
		{name: "no option", create: []string{"resume:sess-x"}, restore: []string{"get:sess-x", "resume:sess-x"}},
		{name: "nil policy", opts: []substrate.Option{(&policyFunc{}).option()},
			create: []string{"egress:sess-x", "resume:sess-x"}, restore: []string{"get:sess-x", "egress-get:sess-x", "resume:sess-x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newEgressControl()
			b := newEgressBackend(c, tc.opts...)
			if err := createSess(b); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Snapshot(context.Background(), api.Incarnation{ID: "sess-x"}, api.SnapshotExternal); err != nil {
				t.Fatal(err)
			}
			if err := restoreSess(b); err != nil {
				t.Fatal(err)
			}
			wantCalls(t, c.calls, slices.Concat([]string{"get:sess-x", "create:sess-x"}, tc.create, []string{"suspend:sess-x"}, tc.restore))
		})
	}
}

// Removing the option does not revoke: without it, a resume of an existing SUSPENDED actor, by
// Restore or by Create attaching to it, makes no egress call, so the policy the actor has stored
// stays in force, unchanged and at its version.
func TestNoEgressPolicyOptionKeepsStoredPolicy(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*substrate.Backend) error
	}{
		{name: "restore", call: restoreSess},
		{name: "create attaching", call: createSess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newSuspendedEgressControl()
			c.setPolicy("sess-x", testPolicy)
			if err := tc.call(newEgressBackend(c)); err != nil {
				t.Fatal(err)
			}
			wantCalls(t, c.calls, []string{"get:sess-x", "resume:sess-x"})
			wantPolicy(t, c.policy(t, "sess-x"), testPolicy, 1)
		})
	}
}

// Without EgressPolicyClient, a function that returns no policy still refuses every resume of an
// existing SUSPENDED actor without asking the function: the actor can hold a policy (an
// out-of-band edit, or one an earlier configuration stored) that only a read finds and only an
// update clears, so resuming it would run compute on that policy. A new actor has none, so
// creating it with no policy wanted needs no egress call (with the client, the backend would store
// an empty one; without it there is nothing to write with), and an actor found RUNNING is attached
// without one, as with the client.
func TestNilEgressPolicyWithoutClientRefusesExistingActor(t *testing.T) {
	unsupported := func(t *testing.T, what string, err error) {
		t.Helper()
		if !errors.Is(err, substrate.ErrEgressPolicyUnsupported) || !errors.Is(err, api.ErrComputeConfigInvalid) {
			t.Fatalf("%s: err=%v, want ErrEgressPolicyUnsupported and api.ErrComputeConfigInvalid", what, err)
		}
	}
	t.Run("restore", func(t *testing.T) {
		m := &mockControl{info: running("worker-a")}
		m.exists("sess-x", substrate.StatusSuspended)
		m.holds("sess-x", "gs://snap/sess-x")
		f := &policyFunc{}
		unsupported(t, "restore", restoreSess(newBackend(m, f.option())))
		wantCalls(t, m.calls, []string{"get:sess-x"})
		if st := m.statusOf("sess-x"); st != substrate.StatusSuspended {
			t.Fatalf("actor status=%v, want SUSPENDED", st)
		}
		if len(f.seen) != 0 {
			t.Fatalf("policy function asked for %v", f.seen)
		}
	})
	t.Run("create attaching to a suspended actor", func(t *testing.T) {
		m := &mockControl{info: running("worker-a")}
		m.exists("sess-x", substrate.StatusSuspended)
		m.holds("sess-x", "gs://snap/sess-x")
		unsupported(t, "create", createSess(newBackend(m, (&policyFunc{}).option())))
		for _, call := range m.calls {
			if strings.HasPrefix(call, "resume:") {
				t.Fatalf("calls=%v, want no resume", m.calls)
			}
		}
		if st := m.statusOf("sess-x"); st != substrate.StatusSuspended {
			t.Fatalf("actor status=%v, want SUSPENDED", st)
		}
	})
	t.Run("new actor", func(t *testing.T) {
		m := &mockControl{info: running("worker-a")}
		if err := createSess(newBackend(m, (&policyFunc{}).option())); err != nil {
			t.Fatalf("a new actor with no policy wanted: %v", err)
		}
	})
	t.Run("running actor", func(t *testing.T) {
		m := &mockControl{info: running("worker-a")}
		m.exists("sess-x", substrate.StatusRunning)
		if err := createSess(newBackend(m, (&policyFunc{}).option())); err != nil {
			t.Fatalf("attach to a running actor: %v", err)
		}
	})
}

// A policy the client cannot install, or a function that fails, refuses Create before any actor
// exists, and Restore before it resumes one.
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

	m = &mockControl{info: running("worker-a")}
	m.exists("sess-x", substrate.StatusSuspended)
	m.holds("sess-x", "gs://snap/sess-x")
	if err := restoreSess(newBackend(m, staticPolicy().option())); !errors.Is(err, substrate.ErrEgressPolicyUnsupported) {
		t.Fatalf("restore err=%v want ErrEgressPolicyUnsupported", err)
	}
	wantCalls(t, m.calls, []string{"get:sess-x"})
}

// A failed install fails Create without resuming the actor it created, which stays SUSPENDED. A
// retry finds it, reads that it still has no policy, and creates one before resuming it.
func TestFailedEgressInstallLeavesTheActorSuspended(t *testing.T) {
	c := newEgressControl()
	denied := errors.New("rpc error: code = PermissionDenied")
	c.createErr = denied
	b := newEgressBackend(c, staticPolicy().option())
	err := createSess(b)
	if !errors.Is(err, denied) || !strings.Contains(err.Error(), "install egress policy") || errors.Is(err, api.ErrComputeConfigInvalid) {
		t.Fatalf("err=%v want the install error, unclassified", err)
	}
	wantCalls(t, c.calls, []string{"get:sess-x", "create:sess-x", "egress:sess-x"})
	if st := c.statusOf("sess-x"); st != substrate.StatusSuspended {
		t.Fatalf("actor status=%v after a failed install, want SUSPENDED", st)
	}

	c.createErr, c.calls = nil, nil
	if err := createSess(b); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, c.calls, []string{"get:sess-x", "egress-get:sess-x", "egress:sess-x", "resume:sess-x"})
}

// A fresh actor's empty policy is a write like any other: when it fails, Create fails and leaves
// the actor SUSPENDED, and a retry reads that it still has none, which a resume leaves as it is,
// and resumes it without creating one; a fork's clone is torn down.
func TestFailedFreshNilEgressPolicy(t *testing.T) {
	denied := errors.New("rpc error: code = PermissionDenied")
	t.Run("create", func(t *testing.T) {
		c := newEgressControl()
		c.createErr = denied
		b := newEgressBackend(c, (&policyFunc{}).option())
		if err := createSess(b); !errors.Is(err, denied) || errors.Is(err, api.ErrComputeConfigInvalid) {
			t.Fatalf("err=%v want the install error, unclassified", err)
		}
		wantCalls(t, c.calls, []string{"get:sess-x", "create:sess-x", "egress:sess-x"})
		if st := c.statusOf("sess-x"); st != substrate.StatusSuspended {
			t.Fatalf("actor status=%v after a failed install, want SUSPENDED", st)
		}
		c.createErr, c.calls = nil, nil
		if err := createSess(b); err != nil {
			t.Fatal(err)
		}
		wantCalls(t, c.calls, []string{"get:sess-x", "egress-get:sess-x", "resume:sess-x"})
	})
	t.Run("fork clone", func(t *testing.T) {
		c := newEgressCloner()
		c.createErr = denied
		if err := forkMemory(newMemoryBackend(c, (&policyFunc{}).option()), context.Background()); !errors.Is(err, denied) {
			t.Fatalf("err=%v want the install error", err)
		}
		wantCalls(t, c.calls, []string{"get:parent", "tag:parent->fork-child", "get:parent", "clone:child:from=fork-child", "egress:child",
			"get:child", "suspend:child", "delete:child", "untag:fork-child"})
	})
}

// Every resume the backend issues on a SUSPENDED actor is preceded by the converge: attaching to
// it, attaching after another host created it first, and restoring it. A policy that changed since
// the actor's was stored is written, under the stored one's version; an equal one is not.
func TestEgressPolicyBeforeEveryResume(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*substrate.Backend) error
		head []string
	}{
		{name: "attach", call: createSess, head: []string{"get:sess-x"}},
		{name: "restore", call: restoreSess, head: []string{"get:sess-x"}},
	} {
		t.Run(tc.name+" converges a changed policy", func(t *testing.T) {
			c := newSuspendedEgressControl()
			c.setPolicy("sess-x", otherPolicy)
			f := staticPolicy()
			if err := tc.call(newEgressBackend(c, f.option())); err != nil {
				t.Fatal(err)
			}
			wantCalls(t, c.calls, append(slices.Clone(tc.head), "egress-get:sess-x", "egress-update:sess-x", "resume:sess-x"))
			wantPolicy(t, c.policy(t, "sess-x"), testPolicy, 2)
			if !reflect.DeepEqual(f.seen, []string{"sess-x"}) {
				t.Fatalf("policy function saw %v, want sess-x once", f.seen)
			}
		})
		t.Run(tc.name+" leaves an equal policy alone", func(t *testing.T) {
			c := newSuspendedEgressControl()
			c.setPolicy("sess-x", testPolicy)
			if err := tc.call(newEgressBackend(c, staticPolicy().option())); err != nil {
				t.Fatal(err)
			}
			wantCalls(t, c.calls, append(slices.Clone(tc.head), "egress-get:sess-x", "resume:sess-x"))
			wantPolicy(t, c.policy(t, "sess-x"), testPolicy, 1)
		})
	}
	t.Run("another host created the actor first", func(t *testing.T) {
		c := newEgressControl()
		c.afterGet = func(name string) {
			c.exists(name, substrate.StatusSuspended)
			c.afterGet = nil
		}
		f := staticPolicy()
		if err := createSess(newEgressBackend(c, f.option())); err != nil {
			t.Fatal(err)
		}
		wantCalls(t, c.calls, []string{"get:sess-x", "create:sess-x", "get:sess-x", "egress-get:sess-x", "egress:sess-x", "resume:sess-x"})
		if len(f.seen) != 2 {
			t.Fatalf("policy function ran %d times, want 2: the check before the create and the converge", len(f.seen))
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
	if err := createSess(b); err != nil {
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

// A failed read or write on a SUSPENDED actor fails the call and resumes nothing.
func TestFailedEgressInstallResumesNothing(t *testing.T) {
	denied := errors.New("rpc error: code = PermissionDenied")
	for _, call := range []struct {
		name string
		fn   func(*substrate.Backend) error
	}{{"attach", createSess}, {"restore", restoreSess}} {
		for _, tc := range []struct {
			name   string
			stored *substrate.EgressPolicy
			fail   func(egressControl)
			calls  []string
		}{
			{name: "read", fail: func(c egressControl) { c.readErr = denied }, calls: []string{"egress-get:sess-x"}},
			{name: "create", fail: func(c egressControl) { c.createErr = denied }, calls: []string{"egress-get:sess-x", "egress:sess-x"}},
			{name: "update", stored: &otherPolicy, fail: func(c egressControl) { c.updateErr = denied }, calls: []string{"egress-get:sess-x", "egress-update:sess-x"}},
		} {
			t.Run(call.name+" "+tc.name, func(t *testing.T) {
				c := newSuspendedEgressControl()
				if tc.stored != nil {
					c.setPolicy("sess-x", *tc.stored)
				}
				tc.fail(c)
				err := call.fn(newEgressBackend(c, staticPolicy().option()))
				if !errors.Is(err, denied) || errors.Is(err, api.ErrComputeConfigInvalid) || errors.Is(err, api.ErrComputeBusy) {
					t.Fatalf("err=%v want the egress error, unclassified", err)
				}
				wantCalls(t, c.calls, append([]string{"get:sess-x"}, tc.calls...))
				if st := c.statusOf("sess-x"); st != substrate.StatusSuspended {
					t.Fatalf("actor status=%v, want SUSPENDED", st)
				}
			})
		}
	}
}

// A busy egress policy call (api.ErrComputeBusy, published as ABORTED) fails the call without a
// retry and resumes nothing. The error keeps the client's in its chain, so it still classifies as
// busy, but not its text, which can carry substrate's own message.
func TestBusyEgressCallKeepsTheClientsTextOut(t *testing.T) {
	actorBusy := fmt.Errorf("%w: sess-x: rpc error: code = Aborted desc = %s", substrate.ErrActorBusy, substrateText)
	// Defensive only: the shipped adapter reports a busy update as ErrActorBusy, a conflict that is
	// retried, so a bare api.ErrComputeBusy is what another client could return.
	computeBusy := fmt.Errorf("%w: %s", api.ErrComputeBusy, substrateText)
	for _, call := range []struct {
		name string
		fn   func(*substrate.Backend) error
	}{{"attach", createSess}, {"restore", restoreSess}} {
		for _, tc := range []struct {
			name   string
			stored *substrate.EgressPolicy
			fail   func(egressControl) error
			calls  []string
		}{
			{name: "read", fail: func(c egressControl) error { c.readErr = actorBusy; return actorBusy }, calls: []string{"egress-get:sess-x"}},
			{name: "create", fail: func(c egressControl) error { c.createErr = actorBusy; return actorBusy }, calls: []string{"egress-get:sess-x", "egress:sess-x"}},
			{name: "update", stored: &otherPolicy, fail: func(c egressControl) error { c.updateErr = computeBusy; return computeBusy },
				calls: []string{"egress-get:sess-x", "egress-update:sess-x"}},
		} {
			t.Run(call.name+" "+tc.name, func(t *testing.T) {
				c := newSuspendedEgressControl()
				if tc.stored != nil {
					c.setPolicy("sess-x", *tc.stored)
				}
				cause := tc.fail(c)
				err := call.fn(newEgressBackend(c, staticPolicy().option()))
				wantBusyWithoutText(t, err, cause)
				wantCalls(t, c.calls, append([]string{"get:sess-x"}, tc.calls...))
				if st := c.statusOf("sess-x"); st != substrate.StatusSuspended {
					t.Fatalf("actor status=%v, want SUSPENDED", st)
				}
			})
		}
	}
	t.Run("create of a new actor", func(t *testing.T) {
		c := newEgressControl()
		c.createErr = actorBusy
		wantBusyWithoutText(t, createSess(newEgressBackend(c, staticPolicy().option())), actorBusy)
		wantCalls(t, c.calls, []string{"get:sess-x", "create:sess-x", "egress:sess-x"})
	})
}

// wantBusyWithoutText fails unless err is busy, retains cause, and leaves cause's text out.
func wantBusyWithoutText(t *testing.T, err, cause error) {
	t.Helper()
	if !errors.Is(err, api.ErrComputeBusy) || !errors.Is(err, cause) || errors.Is(err, api.ErrComputeConfigInvalid) {
		t.Fatalf("err=%v want a busy refusal retaining %v", err, cause)
	}
	if errors.Is(cause, substrate.ErrActorBusy) != errors.Is(err, substrate.ErrActorBusy) {
		t.Fatalf("err=%v classifies as ErrActorBusy differently from its cause %v", err, cause)
	}
	if strings.Contains(err.Error(), substrateText) || !strings.Contains(err.Error(), "install egress policy on actor space/sess-x") {
		t.Fatalf("err=%q carries the client's text, or does not name the install and the actor", err)
	}
}

// A converge that reports the actor missing is classified as a failed resume would be: gone,
// Create places a new actor; replaced, it is retryable ErrActorBusy; gone again after another host
// created it, ErrActorBusy; a gone memory-tier actor on Restore is ErrMemoryStateLost.
func TestEgressInstallOnAMissingActor(t *testing.T) {
	t.Run("gone during attach creates a new actor", func(t *testing.T) {
		c := newEgressControl()
		c.exists("sess-x", substrate.StatusSuspended)
		c.beforeGet = func(name string) {
			c.delete(name)
			c.beforeGet = nil
		}
		if err := createSess(newEgressBackend(c, staticPolicy().option())); err != nil {
			t.Fatal(err)
		}
		wantCalls(t, c.calls, []string{"get:sess-x", "egress-get:sess-x", "egress:sess-x", "get:sess-x", "create:sess-x", "egress:sess-x", "resume:sess-x"})
	})
	t.Run("gone before an update", func(t *testing.T) {
		c := newEgressControl()
		c.exists("sess-x", substrate.StatusSuspended)
		c.setPolicy("sess-x", otherPolicy)
		c.beforeUpdate = func(name string) {
			c.delete(name)
			c.beforeUpdate = nil
		}
		if err := createSess(newEgressBackend(c, staticPolicy().option())); err != nil {
			t.Fatal(err)
		}
		wantCalls(t, c.calls, []string{"get:sess-x", "egress-get:sess-x", "egress-update:sess-x", "get:sess-x", "create:sess-x", "egress:sess-x", "resume:sess-x"})
		wantPolicy(t, c.policy(t, "sess-x"), testPolicy, 1)
	})
	t.Run("replaced during attach is busy", func(t *testing.T) {
		c := newEgressControl()
		c.exists("sess-x", substrate.StatusSuspended)
		c.createErr = substrate.ErrActorNotFound
		c.beforeCreate = func(name string) {
			c.delete(name)
			c.exists(name, substrate.StatusSuspended)
		}
		err := createSess(newEgressBackend(c, staticPolicy().option()))
		if !errors.Is(err, substrate.ErrActorBusy) || !strings.Contains(err.Error(), "egress policy was being converged") {
			t.Fatalf("err=%v want ErrActorBusy for a replaced actor", err)
		}
		wantCalls(t, c.calls, []string{"get:sess-x", "egress-get:sess-x", "egress:sess-x", "get:sess-x"})
	})
	t.Run("gone again after another host created it is busy", func(t *testing.T) {
		c := newEgressControl()
		c.afterGet = func(name string) {
			c.exists(name, substrate.StatusSuspended)
			c.afterGet = nil
		}
		c.beforeCreate = func(name string) { c.delete(name) }
		err := createSess(newEgressBackend(c, staticPolicy().option()))
		if !errors.Is(err, substrate.ErrActorBusy) {
			t.Fatalf("err=%v want ErrActorBusy", err)
		}
		wantCalls(t, c.calls, []string{"get:sess-x", "create:sess-x", "get:sess-x", "egress-get:sess-x", "egress:sess-x", "get:sess-x"})
	})
	t.Run("memory-tier restore of a gone actor is lost state", func(t *testing.T) {
		c := newSuspendedEgressControl()
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

// A step of resuming a SUSPENDED actor that reports it missing, when the actor was replaced
// meanwhile, is retryable ErrActorBusy, published as ABORTED. The error keeps the step's in its
// chain but not its text, which can carry substrate's own message.
func TestReplacedActorRefusalKeepsTheClientsTextOut(t *testing.T) {
	notFound := fmt.Errorf("%w: sess-x: rpc error: code = FailedPrecondition desc = %s", substrate.ErrActorNotFound, substrateText)
	for _, tc := range []struct {
		name, during string
		opts         []substrate.Option
		fail         func(c egressControl)
		calls        []string
	}{
		{name: "egress converge", during: "its egress policy was being converged", opts: []substrate.Option{staticPolicy().option()},
			fail:  func(c egressControl) { c.readErr = notFound },
			calls: []string{"get:sess-x", "egress-get:sess-x", "get:sess-x"}},
		{name: "resume", during: "it was being resumed",
			fail:  func(c egressControl) { c.resumeErr = notFound },
			calls: []string{"get:sess-x", "resume:sess-x", "get:sess-x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newEgressControl()
			c.exists("sess-x", substrate.StatusSuspended)
			c.afterGet = func(name string) {
				c.delete(name)
				c.exists(name, substrate.StatusSuspended)
				c.afterGet = nil
			}
			tc.fail(c)
			err := createSess(newEgressBackend(c, tc.opts...))
			if !errors.Is(err, substrate.ErrActorBusy) || !errors.Is(err, notFound) {
				t.Fatalf("err=%v want ErrActorBusy retaining %v", err, notFound)
			}
			if strings.Contains(err.Error(), substrateText) || !strings.Contains(err.Error(), "actor space/sess-x was replaced while "+tc.during) {
				t.Fatalf("err=%q carries the client's text, or does not say the actor was replaced while %s", err, tc.during)
			}
			wantCalls(t, c.calls, tc.calls)
		})
	}
}

func forkMemory(b *substrate.Backend, ctx context.Context) error {
	_, err := b.Fork(ctx, api.SnapshotRef{Local: "parent", ExternalURI: "snap-parent-1", Memory: true},
		api.ForkOpts{ChildSessionUID: "child"})
	return err
}

// A stateful fork creates the child's policy on the clone between its create and its resume. The
// clone is new, so nothing is read first.
func TestForkCloneInstallsEgressPolicyBeforeResume(t *testing.T) {
	c := newEgressCloner()
	f := staticPolicy()
	if err := forkMemory(newMemoryBackend(c, f.option()), context.Background()); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, c.calls, []string{"get:parent", "tag:parent->fork-child", "get:parent", "clone:child:from=fork-child", "egress:child", "resume:child"})
	wantPolicy(t, c.policy(t, "child"), testPolicy, 1)
	if !reflect.DeepEqual(f.seen, []string{"child", "child"}) {
		t.Fatalf("policy function saw %v, want the child's UID before the tag and after the clone", f.seen)
	}
}

// A fork's clone that already has a policy when the backend creates one is refused, not converged:
// the host has not recorded the child, so the policy is not this backend's, and the actor under the
// name may no longer be the clone, which a policy call cannot tell. Nothing is written and nothing
// is resumed. The clone, if it is still this fork's, is torn down with its policy; a replacement is
// left as it is and only the tag is released. The refusal is retryable and keeps the client's text
// out.
func TestForkCloneRefusesAPolicyCreatedFirst(t *testing.T) {
	head := []string{"get:parent", "tag:parent->fork-child", "get:parent", "clone:child:from=fork-child", "egress:child"}
	t.Run("own clone torn down", func(t *testing.T) {
		c := newEgressCloner()
		c.beforeCreate = func(name string) {
			c.setPolicy(name, otherPolicy)
			c.beforeCreate = nil
		}
		err := forkMemory(newMemoryBackend(c, staticPolicy().option()), context.Background())
		if !errors.Is(err, substrate.ErrActorBusy) || !errors.Is(err, substrate.ErrEgressPolicyExists) {
			t.Fatalf("err=%v want ErrActorBusy retaining the create's conflict", err)
		}
		if !strings.Contains(err.Error(), "install egress policy on actor space/child") {
			t.Fatalf("err=%q does not name the install and the actor", err)
		}
		wantCalls(t, c.calls, append(slices.Clone(head), "get:child", "suspend:child", "delete:child", "untag:fork-child"))
	})
	t.Run("replacement left untouched", func(t *testing.T) {
		c := newEgressCloner()
		var replacement string
		c.beforeCreate = func(name string) {
			c.delete(name)
			c.exists(name, substrate.StatusSuspended)
			c.setPolicy(name, otherPolicy)
			replacement = c.uid(name)
			c.beforeCreate = nil
		}
		err := forkMemory(newMemoryBackend(c, staticPolicy().option()), context.Background())
		if !errors.Is(err, substrate.ErrActorBusy) || !errors.Is(err, substrate.ErrEgressPolicyExists) || !strings.Contains(err.Error(), "the replacement was left in place") {
			t.Fatalf("err=%v want ErrActorBusy for a replaced clone, retaining the create's conflict", err)
		}
		wantCalls(t, c.calls, append(slices.Clone(head), "get:child", "untag:fork-child"))
		if c.uid("child") != replacement || c.statusOf("child") != substrate.StatusSuspended {
			t.Fatalf("replacement uid=%s status=%v, want %s left SUSPENDED", c.uid("child"), c.statusOf("child"), replacement)
		}
		wantPolicy(t, c.policy(t, "child"), otherPolicy, 1)
	})
}

// A stateless fork creates its child through Create, which installs the child's policy.
func TestStatelessForkChildGetsEgressPolicy(t *testing.T) {
	c := newEgressControl()
	f := staticPolicy()
	if _, err := newEgressBackend(c, f.option()).Fork(context.Background(), api.SnapshotRef{Local: "parent"}, api.ForkOpts{ChildSessionUID: "child"}); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, c.calls, []string{"get:child", "create:child", "egress:child", "resume:child"})
	if !reflect.DeepEqual(f.seen, []string{"child", "child"}) {
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
	teardown := []string{"get:child", "suspend:child", "delete:child", "untag:fork-child"}
	denied := errors.New("rpc error: code = PermissionDenied")
	t.Run("clone torn down", func(t *testing.T) {
		c := newEgressCloner()
		c.createErr = denied
		err := forkMemory(newMemoryBackend(c, staticPolicy().option()), context.Background())
		if !errors.Is(err, denied) {
			t.Fatalf("err=%v want the install error", err)
		}
		wantCalls(t, c.calls, append(slices.Clone(head), teardown...))
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
	t.Run("busy install keeps the client's text out", func(t *testing.T) {
		c := newEgressCloner()
		busy := fmt.Errorf("%w: space/child: rpc error: code = Aborted desc = %s", substrate.ErrActorBusy, substrateText)
		c.createErr = busy
		err := forkMemory(newMemoryBackend(c, staticPolicy().option()), context.Background())
		if !errors.Is(err, api.ErrComputeBusy) || !errors.Is(err, busy) || strings.Contains(err.Error(), substrateText) {
			t.Fatalf("err=%q want ErrComputeBusy retaining the install error without its text", err)
		}
		wantCalls(t, c.calls, append(slices.Clone(head), teardown...))
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
		wantCalls(t, c.calls, append(slices.Clone(head), teardown...))
	})
}

// The converge is logged by actor, rule count and outcome only: no hostname or credential
// reference.
func TestEgressInstallLogsNoPolicyContents(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stored  *substrate.EgressPolicy
		f       *policyFunc
		call    func(*substrate.Backend) error // restoreSess when nil
		outcome string
		rules   int
	}{
		{name: "created", f: staticPolicy(), outcome: "created", rules: 1},
		{name: "unchanged", stored: &testPolicy, f: staticPolicy(), outcome: "unchanged", rules: 1},
		{name: "updated", stored: &otherPolicy, f: staticPolicy(), outcome: "updated", rules: 1},
		{name: "cleared", stored: &testPolicy, f: &policyFunc{}, outcome: "cleared", rules: 0},
		{name: "absent", f: &policyFunc{}, outcome: "absent", rules: 0},
		{name: "fenced by a Sync", stored: &testPolicy, f: staticPolicy(), call: syncSess, outcome: "fenced", rules: 1},
		{name: "empty created by a Sync", f: &policyFunc{}, call: syncSess, outcome: "created", rules: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			c := newSuspendedEgressControl()
			if tc.stored != nil {
				c.setPolicy("sess-x", *tc.stored)
			}
			call := tc.call
			if call == nil {
				call = restoreSess
			}
			if err := call(newEgressBackend(c, tc.f.option(), substrate.WithLogger(debugLogger(&out)))); err != nil {
				t.Fatal(err)
			}
			logs := out.String()
			for _, want := range []string{`"operation":"install_egress_policy"`, fmt.Sprintf(`"rules":%d`, tc.rules), `"policy_outcome":"` + tc.outcome + `"`, `"outcome":"success"`} {
				if !strings.Contains(logs, want) {
					t.Fatalf("converge not logged with %s: %s", want, logs)
				}
			}
			for _, secret := range []string{"example.com", "ate-secret://", "Bearer"} {
				if strings.Contains(logs, secret) {
					t.Fatalf("logs carry policy contents %q: %s", secret, logs)
				}
			}
		})
	}
}

// A policy the client refuses as invalid, and a stored policy it cannot read, are refused the same
// way on every call until the configuration changes, so they are api.ErrComputeConfigInvalid.
// Neither the error text nor the log carries the client's message, which can name a rejected
// hostname or credential reference; Go callers reach it through Unwrap, and the log records the
// error_kind. The actor stays SUSPENDED, and a stored policy that cannot be read is not written.
func TestRefusedEgressPolicyIsComputeConfigInvalid(t *testing.T) {
	refusal := "rpc error: code = InvalidArgument desc = rules[0].https.hostnames[0]: secret-host.example.com; credential_uri ate-secret://kv/vault/model-key"
	invalid := fmt.Errorf("%w: space/sess-x: %s", substrate.ErrEgressPolicyInvalid, refusal)
	for _, tc := range []struct {
		name   string
		setup  func(egressControl)
		call   func(*substrate.Backend) error
		calls  []string
		stored bool
	}{
		{name: "create refused", setup: func(c egressControl) { c.createErr = invalid }, call: createSess,
			calls: []string{"get:sess-x", "create:sess-x", "egress:sess-x"}},
		{name: "update refused", setup: func(c egressControl) {
			c.exists("sess-x", substrate.StatusSuspended)
			c.setPolicy("sess-x", otherPolicy)
			c.updateErr = invalid
		}, call: createSess, calls: []string{"get:sess-x", "egress-get:sess-x", "egress-update:sess-x"}},
		{name: "stored policy unreadable", setup: func(c egressControl) {
			c.exists("sess-x", substrate.StatusSuspended)
			c.holds("sess-x", "gs://snap/sess-x")
			c.readErr = invalid
		}, call: restoreSess, calls: []string{"get:sess-x", "egress-get:sess-x"}, stored: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			c := newEgressControl()
			tc.setup(c)
			err := tc.call(newEgressBackend(c, staticPolicy().option(), substrate.WithLogger(debugLogger(&out))))
			if !errors.Is(err, api.ErrComputeConfigInvalid) || !errors.Is(err, substrate.ErrEgressPolicyInvalid) || !errors.Is(err, invalid) {
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
			if got := strings.Contains(err.Error(), "stored policy"); got != tc.stored {
				t.Fatalf("error %q names the stored policy: %v, want %v", err, got, tc.stored)
			}
			wantCalls(t, c.calls, tc.calls)
			if st := c.statusOf("sess-x"); st != substrate.StatusSuspended {
				t.Fatalf("actor status=%v after a refused policy, want SUSPENDED", st)
			}
			logs := out.String()
			if !strings.Contains(logs, `"error_kind":"egress_policy_invalid"`) {
				t.Fatalf("refusal not logged with its error_kind: %s", logs)
			}
			for _, leaked := range []string{"secret-host", "ate-secret"} {
				if strings.Contains(logs, leaked) {
					t.Fatalf("logs carry the client's message (%q): %s", leaked, logs)
				}
			}
		})
	}
}

// The stored policy is compared with the configured one by meaning, as substrate stores it, so an
// unchanged policy costs one read and no write, and only a change is written.
func TestEgressPolicyComparedAsStored(t *testing.T) {
	https := func(r substrate.EgressRule) substrate.EgressPolicy {
		r.Protocol = substrate.EgressHTTPS
		if r.Hostnames == nil {
			r.Hostnames = []string{"a.example.com", "b.example.com"}
		}
		return substrate.EgressPolicy{Rules: []substrate.EgressRule{r}}
	}
	jwt := func(exp int64, aud ...string) []substrate.CredentialHeader {
		return []substrate.CredentialHeader{{Header: "Authorization", Prefix: "Bearer ", ActorJWT: &substrate.ActorJWTSource{Audiences: aud, ExpirationSeconds: exp}}}
	}
	two := substrate.EgressPolicy{Rules: []substrate.EgressRule{
		{Protocol: substrate.EgressHTTP, Hostnames: []string{"a.example.com"}},
		{Protocol: substrate.EgressTLSPassthrough, Hostnames: []string{"b.example.com"}, Ports: []int32{443}},
	}}
	for _, tc := range []struct {
		name            string
		stored, desired substrate.EgressPolicy
		write           bool
	}{
		{name: "default https port", stored: https(substrate.EgressRule{Ports: []int32{443}}), desired: https(substrate.EgressRule{})},
		{name: "default http port", stored: substrate.EgressPolicy{Rules: []substrate.EgressRule{{Protocol: substrate.EgressHTTP, Hostnames: []string{"a"}, Ports: []int32{80}}}},
			desired: substrate.EgressPolicy{Rules: []substrate.EgressRule{{Protocol: substrate.EgressHTTP, Hostnames: []string{"a"}}}}},
		{name: "hostnames and ports reordered", stored: https(substrate.EgressRule{Hostnames: []string{"b", "a"}, Ports: []int32{8443, 443}}),
			desired: https(substrate.EgressRule{Hostnames: []string{"a", "b"}, Ports: []int32{443, 8443}})},
		{name: "same actor JWT", stored: https(substrate.EgressRule{ReplaceHeaders: jwt(300, "x", "y")}), desired: https(substrate.EgressRule{ReplaceHeaders: jwt(300, "x", "y")})},
		{name: "same rules in order", stored: two, desired: two},
		{name: "explicit port not the default", stored: https(substrate.EgressRule{}), desired: https(substrate.EgressRule{Ports: []int32{8443}}), write: true},
		{name: "all ports", stored: https(substrate.EgressRule{}), desired: https(substrate.EgressRule{AllPorts: true}), write: true},
		{name: "hostname added", stored: https(substrate.EgressRule{}), desired: https(substrate.EgressRule{Hostnames: []string{"a.example.com", "b.example.com", "c.example.com"}}), write: true},
		{name: "protocol", stored: https(substrate.EgressRule{}), desired: substrate.EgressPolicy{Rules: []substrate.EgressRule{{Protocol: substrate.EgressHTTP, Hostnames: []string{"a.example.com", "b.example.com"}, Ports: []int32{443}}}}, write: true},
		{name: "credential reference", stored: testPolicy, desired: https(substrate.EgressRule{Hostnames: []string{"api.example.com"}, ReplaceHeaders: []substrate.CredentialHeader{{Header: "Authorization", Prefix: "Bearer ", CredentialURI: "ate-secret://kv/vault/other"}}}), write: true},
		{name: "header prefix", stored: testPolicy, desired: https(substrate.EgressRule{Hostnames: []string{"api.example.com"}, ReplaceHeaders: []substrate.CredentialHeader{{Header: "Authorization", CredentialURI: "ate-secret://kv/vault/model-key"}}}), write: true},
		{name: "header removed", stored: testPolicy, desired: https(substrate.EgressRule{Hostnames: []string{"api.example.com"}}), write: true},
		{name: "JWT expiry", stored: https(substrate.EgressRule{ReplaceHeaders: jwt(300, "x")}), desired: https(substrate.EgressRule{ReplaceHeaders: jwt(600, "x")}), write: true},
		{name: "JWT audiences reordered", stored: https(substrate.EgressRule{ReplaceHeaders: jwt(300, "x", "y")}), desired: https(substrate.EgressRule{ReplaceHeaders: jwt(300, "y", "x")}), write: true},
		{name: "JWT for a credential", stored: testPolicy, desired: https(substrate.EgressRule{Hostnames: []string{"api.example.com"}, ReplaceHeaders: jwt(300, "x")}), write: true},
		{name: "rules reordered", stored: two, desired: substrate.EgressPolicy{Rules: []substrate.EgressRule{two.Rules[1], two.Rules[0]}}, write: true},
		{name: "rule removed", stored: two, desired: substrate.EgressPolicy{Rules: two.Rules[:1]}, write: true},
		{name: "empty to a rule", stored: substrate.EgressPolicy{}, desired: two, write: true},
		{name: "empty to empty", stored: substrate.EgressPolicy{}, desired: substrate.EgressPolicy{Rules: []substrate.EgressRule{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newSuspendedEgressControl()
			c.setPolicy("sess-x", tc.stored)
			if err := restoreSess(newEgressBackend(c, returning(tc.desired).option())); err != nil {
				t.Fatal(err)
			}
			want := []string{"get:sess-x", "egress-get:sess-x", "resume:sess-x"}
			version := int64(1)
			if tc.write {
				want = []string{"get:sess-x", "egress-get:sess-x", "egress-update:sess-x", "resume:sess-x"}
				version = 2
			}
			wantCalls(t, c.calls, want)
			if tc.write {
				wantPolicy(t, c.policy(t, "sess-x"), tc.desired, version)
			} else {
				wantPolicy(t, c.policy(t, "sess-x"), tc.stored, version)
			}
		})
	}
}

// A function that returns no policy is deny-all: a stored policy with rules is cleared to an empty
// rule set under its version, never deleted, and an empty or missing one is left as it is.
func TestNilEgressPolicyClearsAStoredOne(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stored *substrate.EgressPolicy
		calls  []string
	}{
		{name: "rules cleared", stored: &testPolicy, calls: []string{"egress-get:sess-x", "egress-update:sess-x"}},
		{name: "empty kept", stored: &substrate.EgressPolicy{}, calls: []string{"egress-get:sess-x"}},
		{name: "none left", calls: []string{"egress-get:sess-x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newSuspendedEgressControl()
			if tc.stored != nil {
				c.setPolicy("sess-x", *tc.stored)
			}
			if err := restoreSess(newEgressBackend(c, (&policyFunc{}).option())); err != nil {
				t.Fatal(err)
			}
			wantCalls(t, c.calls, append(append([]string{"get:sess-x"}, tc.calls...), "resume:sess-x"))
			info, ok := c.current("sess-x")
			if ok != (tc.stored != nil) || len(info.Policy.Rules) != 0 {
				t.Fatalf("stored policy=%+v (present %v), want an empty one exactly when one was stored", info, ok)
			}
		})
	}
	t.Run("cleared under the version read", func(t *testing.T) {
		c := newSuspendedEgressControl()
		c.setPolicy("sess-x", testPolicy)
		c.beforeUpdate = func(name string) {
			c.bump(name, otherPolicy) // another writer, between the read and the clear
			c.beforeUpdate = nil
		}
		if err := restoreSess(newEgressBackend(c, (&policyFunc{}).option())); err != nil {
			t.Fatal(err)
		}
		wantCalls(t, c.calls, []string{"get:sess-x", "egress-get:sess-x", "egress-update:sess-x", "egress-get:sess-x", "egress-update:sess-x", "resume:sess-x"})
		wantPolicy(t, c.policy(t, "sess-x"), substrate.EgressPolicy{}, 3)
	})
}

// The function is asked after the stored policy is read, so a write based on its answer cannot
// undo a change made before the read; a precondition that no longer holds starts the read, the
// question and the write over.
func TestEgressConvergeOrdersReadBeforeTheFunction(t *testing.T) {
	t.Run("a change made before the read is what is written", func(t *testing.T) {
		c := newSuspendedEgressControl()
		c.setPolicy("sess-x", otherPolicy)
		f := returning(otherPolicy)
		c.beforeGet = func(string) { f.policy = &testPolicy }
		if err := restoreSess(newEgressBackend(c, f.option())); err != nil {
			t.Fatal(err)
		}
		wantPolicy(t, c.policy(t, "sess-x"), testPolicy, 2)
	})
	t.Run("a write that lost a race is redone with a fresh answer", func(t *testing.T) {
		c := newSuspendedEgressControl()
		c.setPolicy("sess-x", otherPolicy)
		f := returning(testPolicy)
		newer := substrate.EgressPolicy{Rules: []substrate.EgressRule{{Protocol: substrate.EgressHTTPS, Hostnames: []string{"newer.example.com"}}}}
		c.beforeUpdate = func(name string) {
			// Another converge wrote a newer policy after this one asked the function.
			c.bump(name, newer)
			f.policy = &newer
			c.beforeUpdate = nil
		}
		if err := restoreSess(newEgressBackend(c, f.option())); err != nil {
			t.Fatal(err)
		}
		wantCalls(t, c.calls, []string{"get:sess-x", "egress-get:sess-x", "egress-update:sess-x", "egress-get:sess-x", "resume:sess-x"})
		wantPolicy(t, c.policy(t, "sess-x"), newer, 2)
		if len(f.seen) != 2 {
			t.Fatalf("policy function ran %d times, want once per attempt", len(f.seen))
		}
	})
	t.Run("a policy deleted before the update is created", func(t *testing.T) {
		c := newSuspendedEgressControl()
		c.setPolicy("sess-x", otherPolicy)
		c.beforeUpdate = func(name string) {
			delete(c.policies, name)
			c.beforeUpdate = nil
		}
		if err := restoreSess(newEgressBackend(c, staticPolicy().option())); err != nil {
			t.Fatal(err)
		}
		wantCalls(t, c.calls, []string{"get:sess-x", "egress-get:sess-x", "egress-update:sess-x", "egress-get:sess-x", "egress:sess-x", "resume:sess-x"})
		wantPolicy(t, c.policy(t, "sess-x"), testPolicy, 1)
	})
	t.Run("a policy created before the create is converged", func(t *testing.T) {
		c := newSuspendedEgressControl()
		c.beforeCreate = func(name string) {
			c.setPolicy(name, otherPolicy)
			c.beforeCreate = nil
		}
		if err := restoreSess(newEgressBackend(c, staticPolicy().option())); err != nil {
			t.Fatal(err)
		}
		wantCalls(t, c.calls, []string{"get:sess-x", "egress-get:sess-x", "egress:sess-x", "egress-get:sess-x", "egress-update:sess-x", "resume:sess-x"})
		wantPolicy(t, c.policy(t, "sess-x"), testPolicy, 2)
	})
	t.Run("Create's fresh actor gets the answer given after it exists", func(t *testing.T) {
		c := newEgressControl()
		c.beforeCreate = func(name string) {
			c.setPolicy(name, testPolicy) // created first by another writer, and already right
			c.beforeCreate = nil
		}
		if err := createSess(newEgressBackend(c, staticPolicy().option())); err != nil {
			t.Fatal(err)
		}
		wantCalls(t, c.calls, []string{"get:sess-x", "create:sess-x", "egress:sess-x", "egress-get:sess-x", "resume:sess-x"})
		wantPolicy(t, c.policy(t, "sess-x"), testPolicy, 1)
	})
}

// A precondition another writer moves on every attempt fails the call after three, without
// resuming the actor, as retryable ErrActorBusy whose text leaves out the client's message.
func TestEgressConvergeConflictsExhausted(t *testing.T) {
	var out bytes.Buffer
	c := newSuspendedEgressControl()
	c.setPolicy("sess-x", otherPolicy)
	c.beforeUpdate = func(name string) { c.bump(name, otherPolicy) }
	f := staticPolicy()
	err := restoreSess(newEgressBackend(c, f.option(), substrate.WithLogger(debugLogger(&out))))
	if !errors.Is(err, substrate.ErrActorBusy) || !errors.Is(err, api.ErrComputeBusy) || errors.Is(err, api.ErrComputeConfigInvalid) {
		t.Fatalf("err=%v want ErrActorBusy", err)
	}
	if strings.Contains(err.Error(), substrateText) || !strings.Contains(err.Error(), "3 attempts") {
		t.Fatalf("err=%q want the attempts named and the client's text left out", err)
	}
	attempt := []string{"egress-get:sess-x", "egress-update:sess-x"}
	wantCalls(t, c.calls, append(append(append([]string{"get:sess-x"}, attempt...), attempt...), attempt...))
	if len(f.seen) != 3 {
		t.Fatalf("policy function ran %d times, want once per attempt", len(f.seen))
	}
	if st := c.statusOf("sess-x"); st != substrate.StatusSuspended {
		t.Fatalf("actor status=%v, want SUSPENDED", st)
	}
	if logs := out.String(); !strings.Contains(logs, `"attempts":3`) || strings.Contains(logs, substrateText) {
		t.Fatalf("converge not logged with its attempts, or with the client's text: %s", logs)
	}
}

// A caller that goes away during a conflict stops the converge before the next attempt.
func TestEgressConvergeStopsWhenTheCallerGoes(t *testing.T) {
	c := newSuspendedEgressControl()
	c.setPolicy("sess-x", otherPolicy)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.updateErr = fmt.Errorf("%w: sess-x: rpc error: code = Aborted", substrate.ErrActorBusy)
	c.beforeUpdate = func(string) { cancel() }
	_, err := newEgressBackend(c, staticPolicy().option()).Restore(ctx, api.SnapshotRef{Local: "sess-x", ExternalURI: "gs://snap/sess-x"})
	if !errors.Is(err, context.Canceled) || errors.Is(err, substrate.ErrActorBusy) {
		t.Fatalf("err=%v want context.Canceled", err)
	}
	wantCalls(t, c.calls, []string{"get:sess-x", "egress-get:sess-x", "egress-update:sess-x"})
}

// A function that fails after the check before the create, or on a later attempt, fails the call
// unclassified, without writing or resuming.
func TestEgressPolicyFunctionFailsLater(t *testing.T) {
	boom := errors.New("policy store unavailable")
	failOn := func(n int) *policyFunc {
		f := staticPolicy()
		f.onCall = func(f *policyFunc) {
			if len(f.seen) == n {
				f.err = boom
			}
		}
		return f
	}
	t.Run("after the create", func(t *testing.T) {
		c := newEgressControl()
		if err := createSess(newEgressBackend(c, failOn(2).option())); !errors.Is(err, boom) || errors.Is(err, api.ErrComputeConfigInvalid) {
			t.Fatalf("err=%v want the function's error, unclassified", err)
		}
		wantCalls(t, c.calls, []string{"get:sess-x", "create:sess-x"})
		if st := c.statusOf("sess-x"); st != substrate.StatusSuspended {
			t.Fatalf("actor status=%v, want SUSPENDED", st)
		}
	})
	t.Run("on a retry", func(t *testing.T) {
		c := newSuspendedEgressControl()
		c.setPolicy("sess-x", otherPolicy)
		c.beforeUpdate = func(name string) { c.bump(name, otherPolicy) }
		if err := restoreSess(newEgressBackend(c, failOn(2).option())); !errors.Is(err, boom) {
			t.Fatalf("err=%v want the function's error", err)
		}
		wantCalls(t, c.calls, []string{"get:sess-x", "egress-get:sess-x", "egress-update:sess-x", "egress-get:sess-x"})
	})
}

// A client whose read reports no UID or version cannot be written under a precondition, which
// substrate requires; the backend refuses rather than send an update substrate would reject as an
// invalid policy, and the error is not classified as one.
func TestEgressReadWithoutAPreconditionIsRefused(t *testing.T) {
	c := newSuspendedEgressControl()
	c.setPolicy("sess-x", otherPolicy)
	c.noPrecondition = true
	err := restoreSess(newEgressBackend(c, staticPolicy().option()))
	if err == nil || errors.Is(err, api.ErrComputeConfigInvalid) || !strings.Contains(err.Error(), "UID and version") {
		t.Fatalf("err=%v want an unclassified error naming the missing UID and version", err)
	}
	wantCalls(t, c.calls, []string{"get:sess-x", "egress-get:sess-x"})

	// An equal policy needs no write on a resume, so no precondition either; a Sync writes it
	// again, so it needs one.
	c = newSuspendedEgressControl()
	c.setPolicy("sess-x", testPolicy)
	c.noPrecondition = true
	b := newEgressBackend(c, staticPolicy().option())
	if err := restoreSess(b); err != nil {
		t.Fatal(err)
	}
	c.calls = nil
	err = syncSess(b)
	if err == nil || errors.Is(err, api.ErrComputeConfigInvalid) || !strings.Contains(err.Error(), "UID and version") {
		t.Fatalf("Sync err=%v want an unclassified error naming the missing UID and version", err)
	}
	wantCalls(t, c.calls, []string{"egress-get:sess-x"})
}

func syncSess(b *substrate.Backend) error {
	return b.SyncEgressPolicy(context.Background(), "sess-x")
}

// SyncEgressPolicy converges a running actor's policy as a resume would, without resuming or
// suspending it or reading its state, except that it always writes: an equal policy is written
// again under the version read, and no policy wanted where none is stored creates an empty one.
func TestSyncEgressPolicyOnARunningActor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stored  *substrate.EgressPolicy
		f       *policyFunc
		calls   []string
		want    substrate.EgressPolicy
		version int64
	}{
		{name: "changed", stored: &otherPolicy, f: staticPolicy(), calls: []string{"egress-get:sess-x", "egress-update:sess-x"}, want: testPolicy, version: 2},
		{name: "equal", stored: &testPolicy, f: staticPolicy(), calls: []string{"egress-get:sess-x", "egress-update:sess-x"}, want: testPolicy, version: 2},
		{name: "none stored", f: staticPolicy(), calls: []string{"egress-get:sess-x", "egress:sess-x"}, want: testPolicy, version: 1},
		{name: "nil clears", stored: &testPolicy, f: &policyFunc{}, calls: []string{"egress-get:sess-x", "egress-update:sess-x"}, version: 2},
		{name: "nil with none stored", f: &policyFunc{}, calls: []string{"egress-get:sess-x", "egress:sess-x"}, version: 1},
		{name: "nil with empty stored", stored: &substrate.EgressPolicy{}, f: &policyFunc{}, calls: []string{"egress-get:sess-x", "egress-update:sess-x"}, version: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newEgressControl()
			c.exists("sess-x", substrate.StatusRunning)
			if tc.stored != nil {
				c.setPolicy("sess-x", *tc.stored)
			}
			if err := syncSess(newEgressBackend(c, tc.f.option())); err != nil {
				t.Fatal(err)
			}
			wantCalls(t, c.calls, tc.calls)
			wantPolicy(t, c.policy(t, "sess-x"), tc.want, tc.version)
			if st := c.statusOf("sess-x"); st != substrate.StatusRunning {
				t.Fatalf("actor status=%v, want RUNNING", st)
			}
		})
	}
	t.Run("a suspended actor is not resumed", func(t *testing.T) {
		c := newSuspendedEgressControl()
		c.setPolicy("sess-x", otherPolicy)
		if err := syncSess(newEgressBackend(c, staticPolicy().option())); err != nil {
			t.Fatal(err)
		}
		wantCalls(t, c.calls, []string{"egress-get:sess-x", "egress-update:sess-x"})
		if st := c.statusOf("sess-x"); st != substrate.StatusSuspended {
			t.Fatalf("actor status=%v, want SUSPENDED", st)
		}
	})
}

// A Sync for a session without an actor has nothing to apply a policy to: one the function wants
// is ErrActorNotFound, and no policy is no error, also when the actor is deleted between the
// Sync's read and its write.
func TestSyncEgressPolicyWithoutAnActor(t *testing.T) {
	c := newEgressControl()
	err := syncSess(newEgressBackend(c, staticPolicy().option()))
	if !errors.Is(err, substrate.ErrActorNotFound) || errors.Is(err, api.ErrComputeConfigInvalid) {
		t.Fatalf("err=%v want ErrActorNotFound", err)
	}
	wantCalls(t, c.calls, []string{"egress-get:sess-x", "egress:sess-x"})

	c = newEgressControl()
	if err := syncSess(newEgressBackend(c, (&policyFunc{}).option())); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, c.calls, []string{"egress-get:sess-x", "egress:sess-x"})

	for _, tc := range []struct {
		name   string
		stored *substrate.EgressPolicy
		f      *policyFunc
		write  string
		found  bool
	}{
		{name: "none wanted, empty stored", stored: &substrate.EgressPolicy{}, f: &policyFunc{}, write: "egress-update:sess-x"},
		{name: "none wanted, rules stored", stored: &testPolicy, f: &policyFunc{}, write: "egress-update:sess-x"},
		{name: "none wanted, none stored", f: &policyFunc{}, write: "egress:sess-x"},
		{name: "a policy wanted", stored: &testPolicy, f: returning(otherPolicy), write: "egress-update:sess-x", found: true},
	} {
		t.Run("deleted before the write: "+tc.name, func(t *testing.T) {
			c := newEgressControl()
			c.exists("sess-x", substrate.StatusRunning)
			if tc.stored != nil {
				c.setPolicy("sess-x", *tc.stored)
			}
			c.beforeCreate = func(name string) { c.delete(name) }
			c.beforeUpdate = c.beforeCreate
			err := syncSess(newEgressBackend(c, tc.f.option()))
			if got := errors.Is(err, substrate.ErrActorNotFound); got != tc.found || (!tc.found && err != nil) {
				t.Fatalf("err=%v, want ErrActorNotFound only when a policy is wanted", err)
			}
			wantCalls(t, c.calls, []string{"egress-get:sess-x", tc.write})
		})
	}
}

// A Sync that cannot run makes no call; its errors are classified as a resume's.
func TestSyncEgressPolicyErrors(t *testing.T) {
	t.Run("without WithEgressPolicy", func(t *testing.T) {
		c := newEgressControl()
		c.exists("sess-x", substrate.StatusRunning)
		if err := syncSess(newEgressBackend(c)); err == nil || errors.Is(err, api.ErrComputeConfigInvalid) {
			t.Fatalf("err=%v want an unclassified error", err)
		}
		wantCalls(t, c.calls, nil)
	})
	t.Run("without a session", func(t *testing.T) {
		c := newEgressControl()
		f := staticPolicy()
		if err := newEgressBackend(c, f.option()).SyncEgressPolicy(context.Background(), ""); err == nil {
			t.Fatal("Sync of an empty session uid succeeded")
		}
		wantCalls(t, c.calls, nil)
		if len(f.seen) != 0 {
			t.Fatalf("policy function asked for %v", f.seen)
		}
	})
	t.Run("without EgressPolicyClient", func(t *testing.T) {
		m := &mockControl{info: running("worker-a")}
		m.exists("sess-x", substrate.StatusRunning)
		if err := syncSess(newBackend(m, staticPolicy().option())); !errors.Is(err, substrate.ErrEgressPolicyUnsupported) || !errors.Is(err, api.ErrComputeConfigInvalid) {
			t.Fatalf("err=%v want ErrEgressPolicyUnsupported", err)
		}
		// Even with no policy wanted: the actor can hold one that only a read finds and only an
		// update clears. The function is not asked.
		none := &policyFunc{}
		if err := syncSess(newBackend(m, none.option())); !errors.Is(err, substrate.ErrEgressPolicyUnsupported) || !errors.Is(err, api.ErrComputeConfigInvalid) {
			t.Fatalf("nil policy: err=%v want ErrEgressPolicyUnsupported", err)
		}
		if len(none.seen) != 0 {
			t.Fatalf("policy function asked for %v", none.seen)
		}
		wantCalls(t, m.calls, nil)
	})
	t.Run("refused", func(t *testing.T) {
		c := newEgressControl()
		c.exists("sess-x", substrate.StatusRunning)
		c.setPolicy("sess-x", otherPolicy)
		c.updateErr = fmt.Errorf("%w: rpc error: code = InvalidArgument desc = %s", substrate.ErrEgressPolicyInvalid, substrateText)
		err := syncSess(newEgressBackend(c, staticPolicy().option()))
		if !errors.Is(err, api.ErrComputeConfigInvalid) || strings.Contains(err.Error(), substrateText) {
			t.Fatalf("err=%v want api.ErrComputeConfigInvalid without the client's text", err)
		}
	})
	t.Run("conflicts exhausted", func(t *testing.T) {
		c := newEgressControl()
		c.exists("sess-x", substrate.StatusRunning)
		c.setPolicy("sess-x", otherPolicy)
		c.beforeUpdate = func(name string) { c.bump(name, otherPolicy) }
		if err := syncSess(newEgressBackend(c, staticPolicy().option())); !errors.Is(err, substrate.ErrActorBusy) {
			t.Fatalf("err=%v want ErrActorBusy", err)
		}
	})
}

// A Sync after a change to the function's answer is not undone by a resume converging at the same
// time on the same actor: whichever reads second asks the function after the change, and a write
// based on an older read conflicts and starts over.
func TestSyncEgressPolicyRacesAResume(t *testing.T) {
	newer := substrate.EgressPolicy{Rules: []substrate.EgressRule{{Protocol: substrate.EgressHTTPS, Hostnames: []string{"newer.example.com"}}}}
	t.Run("Sync before the resume reads", func(t *testing.T) {
		c := newSuspendedEgressControl()
		c.setPolicy("sess-x", testPolicy)
		f := staticPolicy()
		b := newEgressBackend(c, f.option())
		c.beforeGet = func(string) {
			c.beforeGet = nil
			f.policy = &newer
			if err := syncSess(b); err != nil {
				t.Fatal(err)
			}
		}
		if err := restoreSess(b); err != nil {
			t.Fatal(err)
		}
		wantCalls(t, c.calls, []string{"get:sess-x", "egress-get:sess-x", "egress-get:sess-x", "egress-update:sess-x", "resume:sess-x"})
		wantPolicy(t, c.policy(t, "sess-x"), newer, 2)
	})
	t.Run("Sync between the resume's read and its write", func(t *testing.T) {
		c := newSuspendedEgressControl()
		c.setPolicy("sess-x", otherPolicy)
		f := staticPolicy()
		b := newEgressBackend(c, f.option())
		c.beforeUpdate = func(string) {
			c.beforeUpdate = nil
			f.policy = &newer
			if err := syncSess(b); err != nil {
				t.Fatal(err)
			}
		}
		if err := restoreSess(b); err != nil {
			t.Fatal(err)
		}
		wantCalls(t, c.calls, []string{"get:sess-x", "egress-get:sess-x", "egress-update:sess-x",
			"egress-get:sess-x", "egress-update:sess-x", // the Sync
			"egress-get:sess-x", "resume:sess-x"})
		wantPolicy(t, c.policy(t, "sess-x"), newer, 2)
	})
	t.Run("Sync between CreateActor and the fresh policy", func(t *testing.T) {
		c := newEgressControl()
		f := staticPolicy()
		b := newEgressBackend(c, f.option())
		c.beforeCreate = func(string) {
			c.beforeCreate = nil
			f.policy = &newer
			if err := syncSess(b); err != nil {
				t.Fatal(err)
			}
		}
		if err := createSess(b); err != nil {
			t.Fatal(err)
		}
		wantCalls(t, c.calls, []string{"get:sess-x", "create:sess-x", "egress:sess-x",
			"egress-get:sess-x", "egress:sess-x", // the Sync
			"egress-get:sess-x", "resume:sess-x"})
		wantPolicy(t, c.policy(t, "sess-x"), newer, 1)
	})
}

// staleConfig is a WithEgressPolicy function whose configured policy changes from old to new during
// its nth call. That call runs during after the change (a Sync, or a Create, racing the caller) and
// still returns old: an answer evaluated before the change and written late. Later calls, the ones
// during makes included, return new.
type staleConfig struct {
	old, new *substrate.EgressPolicy
	n, calls int
	during   func()
}

func (f *staleConfig) option() substrate.Option {
	return substrate.WithEgressPolicy(func(context.Context, *api.SessionSpec) (*substrate.EgressPolicy, error) {
		f.calls++
		if f.calls < f.n {
			return f.old, nil
		}
		if f.calls > f.n {
			return f.new, nil
		}
		f.during()
		return f.old, nil
	})
}

// A fresh actor whose function returns no policy gets an empty one, created without a read like any
// other: a policy another writer created on it after CreateActor (a stale Sync, or another host)
// is then a conflict, and the next attempt reads and clears it, instead of resuming the actor on
// it. A fork's clone is refused and torn down instead (TestForkCloneRefusesAPolicyCreatedFirst).
func TestFreshNilEgressPolicyClearsAnotherWritersPolicy(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		c := newEgressControl()
		f := &policyFunc{}
		f.onCall = func(f *policyFunc) {
			if len(f.seen) == 2 { // after CreateActor
				c.setPolicy("sess-x", testPolicy)
			}
		}
		if err := createSess(newEgressBackend(c, f.option())); err != nil {
			t.Fatal(err)
		}
		wantPolicy(t, c.policy(t, "sess-x"), substrate.EgressPolicy{}, 2)
		wantCalls(t, c.calls, []string{"get:sess-x", "create:sess-x", "egress:sess-x", "egress-get:sess-x", "egress-update:sess-x", "resume:sess-x"})
	})
	t.Run("fork clone", func(t *testing.T) {
		c := newEgressCloner()
		f := &policyFunc{}
		f.onCall = func(f *policyFunc) {
			if len(f.seen) == 2 { // after the clone
				c.setPolicy("child", testPolicy)
			}
		}
		if err := forkMemory(newMemoryBackend(c, f.option()), context.Background()); !errors.Is(err, substrate.ErrActorBusy) {
			t.Fatalf("err=%v want ErrActorBusy", err)
		}
		wantCalls(t, c.calls, []string{"get:parent", "tag:parent->fork-child", "get:parent", "clone:child:from=fork-child",
			"egress:child", "get:child", "suspend:child", "delete:child", "untag:fork-child"})
	})
}

// A Sync that read before the actor existed, and asked the function before the configuration
// changed to no policy, does not leave its stale policy on the actor a Create made in between: the
// fresh actor's empty policy makes the Sync's create conflict, and its next attempt reads and asks
// the function again.
func TestStaleSyncCreateAfterAFreshNilActor(t *testing.T) {
	c := newEgressControl()
	f := staticPolicy()
	b := newEgressBackend(c, f.option())
	c.beforeCreate = func(string) {
		c.beforeCreate = nil
		f.policy = nil
		if err := createSess(b); err != nil {
			t.Fatal(err)
		}
	}
	if err := syncSess(b); err != nil {
		t.Fatal(err)
	}
	wantPolicy(t, c.policy(t, "sess-x"), substrate.EgressPolicy{}, 2)
	wantCalls(t, c.calls, []string{"egress-get:sess-x", "egress:sess-x",
		"get:sess-x", "create:sess-x", "egress:sess-x", "resume:sess-x", // the Create
		"egress-get:sess-x", "egress-update:sess-x"})
}

// Every Sync writes, even when the stored policy already matches, so the version it writes fences
// a converge that read before it: a stale answer that converge got from the function, before the
// change Sync applied, conflicts and is asked again, instead of undoing the Sync.
func TestSyncFencesAStaleConverge(t *testing.T) {
	for _, tc := range []struct {
		name          string
		stored        *substrate.EgressPolicy
		stale, config *substrate.EgressPolicy
		calls         []string
		want          substrate.EgressPolicy
		version       int64
	}{
		{name: "none stored, none configured", stale: &testPolicy,
			calls: []string{"egress-get:sess-x", "egress-get:sess-x", "egress:sess-x", "egress:sess-x", "egress-get:sess-x"},
			want:  substrate.EgressPolicy{}, version: 1},
		{name: "equal policy", stored: &testPolicy, stale: &otherPolicy, config: &testPolicy,
			calls: []string{"egress-get:sess-x", "egress-get:sess-x", "egress-update:sess-x", "egress-update:sess-x", "egress-get:sess-x"},
			want:  testPolicy, version: 2},
		{name: "empty stored, none configured", stored: &substrate.EgressPolicy{}, stale: &otherPolicy,
			calls: []string{"egress-get:sess-x", "egress-get:sess-x", "egress-update:sess-x", "egress-update:sess-x", "egress-get:sess-x"},
			want:  substrate.EgressPolicy{}, version: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newSuspendedEgressControl()
			if tc.stored != nil {
				c.setPolicy("sess-x", *tc.stored)
			}
			var b *substrate.Backend
			f := &staleConfig{old: tc.stale, new: tc.config, n: 1, during: func() {
				if err := syncSess(b); err != nil {
					t.Fatal(err)
				}
			}}
			b = newEgressBackend(c, f.option())
			if err := restoreSess(b); err != nil {
				t.Fatal(err)
			}
			wantPolicy(t, c.policy(t, "sess-x"), tc.want, tc.version)
			wantCalls(t, c.calls, append(append([]string{"get:sess-x"}, tc.calls...), "resume:sess-x"))
		})
	}
	t.Run("fresh create with a stale policy", func(t *testing.T) {
		c := newEgressControl()
		var b *substrate.Backend
		// Call 1 is the check before CreateActor, call 2 the fresh converge's.
		f := &staleConfig{old: &testPolicy, n: 2, during: func() {
			if err := syncSess(b); err != nil {
				t.Fatal(err)
			}
		}}
		b = newEgressBackend(c, f.option())
		if err := createSess(b); err != nil {
			t.Fatal(err)
		}
		wantPolicy(t, c.policy(t, "sess-x"), substrate.EgressPolicy{}, 1)
		wantCalls(t, c.calls, []string{"get:sess-x", "create:sess-x",
			"egress-get:sess-x", "egress:sess-x", // the Sync
			"egress:sess-x", "egress-get:sess-x", "resume:sess-x"})
	})
}
