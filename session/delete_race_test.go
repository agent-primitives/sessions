package session_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
)

func deleteRaceRegistry(t *testing.T) (*placement.Registry, map[string]*deleteBackend) {
	t.Helper()
	backends := map[string]*deleteBackend{}
	placers := map[string]*placement.Placer{}
	for _, name := range []string{"recorded", "override", "added"} {
		b := &deleteBackend{Backend: local.New(echoagent.Harness{}), calls: map[string]int{}}
		t.Cleanup(func() { _ = b.Close() })
		backends[name] = b
		placers[name] = placement.New(b, func(ctx context.Context, req api.ModelRequest) (api.ModelResponse, error) {
			b.count("model")
			return echoagent.Model(ctx, req)
		})
	}
	r, err := placement.NewRegistry("recorded", map[string]*placement.Placer{"recorded": placers["recorded"], "override": placers["override"]})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Add("added", placers["added"]); err != nil {
		t.Fatal(err)
	}
	return r, backends
}

func deleteRaceMutation(ctx context.Context, c v1.SessionsClient, op, uid, override string) error {
	switch op {
	case "Exec":
		stream, err := c.Exec(ctx, &v1.ExecRequest{Session: uid, Harness: override})
		if err != nil {
			return err
		}
		return drainExec(stream)
	case "Suspend":
		_, err := c.Suspend(ctx, &v1.SuspendRequest{Session: uid})
		return err
	case "Resume":
		_, err := c.Resume(ctx, &v1.ResumeRequest{Session: uid})
		return err
	case "Fork":
		_, err := c.Fork(ctx, &v1.ForkRequest{Session: uid, Count: 2})
		return err
	case "Approve":
		_, err := c.Approve(ctx, publicDecision(uid))
		return err
	default:
		panic("unknown mutation")
	}
}

func resetDeleteBackends(backends map[string]*deleteBackend) {
	for _, b := range backends {
		b.reset()
	}
}

func assertDeleteNoEffects(t *testing.T, backends map[string]*deleteBackend) {
	t.Helper()
	for name, b := range backends {
		if calls := b.effects(); len(calls) != 0 {
			t.Errorf("%s runtime/model effects: %v", name, calls)
		}
	}
}

// Dropping any mutation's shared guard lets Delete commit while that operation is parked
// before its first runtime effect. Both static override and runtime-added routes are exercised.
func TestDeleteLosesToEachInFlightMutation(t *testing.T) {
	for _, route := range []string{"override", "added"} {
		for _, op := range []string{"Exec", "Suspend", "Resume", "Fork"} {
			t.Run(route+"/"+op, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				path := filepath.Join(t.TempDir(), "journal.db")
				store := openStore(t, path)
				db := deletionSQL(t, path)
				r, backends := deleteRaceRegistry(t)
				client := serveRegistry(t, store, r)
				uid := "holder"
				stored, override := route, ""
				if op == "Exec" && route == "override" {
					stored, override = "recorded", route
				}
				if err := store.PutSession(sqlitelog.SessionMeta{UID: uid, Harness: stored}); err != nil {
					t.Fatal(err)
				}
				if op == "Resume" {
					// Resolve an interrupted turn's recorded route, not only the metadata route.
					if route == "override" {
						if err := store.PutSession(sqlitelog.SessionMeta{UID: uid, Harness: "recorded"}); err != nil {
							t.Fatal(err)
						}
					}
					log := store.Session(uid)
					fence, err := log.NewFence()
					if err != nil {
						t.Fatal(err)
					}
					zero := int64(0)
					if _, err := log.Append(0, fence, api.Event{Kind: api.EventExecutionStart, ExecutionID: "interrupted", ExecutionStart: &api.ExecutionStart{Harness: route, InputCount: &zero}}); err != nil {
						t.Fatal(err)
					}
				}
				entered, release := make(chan struct{}), make(chan struct{})
				unblock := sync.OnceFunc(func() { close(release) })
				t.Cleanup(unblock)
				phase := map[string]string{"Exec": "Create", "Suspend": "Snapshot", "Resume": "Restore", "Fork": "Fork"}[op]
				var once sync.Once
				backends[route].hook = func(name string) {
					if name == phase {
						once.Do(func() { close(entered); <-release })
					}
				}
				result := make(chan error, 1)
				go func() { result <- deleteRaceMutation(ctx, client, op, uid, override) }()
				select {
				case <-entered:
				case err := <-result:
					t.Fatalf("holder ended before barrier: %v", err)
				case <-ctx.Done():
					t.Fatal("holder never reached barrier")
				}
				before := sessionDeleteRaw(t, db)
				calls := backends[route].effects()
				for range 2 {
					if _, err := client.DeleteSession(ctx, &v1.DeleteSessionRequest{Uid: uid}); status.Code(err) != codes.Aborted {
						t.Errorf("Delete while %s holds %s = %v, want Aborted", op, route, err)
					}
				}
				if !reflect.DeepEqual(before, sessionDeleteRaw(t, db)) || !reflect.DeepEqual(calls, backends[route].effects()) {
					t.Error("busy Delete mutated journal/projection/fence or contacted backend")
				}
				unblock()
				if err := <-result; err != nil {
					t.Fatalf("Delete disrupted the winning operation: %v", err)
				}
				resetDeleteBackends(backends)
				deleted, err := client.DeleteSession(ctx, &v1.DeleteSessionRequest{Uid: uid})
				if err != nil || deleted.GetDeletionState() != v1.DeletionState_DELETION_PENDING {
					t.Fatalf("guard not released after holder: %v %v", deleted, err)
				}
				assertDeleteNoEffects(t, backends)
			})
		}
	}
}

