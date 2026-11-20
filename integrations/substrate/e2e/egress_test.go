package e2e

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/harness/echoagent"
	ateadapter "github.com/agent-primitives/sessions/integrations/substrate"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/substrate"
)

// e2ePolicy is a valid policy for the actors here. The counter harness makes no egress call, so
// these tests check that substrate stores the policy at each install point, not that it enforces
// it. Every rule names its ports: substrate stores the default port of a rule that leaves them
// unset, so such a rule would not read back equal.
var e2ePolicy = substrate.EgressPolicy{Rules: []substrate.EgressRule{
	{Protocol: substrate.EgressHTTPS, Hostnames: []string{"api.example.com"}, Ports: []int32{443}},
	{Protocol: substrate.EgressTLSPassthrough, Hostnames: []string{"*.example.org"}, Ports: []int32{443}},
}}

// e2eBadPolicy is refused by substrate's validation (INVALID_ARGUMENT) when it is created.
var e2eBadPolicy = substrate.EgressPolicy{Rules: []substrate.EgressRule{
	{Protocol: substrate.EgressHTTPS, Hostnames: []string{"not a host!"}},
}}

// egressBackend is the counter backend with WithEgressPolicy, whose function returns policy(uid).
func (f *fixture) egressBackend(t *testing.T, policy func(uid string) *substrate.EgressPolicy) *substrate.Backend {
	t.Helper()
	name := f.ensureTemplate(t, counterTemplateSpec)
	return substrate.New(ateadapter.New(f.conn), f.atespace,
		substrate.ObjectRef{Atespace: f.tmplAtespace, Name: name}, counterDescriptor,
		substrate.WithRouter(f.router),
		substrate.WithEgressPolicy(func(_ context.Context, s *api.SessionSpec) (*substrate.EgressPolicy, error) {
			return policy(s.SessionUID), nil
		}))
}

// requirePolicy fails unless the session's actor holds want, read back through the adapter.
func (f *fixture) requirePolicy(ctx context.Context, t *testing.T, uid string, want substrate.EgressPolicy) substrate.EgressPolicyInfo {
	t.Helper()
	got, err := ateadapter.New(f.conn).GetActorEgressPolicy(ctx, substrate.ActorRef{Atespace: f.atespace, Name: uid})
	if err != nil {
		t.Fatalf("actor %s egress policy: %v", uid, err)
	}
	if !reflect.DeepEqual(got.Policy, want) {
		t.Fatalf("actor %s egress policy = %+v, want %+v", uid, got.Policy, want)
	}
	return got
}

// TestEgressPolicyAtEveryInstallPoint runs a REQUIRES_MEMORY_SNAPSHOT session through Create,
// suspend and resume, and a fork, with WithEgressPolicy set: the actor holds the policy after its
// first turn, keeps the same stored policy (same UID and version) across suspend and resume, where
// the converge finds it equal and writes nothing, and the fork child gets its own.
func TestEgressPolicyAtEveryInstallPoint(t *testing.T) {
	f := newFixture(t, env("SUBSTRATE_ATESPACE", "e2e-egress"))
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	backend := f.egressBackend(t, func(string) *substrate.EgressPolicy { return &e2ePolicy })
	store := journal(t)
	parent := uniqueUID("egress")
	parentLog := store.Session(parent)
	p := newPlacer(t, backend, echoagent.Model)
	defer stopQuietly(t, backend, parent)

	driveTurns(ctx, t, p, parentLog, parent, 1)
	created := f.requirePolicy(ctx, t, parent, e2ePolicy)

	if _, err := p.Suspend(ctx, parentLog, parent); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if err := p.Resume(ctx, parentLog, parent); err != nil {
		t.Fatalf("resume: %v", err)
	}
	driveTurns(ctx, t, p, parentLog, parent, 1)
	if got, want := lastOutput(t, parentLog), "2"; got != want {
		t.Fatalf("count after resume = %s, want %s", got, want)
	}
	if resumed := f.requirePolicy(ctx, t, parent, e2ePolicy); resumed.UID != created.UID || resumed.Version != created.Version {
		t.Fatalf("policy after resume is %s v%d, want the one created first, %s v%d", resumed.UID, resumed.Version, created.UID, created.Version)
	}

	atSeq, err := parentLog.Head()
	if err != nil {
		t.Fatal(err)
	}
	child := parent + "-child"
	defer stopQuietly(t, backend, child)
	if err := p.Fork(ctx, parentLog, parent, []placement.ForkChild{{UID: child, Log: store.Session(child)}}, atSeq); err != nil {
		t.Fatalf("fork: %v", err)
	}
	driveTurns(ctx, t, p, store.Session(child), child, 1)
	if got, want := lastOutput(t, store.Session(child)), "3"; got != want {
		t.Fatalf("child count = %s, want %s", got, want)
	}
	if fork := f.requirePolicy(ctx, t, child, e2ePolicy); fork.UID == created.UID {
		t.Fatalf("child policy UID %s is the parent's: want its own policy", fork.UID)
	}
}

