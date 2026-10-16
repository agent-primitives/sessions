package session_test

import (
	"context"
	"database/sql"
	"io"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
	"github.com/agent-primitives/sessions/sqlitelog"
)

type deleteBackend struct {
	*local.Backend
	mu    sync.Mutex
	calls map[string]int
	hook  func(string)
}

func (b *deleteBackend) count(name string) {
	b.mu.Lock()
	b.calls[name]++
	b.mu.Unlock()
	if b.hook != nil {
		b.hook(name)
	}
}
func (b *deleteBackend) reset() { b.mu.Lock(); b.calls = map[string]int{}; b.mu.Unlock() }
func (b *deleteBackend) effects() map[string]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]int{}
	for k, v := range b.calls {
		out[k] = v
	}
	return out
}
func (b *deleteBackend) Describe(ctx context.Context) (api.Descriptor, error) {
	b.count("Describe")
	return b.Backend.Describe(ctx)
}
func (b *deleteBackend) Create(ctx context.Context, s *api.SessionSpec) (api.Incarnation, error) {
	b.count("Create")
	return b.Backend.Create(ctx, s)
}
func (b *deleteBackend) Restore(ctx context.Context, r api.SnapshotRef) (api.Incarnation, error) {
	b.count("Restore")
	return b.Backend.Restore(ctx, r)
}
func (b *deleteBackend) Snapshot(ctx context.Context, i api.Incarnation, k api.SnapshotKind) (api.SnapshotRef, error) {
	b.count("Snapshot")
	return b.Backend.Snapshot(ctx, i, k)
}
func (b *deleteBackend) Fork(ctx context.Context, r api.SnapshotRef, o api.ForkOpts) (api.Incarnation, error) {
	b.count("Fork")
	return b.Backend.Fork(ctx, r, o)
}
func (b *deleteBackend) Stop(ctx context.Context, i api.Incarnation) error {
	b.count("Stop")
	return b.Backend.Stop(ctx, i)
}
func deletionRegistry(t *testing.T) (*placement.Registry, *deleteBackend) {
	t.Helper()
	b := &deleteBackend{Backend: local.New(echoagent.Harness{}), calls: map[string]int{}}
	t.Cleanup(func() { _ = b.Close() })
	r, err := placement.NewRegistry("echo", map[string]*placement.Placer{"echo": placement.New(b, echoagent.Model)})
	if err != nil {
		t.Fatal(err)
	}
	return r, b
}
func deletionSQL(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
func sessionDeleteRaw(t *testing.T, db *sql.DB) [][]any {
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
func deletionExec(t *testing.T, c v1.SessionsClient, uid, harness string) error {
	t.Helper()
	stream, err := c.Exec(t.Context(), &v1.ExecRequest{Session: uid, Harness: harness})
	if err == nil {
		err = drainExec(stream)
	}
	return err
}
func deletionReplay(t *testing.T, c v1.SessionsClient, uid string) []*v1.LogRecord {
	t.Helper()
	stream, err := c.Replay(t.Context(), &v1.ReplayRequest{Session: uid})
	if err != nil {
		t.Fatal(err)
	}
	var out []*v1.LogRecord
	for {
		r, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// DELETE must render durable metadata and timestamps, not invent completion or move compute.
func TestDeletePublicGRPCAuditRetryAndReopen(t *testing.T) {
	for _, shape := range []string{"empty", "empty-live", "executed", "executed-none", "suspended"} {
		t.Run(shape, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal.db")
			store := openStore(t, path)
			db := deletionSQL(t, path)
			registry, backend := deletionRegistry(t)
			client := serveRegistry(t, store, registry)
			active, err := client.CreateSession(t.Context(), &v1.CreateSessionRequest{Session: &v1.Session{Metadata: &v1.ResourceMetadata{Project: "project", Name: "named"}, Labels: map[string]string{"team": "one"}, Annotations: map[string]string{"note": "audit"}, Model: "model"}})
			if err != nil {
				t.Fatal(err)
			}
			uid := active.GetMetadata().GetUid()
			if active.GetDeletionState() != v1.DeletionState_DELETION_ACTIVE || active.GetDeletionRequestedTime() != nil {
				t.Errorf("active deletion fields=%v", active)
			}
			wantExec, wantCompute := v1.ExecState_EXEC_PENDING, v1.ComputeState_COMPUTE_NONE
			if shape == "executed" || shape == "executed-none" || shape == "suspended" {
				if err := deletionExec(t, client, uid, ""); err != nil {
					t.Fatal(err)
				}
				wantExec = v1.ExecState_EXEC_COMPLETED
				wantCompute = v1.ComputeState_COMPUTE_LIVE
			}
			if shape == "suspended" {
				if _, err := client.Suspend(t.Context(), &v1.SuspendRequest{Session: uid}); err != nil {
					t.Fatal(err)
				}
				wantCompute = v1.ComputeState_COMPUTE_COLD
			}
			if shape == "empty-live" || shape == "executed-none" {
				state := api.ComputeLive
				if shape == "executed-none" {
					state = api.ComputeNone
				}
				if _, err := db.Exec("UPDATE sessions SET compute_state = ? WHERE session = ?", string(state), uid); err != nil {
					t.Fatal(err)
				}
				if shape == "empty-live" {
					wantCompute = v1.ComputeState_COMPUTE_LIVE
				} else {
					wantCompute = v1.ComputeState_COMPUTE_NONE
				}
			}
			before, err := client.GetSession(t.Context(), &v1.GetSessionRequest{Uid: uid})
			if err != nil {
				t.Fatal(err)
			}
			prefix := deletionReplay(t, client, uid)
			backend.reset()
			deleted, err := client.DeleteSession(t.Context(), &v1.DeleteSessionRequest{Uid: uid})
			if err != nil {
				t.Fatal(err)
			}
			if deleted.GetDeletionState() != v1.DeletionState_DELETION_PENDING || deleted.GetDeletionRequestedTime() == nil {
				t.Errorf("deletion state/times=%v", deleted)
			}
			if deleted.GetExecState() != wantExec || deleted.GetComputeState() != wantCompute || deleted.GetLastSeq() != before.GetLastSeq()+1 {
				t.Errorf("deletion axes/cursor=%v", deleted)
			}
			if len(backend.effects()) != 0 {
				t.Errorf("Delete reached runtime: %v", backend.effects())
			}
			normalized := proto.Clone(deleted).(*v1.Session)
			normalized.DeletionState = before.DeletionState
			normalized.DeletionRequestedTime = nil
			normalized.LastSeq = before.LastSeq
			normalized.Metadata.UpdateTime = before.Metadata.UpdateTime
			if !proto.Equal(before, normalized) {
				t.Errorf("Delete lost stored metadata: before=%v after=%v", before, normalized)
			}
			records := deletionReplay(t, client, uid)
			if len(records) != len(prefix)+1 {
				t.Fatal("missing DELETE audit record")
			}
			for i := range prefix {
				if !proto.Equal(prefix[i], records[i]) {
					t.Error("DELETE altered original prefix")
				}
			}
			deletes := 0
			for _, r := range records {
				if r.GetEvent().GetLifecycle().GetKind() == v1.Lifecycle_LIFECYCLE_DELETE {
					deletes++
					if r.GetEvent().GetExecutionId() != "" {
						t.Error("DELETE has execution id")
					}
				}
			}
			if deletes != 1 {
				t.Errorf("DELETE count=%d", deletes)
			}
			page, err := client.ListSessions(t.Context(), &v1.ListSessionsRequest{Project: "project"})
			if err != nil || len(page.GetSessions()) != 0 {
				t.Errorf("deleted List=%v %v", page, err)
			}
			raw := sessionDeleteRaw(t, db)
			for _, op := range []string{"Exec", "Fork-historical", "Suspend", "Resume"} {
				var err error
				switch op {
				case "Exec":
					err = deletionExec(t, client, uid, "no-such-override")
				case "Fork-historical":
					_, err = client.Fork(t.Context(), &v1.ForkRequest{Session: uid, AtSeq: 1})
				case "Suspend":
					_, err = client.Suspend(t.Context(), &v1.SuspendRequest{Session: uid})
				case "Resume":
					_, err = client.Resume(t.Context(), &v1.ResumeRequest{Session: uid})
				}
				if status.Code(err) != codes.FailedPrecondition {
					t.Errorf("deleted %s=%v", op, err)
				}
			}
			for range 2 {
				retry, err := client.DeleteSession(t.Context(), &v1.DeleteSessionRequest{Uid: uid})
				if err != nil || !proto.Equal(deleted, retry) {
					t.Errorf("retry=%v %v", retry, err)
				}
				got, err := client.GetSession(t.Context(), &v1.GetSessionRequest{Uid: uid})
				if err != nil || !proto.Equal(deleted, got) {
					t.Errorf("Get=%v %v", got, err)
				}
			}
			if !reflect.DeepEqual(raw, sessionDeleteRaw(t, db)) || len(backend.effects()) != 0 {
				t.Errorf("retry/refusal had effects: %v", backend.effects())
			}
			if err := store.Session(uid).Verify(); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := openStore(t, path)
			client = serveRegistry(t, reopened, registry)
			retry, err := client.DeleteSession(t.Context(), &v1.DeleteSessionRequest{Uid: uid})
			if err != nil || !proto.Equal(deleted, retry) {
				t.Errorf("reopen retry=%v %v", retry, err)
			}
		})
	}
}
func TestDeleteUnknownEmptyAndUnservedStoredHarness(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	store := openStore(t, path)
	db := deletionSQL(t, path)
	registry, backend := deletionRegistry(t)
	client := serveRegistry(t, store, registry)
	before := sessionDeleteRaw(t, db)
	for _, tc := range []struct {
		uid  string
		code codes.Code
	}{{"", codes.InvalidArgument}, {"unknown", codes.NotFound}} {
		if _, err := client.DeleteSession(t.Context(), &v1.DeleteSessionRequest{Uid: tc.uid}); status.Code(err) != tc.code {
			t.Errorf("Delete(%q)=%v", tc.uid, err)
		}
	}
	if !reflect.DeepEqual(before, sessionDeleteRaw(t, db)) {
		t.Error("unknown/empty Delete created a row")
	}
	for _, name := range []string{"missing", "retired"} {
		uid := "session-" + name
		if err := store.PutSession(sqlitelog.SessionMeta{UID: uid, Harness: name}); err != nil {
			t.Fatal(err)
		}
		if _, err := client.DeleteSession(t.Context(), &v1.DeleteSessionRequest{Uid: uid}); err != nil {
			t.Fatalf("unserved Delete=%v", err)
		}
		for _, op := range []string{"Exec", "Fork", "Suspend", "Resume"} {
			var err error
			switch op {
			case "Exec":
				err = deletionExec(t, client, uid, "echo")
			case "Fork":
				_, err = client.Fork(t.Context(), &v1.ForkRequest{Session: uid, AtSeq: 1})
			case "Suspend":
				_, err = client.Suspend(t.Context(), &v1.SuspendRequest{Session: uid})
			case "Resume":
				_, err = client.Resume(t.Context(), &v1.ResumeRequest{Session: uid})
			}
			if status.Code(err) != codes.FailedPrecondition {
				t.Errorf("unserved deleted %s=%v", op, err)
			}
		}
	}
	if len(backend.effects()) != 0 {
		t.Errorf("unserved deletion effects=%v", backend.effects())
	}
}

// A real SQL insert failure rolls back and releases the shared guard for a successful retry.
func TestDeletePublicStoreFaultHasNoMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	store := openStore(t, path)
	db := deletionSQL(t, path)
	registry, backend := deletionRegistry(t)
	client := serveRegistry(t, store, registry)
	uid := mustCreate(t, client)
	if _, err := db.Exec("CREATE TRIGGER fail_delete BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT, 'delete insert fault'); END"); err != nil {
		t.Fatal(err)
	}
	before := sessionDeleteRaw(t, db)
	if _, err := client.DeleteSession(t.Context(), &v1.DeleteSessionRequest{Uid: uid}); status.Code(err) != codes.Internal {
		t.Errorf("store fault=%v", err)
	}
	if !reflect.DeepEqual(before, sessionDeleteRaw(t, db)) || len(backend.effects()) != 0 {
		t.Error("store failure changed state or reached runtime")
	}
	if _, err := db.Exec("DROP TRIGGER fail_delete"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.DeleteSession(t.Context(), &v1.DeleteSessionRequest{Uid: uid}); err != nil {
		t.Fatalf("guard retained after store fault: %v", err)
	}
}

var _ eventlog.Store = (*sqlitelog.Log)(nil)
