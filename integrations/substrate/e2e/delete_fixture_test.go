package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"reflect"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/client"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/placement"
)

// Observe, never replace, the runtime: a successful Delete must not even consult it. Cleanup and
// deliberate setup are outside the before/after observation window.
type deleteRuntime struct {
	placement.Backend
	mu    sync.Mutex
	calls int
}

func (b *deleteRuntime) called()    { b.mu.Lock(); b.calls++; b.mu.Unlock() }
func (b *deleteRuntime) count() int { b.mu.Lock(); defer b.mu.Unlock(); return b.calls }
func (b *deleteRuntime) Describe(ctx context.Context) (api.Descriptor, error) {
	b.called()
	return b.Backend.Describe(ctx)
}
func (b *deleteRuntime) Create(ctx context.Context, s *api.SessionSpec) (api.Incarnation, error) {
	b.called()
	return b.Backend.Create(ctx, s)
}
func (b *deleteRuntime) Snapshot(ctx context.Context, i api.Incarnation, k api.SnapshotKind) (api.SnapshotRef, error) {
	b.called()
	return b.Backend.Snapshot(ctx, i, k)
}
func (b *deleteRuntime) Restore(ctx context.Context, s api.SnapshotRef) (api.Incarnation, error) {
	b.called()
	return b.Backend.Restore(ctx, s)
}
func (b *deleteRuntime) Fork(ctx context.Context, s api.SnapshotRef, o api.ForkOpts) (api.Incarnation, error) {
	b.called()
	return b.Backend.Fork(ctx, s, o)
}
func (b *deleteRuntime) Stop(ctx context.Context, i api.Incarnation) error {
	b.called()
	return b.Backend.Stop(ctx, i)
}
func (b *deleteRuntime) Status(ctx context.Context, i api.Incarnation) (api.ComputeState, error) {
	b.called()
	return b.Backend.Status(ctx, i)
}

type deleteAPI struct {
	*sessionAPI
}

// newDeleteAPI serves the delete fixture on runtime. A substrate runtime passes the router dialer
// (newPlacer's); the offline run on runtime/local keeps the default unix-socket dialer.
func newDeleteAPI(t *testing.T, runtime placement.Backend, opts ...placement.Option) *deleteAPI {
	t.Helper()
	registry, err := placement.NewRegistry("delete-harness", map[string]*placement.Placer{"delete-harness": placement.New(runtime, echoagent.Model, opts...)})
	if err != nil {
		t.Fatal(err)
	}
	return &deleteAPI{sessionAPI: newSessionAPI(t, registry)}
}
func (a *deleteAPI) create(t *testing.T, ctx context.Context, name string) string {
	t.Helper()
	s, err := a.client.CreateSession(ctx, &v1.Session{Metadata: &v1.ResourceMetadata{Project: "delete-fixture", Name: name}, Labels: map[string]string{"audit": "retain"}})
	if err != nil {
		t.Fatal(err)
	}
	return s.GetMetadata().GetUid()
}
func (a *deleteAPI) exec(t *testing.T, ctx context.Context, uid, input string) string {
	t.Helper()
	turn, err := a.client.Exec(ctx, client.ExecOptions{Session: uid, Inputs: []string{input}})
	if err != nil {
		t.Fatalf("Exec %s: %v", uid, err)
	}
	return turn.Output
}
func (a *deleteAPI) fork(t *testing.T, ctx context.Context, uid string, count int32) []string {
	t.Helper()
	children, err := a.client.Fork(ctx, uid, client.ForkOptions{Count: count})
	if err != nil {
		t.Fatalf("Fork %s: %v", uid, err)
	}
	out := make([]string, len(children))
	for i, child := range children {
		out[i] = child.GetMetadata().GetUid()
	}
	if len(out) != int(count) {
		t.Fatalf("Fork children=%d want %d", len(out), count)
	}
	return out
}

