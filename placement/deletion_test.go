package placement

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/runtime/local"
	"github.com/agent-primitives/sessions/sqlitelog"
)

type deletionBackend struct {
	*local.Backend
	memory  bool
	calls   map[string]int
	failure error
}

func (b *deletionBackend) count(name string) { b.calls[name]++ }
func (b *deletionBackend) Describe(ctx context.Context) (api.Descriptor, error) {
	b.count("Describe")
	if b.failure != nil {
		return api.Descriptor{}, b.failure
	}
	d, err := b.Backend.Describe(ctx)
	if b.memory {
		d.Capabilities.Resumability = api.ResumabilityRequiresMemorySnapshot
	}
	return d, err
}
func (b *deletionBackend) Capabilities() api.RuntimeCapabilities {
	return api.RuntimeCapabilities{MemorySnapshot: b.memory}
}
func (b *deletionBackend) Create(ctx context.Context, s *api.SessionSpec) (api.Incarnation, error) {
	b.count("Create")
	return b.Backend.Create(ctx, s)
}
func (b *deletionBackend) Restore(ctx context.Context, r api.SnapshotRef) (api.Incarnation, error) {
	b.count("Restore")
	return b.Backend.Restore(ctx, r)
}
func (b *deletionBackend) Snapshot(_ context.Context, i api.Incarnation, _ api.SnapshotKind) (api.SnapshotRef, error) {
	b.count("Snapshot")
	return api.SnapshotRef{Local: i.ID, ExternalURI: "snapshot://" + i.ID, Memory: b.memory}, nil
}
func (b *deletionBackend) Fork(ctx context.Context, r api.SnapshotRef, o api.ForkOpts) (api.Incarnation, error) {
	b.count("Fork")
	return b.Backend.Fork(ctx, r, o)
}
func (b *deletionBackend) Stop(ctx context.Context, i api.Incarnation) error {
	b.count("Stop")
	return b.Backend.Stop(ctx, i)
}
func newDeletionPlacer(t *testing.T, memory bool) (*Placer, *deletionBackend) {
	t.Helper()
	b := &deletionBackend{Backend: local.New(echoagent.Harness{}), memory: memory, calls: map[string]int{}}
	t.Cleanup(func() { _ = b.Close() })
	p := New(b, func(ctx context.Context, r api.ModelRequest) (api.ModelResponse, error) {
		b.count("model")
		return echoagent.Model(ctx, r)
	}, WithDialer(func(api.Incarnation) (api.Harness, func() error, error) {
		b.count("dial")
		return echoagent.Harness{}, func() error { return nil }, nil
	}))
	return p, b
}
func deletionStore(t *testing.T) (*sqlitelog.Store, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal.db")
	s, err := sqlitelog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return s, db
}
func rawDeletionState(t *testing.T, db *sql.DB) [][]any {
	t.Helper()
	var out [][]any
	for _, q := range []string{"SELECT * FROM sessions ORDER BY session", "SELECT * FROM events ORDER BY session,seq"} {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			v := make([]any, len(cols))
			p := make([]any, len(cols))
			for i := range v {
				p[i] = &v[i]
			}
			if err := rows.Scan(p...); err != nil {
				t.Fatal(err)
			}
			out = append(out, v)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// All static and runtime-added entries must refuse before admission/provisioning/recovery routing.
func TestDeletedPlacementAdmissionNoEffects(t *testing.T) {
	s, db := deletionStore(t)
	if err := s.PutSession(sqlitelog.SessionMeta{UID: "deleted", Harness: "unserved"}); err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	log := s.Session("deleted")
	f, err := log.NewFence()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(0, f, api.Event{Kind: api.EventExecutionStart, ExecutionID: "pending", ExecutionStart: &api.ExecutionStart{Harness: "unserved", InputCount: &zero}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteSession(t.Context(), "deleted"); err != nil {
		t.Fatal(err)
	}
	p1, b1 := newDeletionPlacer(t, false)
	p2, b2 := newDeletionPlacer(t, false)
	p3, b3 := newDeletionPlacer(t, false)
	r, err := NewRegistry("static", map[string]*Placer{"static": p1})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Add("added1", p2); err != nil {
		t.Fatal(err)
	}
	if err := r.Add("added2", p3); err != nil {
		t.Fatal(err)
	}
	before := rawDeletionState(t, db)
	for _, p := range []*Placer{p1, p2, p3} {
		for _, op := range []string{"Exec", "Suspend", "Resume", "Fork"} {
			t.Run(op, func(t *testing.T) {
				var err error
				switch op {
				case "Exec":
					_, err = p.Exec(t.Context(), log, "deleted", nil, 0)
				case "Suspend":
					_, err = p.Suspend(t.Context(), log, "deleted")
				case "Resume":
					err = p.Resume(t.Context(), log, "deleted")
				case "Fork":
					err = p.Fork(t.Context(), log, "deleted", []ForkChild{{UID: "fresh", Log: s.Session("fresh")}}, 1)
				}
				if !errors.Is(err, eventlog.ErrDeleted) {
					t.Errorf("%s=%v, want ErrDeleted", op, err)
				}
				if n := guardEntries(p); n != 0 {
					t.Errorf("guard retained %d entries", n)
				}
			})
		}
	}
	checkCalls := 0
	if err := r.Resume(t.Context(), log, "deleted", "unserved", WithResolvedHarnessCheck(func(string) error { checkCalls++; return nil })); !errors.Is(err, eventlog.ErrDeleted) {
		t.Errorf("Registry.Resume=%v, want ErrDeleted before route", err)
	}
	if checkCalls != 0 {
		t.Error("deleted registry called resolved harness check")
	}
	for _, b := range []*deletionBackend{b1, b2, b3} {
		if len(b.calls) != 0 {
			t.Errorf("deleted entry reached runtime/model/dial: %v", b.calls)
		}
	}
	if !reflect.DeepEqual(before, rawDeletionState(t, db)) {
		t.Error("deleted admission mutated raw state")
	}
}

// A later deleted child must not checkpoint the parent, provision earlier children, or enter rollback.
func TestForkChecksEveryChildBeforeEffects(t *testing.T) {
	for _, memory := range []bool{false, true} {
		for _, index := range []int{0, 1} {
			t.Run(map[bool]string{false: "stateless", true: "memory"}[memory]+"/"+map[int]string{0: "first", 1: "later"}[index], func(t *testing.T) {
				s, db := deletionStore(t)
				if err := s.PutSession(sqlitelog.SessionMeta{UID: "parent"}); err != nil {
					t.Fatal(err)
				}
				children := []ForkChild{{UID: "child1", Log: s.Session("child1")}, {UID: "child2", Log: s.Session("child2")}}
				if err := s.PutSession(sqlitelog.SessionMeta{UID: children[index].UID}); err != nil {
					t.Fatal(err)
				}
				if _, err := s.DeleteSession(t.Context(), children[index].UID); err != nil {
					t.Fatal(err)
				}
				p, b := newDeletionPlacer(t, memory)
				before := rawDeletionState(t, db)
				if err := p.Fork(t.Context(), s.Session("parent"), "parent", children, 0); !errors.Is(err, eventlog.ErrDeleted) {
					t.Errorf("Fork=%v, want ErrDeleted", err)
				}
				if len(b.calls) != 0 {
					t.Errorf("preflight failure effects (including rollback Stop): %v", b.calls)
				}
				if !reflect.DeepEqual(before, rawDeletionState(t, db)) {
					t.Error("child preflight changed parent/child events/fence/time")
				}
				if n := guardEntries(p); n != 0 {
					t.Errorf("guard retained %d entries", n)
				}
			})
		}
	}
}
func TestForkUnknownFreshChildrenAndNilFanout(t *testing.T) {
	for _, memory := range []bool{false, true} {
		for _, empty := range []bool{false, true} {
			t.Run(map[bool]string{false: "stateless", true: "memory"}[memory]+"/"+map[bool]string{false: "children", true: "nil"}[empty], func(t *testing.T) {
				s, _ := deletionStore(t)
				if err := s.PutSession(sqlitelog.SessionMeta{UID: "parent"}); err != nil {
					t.Fatal(err)
				}
				p, b := newDeletionPlacer(t, memory)
				var children []ForkChild
				if !empty {
					children = []ForkChild{{UID: "fresh1", Log: s.Session("fresh1")}, {UID: "fresh2", Log: s.Session("fresh2")}}
				}
				if err := p.Fork(t.Context(), s.Session("parent"), "parent", children, 0); err != nil {
					t.Fatal(err)
				}
				if b.calls["Fork"] != len(children) || b.calls["Stop"] != 0 {
					t.Fatalf("fresh fork effects=%v", b.calls)
				}
				for _, child := range children {
					if err := s.Session(child.UID).Verify(); err != nil {
						t.Fatal(err)
					}
				}
				if n := guardEntries(p); n != 0 {
					t.Errorf("guard retained %d entries", n)
				}
			})
		}
	}
}

type preflightFailureLog struct {
	eventlog.Store
	cause error
	reads int
}

func (s *preflightFailureLog) CheckWritable() error { return s.cause }
func (s *preflightFailureLog) Read(seq int64) ([]eventlog.Record, error) {
	s.reads++
	return s.Store.Read(seq)
}
func TestPlacementWritableFailurePreservesCauseAndHasNoEffects(t *testing.T) {
	for _, op := range []string{"Exec", "Suspend", "Resume", "Registry.Resume", "Fork-parent", "Fork-child"} {
		t.Run(op, func(t *testing.T) {
			s, db := deletionStore(t)
			p, b := newDeletionPlacer(t, true)
			r, err := NewRegistry("default", map[string]*Placer{"default": p})
			if err != nil {
				t.Fatal(err)
			}
			cause := errors.New("read storage failed")
			bad := &preflightFailureLog{Store: s.Session("bad"), cause: cause}
			good := s.Session("parent")
			before := rawDeletionState(t, db)
			switch op {
			case "Exec":
				_, err = p.Exec(t.Context(), bad, "bad", nil, 0)
			case "Suspend":
				_, err = p.Suspend(t.Context(), bad, "bad")
			case "Resume":
				err = p.Resume(t.Context(), bad, "bad")
			case "Registry.Resume":
				err = r.Resume(t.Context(), bad, "bad", "missing")
			case "Fork-parent":
				err = p.Fork(t.Context(), bad, "bad", []ForkChild{{UID: "fresh", Log: s.Session("fresh")}}, 0)
			case "Fork-child":
				err = p.Fork(t.Context(), good, "parent", []ForkChild{{UID: "bad", Log: bad}}, 0)
			}
			if !errors.Is(err, cause) {
				t.Errorf("%s=%v, lost cause", op, err)
			}
			if bad.reads != 0 || len(b.calls) != 0 {
				t.Errorf("read failure had effects: reads=%d backend=%v", bad.reads, b.calls)
			}
			if !reflect.DeepEqual(before, rawDeletionState(t, db)) {
				t.Error("preflight fault mutated state")
			}
			if n := guardEntries(p); n != 0 {
				t.Errorf("guard retained %d entries", n)
			}
		})
	}
}

// The callback shares ALL Placers' guard, is synchronous, skips busy callbacks, and releases failures.
func TestRegistryWithSessionGuard(t *testing.T) {
	p1, p2 := New(nil, nil), New(nil, nil)
	r, err := NewRegistry("static", map[string]*Placer{"static": p1})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Add("added", p2); err != nil {
		t.Fatal(err)
	}
	type guarded interface {
		WithSessionGuard(string, func() error) error
	}
	g, ok := any(r).(guarded)
	if !ok {
		t.Fatal("Registry does not support shared callback admission")
	}
	for _, p := range []*Placer{p1, p2} {
		release, err := p.trySessionLock("uid")
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		if err := g.WithSessionGuard("uid", func() error { calls++; return nil }); !errors.Is(err, ErrSessionBusy) {
			t.Errorf("busy=%v", err)
		}
		if calls != 0 || guardEntries(p) != 1 {
			t.Error("busy callback ran or holder was released")
		}
		release()
	}
	fault := errors.New("callback storage fault")
	for _, want := range []error{nil, fault} {
		calls := 0
		err := g.WithSessionGuard("uid", func() error {
			calls++
			if _, err := p2.trySessionLock("uid"); !errors.Is(err, ErrSessionBusy) {
				t.Errorf("callback not guarded: %v", err)
			}
			return want
		})
		if !errors.Is(err, want) || calls != 1 {
			t.Errorf("callback=%v calls=%d", err, calls)
		}
		if guardEntries(p1) != 0 {
			t.Error("callback leaked guard")
		}
	}
	// A failing callback cannot strand the uid; an unrelated operation is never excluded.
	var once sync.Once
	if err := g.WithSessionGuard("uid", func() error {
		once.Do(func() {
			release, err := p1.trySessionLock("other")
			if err != nil {
				t.Error(err)
			} else {
				release()
			}
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
