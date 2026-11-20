package placement

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/sqlitelog"
)

// These doubles expose the allocation boundary; journal operations use the real in-memory store.
type allocationLog struct {
	eventlog.Store
	trace                                *[]string
	fail                                 string
	cause                                error
	backend, operation, confirmedAttempt string
	confirmed                            api.Incarnation
	resources                            []api.CleanupResource
	unpublished                          bool
}

func (l *allocationLog) BeginAllocation(_ context.Context, backend, operation string, resources ...api.CleanupResource) (string, error) {
	*l.trace = append(*l.trace, "begin")
	l.backend, l.operation, l.resources = backend, operation, append([]api.CleanupResource(nil), resources...)
	if l.fail == "begin" {
		return "", l.cause
	}
	return "attempt-1", nil
}
func (l *allocationLog) BeginUnpublishedAllocation(ctx context.Context, backend, operation string, resources ...api.CleanupResource) (string, error) {
	l.unpublished = true
	return l.BeginAllocation(ctx, backend, operation, resources...)
}
func (l *allocationLog) ConfirmAllocation(_ context.Context, attempt string, in api.Incarnation) error {
	*l.trace = append(*l.trace, "confirm")
	l.confirmedAttempt, l.confirmed = attempt, in
	if l.fail == "confirm" {
		return l.cause
	}
	return nil
}
func (l *allocationLog) NewFence() (int64, error) {
	*l.trace = append(*l.trace, "fence")
	return l.Store.NewFence()
}

// This backend deliberately has no cleanup opt-in. Normal placement must still run, with an
// empty (unreleasable) ledger key, and tracked abort must never use its generic Stop.
type allocationBackend struct {
	Backend
	trace     *[]string
	cause     error
	stops     int
	describes int
	forks     int
	failFork  int
	// address overrides the incarnation's address; live opts in to LiveDescriber.
	address string
	live    bool
	// computeInstance is the Incarnation.ComputeInstance every allocation returns.
	computeInstance string
}

func (b *allocationBackend) Describe(ctx context.Context) (api.Descriptor, error) {
	b.describes++
	return (echoagent.Harness{}).Describe(ctx)
}
func (b *allocationBackend) Capabilities() api.RuntimeCapabilities { return api.RuntimeCapabilities{} }
func (b *allocationBackend) DescribesLiveHarness() bool            { return b.live }
func (b *allocationBackend) allocate(uid, operation string) (api.Incarnation, error) {
	*b.trace = append(*b.trace, operation)
	address := "shared:1234"
	if b.address != "" {
		address = b.address
	}
	in := api.Incarnation{ID: "resource-" + uid, Worker: "worker", Address: address, Runtime: "test",
		ComputeInstance: b.computeInstance}
	return in, b.cause
}
func (b *allocationBackend) Create(_ context.Context, spec *api.SessionSpec) (api.Incarnation, error) {
	return b.allocate(spec.SessionUID, "create")
}
func (b *allocationBackend) Restore(_ context.Context, ref api.SnapshotRef) (api.Incarnation, error) {
	return b.allocate(ref.Local, "restore")
}
func (b *allocationBackend) Fork(_ context.Context, _ api.SnapshotRef, opts api.ForkOpts) (api.Incarnation, error) {
	b.forks++
	in, err := b.allocate(opts.ChildSessionUID, "fork")
	if b.failFork != 0 && b.forks != b.failFork {
		err = nil
	}
	return in, err
}
func (b *allocationBackend) Stop(context.Context, api.Incarnation) error { b.stops++; return nil }

type ownedAllocationBackend struct {
	*allocationBackend
	key       string
	planCalls int
	plans     map[api.CleanupOperation][]api.CleanupResource
	released  []api.CleanupResource
	observed  []api.CleanupResource
	forkErr   error
}

