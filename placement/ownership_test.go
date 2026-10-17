package placement

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/harness/echoagent"
)

// These doubles expose the allocation boundary; journal operations use the real in-memory store.
type allocationLog struct {
	eventlog.Store
	trace                                *[]string
	fail                                 string
	cause                                error
	backend, operation, confirmedAttempt string
	confirmed                            api.Incarnation
}

func (l *allocationLog) BeginAllocation(_ context.Context, backend, operation string) (string, error) {
	*l.trace = append(*l.trace, "begin")
	l.backend, l.operation = backend, operation
	if l.fail == "begin" {
		return "", l.cause
	}
	return "attempt-1", nil
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
}

func (b *allocationBackend) Describe(ctx context.Context) (api.Descriptor, error) {
	b.describes++
	return (echoagent.Harness{}).Describe(ctx)
}
func (b *allocationBackend) Capabilities() api.RuntimeCapabilities { return api.RuntimeCapabilities{} }
func (b *allocationBackend) allocate(uid, operation string) (api.Incarnation, error) {
	*b.trace = append(*b.trace, operation)
	return api.Incarnation{ID: "resource-" + uid, Worker: "worker", Address: "shared:1234", Runtime: "test"}, b.cause
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

type safeAllocationBackend struct {
	*allocationBackend
	key        string
	released   []string
	releaseErr error
}

func (b *safeAllocationBackend) CleanupKey() string { return b.key }
func (b *safeAllocationBackend) ReleaseSession(_ context.Context, uid string) error {
	b.released = append(b.released, uid)
	return b.releaseErr
}

func TestAllocationWriteAheadAndAcknowledgement(t *testing.T) {
	for _, operation := range []string{"create", "restore", "fork"} {
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
				case "fork":
					err = p.Fork(t.Context(), eventlog.AsStore(eventlog.New()), "parent", []ForkChild{{UID: "s", Log: log}}, 0)
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
					if operation == "fork" {
						wantTrace = append(wantTrace, "fence")
						wantErr = nil
					} else {
						wantTrace = append(wantTrace, "dial")
						wantErr = nextEffect
					}
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

func TestTrackedForkFailureRetainsEveryAttemptWithoutGenericStop(t *testing.T) {
	for _, failure := range []string{"backend", "confirm", "copy"} {
		t.Run(failure, func(t *testing.T) {
			var trace []string
			cause := errors.New("second child failed")
			b := &allocationBackend{trace: &trace}
			if failure == "backend" {
				b.cause, b.failFork = cause, 2
			}
			first := &allocationLog{Store: eventlog.AsStore(eventlog.New()), trace: &trace}
			second := &allocationLog{Store: eventlog.AsStore(eventlog.New()), trace: &trace, cause: cause}
			if failure == "confirm" {
				second.fail = "confirm"
			}
			if failure == "copy" {
				second.Store = &allocationCopyFailure{Store: second.Store, cause: cause}
			}
			p := New(b, echoagent.Model)
			err := p.Fork(t.Context(), eventlog.AsStore(eventlog.New()), "parent", []ForkChild{{UID: "one", Log: first}, {UID: "two", Log: second}}, 0)
			if !errors.Is(err, cause) {
				t.Fatalf("fork = %v, want original cause", err)
			}
			if first.operation != "fork" || second.operation != "fork" || b.forks != 2 {
				t.Errorf("lost attempted child: first=%q second=%q forks=%d", first.operation, second.operation, b.forks)
			}
			if b.stops != 0 {
				t.Errorf("durable abort called unsupported generic Stop %d times", b.stops)
			}
		})
	}
}

type allocationCopyFailure struct {
	eventlog.Store
	cause error
}

func (l *allocationCopyFailure) NewFence() (int64, error) { return 0, l.cause }

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

type preparingPlacementLog struct {
	eventlog.Store
	reads, fences int
}

func (*preparingPlacementLog) CheckAdmissible() error {
	return fmt.Errorf("PREPARING: %w", eventlog.ErrUnpublished)
}
func (*preparingPlacementLog) CheckWritable() error { return nil }
func (l *preparingPlacementLog) Read(seq int64) ([]eventlog.Record, error) {
	l.reads++
	return l.Store.Read(seq)
}
func (l *preparingPlacementLog) NewFence() (int64, error) { l.fences++; return l.Store.NewFence() }

func TestPreparingPlacementAdmissionHasNoEffects(t *testing.T) {
	for _, operation := range []string{"exec", "resume", "suspend", "fork", "registry-resume", "registry-suspend", "registry-approve"} {
		t.Run(operation, func(t *testing.T) {
			p, b := newDeletionPlacer(t, true)
			r, err := NewRegistry("default", map[string]*Placer{"default": p})
			if err != nil {
				t.Fatal(err)
			}
			log := &preparingPlacementLog{Store: eventlog.AsStore(eventlog.New())}
			switch operation {
			case "exec":
				_, err = p.Exec(t.Context(), log, "preparing", nil, 0)
			case "resume":
				err = p.Resume(t.Context(), log, "preparing")
			case "suspend":
				_, err = p.Suspend(t.Context(), log, "preparing")
			case "fork":
				err = p.Fork(t.Context(), log, "preparing", nil, 0)
			case "registry-resume":
				err = r.Resume(t.Context(), log, "preparing", "missing")
			case "registry-suspend":
				_, err = r.Suspend(t.Context(), log, "preparing", "missing")
			case "registry-approve":
				_, err = r.Approve(t.Context(), log, "preparing", api.ApprovalDecision{})
			}
			if !errors.Is(err, eventlog.ErrUnpublished) {
				t.Errorf("admission = %v, want ErrUnpublished", err)
			}
			if len(b.calls) != 0 || log.reads != 0 || log.fences != 0 {
				t.Errorf("preparing admission had effects: runtime=%v reads=%d fences=%d", b.calls, log.reads, log.fences)
			}
		})
	}
}

func TestPreparingForkChildAcceptsTrustedCopy(t *testing.T) {
	var trace []string
	log := &preparingPlacementLog{Store: eventlog.AsStore(eventlog.New())}
	p := New(&allocationBackend{trace: &trace}, nil)
	if err := p.Fork(t.Context(), eventlog.AsStore(eventlog.New()), "parent", []ForkChild{{UID: "preparing", Log: log}}, 0); err != nil {
		t.Fatalf("trusted PREPARING copy: %v", err)
	}
	if log.fences != 1 {
		t.Fatalf("child copy did not mint its fence: %d", log.fences)
	}
}