// TestEgressPolicyChangedAcrossResume changes the configured policy while a session's actor is
// suspended: the resume replaces the stored policy under its version, keeping its UID. Configuring
// none then clears it to an empty rule set at the next resume, which substrate enforces as
// deny-all.
func TestEgressPolicyChangedAcrossResume(t *testing.T) {
	f := newFixture(t, env("SUBSTRATE_ATESPACE", "e2e-egress"))
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	var current atomic.Pointer[substrate.EgressPolicy]
	current.Store(&e2ePolicy)
	backend := f.egressBackend(t, func(string) *substrate.EgressPolicy { return current.Load() })
	session := uniqueUID("egress-changed")
	log := journal(t).Session(session)
	p := newPlacer(t, backend, echoagent.Model)
	defer stopQuietly(t, backend, session)

	driveTurns(ctx, t, p, log, session, 1)
	created := f.requirePolicy(ctx, t, session, e2ePolicy)

	changed := substrate.EgressPolicy{Rules: []substrate.EgressRule{
		{Protocol: substrate.EgressHTTPS, Hostnames: []string{"api.example.com"}, Ports: []int32{8443}},
	}}
	suspendResume := func(want int) {
		t.Helper()
		if _, err := p.Suspend(ctx, log, session); err != nil {
			t.Fatalf("suspend: %v", err)
		}
		if err := p.Resume(ctx, log, session); err != nil {
			t.Fatalf("resume: %v", err)
		}
		driveTurns(ctx, t, p, log, session, 1)
		if got := lastOutput(t, log); got != fmt.Sprint(want) {
			t.Fatalf("count after resume = %s, want %d", got, want)
		}
	}
	current.Store(&changed)
	suspendResume(2)
	updated := f.requirePolicy(ctx, t, session, changed)
	if updated.UID != created.UID || updated.Version <= created.Version {
		t.Fatalf("policy after a changed resume is %s v%d, want %s at a version after %d", updated.UID, updated.Version, created.UID, created.Version)
	}

	current.Store(nil)
	suspendResume(3)
	cleared, err := ateadapter.New(f.conn).GetActorEgressPolicy(ctx, substrate.ActorRef{Atespace: f.atespace, Name: session})
	if err != nil {
		t.Fatalf("egress policy after configuring none: %v (want an empty rule set, not none)", err)
	}
	if len(cleared.Policy.Rules) != 0 || cleared.UID != created.UID {
		t.Fatalf("policy after configuring none = %+v (%s), want no rules under %s", cleared.Policy, cleared.UID, created.UID)
	}
}

// TestEgressPolicyNotConfigured: without WithEgressPolicy the backend creates no policy, so the
// actor has none.
func TestEgressPolicyNotConfigured(t *testing.T) {
	f := newFixture(t, env("SUBSTRATE_ATESPACE", "e2e-egress"))
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	backend := f.backend(t, counterTemplateSpec, counterDescriptor)
	session := uniqueUID("egress-none")
	p := newPlacer(t, backend, echoagent.Model)
	defer stopQuietly(t, backend, session)
	driveTurns(ctx, t, p, journal(t).Session(session), session, 1)

	_, err := ateadapter.New(f.conn).GetActorEgressPolicy(ctx, substrate.ActorRef{Atespace: f.atespace, Name: session})
	if !errors.Is(err, substrate.ErrEgressPolicyNotFound) {
		t.Fatalf("egress policy of an actor created without one: err = %v, want ErrEgressPolicyNotFound", err)
	}
}

// TestEgressPolicyInstallFailure: a policy substrate refuses fails Create with the actor created
// but never resumed, and fails a fork with the child's clone torn down and the parent's policy
// unchanged.
func TestEgressPolicyInstallFailure(t *testing.T) {
	f := newFixture(t, env("SUBSTRATE_ATESPACE", "e2e-egress"))
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	ctl := ateadapter.New(f.conn)

	t.Run("create", func(t *testing.T) {
		backend := f.egressBackend(t, func(string) *substrate.EgressPolicy { return &e2eBadPolicy })
		session := uniqueUID("egress-refused")
		defer stopQuietly(t, backend, session)
		// The retry finds the actor SUSPENDED and installs again before resuming it, which fails
		// the same way.
		for attempt := range 2 {
			if _, err := backend.Create(ctx, &api.SessionSpec{SessionUID: session}); err == nil {
				t.Fatalf("Create %d with a policy substrate refuses succeeded", attempt+1)
			}
			info, err := ctl.GetActor(ctx, substrate.ActorRef{Atespace: f.atespace, Name: session})
			if err != nil {
				t.Fatalf("get actor: %v", err)
			}
			if info.Status != substrate.StatusSuspended {
				t.Fatalf("actor status after refused install %d = %v, want SUSPENDED (never resumed)", attempt+1, info.Status)
			}
		}
	})

	t.Run("fork", func(t *testing.T) {
		parent := uniqueUID("egress-fork-refused")
		child := parent + "-child"
		backend := f.egressBackend(t, func(uid string) *substrate.EgressPolicy {
			if uid == child {
				return &e2eBadPolicy
			}
			return &e2ePolicy
		})
		store := journal(t)
		parentLog := store.Session(parent)
		p := newPlacer(t, backend, echoagent.Model)
		defer stopQuietly(t, backend, parent)
		defer stopQuietly(t, backend, child)

		driveTurns(ctx, t, p, parentLog, parent, 1)
		atSeq, err := parentLog.Head()
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Fork(ctx, parentLog, parent, []placement.ForkChild{{UID: child, Log: store.Session(child)}}, atSeq); err == nil {
			t.Fatal("fork whose child policy substrate refuses succeeded")
		}
		if _, err := ctl.GetActor(ctx, substrate.ActorRef{Atespace: f.atespace, Name: child}); !errors.Is(err, substrate.ErrActorNotFound) {
			t.Fatalf("child actor after a refused install: err = %v, want ErrActorNotFound (clone torn down)", err)
		}
		f.requirePolicy(ctx, t, parent, e2ePolicy)
	})
}