// A writer lock parks the actual gRPC Delete inside its guarded atomic store callback. A busy
// probe establishes ownership without a timing sleep. Retry only probe-induced contention;
// once Delete owns the guard, every valid mutation must lose before route/runtime work.
func TestDeleteWinsBeforeEachMutationAcrossPlacers(t *testing.T) {
	for _, route := range []string{"override", "added"} {
		t.Run(route, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			path := filepath.Join(t.TempDir(), "journal.db")
			store := openStore(t, path)
			db := deletionSQL(t, path)
			r, backends := deleteRaceRegistry(t)
			client := serveRegistry(t, store, r)
			// Delete occupies its Store's single connection while waiting for the writer.
			// A second real connection lets competing Services read metadata in WAL mode.
			mutations := serveRegistry(t, openStore(t, path), r)
			uid := "delete-winner"
			if err := store.PutSession(sqlitelog.SessionMeta{UID: uid, Harness: route}); err != nil {
				t.Fatal(err)
			}
			writer, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			if _, err := writer.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
				t.Fatal(err)
			}
			defer func() { _, _ = writer.ExecContext(context.Background(), "ROLLBACK") }()
			result := make(chan error, 1)
			go func() {
				for {
					_, err := client.DeleteSession(ctx, &v1.DeleteSessionRequest{Uid: uid})
					if status.Code(err) != codes.Aborted {
						result <- err
						return
					}
					runtime.Gosched()
				}
			}()
			for {
				err := r.WithSessionGuard(uid, func() error { return nil })
				if errors.Is(err, placement.ErrSessionBusy) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-result:
					t.Fatalf("Delete finished before writer barrier: %v", err)
				case <-ctx.Done():
					t.Fatal("Delete never acquired guard")
				default:
					runtime.Gosched()
				}
			}
			before := sessionDeleteRaw(t, db)
			for _, op := range []string{"Exec", "Suspend", "Resume", "Fork"} {
				if err := deleteRaceMutation(ctx, mutations, op, uid, ""); status.Code(err) != codes.Aborted {
					t.Errorf("%s during Delete=%v, want Aborted", op, err)
				}
			}
			if !reflect.DeepEqual(before, sessionDeleteRaw(t, db)) {
				t.Error("losing mutations changed raw state")
			}
			assertDeleteNoEffects(t, backends)
			if _, err := writer.ExecContext(ctx, "ROLLBACK"); err != nil {
				t.Fatal(err)
			}
			if err := <-result; err != nil {
				t.Fatalf("winning Delete failed: %v", err)
			}
			before = sessionDeleteRaw(t, db)
			for _, op := range []string{"Exec", "Suspend", "Resume", "Fork"} {
				if err := deleteRaceMutation(ctx, mutations, op, uid, ""); status.Code(err) != codes.FailedPrecondition {
					t.Errorf("%s after Delete=%v, want FailedPrecondition", op, err)
				}
			}
			if !reflect.DeepEqual(before, sessionDeleteRaw(t, db)) {
				t.Error("deleted refusals changed raw state")
			}
			assertDeleteNoEffects(t, backends)
			if err := r.WithSessionGuard(uid, func() error { return nil }); err != nil {
				t.Fatalf("Delete/refusal leaked guard: %v", err)
			}
		})
	}
}