func (b *ownedAllocationBackend) CleanupKey() string { return b.key }
func (b *ownedAllocationBackend) CleanupResources(op api.CleanupOperation, _ string) []api.CleanupResource {
	b.planCalls++
	return append([]api.CleanupResource(nil), b.plans[op]...)
}
func (b *ownedAllocationBackend) ReleaseResource(_ context.Context, resource api.CleanupResource) error {
	b.released = append(b.released, resource)
	return nil
}
func (b *ownedAllocationBackend) Fork(_ context.Context, _ api.SnapshotRef, opts api.ForkOpts) (api.Incarnation, error) {
	b.forks++
	inc, _ := b.allocate(opts.ChildSessionUID, "fork")
	if len(b.observed) > 0 {
		evidence, err := api.NewCleanupEvidence(b.observed)
		if err != nil {
			return inc, err
		}
		inc.Cleanup = evidence
	}
	return inc, b.forkErr
}

type safeAllocationBackend struct {
	*allocationBackend
	key        string
	released   []string
	releaseErr error
	// releaseCtxErrs records each release context's error when ReleaseSession was called.
	releaseCtxErrs []error
}

func (b *safeAllocationBackend) CleanupKey() string { return b.key }
func (b *safeAllocationBackend) ReleaseSession(ctx context.Context, uid string) error {
	b.released = append(b.released, uid)
	b.releaseCtxErrs = append(b.releaseCtxErrs, ctx.Err())
	return b.releaseErr
}

func TestAllocationWriteAheadAndAcknowledgement(t *testing.T) {
	for _, operation := range []string{"create", "restore"} {
		for _, failure := range []string{"begin", "backend", "confirm", "none"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				var trace []string
				cause := errors.New("injected boundary failure")
				log := &allocationLog{Store: eventlog.AsStore(eventlog.New()), trace: &trace, fail: failure, cause: cause}
				backend := &allocationBackend{trace: &trace}
				if failure == "backend" {
					backend.cause = cause
				}
				nextEffect := errors.New("dial reached")
				p := New(backend, echoagent.Model, WithDialer(func(api.Incarnation) (api.Harness, func() error, error) {
					trace = append(trace, "dial")
					return nil, nil, nextEffect
				}))
				var err error
				switch operation {
				case "create":
					_, err = p.Exec(t.Context(), log, "s", nil, 0)
				case "restore":
					err = p.Resume(t.Context(), log, "s")
				}
				wantTrace := []string{"begin"}
				if failure != "begin" {
					wantTrace = append(wantTrace, operation)
				}
				if failure == "confirm" || failure == "none" {
					wantTrace = append(wantTrace, "confirm")
				}
				wantErr := cause
				if failure == "none" {
					wantTrace = append(wantTrace, "dial")
					wantErr = nextEffect
				}
				if !errors.Is(err, wantErr) {
					t.Errorf("error = %v, want cause %v", err, wantErr)
				}
				if !reflect.DeepEqual(trace, wantTrace) {
					t.Errorf("effects = %v, want %v", trace, wantTrace)
				}
				if log.backend != "" || log.operation != operation {
					t.Errorf("ownership = (%q,%q), want unsupported key and %q", log.backend, log.operation, operation)
				}
				if failure == "confirm" || failure == "none" {
					want := api.Incarnation{ID: "resource-s", Worker: "worker", Address: "shared:1234", Runtime: "test"}
					if log.confirmedAttempt != "attempt-1" || !reflect.DeepEqual(log.confirmed, want) {
						t.Errorf("ack = %q %+v, want exact attempt and complete incarnation", log.confirmedAttempt, log.confirmed)
					}
				}
				if backend.stops != 0 {
					t.Errorf("tracked allocation called generic Stop %d times", backend.stops)
				}
			})
		}
	}
}

