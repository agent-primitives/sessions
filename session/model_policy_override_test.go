package session_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// W441: creation on an unconfigured route must not make a newly created session legacy when
// it later selects a restricted route. A genuine pre-v6 empty session is the exception.
func TestAdversarialUnconfiguredToRestrictedOverride(t *testing.T) {
	for _, tc := range []struct {
		name, model, want               string
		legacy, reopen, fork, priorOpen bool
		code                            codes.Code
	}{
		{name: "new_default", want: "allowed"},
		{name: "new_explicit", model: "chosen", want: "chosen"},
		{name: "new_forbidden", model: "forbidden", code: codes.InvalidArgument},
		{name: "new_reopened", want: "allowed", reopen: true},
		{name: "new_after_open_turn", want: "allowed", priorOpen: true},
		{name: "new_fork", want: "allowed", fork: true},
		{name: "legacy_empty", model: "forbidden", want: "legacy", legacy: true},
		{name: "legacy_fork", model: "forbidden", want: "legacy", legacy: true, fork: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal.db")
			uid := "legacy-empty"
			if tc.legacy {
				// Insert with the old schema before migration, not with today's CreateSession. The
				// frozen fixture remains untouched; this copied v4 database has no model_policy column.
				path = copyJournal(t, schemaV4Fixture)
				if userVersionOf(t, path) != 4 {
					t.Fatal("legacy setup is not pre-v6")
				}
				db, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				_, err = db.Exec(`INSERT INTO sessions(session, project, name, harness, model, created_at, updated_at) VALUES(?, 'default', 'legacy-empty', 'open', ?, 1, 1)`, uid, tc.model)
				_ = db.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			store := openStore(t, path)
			var received atomic.Value
			var calls atomic.Int32
			entries := map[string]*placement.Placer{}
			backends := map[string]*publicGateBackend{}
			for _, name := range []string{"open", "restricted"} {
				b := &publicGateBackend{Backend: local.New(sessionPolicyHarness{})}
				t.Cleanup(func() { _ = b.Close() })
				backends[name] = b
				var opts []placement.Option
				if name == "restricted" {
					opts = append(opts, placement.WithModelPolicy(api.ModelPolicy{Default: "allowed", Allowed: []string{"allowed", "chosen"}}))
				}
				entries[name] = placement.New(b, func(_ context.Context, req api.ModelRequest) (api.ModelResponse, error) {
					received.Store(req.Model)
					calls.Add(1)
					return api.ModelResponse{Message: *api.TextMessage("assistant", "ok")}, nil
				}, opts...)
			}
			r, err := placement.NewRegistry("open", entries)
			if err != nil {
				t.Fatal(err)
			}
			client := serveRegistry(t, store, r)
			if !tc.legacy {
				sess, err := client.CreateSession(t.Context(), &v1.CreateSessionRequest{Session: &v1.Session{Harness: "open", Model: tc.model}})
				if err != nil {
					t.Fatal(err)
				}
				uid = sess.GetMetadata().GetUid()
			}
			if tc.priorOpen {
				if _, err := collectPublicExec(client, &v1.ExecRequest{Session: uid}); err != nil {
					t.Fatal(err)
				}
				if calls.Load() != 1 || received.Load() != "legacy" {
					t.Fatal("new session changed unconfigured-route behavior")
				}
				calls.Store(0)
				received.Store("")
			}
			if tc.reopen {
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				store = openStore(t, path)
				client = serveRegistry(t, store, r)
			}
			if tc.fork {
				forked, err := client.Fork(t.Context(), &v1.ForkRequest{Session: uid, Count: 1})
				if err != nil {
					t.Fatal(err)
				}
				uid = forked.GetChildren()[0].GetMetadata().GetUid()
			}
			info, err := store.SessionInfo(uid)
			if err != nil || info.ModelPolicy || info.ModelPolicyEligible == tc.legacy {
				t.Fatalf("birth flags: %+v,%v", info, err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			var flags, beforeFence int64
			if err := db.QueryRow(`SELECT model_policy,fence FROM sessions WHERE session=?`, uid).Scan(&flags, &beforeFence); err != nil {
				t.Fatal(err)
			}
			wantFlags := int64(2)
			if tc.legacy {
				wantFlags = 0
			}
			if flags != wantFlags {
				t.Fatalf("persisted flags=%d,want%d", flags, wantFlags)
			}
			before := routingRecords(t, store.Session(uid))
			_, err = collectPublicExec(client, &v1.ExecRequest{Session: uid, Harness: "restricted"})
			if status.Code(err) != tc.code {
				t.Fatalf("selected restricted route: %v,want%s", err, tc.code)
			}
			if tc.code != codes.OK {
				if calls.Load() != 0 || !reflect.DeepEqual(before, routingRecords(t, store.Session(uid))) {
					t.Fatal("rejected override called provider or changed journal")
				}
				var fence int64
				if err := db.QueryRow(`SELECT fence FROM sessions WHERE session=?`, uid).Scan(&fence); err != nil || fence != beforeFence {
					t.Fatalf("rejected override advanced fence: %d,%v", fence, err)
				}
				for name, b := range backends {
					if b.creates.Load() != 0 || b.restores.Load() != 0 || b.snapshots.Load() != 0 {
						t.Fatalf("rejected override changed %s compute: %v", name, b.ioCounts())
					}
				}
				return
			}
			if calls.Load() != 1 || received.Load() != tc.want {
				t.Fatalf("restricted provider received %v (%d calls),want%s", received.Load(), calls.Load(), tc.want)
			}
			var startModel string
			for _, record := range routingRecords(t, store.Session(uid)) {
				if record.Event.ExecutionStart != nil {
					startModel = record.Event.ExecutionStart.Model
				}
			}
			wantDefault := tc.want
			if tc.legacy {
				wantDefault = ""
			}
			if startModel != wantDefault {
				t.Fatalf("recorded default=%q,want%q", startModel, wantDefault)
			}
			if err := store.Session(uid).Verify(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
