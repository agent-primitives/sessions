package placement_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
	"github.com/agent-primitives/sessions/sqlitelog"
	"github.com/agent-primitives/sessions/wire"
)

// Resume of a pending approval runs no controller, so placement verifies the chain itself
// (CHAIN-12). A journal whose TOOL_CALL changed in storage is refused with ErrJournalTampered,
// wrapping eventlog.ErrTampered and sqlitelog.ErrTampered, both for a recorded request (the park
// that would otherwise be reported) and for a call-only cut (the repair that would otherwise
// append), through Placer.Resume and Registry.Resume, with no runtime IO and no record written.
// The untampered controls park, the call-only cut after repairing its request.
func TestApprovalResumeRefusesATamperedJournal(t *testing.T) {
	for _, cut := range []api.EventKind{api.EventToolCall, api.EventApprovalRequest} {
		for _, via := range []string{"placer", "registry"} {
			for _, tamper := range []bool{false, true} {
				name := string(cut) + "/" + via + "/untampered"
				if tamper {
					name = string(cut) + "/" + via + "/tampered"
				}
				t.Run(name, func(t *testing.T) {
					path := filepath.Join(t.TempDir(), "journal.db")
					store, err := sqlitelog.Open(path)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = store.Close() })
					log := store.Session("s")
					seedPlacementApproval(t, log, "recorded", cut)
					db, err := sql.Open("sqlite", path)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = db.Close() })
					if tamper {
						tamperSessionEvent(t, db, "s", 2, func(ev *api.Event) {
							if ev.Kind != api.EventToolCall {
								t.Fatalf("seq 2 is %v, want TOOL_CALL", ev.Kind)
							}
							ev.ToolCall.Args = map[string]any{"path": "/etc/passwd"}
						})
					}
					before := sessionRows(t, db, "s")

					runtime := local.New(placementGateHarness{})
					t.Cleanup(func() { _ = runtime.Close() })
					b := &approvalBackend{Backend: runtime}
					p := placement.New(b, nil)
					switch via {
					case "placer":
						err = p.Resume(context.Background(), log, "s")
					case "registry":
						r, rerr := placement.NewRegistry("recorded", map[string]*placement.Placer{"recorded": p})
						if rerr != nil {
							t.Fatal(rerr)
						}
						err = r.Resume(context.Background(), log, "s", "recorded")
					}
					b.assertNoCompute(t)
					after := sessionRows(t, db, "s")

					if !tamper {
						var park *api.ApprovalParkedError
						if !errors.As(err, &park) || park.Ref.RequestSeq != 3 {
							t.Fatalf("Resume = %v, want a park at request seq 3", err)
						}
						if want := 3; len(after) != want {
							t.Fatalf("journal has %d records, want %d", len(after), want)
						}
						return
					}
					if !errors.Is(err, controller.ErrJournalTampered) || !errors.Is(err, eventlog.ErrTampered) ||
						!errors.Is(err, sqlitelog.ErrTampered) {
						t.Fatalf("Resume = %v, want ErrJournalTampered wrapping sqlitelog.ErrTampered", err)
					}
					if errors.Is(err, api.ErrApprovalParked) {
						t.Fatalf("Resume = %v, reported a park", err)
					}
					if !reflect.DeepEqual(after, before) {
						t.Fatalf("journal changed: %d records -> %d", len(before), len(after))
					}
				})
			}
		}
	}
}

// tamperSessionEvent rewrites the stored event at seq, keeping its prev_hash and hash.
func tamperSessionEvent(t *testing.T, db *sql.DB, session string, seq int64, change func(*api.Event)) {
	t.Helper()
	var blob []byte
	if err := db.QueryRow(`SELECT event FROM events WHERE session = ? AND seq = ?`, session, seq).Scan(&blob); err != nil {
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
	if _, err := db.Exec(`UPDATE events SET event = ? WHERE session = ? AND seq = ?`, blob, session, seq); err != nil {
		t.Fatal(err)
	}
}

// sessionRows is every stored column of a session's events.
func sessionRows(t *testing.T, db *sql.DB, session string) [][]any {
	t.Helper()
	rows, err := db.Query(`SELECT seq, prev_hash, hash, fence, event FROM events WHERE session = ? ORDER BY seq`, session)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out [][]any
	for rows.Next() {
		var seq, fence int64
		var prev, hash string
		var event []byte
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
