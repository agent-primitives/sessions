package sqlitelog

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sqlite "modernc.org/sqlite"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
)

func deletionStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path, WithDefaultProject("tenant"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func deletionInfo(t *testing.T, s *Store, uid string) SessionInfo {
	t.Helper()
	info, err := s.SessionInfo(uid)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

type storedEvent struct {
	Seq, Fence int64
	Prev, Hash string
	Blob       []byte
}

type deletionSnapshot struct {
	Info   SessionInfo
	Fence  int64
	Events []storedEvent
}

// Capture stored bytes as well as the public projection: re-encoding reads cannot prove retention.
func captureDeletion(t *testing.T, s *Store, uid string) deletionSnapshot {
	t.Helper()
	out := deletionSnapshot{Info: deletionInfo(t, s, uid)}
	if err := s.db.QueryRow(`SELECT fence FROM sessions WHERE session = ?`, uid).Scan(&out.Fence); err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.Query(`SELECT seq, fence, prev_hash, hash, event FROM events WHERE session = ? ORDER BY seq`, uid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var r storedEvent
		if err := rows.Scan(&r.Seq, &r.Fence, &r.Prev, &r.Hash, &r.Blob); err != nil {
			t.Fatal(err)
		}
		out.Events = append(out.Events, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func assertDeletionUnchanged(t *testing.T, s *Store, uid string, before deletionSnapshot) {
	t.Helper()
	if after := captureDeletion(t, s, uid); !reflect.DeepEqual(after, before) {
		t.Fatalf("session changed:\nbefore %+v\nafter  %+v", before, after)
	}
}

// Missing the host-only barrier allows journal/metadata disagreement even on an active UID.
func TestAppendRejectsHostOwnedDelete(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown", true: "active"}[known], func(t *testing.T) {
			s := deletionStore(t, ":memory:")
			var before deletionSnapshot
			if known {
				if err := s.PutSession(SessionMeta{UID: "s"}); err != nil {
					t.Fatal(err)
				}
				before = captureDeletion(t, s, "s")
			}
			_, err := s.Session("s").Append(0, 999, api.Event{Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: api.LifecycleDelete}})
			if !errors.Is(err, errReservedDelete) {
				t.Fatalf("Append DELETE = %v, want reserved-delete error", err)
			}
			head, err := s.Session("s").Head()
			if err != nil || head != 0 {
				t.Fatalf("refused append changed head: %d, %v", head, err)
			}
			var n int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if want := map[bool]int{false: 0, true: 1}[known]; n != want {
				t.Fatalf("rows = %d, want %d", n, want)
			}
			if known {
				assertDeletionUnchanged(t, s, "s", before)
			}
		})
	}
}

func TestDeleteSessionAtomicIntent(t *testing.T) {
	for _, storage := range []string{"memory", "file"} {
		for _, shape := range []string{"empty", "implicit-fence", "implicit-append", "executed", "suspended"} {
			t.Run(storage+"/"+shape, func(t *testing.T) {
				path := ":memory:"
				if storage == "file" {
					path = filepath.Join(t.TempDir(), "intent.db")
				}
				s := deletionStore(t, path)
				log := s.Session("s")
				if shape == "empty" || shape == "executed" || shape == "suspended" {
					if err := s.PutSession(SessionMeta{UID: "s", Project: "acme", Name: "audit", Harness: "h", Model: "m", ParentUID: "parent", ForkSeq: 7, CreatedAt: time.Unix(123, 456), Labels: "labels", Annotations: "annotations", Origin: "origin", Identity: "identity"}); err != nil {
						t.Fatal(err)
					}
				}
				if shape == "implicit-fence" || shape == "executed" || shape == "suspended" {
					if _, err := log.NewFence(); err != nil {
						t.Fatal(err)
					}
				}
				if shape == "implicit-append" || shape == "executed" || shape == "suspended" {
					// A high supplied append fence does not own the sessions-row fencing sequence.
					if _, err := log.Append(0, 99, api.Event{Kind: api.EventOutput, ExecutionID: "exec", Message: api.TextMessage("assistant", "retained")}); err != nil {
						t.Fatal(err)
					}
				}
				if shape == "suspended" {
					if _, err := log.Append(1, 99, api.Event{Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: api.LifecycleSuspend, Snapshot: &api.SnapshotRef{Local: "retained-handle", ExternalURI: "retained-uri", Memory: true}}}); err != nil {
						t.Fatal(err)
					}
				}
				before := captureDeletion(t, s, "s")
				if before.Info.DeletionState != api.DeletionActive || !before.Info.DeletionRequestedAt.IsZero() || !before.Info.DeletionCompletedAt.IsZero() || before.Info.DeletionSeq != 0 {
					t.Fatalf("fresh deletion metadata = %+v", before.Info)
				}
				start := time.Now()
				got, err := s.DeleteSession(context.Background(), "s")
				end := time.Now()
				if err != nil {
					t.Fatalf("delete: %v", err)
				}
				after := captureDeletion(t, s, "s")
				if !reflect.DeepEqual(got, after.Info) {
					t.Fatalf("returned uncommitted/wrong info: %+v != %+v", got, after.Info)
				}
				if got.DeletionState != api.DeletionPending || got.DeletionSeq != before.Info.LastSeq+1 || got.LastSeq != got.DeletionSeq || !got.DeletionCompletedAt.IsZero() {
					t.Fatalf("intent = %+v", got)
				}
				if got.DeletionRequestedAt.Before(start) || got.DeletionRequestedAt.After(end) || !got.UpdatedAt.Equal(got.DeletionRequestedAt) {
					t.Fatalf("request/updated timestamp = %v/%v outside %v..%v", got.DeletionRequestedAt, got.UpdatedAt, start, end)
				}
				if after.Fence != before.Fence+1 || got.ComputeState != before.Info.ComputeState {
					t.Fatalf("fence/compute changed incorrectly: %+v -> %+v", before, after)
				}
				meta := got.SessionMeta
				meta.UpdatedAt = before.Info.UpdatedAt
				if !reflect.DeepEqual(meta, before.Info.SessionMeta) {
					t.Fatalf("delete rewrote caller metadata: %+v -> %+v", before.Info.SessionMeta, meta)
				}
				if len(after.Events) != len(before.Events)+1 || (len(before.Events) > 0 && !reflect.DeepEqual(after.Events[:len(before.Events)], before.Events)) {
					t.Fatalf("prefix bytes changed: %+v -> %+v", before.Events, after.Events)
				}
				recs, err := log.Read(1)
				if err != nil {
					t.Fatal(err)
				}
				var deletes []eventlog.Record
				for _, r := range recs {
					if r.Event.Kind == api.EventLifecycle && r.Event.Lifecycle != nil && r.Event.Lifecycle.Kind == api.LifecycleDelete {
						deletes = append(deletes, r)
					}
				}
				if len(deletes) != 1 {
					t.Fatalf("DELETE records = %v", deletes)
				}
				d := deletes[0]
				if d.Seq != got.DeletionSeq || d.Fence != after.Fence || d.Event.ExecutionID != "" || !d.Event.Timestamp.Equal(got.DeletionRequestedAt) || d.Event.Lifecycle.Snapshot != nil {
					t.Fatalf("DELETE intent = %+v", d)
				}
				if err := log.Verify(); err != nil {
					t.Fatalf("audit verify: %v", err)
				}
				if _, err := log.Snapshot(); err != nil {
					t.Fatal(err)
				}
				if storage == "file" {
					s.Close()
					s = deletionStore(t, path)
				}
				retry, err := s.DeleteSession(context.Background(), "s")
				if err != nil || !reflect.DeepEqual(retry, got) {
					t.Fatalf("retry changed intent: %+v, %v; want %+v", retry, err, got)
				}
				assertDeletionUnchanged(t, s, "s", after)
				if err := s.Session("s").Verify(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestDeleteUnknownAndEmptyNeverCreate(t *testing.T) {
	s := deletionStore(t, ":memory:")
	if _, err := s.DeleteSession(context.Background(), "missing"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("unknown delete = %v, want ErrSessionNotFound", err)
	}
	if _, err := s.DeleteSession(context.Background(), ""); err == nil {
		t.Fatal("empty UID accepted")
	}
	for _, uid := range []string{"missing", ""} {
		if _, err := s.SessionInfo(uid); !errors.Is(err, ErrSessionNotFound) {
			t.Fatalf("refused UID %q created: %v", uid, err)
		}
		head, err := s.Session(uid).Head()
		if err != nil || head != 0 {
			t.Fatalf("unknown head = %d, %v", head, err)
		}
	}
}

func TestDeletedWriteBarriersPrecedeCASAndFence(t *testing.T) {
	s := deletionStore(t, ":memory:")
	if err := s.PutSession(SessionMeta{UID: "s"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteSession(context.Background(), "s"); err != nil {
		t.Fatal(err)
	}
	for _, state := range []api.DeletionState{api.DeletionPending, api.DeletionComplete} {
		t.Run(string(state), func(t *testing.T) {
			// COMPLETE is future wire vocabulary; seed it only to prove the durable terminal barrier.
			if _, err := s.db.Exec(`UPDATE sessions SET deletion_state = ? WHERE session = 's'`, string(state)); err != nil {
				t.Fatal(err)
			}
			completed := time.Unix(1700000000, 123)
			if state == api.DeletionComplete {
				if _, err := s.db.Exec(`UPDATE sessions SET deletion_completed_at = ?, updated_at = ? WHERE session = 's'`, completed.UnixNano(), completed.UnixNano()); err != nil {
					t.Fatal(err)
				}
			}
			before := captureDeletion(t, s, "s")
			if state == api.DeletionComplete && !before.Info.DeletionCompletedAt.Equal(completed) {
				t.Fatalf("completed time lost: %+v", before.Info)
			}
			for _, fence := range []int64{0, before.Fence, 1 << 60} {
				for _, head := range []int64{0, before.Info.LastSeq, 1 << 60} {
					_, err := s.Session("s").Append(head, fence, api.Event{Kind: api.EventOutput, Message: api.TextMessage("assistant", "forbidden")})
					if !errors.Is(err, eventlog.ErrDeleted) || errors.Is(err, eventlog.ErrFenced) || errors.Is(err, eventlog.ErrConflict) {
						t.Fatalf("Append(%d,%d) = %v, want distinct ErrDeleted", head, fence, err)
					}
				}
			}
			_, err := s.Session("s").Append(0, 1<<60, api.Event{Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: api.LifecycleDelete}})
			if !errors.Is(err, eventlog.ErrDeleted) {
				t.Fatalf("deleted forged DELETE = %v", err)
			}
			if _, err := s.Session("s").NewFence(); !errors.Is(err, eventlog.ErrDeleted) {
				t.Fatalf("NewFence = %v", err)
			}
			if err := eventlog.CheckWritable(s.Session("s")); !errors.Is(err, eventlog.ErrDeleted) {
				t.Fatalf("preflight = %v", err)
			}
			m := SessionMeta{UID: "s", Project: "other", Name: "resurrect", CreatedAt: time.Now()}
			if err := s.PutSession(m); !errors.Is(err, eventlog.ErrDeleted) {
				t.Fatalf("PutSession = %v", err)
			}
			for _, tc := range []struct {
				harness    string
				registered bool
			}{{"static", false}, {"missing", true}} {
				m.Harness = tc.harness
				if err := s.PutSessionOnActiveHarness(m, tc.registered); !errors.Is(err, eventlog.ErrDeleted) {
					t.Fatalf("PutSessionOnActiveHarness(%+v) = %v", tc, err)
				}
			}
			if _, _, err := s.RegisterHarness(HarnessRecord{Name: "active", UID: "h", Spec: "{}", SpecDigest: "digest"}); err != nil {
				t.Fatal(err)
			}
			m.Harness = "active"
			if err := s.PutSessionOnActiveHarness(m, true); !errors.Is(err, eventlog.ErrDeleted) {
				t.Fatalf("active-harness Put = %v", err)
			}
			if _, err := s.RetireHarness("active", "retired"); err != nil {
				t.Fatal(err)
			}
			if err := s.PutSessionOnActiveHarness(m, true); !errors.Is(err, eventlog.ErrDeleted) {
				t.Fatalf("retired-harness deleted Put = %v", err)
			}
			retry, err := s.DeleteSession(context.Background(), "s")
			if err != nil || !reflect.DeepEqual(retry, before.Info) {
				t.Fatalf("terminal retry = %+v, %v", retry, err)
			}
			assertDeletionUnchanged(t, s, "s", before)
		})
	}
}

func TestCheckWritableUnknownActiveAndReadFailure(t *testing.T) {
	s := deletionStore(t, ":memory:")
	if err := eventlog.CheckWritable(s.Session("fresh")); err != nil {
		t.Fatalf("fresh log refused: %v", err)
	}
	if _, err := s.SessionInfo("fresh"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("preflight created metadata: %v", err)
	}
	if err := s.PutSession(SessionMeta{UID: "active"}); err != nil {
		t.Fatal(err)
	}
	before := captureDeletion(t, s, "active")
	if err := eventlog.CheckWritable(s.Session("active")); err != nil {
		t.Fatal(err)
	}
	assertDeletionUnchanged(t, s, "active", before)
	if _, err := s.db.Exec(`DROP TABLE sessions`); err != nil {
		t.Fatal(err)
	}
	if err := eventlog.CheckWritable(s.Session("fresh")); err == nil {
		t.Fatal("preflight swallowed SQL read error")
	} else {
		var cause *sqlite.Error
		if !errors.As(err, &cause) || !strings.Contains(err.Error(), "fresh") {
			t.Fatalf("read cause/context lost: %v", err)
		}
	}
}

func TestDeleteSessionRollsBackEveryPhase(t *testing.T) {
	for _, tc := range []struct{ name, setup, phase string }{
		{"event", `CREATE TRIGGER fail_delete BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT,'delete event fault'); END;`, "append"},
		{"metadata", `CREATE TRIGGER fail_delete BEFORE UPDATE ON sessions BEGIN SELECT RAISE(ABORT,'delete row fault'); END;`, "update"},
		{"commit", `PRAGMA foreign_keys=ON;
			CREATE TABLE deletion_parent (id INTEGER PRIMARY KEY);
			CREATE TABLE deletion_child (id INTEGER REFERENCES deletion_parent(id) DEFERRABLE INITIALLY DEFERRED);
			CREATE TRIGGER fail_delete AFTER UPDATE ON sessions BEGIN INSERT INTO deletion_child VALUES(42); END;`, "commit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rollback.db")
			s := deletionStore(t, path)
			if _, err := s.Session("s").Append(0, 0, api.Event{Kind: api.EventInput, Message: api.TextMessage("user", "prefix")}); err != nil {
				t.Fatal(err)
			}
			before := captureDeletion(t, s, "s")
			if _, err := s.db.Exec(tc.setup); err != nil {
				t.Fatal(err)
			}
			_, err := s.DeleteSession(context.Background(), "s")
			var cause *sqlite.Error
			if err == nil || !errors.As(err, &cause) || !strings.Contains(err.Error(), `"s"`) || !strings.Contains(err.Error(), tc.phase) {
				t.Fatalf("delete fault did not retain UID/phase/sqlite cause: %v", err)
			}
			assertDeletionUnchanged(t, s, "s", before)
			if err := s.Session("s").Verify(); err != nil {
				t.Fatal(err)
			}
			s.Close()
			s = deletionStore(t, path)
			assertDeletionUnchanged(t, s, "s", before)
			if _, err := s.db.Exec(`DROP TRIGGER fail_delete`); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DeleteSession(context.Background(), "s"); err != nil {
				t.Fatalf("retry after rollback: %v", err)
			}
		})
	}
}

func TestDeleteSessionContextCausesAndNoMutation(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
		want error
	}{
		{"canceled", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		}, context.Canceled},
		{"expired", func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		}, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := deletionStore(t, ":memory:")
			if err := s.PutSession(SessionMeta{UID: "s"}); err != nil {
				t.Fatal(err)
			}
			before := captureDeletion(t, s, "s")
			ctx, cancel := tc.ctx()
			defer cancel()
			if _, err := s.DeleteSession(ctx, "s"); !errors.Is(err, tc.want) {
				t.Fatalf("context cause = %v, want %v", err, tc.want)
			}
			assertDeletionUnchanged(t, s, "s", before)
		})
	}
	t.Run("waiting-for-connection", func(t *testing.T) {
		s := deletionStore(t, ":memory:")
		if err := s.PutSession(SessionMeta{UID: "s"}); err != nil {
			t.Fatal(err)
		}
		before := captureDeletion(t, s, "s")
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		done := make(chan error, 1)
		go func() { _, err := s.DeleteSession(ctx, "s"); done <- err }()
		select {
		case err := <-done:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("waiting cause = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("Delete ignored context while sole connection was held")
		}
		tx.Rollback()
		assertDeletionUnchanged(t, s, "s", before)
	})
}

var deletionCancelFunctionID atomic.Uint64

// SQL triggers run the cancellation inside the real statement, after admission and (for update
// phases) after the event insert. No production fault hook or fake Store participates.
func TestDeleteSessionCancellationDuringSQLRollsBack(t *testing.T) {
	for _, tc := range []struct {
		name, trigger string
		deadline      bool
	}{
		{"event-insert", "BEFORE INSERT ON events", false},
		{"metadata-update", "BEFORE UPDATE ON sessions", false},
		{"after-metadata-update", "AFTER UPDATE ON sessions", false},
		{"deadline-during-update", "BEFORE UPDATE ON sessions", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ctx context.Context
			var cancel context.CancelFunc
			var reached atomic.Int32
			name := fmt.Sprintf("delete_cancel_%d", deletionCancelFunctionID.Add(1))
			if err := sqlite.RegisterScalarFunction(name, 0, func(_ *sqlite.FunctionContext, _ []driver.Value) (driver.Value, error) {
				reached.Add(1)
				if tc.deadline {
					<-ctx.Done()
				} else {
					cancel()
				}
				return int64(0), nil
			}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "sql-canceled.db")
			s := deletionStore(t, path)
			if _, err := s.Session("s").Append(0, 0, api.Event{Kind: api.EventInput, Message: api.TextMessage("user", "prefix")}); err != nil {
				t.Fatal(err)
			}
			before := captureDeletion(t, s, "s")
			if _, err := s.db.Exec(`CREATE TRIGGER cancel_delete ` + tc.trigger + ` BEGIN SELECT ` + name + `(); END`); err != nil {
				t.Fatal(err)
			}
			want := error(context.Canceled)
			if tc.deadline {
				ctx, cancel = context.WithTimeout(context.Background(), time.Second)
				want = context.DeadlineExceeded
			} else {
				ctx, cancel = context.WithCancel(context.Background())
			}
			defer cancel()
			_, err := s.DeleteSession(ctx, "s")
			if reached.Load() != 1 {
				t.Fatalf("did not cancel inside SQL: reached %d, err %v", reached.Load(), err)
			}
			if !errors.Is(err, want) || !strings.Contains(err.Error(), `"s"`) {
				t.Fatalf("SQL cancellation cause/context lost: %v, want %v", err, want)
			}
			t.Logf("SQL cancellation: %v", err)
			assertDeletionUnchanged(t, s, "s", before)
			if err := s.Session("s").Verify(); err != nil {
				t.Fatal(err)
			}
			s.Close()
			s = deletionStore(t, path)
			assertDeletionUnchanged(t, s, "s", before)
			if _, err := s.db.Exec(`DROP TRIGGER cancel_delete`); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DeleteSession(context.Background(), "s"); err != nil {
				t.Fatalf("retry after SQL cancellation: %v", err)
			}
		})
	}
}

func TestDeleteSessionCancellationWaitingForWriterLock(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprintf("deadline=%v", deadline), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "locked.db")
			holder, contender := deletionStore(t, path), deletionStore(t, path)
			if err := holder.PutSession(SessionMeta{UID: "s"}); err != nil {
				t.Fatal(err)
			}
			before := captureDeletion(t, holder, "s")
			tx, err := holder.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.Exec(`UPDATE sessions SET name = 'uncommitted-holder' WHERE session = 's'`); err != nil {
				t.Fatal(err)
			}
			var ctx context.Context
			var cancel context.CancelFunc
			want := error(context.Canceled)
			if deadline {
				ctx, cancel = context.WithTimeout(context.Background(), time.Second)
				want = context.DeadlineExceeded
			} else {
				ctx, cancel = context.WithCancel(context.Background())
			}
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := contender.DeleteSession(ctx, "s"); done <- err }()
			// The second Store owns its own connection but cannot acquire BEGIN IMMEDIATE.
			until := time.Now().Add(time.Second)
			for contender.db.Stats().InUse != 1 {
				if time.Now().After(until) {
					t.Fatal("contender never entered database operation")
				}
				time.Sleep(time.Millisecond)
			}
			select {
			case err := <-done:
				t.Fatalf("Delete passed the held writer lock: %v", err)
			case <-time.After(25 * time.Millisecond):
			}
			if deadline {
				<-ctx.Done()
			} else {
				cancel()
			}
			// SQLite's busy handler may not observe interrupt until the lock is released. Preserve
			// its existing timeout policy while proving cancellation cannot subsequently mutate.
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, want) {
					t.Fatalf("writer-lock cancellation = %v, want %v", err, want)
				}
				t.Logf("writer-lock cancellation: %v", err)
			case <-time.After(6 * time.Second):
				t.Fatal("Delete did not return after writer release")
			}
			assertDeletionUnchanged(t, holder, "s", before)
			assertDeletionUnchanged(t, contender, "s", before)
			if _, err := contender.DeleteSession(context.Background(), "s"); err != nil {
				t.Fatalf("retry after canceled lock wait: %v", err)
			}
		})
	}
}

