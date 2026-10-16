package placement

import (
	"context"
	"errors"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/harness/echoagent"
)

type deletionForkFaultBackend struct {
	*deletionBackend
	phase         string
	cause         error
	forks         int
	rollbackCheck func()
}

func (b *deletionForkFaultBackend) Snapshot(ctx context.Context, inc api.Incarnation, kind api.SnapshotKind) (api.SnapshotRef, error) {
	if b.phase == "Snapshot" {
		return api.SnapshotRef{}, b.cause
	}
	return b.deletionBackend.Snapshot(ctx, inc, kind)
}
func (b *deletionForkFaultBackend) Fork(ctx context.Context, ref api.SnapshotRef, opts api.ForkOpts) (api.Incarnation, error) {
	b.forks++
	if (b.phase == "Fork-first" && b.forks == 1) || (b.phase == "Fork-later" && b.forks == 2) {
		return api.Incarnation{}, b.cause
	}
	return b.deletionBackend.Fork(ctx, ref, opts)
}
func (b *deletionForkFaultBackend) Stop(ctx context.Context, inc api.Incarnation) error {
	if b.rollbackCheck != nil {
		b.rollbackCheck()
	}
	return b.deletionBackend.Stop(ctx, inc)
}

type deletionForkFaultLog struct {
	eventlog.Store
	phase string
	cause error
}

func (l deletionForkFaultLog) CheckWritable() error { return eventlog.CheckWritable(l.Store) }
func (l deletionForkFaultLog) NewFence() (int64, error) {
	if l.phase == "NewFence" {
		return 0, l.cause
	}
	return l.Store.NewFence()
}
func (l deletionForkFaultLog) Append(seq, fence int64, event api.Event) (eventlog.Record, error) {
	if l.phase == "Append" {
		return eventlog.Record{}, l.cause
	}
	return l.Store.Append(seq, fence, event)
}

// Releasing shared deletion admission before rollback would permit Delete alongside Stop.
// Parent operations remain available after the checkpoint releases, as required by stream fencing.
// Two Placers share the tested Registry, and every return must drain all admission entries.
func TestForkSharedGuardCoversRollbackAndDrainsEveryReturn(t *testing.T) {
	for _, memory := range []bool{false, true} {
		for _, phase := range []string{"none", "Describe", "Snapshot", "Fork-first", "Fork-later", "NewFence", "Append"} {
			if phase == "Snapshot" && !memory {
				continue
			}
			t.Run(map[bool]string{false: "stateless", true: "memory"}[memory]+"/"+phase, func(t *testing.T) {
				s, _ := deletionStore(t)
				_, base := newDeletionPlacer(t, memory)
				cause := errors.New("fork phase fault")
				b := &deletionForkFaultBackend{deletionBackend: base, phase: phase, cause: cause}
				if phase == "Describe" {
					base.failure = cause
				}
				p := New(b, echoagent.Model)
				other := New(nil, nil)
				r, err := NewRegistry("fork", map[string]*Placer{"fork": p})
				if err != nil {
					t.Fatal(err)
				}
				if err := r.Add("added", other); err != nil {
					t.Fatal(err)
				}
				rollbacks := 0
				b.rollbackCheck = func() {
					rollbacks++
					called := false
					if err := r.WithSessionGuard("parent", func() error { called = true; return nil }); !errors.Is(err, ErrSessionBusy) || called {
						t.Errorf("rollback did not retain parent Delete guard: %v callback=%v", err, called)
					}
					release, err := other.trySessionLock("parent")
					if err != nil {
						t.Errorf("rollback refused added Placer parent operation: %v", err)
					} else {
						release()
					}
				}
				children := []ForkChild{{UID: "one", Log: s.Session("one")}, {UID: "two", Log: s.Session("two")}}
				if phase == "NewFence" || phase == "Append" {
					children[1].Log = deletionForkFaultLog{Store: children[1].Log, phase: phase, cause: cause}
				}
				err = p.Fork(t.Context(), s.Session("parent"), "parent", children, 0)
				if phase == "none" {
					if err != nil {
						t.Fatal(err)
					}
				} else if !errors.Is(err, cause) {
					t.Fatalf("phase %s=%v, want exact cause", phase, err)
				}
				wantRollback := map[string]int{"Fork-later": 1, "NewFence": 2, "Append": 2}[phase]
				if rollbacks != wantRollback {
					t.Errorf("rollback calls=%d, want %d", rollbacks, wantRollback)
				}
				if n := guardEntries(p); n != 0 {
					t.Fatalf("%s retained %d shared guards", phase, n)
				}
				if err := r.WithSessionGuard("parent", func() error { return nil }); err != nil {
					t.Fatalf("next Delete cannot acquire guard: %v", err)
				}
				if guardEntries(other) != 0 {
					t.Fatal("callback did not drain guard")
				}
			})
		}
	}
}