func TestAllocationResourcesRecordedOnBackendError(t *testing.T) {
	var trace []string
	plan := []api.CleanupResource{{Reference: "opaque:actor:s", Borrower: ""}}
	log := &allocationLog{Store: eventlog.AsStore(eventlog.New()), trace: &trace}
	backendErr := errors.New("backend failed after effect")
	backend := &ownedAllocationBackend{
		allocationBackend: &allocationBackend{trace: &trace},
		key:               "control-plane-A",
		plans:             map[api.CleanupOperation][]api.CleanupResource{api.CleanupCreate: plan},
	}
	owned, err := api.NewCleanupEvidence([]api.CleanupResource{{Reference: plan[0].Reference, UID: "actor-uid"}})
	if err != nil {
		t.Fatal(err)
	}
	p := New(backend, nil)
	_, err = p.allocate(t.Context(), log, "s", "create", func() (api.Incarnation, error) {
		trace = append(trace, "effect")
		return api.Incarnation{ID: "s", Cleanup: owned}, backendErr
	})
	if !errors.Is(err, backendErr) {
		t.Fatalf("error=%v, want backend error", err)
	}
	if !reflect.DeepEqual(log.resources, plan) || log.backend != "control-plane-A" {
		t.Fatalf("recorded plan=%+v backend=%q", log.resources, log.backend)
	}
	if log.confirmedAttempt != "attempt-1" || !reflect.DeepEqual(log.confirmed.Cleanup.Resources(), owned.Resources()) {
		t.Fatalf("confirmed attempt=%q cleanup=%+v", log.confirmedAttempt, log.confirmed.Cleanup.Resources())
	}
	if want := []string{"begin", "effect", "confirm"}; !reflect.DeepEqual(trace, want) {
		t.Fatalf("trace=%v want=%v", trace, want)
	}
}

func TestAllocationResourcesJoinRuntimeAndConfirmationErrors(t *testing.T) {
	var trace []string
	backendErr := errors.New("backend")
	confirmErr := errors.New("confirm")
	plan := []api.CleanupResource{{Reference: "opaque:actor:s"}}
	log := &allocationLog{Store: eventlog.AsStore(eventlog.New()), trace: &trace, fail: "confirm", cause: confirmErr}
	backend := &ownedAllocationBackend{
		allocationBackend: &allocationBackend{trace: &trace},
		key:               "route",
		plans:             map[api.CleanupOperation][]api.CleanupResource{api.CleanupCreate: plan},
	}
	evidence, err := api.NewCleanupEvidence([]api.CleanupResource{{Reference: plan[0].Reference, UID: "uid"}})
	if err != nil {
		t.Fatal(err)
	}
	p := New(backend, nil)
	_, err = p.allocate(t.Context(), log, "s", "create", func() (api.Incarnation, error) {
		trace = append(trace, "effect")
		return api.Incarnation{Cleanup: evidence}, backendErr
	})
	if !errors.Is(err, backendErr) || !errors.Is(err, confirmErr) {
		t.Fatalf("joined error=%v", err)
	}
}