func TestCheckWritablePreservesInMemoryStore(t *testing.T) {
	log := eventlog.AsStore(eventlog.New())
	if err := eventlog.CheckWritable(log); err != nil {
		t.Fatal(err)
	}
	fence, err := log.NewFence()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(0, fence, api.Event{Kind: api.EventInput, Message: api.TextMessage("user", "ordinary")}); err != nil {
		t.Fatal(err)
	}
	if err := log.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteSessionTwoStoreRaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "race.db")
	a, b := deletionStore(t, path), deletionStore(t, path)
	for _, operation := range []string{"append", "fence", "put", "active-harness-put", "delete"} {
		t.Run(operation, func(t *testing.T) {
			for iteration := 0; iteration < 12; iteration++ {
				uid := fmt.Sprintf("%s-%d", operation, iteration)
				if err := a.PutSession(SessionMeta{UID: uid, Name: "before"}); err != nil {
					t.Fatal(err)
				}
				start := make(chan struct{})
				deleted := make(chan error, 1)
				written := make(chan error, 1)
				go func() { <-start; _, err := a.DeleteSession(context.Background(), uid); deleted <- err }()
				go func() {
					<-start
					var err error
					switch operation {
					case "append":
						_, err = b.Session(uid).Append(0, 999, api.Event{Kind: api.EventInput, Message: api.TextMessage("user", "race")})
					case "fence":
						_, err = b.Session(uid).NewFence()
					case "put":
						err = b.PutSession(SessionMeta{UID: uid, Name: "writer"})
					case "active-harness-put":
						err = b.PutSessionOnActiveHarness(SessionMeta{UID: uid, Name: "writer", Harness: "static"}, false)
					case "delete":
						_, err = b.DeleteSession(context.Background(), uid)
					}
					written <- err
				}()
				close(start)
				if err := <-deleted; err != nil {
					t.Fatalf("delete: %v", err)
				}
				writeErr := <-written
				if writeErr != nil && !errors.Is(writeErr, eventlog.ErrDeleted) {
					t.Fatalf("writer leaked non-deleted error: %v", writeErr)
				}
				if operation == "delete" && writeErr != nil {
					t.Fatalf("double delete: %v", writeErr)
				}
				got := captureDeletion(t, a, uid)
				wantHead, wantFence := int64(1), int64(1)
				if writeErr == nil {
					if operation == "append" {
						wantHead = 2
					}
					if operation == "fence" {
						wantFence = 2
					}
				}
				if got.Info.DeletionState != api.DeletionPending || got.Info.LastSeq != wantHead || got.Info.DeletionSeq != wantHead || got.Fence != wantFence {
					t.Fatalf("race intent = %+v, want head/fence %d/%d", got, wantHead, wantFence)
				}
				if operation == "put" || operation == "active-harness-put" {
					wantName := "before"
					if writeErr == nil {
						wantName = "writer"
					}
					if got.Info.Name != wantName {
						t.Fatalf("writer/delete ordering lost metadata: %q, want %q", got.Info.Name, wantName)
					}
				}
				if err := b.Session(uid).Verify(); err != nil {
					t.Fatal(err)
				}
				retry, err := b.DeleteSession(context.Background(), uid)
				if err != nil || !reflect.DeepEqual(retry, got.Info) {
					t.Fatalf("cross-store retry = %+v,%v", retry, err)
				}
				assertDeletionUnchanged(t, a, uid, got)
			}
		})
	}
}
