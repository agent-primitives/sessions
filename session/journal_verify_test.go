package session_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
	"github.com/agent-primitives/sessions/wire"
)

// A journal whose hash chain fails verification is DATA_LOSS on Exec, Resume and Fork (CHAIN-12),
// and none of them calls the model or writes the journal.
func TestTamperedJournalIsDataLoss(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	store := openStore(t, path)
	db := deletionSQL(t, path)
	var models atomic.Int32
	backend := local.New(echoagent.Harness{})
	t.Cleanup(func() { _ = backend.Close() })
	registry, err := placement.NewRegistry("echo", map[string]*placement.Placer{"echo": placement.New(backend,
		func(ctx context.Context, req api.ModelRequest) (api.ModelResponse, error) {
			models.Add(1)
			return echoagent.Model(ctx, req)
		})})
	if err != nil {
		t.Fatal(err)
	}
	client := serveRegistry(t, store, registry)
	created, err := client.CreateSession(t.Context(), &v1.CreateSessionRequest{Session: &v1.Session{}})
	if err != nil {
		t.Fatal(err)
	}
	uid := created.GetMetadata().GetUid()
	exec := func() error {
		stream, err := client.Exec(t.Context(), &v1.ExecRequest{Session: uid, Inputs: []*v1.Message{wire.MessageToProto(api.TextMessage("user", "hi"))}})
		if err == nil {
			err = drainExec(stream)
		}
		return err
	}
	if err := exec(); err != nil {
		t.Fatal(err)
	}

	// The record before END is the turn's OUTPUT.
	head, err := store.Session(uid).Head()
	if err != nil {
		t.Fatal(err)
	}
	tamperEvent(t, db, uid, head-1, func(ev *api.Event) {
		if ev.Kind != api.EventOutput {
			t.Fatalf("seq %d is %v, want OUTPUT", head-1, ev.Kind)
		}
		ev.Message = api.TextMessage(ev.Message.Role, "tampered")
	})

	before := journalRows(t, db, uid)
	models.Store(0)
	for _, operation := range []string{"Exec", "Resume", "Fork"} {
		var err error
		switch operation {
		case "Exec":
			err = exec()
		case "Resume":
			_, err = client.Resume(t.Context(), &v1.ResumeRequest{Session: uid})
		case "Fork":
			_, err = client.Fork(t.Context(), &v1.ForkRequest{Session: uid})
		}
		if status.Code(err) != codes.DataLoss {
			t.Errorf("%s = %v, want DATA_LOSS", operation, err)
		}
	}
	if n := models.Load(); n != 0 {
		t.Errorf("model calls = %d", n)
	}
	if after := journalRows(t, db, uid); !reflect.DeepEqual(after, before) {
		t.Errorf("journal changed: %d records -> %d", len(before), len(after))
	}
}

// A pending approval is parked or repaired without running the controller, so placement verifies
// the chain itself: a recorded request or a call-only cut whose TOOL_CALL changed in storage is
// DATA_LOSS on Resume and on input-less Exec recovery, with no pending approval served, no
// Describe, no compute and no repair record.
func TestTamperedJournalRefusesApprovalParkAndRepair(t *testing.T) {
	for _, cut := range []api.EventKind{api.EventToolCall, api.EventApprovalRequest} {
		t.Run(string(cut), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal.db")
			store := openStore(t, path)
			db := deletionSQL(t, path)
			r, b := publicGateRegistry(t, &publicGateHarness{}, nil)
			c := serveRegistry(t, store, r)
			uid := mustCreate(t, c)
			seedPublicGate(t, store.Session(uid), "gate", cut)
			tamperEvent(t, db, uid, 2, func(ev *api.Event) {
				if ev.Kind != api.EventToolCall {
					t.Fatalf("seq 2 is %v, want TOOL_CALL", ev.Kind)
				}
				ev.ToolCall.Args = map[string]any{"path": "/etc/passwd"}
			})
			before := journalRows(t, db, uid)
			if _, err := c.Resume(t.Context(), &v1.ResumeRequest{Session: uid}); status.Code(err) != codes.DataLoss {
				t.Errorf("Resume = %v, want DATA_LOSS", err)
			}
			// Exec sends the session frame before the turn runs; nothing after it is served.
			frames, err := collectPublicExec(c, &v1.ExecRequest{Session: uid})
			if status.Code(err) != codes.DataLoss {
				t.Errorf("input-less Exec = %v, want DATA_LOSS", err)
			}
			if len(frames) != 1 || frames[0].GetSession() == nil {
				t.Errorf("input-less Exec sent %v, want only the leading session frame", frames)
			}
			if after := journalRows(t, db, uid); !reflect.DeepEqual(after, before) {
				t.Errorf("journal changed: %d records -> %d", len(before), len(after))
			}
			if b.ioCounts() != ([4]int32{}) {
				t.Errorf("runtime reached: %v", b.ioCounts())
			}
		})
	}
}

// tamperEvent rewrites the stored event at seq, keeping its prev_hash and hash.
func tamperEvent(t *testing.T, db *sql.DB, uid string, seq int64, change func(*api.Event)) {
	t.Helper()
	var blob []byte
	if err := db.QueryRow(`SELECT event FROM events WHERE session = ? AND seq = ?`, uid, seq).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	pb := &v1.Event{}
	if err := proto.Unmarshal(blob, pb); err != nil {
		t.Fatal(err)
	}
	ev := wire.EventFromProto(pb)
	change(&ev)
	blob, err := proto.Marshal(wire.EventToProto(ev))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE events SET event = ? WHERE session = ? AND seq = ?`, blob, uid, seq); err != nil {
		t.Fatal(err)
	}
}

// journalRows is every stored column of a session's events.
func journalRows(t *testing.T, db *sql.DB, uid string) [][]any {
	t.Helper()
	rows, err := db.Query(`SELECT seq, prev_hash, hash, fence, event FROM events WHERE session = ? ORDER BY seq`, uid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out [][]any
	for rows.Next() {
		var (
			seq, fence int64
			prev, hash string
			event      []byte
		)
		if err := rows.Scan(&seq, &prev, &hash, &fence, &event); err != nil {
			t.Fatal(err)
		}
		out = append(out, []any{seq, prev, hash, fence, event})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