// Initial mutation metadata reads must acquire admission before asking the same Store for its
// sole connection. Otherwise a blocked Delete queues valid requests until writer release, and
// they either report a delayed tombstone error or run after a failed Delete instead of Aborted.
func TestDeleteSameServiceBusyBeforeMetadataLookup(t *testing.T) {
	for _, outcome := range []string{"commit", "store-failure"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			path := filepath.Join(t.TempDir(), "journal.db")
			store := openStore(t, path)
			db := deletionSQL(t, path)
			r, backends := deleteRaceRegistry(t)
			// One client, Service and Store for Delete and every competing mutation.
			client := serveRegistry(t, store, r)
			uid := "same-store"
			if err := store.PutSession(sqlitelog.SessionMeta{UID: uid, Harness: "recorded"}); err != nil {
				t.Fatal(err)
			}
			if outcome == "store-failure" {
				if _, err := db.Exec("CREATE TRIGGER fail_delete BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT, 'intent failure'); END"); err != nil {
					t.Fatal(err)
				}
			}
			before := sessionDeleteRaw(t, db)
			writer, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			if _, err := writer.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
				t.Fatal(err)
			}
			defer func() { _, _ = writer.ExecContext(context.Background(), "ROLLBACK") }()
			deleted := make(chan error, 1)
			go func() {
				for {
					_, err := client.DeleteSession(ctx, &v1.DeleteSessionRequest{Uid: uid})
					if status.Code(err) != codes.Aborted {
						deleted <- err
						return
					}
					runtime.Gosched()
				}
			}()
			// Contention can only come from Delete. Probe-induced Delete refusals retry;
			// after acquisition its BEGIN stays blocked until this test releases writer.
			for {
				err := r.WithSessionGuard(uid, func() error { return nil })
				if errors.Is(err, placement.ErrSessionBusy) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-deleted:
					t.Fatalf("Delete ended before writer barrier: %v", err)
				case <-ctx.Done():
					t.Fatal("Delete never acquired guard")
				default:
					runtime.Gosched()
				}
			}
			type result struct {
				op  string
				err error
			}
			results := make(chan result, 5)
			for _, op := range []string{"Exec", "Fork", "Suspend", "Resume", "Approve"} {
				go func() { results <- result{op, deleteRaceMutation(ctx, client, op, uid, "")} }()
			}
			// This deadline only bounds a failed test; success is receipt of all five
			// Aborted replies while the independent writer lock is still held.
			wait, stopWait := context.WithTimeout(ctx, 2*time.Second)
			defer stopWait()
			received := 0
			for received < 5 {
				select {
				case got := <-results:
					received++
					if status.Code(got.err) != codes.Aborted {
						t.Errorf("%s before writer release=%v, want Aborted", got.op, got.err)
					}
				case <-wait.Done():
					t.Errorf("only %d/5 mutations returned before writer release; same-Store metadata reads blocked before busy admission", received)
					goto releaseWriter
				}
			}
		releaseWriter:
			assertDeleteNoEffects(t, backends)
			if !reflect.DeepEqual(before, sessionDeleteRaw(t, db)) {
				t.Error("busy mutations changed raw state while Delete waited")
			}
			select {
			case err := <-deleted:
				t.Fatalf("busy mutations canceled/released holder: %v", err)
			default:
			}
			if _, err := writer.ExecContext(ctx, "ROLLBACK"); err != nil {
				t.Fatal(err)
			}
			want := codes.OK
			if outcome == "store-failure" {
				want = codes.Internal
			}
			if err := <-deleted; status.Code(err) != want {
				t.Errorf("Delete outcome=%v, want %v", err, want)
			}
			for received < 5 {
				select {
				case got := <-results:
					received++
					if status.Code(got.err) != codes.Aborted {
						t.Errorf("queued %s continued after writer release: %v, want original Aborted", got.op, got.err)
					}
				case <-ctx.Done():
					t.Fatal("mutation remained queued after holder finished")
				}
			}
			assertDeleteNoEffects(t, backends)
			if outcome == "store-failure" {
				if !reflect.DeepEqual(before, sessionDeleteRaw(t, db)) {
					t.Error("failed Delete or delayed mutation changed raw state")
				}
				if _, err := db.Exec("DROP TRIGGER fail_delete"); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.WithSessionGuard(uid, func() error { return nil }); err != nil {
				t.Fatalf("Delete/busy path leaked guard: %v", err)
			}
			if _, err := client.DeleteSession(ctx, &v1.DeleteSessionRequest{Uid: uid}); err != nil {
				t.Fatalf("Delete retry after outcome=%v", err)
			}
			if err := store.Session(uid).Verify(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Routing SQL must not queue behind a Delete that starts after the initial ACTIVE lookup.
// Removing the guarded routing phase takes the Delete-winner branch: its failed transaction
// then releases the sole Store connection to an Exec that should already have returned busy.
func TestDeleteSameServiceBusyAfterMetadataLookup(t *testing.T) {
	for _, outcome := range []string{"commit", "store-failure"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			path := filepath.Join(t.TempDir(), "journal.db")
			store := openStore(t, path)
			db := deletionSQL(t, path)
			r, backends := deleteRaceRegistry(t)
			svc := session.NewService(store, r)
			entered, release := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			t.Cleanup(unblock)
			svc.SetAfterPinCheck(func() { close(entered); <-release })
			// Exec and Delete use the exact same gRPC client, Service and Store.
			client := serveSessions(t, svc)
			uid := mustCreate(t, client)
			if outcome == "store-failure" {
				if _, err := db.Exec("CREATE TRIGGER fail_delete BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT, 'intent failure'); END"); err != nil {
					t.Fatal(err)
				}
			}
			executed := make(chan error, 1)
			go func() { executed <- deleteRaceMutation(ctx, client, "Exec", uid, "override") }()
			select {
			case <-entered:
			case err := <-executed:
				t.Fatalf("Exec ended before post-lookup pin-check barrier: %v", err)
			case <-ctx.Done():
				t.Fatal("Exec never reached post-lookup barrier")
			}
			before := sessionDeleteRaw(t, db)
			writer, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			if _, err := writer.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
				t.Fatal(err)
			}
			defer func() { _, _ = writer.ExecContext(context.Background(), "ROLLBACK") }()
			// Only Exec exists at this point. Busy proves it owns the routing guard;
			// otherwise Delete must acquire it before Exec is released to routing SQL.
			err = r.WithSessionGuard(uid, func() error { return nil })
			if errors.Is(err, placement.ErrSessionBusy) {
				t.Log("Exec owns Registry guard at post-lookup routing barrier")
				deleted := make(chan error, 1)
				go func() { _, err := client.DeleteSession(ctx, &v1.DeleteSessionRequest{Uid: uid}); deleted <- err }()
				select {
				case err := <-deleted:
					if status.Code(err) != codes.Aborted {
						t.Errorf("Delete during guarded routing=%v, want Aborted", err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("Delete queued behind the routing-phase holder")
				}
				assertDeleteNoEffects(t, backends)
				if !reflect.DeepEqual(before, sessionDeleteRaw(t, db)) {
					t.Error("losing Delete changed raw state")
				}
				if _, err := writer.ExecContext(ctx, "ROLLBACK"); err != nil {
					t.Fatal(err)
				}
				if outcome == "store-failure" {
					if _, err := db.Exec("DROP TRIGGER fail_delete"); err != nil {
						t.Fatal(err)
					}
				}
				unblock()
				if err := <-executed; err != nil {
					t.Fatalf("busy Delete disrupted winning Exec: %v", err)
				}
				if backends["override"].effects()["Create"] != 1 {
					t.Error("winning Exec did not run on its override")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				deleted := make(chan error, 1)
				go func() {
					for {
						_, err := client.DeleteSession(ctx, &v1.DeleteSessionRequest{Uid: uid})
						if status.Code(err) != codes.Aborted {
							deleted <- err
							return
						}
						runtime.Gosched()
					}
				}()
				for {
					if err := r.WithSessionGuard(uid, func() error { return nil }); errors.Is(err, placement.ErrSessionBusy) {
						break
					} else if err != nil {
						t.Fatal(err)
					}
					select {
					case err := <-deleted:
						t.Fatalf("Delete ended before writer barrier: %v", err)
					case <-ctx.Done():
						t.Fatal("Delete never acquired guard")
					default:
						runtime.Gosched()
					}
				}
				t.Log("Delete owns Registry guard; BEGIN IMMEDIATE blocked by independent writer; release Exec to routing SQL")
				unblock()
				received := false
				select {
				case err := <-executed:
					received = true
					if status.Code(err) != codes.Aborted {
						t.Errorf("Exec before writer release=%v, want Aborted", err)
					}
				case <-time.After(2 * time.Second):
					t.Error("post-lookup routing SQL queued behind Delete's sole Store connection instead of prompt Aborted")
				}
				assertDeleteNoEffects(t, backends)
				if !reflect.DeepEqual(before, sessionDeleteRaw(t, db)) {
					t.Error("losing Exec changed raw state before writer release")
				}
				if _, err := writer.ExecContext(ctx, "ROLLBACK"); err != nil {
					t.Fatal(err)
				}
				want := codes.OK
				if outcome == "store-failure" {
					want = codes.Internal
				}
				if err := <-deleted; status.Code(err) != want {
					t.Errorf("Delete outcome=%v, want %v", err, want)
				}
				if !received {
					if err := <-executed; status.Code(err) != codes.Aborted {
						t.Errorf("queued Exec after writer release=%v, want original Aborted", err)
					}
				}
				assertDeleteNoEffects(t, backends)
				if outcome == "store-failure" {
					if !reflect.DeepEqual(before, sessionDeleteRaw(t, db)) {
						t.Error("failed Delete/delayed Exec changed raw state")
					}
					if _, err := db.Exec("DROP TRIGGER fail_delete"); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := r.WithSessionGuard(uid, func() error { return nil }); err != nil {
				t.Fatalf("routing/Delete leaked guard: %v", err)
			}
			if err := store.Session(uid).Verify(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The initial session update is sent after the SQL phase releases its guard but before
// Placer.Exec acquires the runtime guard. Intercept that real gRPC boundary in tests only.
type deleteBeforeRuntimeServer struct {
	v1.SessionsServer
	beforeRuntime func()
}

func (s deleteBeforeRuntimeServer) Exec(req *v1.ExecRequest, stream v1.Sessions_ExecServer) error {
	return s.SessionsServer.Exec(req, deleteBeforeRuntimeStream{Sessions_ExecServer: stream, beforeRuntime: s.beforeRuntime})
}

type deleteBeforeRuntimeStream struct {
	v1.Sessions_ExecServer
	beforeRuntime func()
}

func (s deleteBeforeRuntimeStream) Send(update *v1.ExecUpdate) error {
	if err := s.Sessions_ExecServer.Send(update); err != nil {
		return err
	}
	if update.GetSession() != nil {
		s.beforeRuntime()
	}
	return nil
}

// The guarded SQL phase is not a whole-RPC lease. If Delete wins the gap before the
// runtime guard, Exec must still return busy without another SQL read or runtime effect.
func TestDeleteSameServiceBusyAfterRoutingBeforeRuntime(t *testing.T) {
	for _, outcome := range []string{"commit", "store-failure"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			path := filepath.Join(t.TempDir(), "journal.db")
			store := openStore(t, path)
			db := deletionSQL(t, path)
			r, backends := deleteRaceRegistry(t)
			svc := session.NewService(store, r)
			entered, release := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			t.Cleanup(unblock)
			client := serveSessions(t, deleteBeforeRuntimeServer{SessionsServer: svc, beforeRuntime: func() { close(entered); <-release }})
			uid := mustCreate(t, client)
			if outcome == "store-failure" {
				if _, err := db.Exec("CREATE TRIGGER fail_delete BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT, 'intent failure'); END"); err != nil {
					t.Fatal(err)
				}
			}
			executed := make(chan error, 1)
			go func() { executed <- deleteRaceMutation(ctx, client, "Exec", uid, "override") }()
			select {
			case <-entered:
			case err := <-executed:
				t.Fatalf("Exec ended before runtime gap barrier: %v", err)
			case <-ctx.Done():
				t.Fatal("Exec never reached runtime gap")
			}
			before := sessionDeleteRaw(t, db)
			writer, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			if _, err := writer.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
				t.Fatal(err)
			}
			defer func() { _, _ = writer.ExecContext(context.Background(), "ROLLBACK") }()
			deleted := make(chan error, 1)
			go func() {
				for {
					_, err := client.DeleteSession(ctx, &v1.DeleteSessionRequest{Uid: uid})
					if status.Code(err) != codes.Aborted {
						deleted <- err
						return
					}
					runtime.Gosched()
				}
			}()
			for {
				if err := r.WithSessionGuard(uid, func() error { return nil }); errors.Is(err, placement.ErrSessionBusy) {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-deleted:
					t.Fatalf("Delete ended before writer barrier: %v", err)
				case <-ctx.Done():
					t.Fatal("Delete never acquired guard")
				default:
					runtime.Gosched()
				}
			}
			t.Log("Delete owns Registry guard after SQL phase; release Exec to authoritative runtime guard")
			unblock()
			select {
			case err := <-executed:
				if status.Code(err) != codes.Aborted {
					t.Errorf("Exec during ongoing Delete=%v, want Aborted", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Exec queued in gap after guarded routing")
			}
			assertDeleteNoEffects(t, backends)
			if !reflect.DeepEqual(before, sessionDeleteRaw(t, db)) {
				t.Error("busy Exec changed raw state")
			}
			if _, err := writer.ExecContext(ctx, "ROLLBACK"); err != nil {
				t.Fatal(err)
			}
			want := codes.OK
			if outcome == "store-failure" {
				want = codes.Internal
			}
			if err := <-deleted; status.Code(err) != want {
				t.Errorf("Delete outcome=%v, want %v", err, want)
			}
			assertDeleteNoEffects(t, backends)
			if outcome == "store-failure" && !reflect.DeepEqual(before, sessionDeleteRaw(t, db)) {
				t.Error("failed Delete changed raw state")
			}
			if err := r.WithSessionGuard(uid, func() error { return nil }); err != nil {
				t.Fatalf("Delete/refusal leaked guard: %v", err)
			}
		})
	}
}

// Removing Placer's authoritative preflight would Create after Service read ACTIVE metadata
// but before it acquired the shared runtime guard. Preserve this boundary independently of
// the earlier pin-check hook, which now correctly runs inside the SQL guard.
func TestDeletedStaleServiceLookupRefusesBeforeCompute(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "journal.db")
	store := openStore(t, path)
	db := deletionSQL(t, path)
	r, backends := deleteRaceRegistry(t)
	svc := session.NewService(store, r)
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	client := serveSessions(t, deleteBeforeRuntimeServer{SessionsServer: svc, beforeRuntime: func() { close(entered); <-release }})
	uid := mustCreate(t, client)
	result := make(chan error, 1)
	go func() { result <- deleteRaceMutation(ctx, client, "Exec", uid, "override") }()
	select {
	case <-entered:
	case err := <-result:
		t.Fatalf("Exec finished before stale lookup barrier: %v", err)
	case <-ctx.Done():
		t.Fatal("Exec did not reach stale lookup barrier")
	}
	if _, err := client.DeleteSession(ctx, &v1.DeleteSessionRequest{Uid: uid}); err != nil {
		t.Fatal(err)
	}
	before := sessionDeleteRaw(t, db)
	unblock()
	if err := <-result; status.Code(err) != codes.FailedPrecondition {
		t.Errorf("stale pre-lock Exec=%v, want FailedPrecondition", err)
	}
	assertDeleteNoEffects(t, backends)
	if !reflect.DeepEqual(before, sessionDeleteRaw(t, db)) {
		t.Error("stale lookup changed journal/fence/projections")
	}
	if err := r.WithSessionGuard(uid, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}