func TestAllocationResourcesEmptyKeyUsesLegacyPath(t *testing.T) {
	var trace []string
	log := &allocationLog{Store: eventlog.AsStore(eventlog.New()), trace: &trace}
	backend := &ownedAllocationBackend{
		allocationBackend: &allocationBackend{trace: &trace},
		plans: map[api.CleanupOperation][]api.CleanupResource{
			api.CleanupCreate: {{Reference: "opaque:actor:s"}},
		},
	}
	p := New(backend, nil)
	if _, err := p.allocate(t.Context(), log, "s", "create", func() (api.Incarnation, error) {
		return api.Incarnation{ID: "legacy-handle"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if backend.planCalls != 0 || len(log.resources) != 0 {
		t.Fatalf("empty key requested owned plan: calls=%d resources=%+v", backend.planCalls, log.resources)
	}
	if log.confirmed.ID != "legacy-handle" {
		t.Fatalf("legacy confirmation=%+v", log.confirmed)
	}
}

func TestForkCleanupRequiresUnpublishedRecorderForOwnedBackend(t *testing.T) {
	var trace []string
	backend := &ownedAllocationBackend{
		allocationBackend: &allocationBackend{trace: &trace},
		key:               "route",
		plans:             map[api.CleanupOperation][]api.CleanupResource{api.CleanupFork: {{Reference: "opaque:actor:child"}}},
	}
	p := New(backend, echoagent.Model)
	parent := eventlog.AsStore(eventlog.New())
	child := eventlog.AsStore(eventlog.New())
	if err := p.Fork(t.Context(), parent, "parent", []ForkChild{{UID: "child", Log: child}}, 0); err == nil {
		t.Fatal("owned Fork accepted a child log without unpublished recorder")
	}
	if backend.forks != 0 {
		t.Fatalf("runtime Fork called %d times", backend.forks)
	}
}

func TestForkCleanupRecordsPartialEvidenceBeforeRollback(t *testing.T) {
	s, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var trace []string
	forkErr := errors.New("child actor response lost")
	plan := []api.CleanupResource{
		{Reference: "opaque:tag:child", Borrower: "opaque:actor:child"},
		{Reference: "opaque:actor:child"},
	}
	backend := &ownedAllocationBackend{
		allocationBackend: &allocationBackend{trace: &trace},
		key:               "route",
		plans:             map[api.CleanupOperation][]api.CleanupResource{api.CleanupFork: plan},
		observed:          []api.CleanupResource{{Reference: "opaque:tag:child", UID: "tag-uid", Borrower: "opaque:actor:child"}},
		forkErr:           forkErr,
	}
	p := New(backend, echoagent.Model)
	child := s.ForkSession(sqlitelog.SessionMeta{UID: "child", ParentUID: "parent"})
	if err := p.Fork(t.Context(), s.Session("parent"), "parent", []ForkChild{{UID: "child", Log: child}}, 0); !errors.Is(err, forkErr) {
		t.Fatalf("fork=%v want %v", err, forkErr)
	}
	if _, err := s.SessionInfo("child"); !errors.Is(err, sqlitelog.ErrSessionNotFound) {
		t.Fatalf("failed fork published child: %v", err)
	}
	allocations, err := s.Allocations(t.Context(), "child")
	if err != nil || len(allocations) != 1 {
		t.Fatalf("allocations=%+v err=%v", allocations, err)
	}
	resources, err := s.AllocationResources(t.Context(), "child", allocations[0].ID)
	if err != nil || len(resources) != 2 {
		t.Fatalf("resources=%+v err=%v", resources, err)
	}
	for _, resource := range resources {
		switch resource.Reference {
		case "opaque:tag:child":
			if !resource.Confirmed || resource.UID != "tag-uid" {
				t.Fatalf("tag evidence=%+v", resource)
			}
		case "opaque:actor:child":
			if resource.Confirmed {
				t.Fatalf("unobserved actor confirmed: %+v", resource)
			}
		}
	}
}

func TestAllocationRecordsSelectedConfigurationNotRegistryName(t *testing.T) {
	var trace []string
	p1 := New(&safeAllocationBackend{allocationBackend: &allocationBackend{trace: &trace}, key: "endpoint-A"}, nil)
	p2 := New(&safeAllocationBackend{allocationBackend: &allocationBackend{trace: &trace}, key: "endpoint-B"}, nil)
	r, err := NewRegistry("default", map[string]*Placer{"default": p1, "override": p2, "alias": p1})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ route, want string }{{"", "endpoint-A"}, {"override", "endpoint-B"}, {"alias", "endpoint-A"}} {
		p, err := r.For(tc.route)
		if err != nil {
			t.Fatal(err)
		}
		log := &allocationLog{Store: eventlog.AsStore(eventlog.New()), trace: &trace}
		dialReached := errors.New("no turn needed")
		p.dial = func(api.Incarnation) (api.Harness, func() error, error) {
			return nil, nil, dialReached
		}
		if _, err := p.Exec(t.Context(), log, "same-session", nil, 0); !errors.Is(err, dialReached) {
			t.Fatalf("route %q failed before allocation completed: %v", tc.route, err)
		}
		if log.backend != tc.want {
			t.Errorf("route %q recorded backend %q, want %q", tc.route, log.backend, tc.want)
		}
	}
}