// Read-only retries/refusals must preserve exact durable metadata, fences and original event bytes
// (Record includes the raw canonical hash). API Replay must expose that same audit, including DELETE.
func (a *deleteAPI) pending(t *testing.T, ctx context.Context, b *deleteRuntime, uid string) *v1.Session {
	t.Helper()
	before, err := a.client.GetSession(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	prefix := a.records(t, uid)
	prefixRows := a.freeze(t, uid)
	calls := b.count()
	deleted, err := a.client.DeleteSession(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if b.count() != calls {
		t.Fatal("Delete consulted runtime (including Stop)")
	}
	if deleted.GetDeletionState() != v1.DeletionState_DELETION_PENDING || deleted.GetDeletionRequestedTime() == nil || deleted.GetMetadata().GetUid() != uid || deleted.GetLastSeq() != before.GetLastSeq()+1 || deleted.GetComputeState() != before.GetComputeState() || deleted.GetExecState() != before.GetExecState() {
		t.Fatalf("intent-only deletion axes: %v", deleted)
	}
	records := a.records(t, uid)
	if len(records) != len(prefix)+1 || !reflect.DeepEqual(prefix, records[:len(prefix)]) {
		t.Fatal("Delete changed audit prefix or added more than one event")
	}
	deletes := 0
	for _, r := range records {
		if r.Event.Kind == api.EventLifecycle && r.Event.Lifecycle.Kind == api.LifecycleDelete {
			deletes++
			if r.Event.ExecutionID != "" || !r.Event.Timestamp.Equal(deleted.GetDeletionRequestedTime().AsTime()) {
				t.Fatal("DELETE scope/time mismatch")
			}
		}
	}
	if deletes != 1 {
		t.Fatalf("DELETE count=%d", deletes)
	}
	info, err := a.store.SessionInfo(uid)
	if err != nil {
		t.Fatal(err)
	}
	frozen := a.freeze(t, uid)
	// Row zero is mutable session metadata; every original event row must be byte-identical.
	if len(frozen) != len(prefixRows)+1 || !reflect.DeepEqual(prefixRows[1:], frozen[1:len(prefixRows)]) {
		t.Fatal("Delete rewrote raw audit event bytes")
	}
	for _, op := range []struct {
		name string
		run  func() error
	}{
		{"exec", func() error {
			_, err := a.client.Exec(ctx, client.ExecOptions{Session: uid, Inputs: []string{"refuse"}})
			return err
		}},
		{"historical fork", func() error { _, err := a.client.Fork(ctx, uid, client.ForkOptions{AtSeq: 1}); return err }},
		{"resume", func() error { _, err := a.client.Resume(ctx, uid, false); return err }},
		{"boot resume", func() error { _, err := a.client.Resume(ctx, uid, true); return err }},
		{"suspend", func() error { _, err := a.client.Suspend(ctx, uid); return err }},
	} {
		if err := op.run(); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("%s: want FailedPrecondition, got %v", op.name, err)
		}
	}
	for _, reopen := range []bool{false, true} {
		if reopen {
			a.reopen(t)
		}
		retry, err := a.client.DeleteSession(ctx, uid)
		if err != nil || !sameDeleteMessage(t, deleted, retry) {
			t.Fatalf("retry/reopen changed Session: %v %v", retry, err)
		}
		get, err := a.client.GetSession(ctx, uid)
		if err != nil || !sameDeleteMessage(t, deleted, get) {
			t.Fatalf("Get changed Session: %v %v", get, err)
		}
		after, err := a.store.SessionInfo(uid)
		if err != nil || !reflect.DeepEqual(info, after) || !reflect.DeepEqual(records, a.records(t, uid)) || !reflect.DeepEqual(frozen, a.freeze(t, uid)) {
			t.Fatalf("retry/refusal/reopen changed durable state: %v", err)
		}
		replay, err := a.client.Replay(ctx, uid, 1, 0)
		if err != nil || len(replay) != len(records) {
			t.Fatalf("Replay: %v", err)
		}
		for i, r := range records {
			if !sameDeleteMessage(t, eventlog.RecordToProto(r), replay[i]) {
				t.Fatalf("Replay changed record %d", r.Seq)
			}
		}
	}
	if b.count() != calls {
		t.Fatal("deleted mutation/read/retry consulted runtime")
	}
	all, err := a.client.ListSessions(ctx, "delete-fixture")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range all {
		if s.GetMetadata().GetUid() == uid {
			t.Fatal("List exposed tombstone")
		}
	}
	bytes, err := json.Marshal(deleted)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("PENDING audit %s head=%d fence=%d SessionSHA256=%x lastHash=%s db=%s", uid, info.LastSeq, records[len(records)-1].Fence, sha256.Sum256(bytes), records[len(records)-1].Hash, a.path)
	return deleted
}

// Compare exported wire fields rather than protobuf runtime caches; JSON is only a test comparison,
// never canonical provenance. Raw SQLite bytes and Log.Verify independently check the chain.
func sameDeleteMessage(t *testing.T, a, b any) bool {
	t.Helper()
	left, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	right, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(left, right)
}

// Freeze actual SQL rows, including store-owned fence and raw event blobs, not only API projections.
func (a *deleteAPI) freeze(t *testing.T, uid string) [][]any {
	t.Helper()
	db, err := sql.Open("sqlite", a.path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	var out [][]any
	for _, query := range []string{"SELECT * FROM sessions WHERE session = ?", "SELECT * FROM events WHERE session = ? ORDER BY seq"} {
		rows, err := db.Query(query, uid)
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(cols))
			pointers := make([]any, len(cols))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			out = append(out, values)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func assertDeletePrefix(t *testing.T, prefix, after []eventlog.Record) {
	t.Helper()
	if len(after) < len(prefix) || !reflect.DeepEqual(prefix, after[:len(prefix)]) {
		t.Fatal("ancestor deletion/descendant mutation changed copied prefix")
	}
}

func deletionStatus(t *testing.T, ctx context.Context, a *deleteAPI, uid string, cold bool) {
	t.Helper()
	before, err := a.client.GetSession(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if cold {
		// Memory Fork already checkpoints the source; do not ask Substrate to suspend a cold actor.
		if before.GetComputeState() != v1.ComputeState_COMPUTE_COLD {
			if _, err := a.client.Suspend(ctx, uid); err != nil {
				t.Fatal(err)
			}
		}
	} else {
		if _, err := a.client.Resume(ctx, uid, false); err != nil {
			t.Fatal(err)
		}
	}
	get, err := a.client.GetSession(ctx, uid)
	want := v1.ComputeState_COMPUTE_LIVE
	if cold {
		want = v1.ComputeState_COMPUTE_COLD
	}
	if err != nil || get.GetComputeState() != want {
		t.Fatalf("setup compute %s: %v %v", uid, get, err)
	}
}
