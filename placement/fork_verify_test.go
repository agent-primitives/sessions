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
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/sqlitelog"
	"github.com/agent-primitives/sessions/wire"
)

// A parent whose copied prefix fails hash-chain verification is refused before Fork checkpoints it or
// provisions a child (CHAIN-12). For a memory-tier harness that means no snapshot, no SUSPEND on the
// parent and no clone; for a stateless one, no child compute. The parent journal is left byte for
// byte as it was and the child stays empty. The untampered control forks.
func TestForkRefusesATamperedParentBeforeEffects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		memory bool
		tamper bool
	}{
		{"memory/untampered", true, false},
		{"memory/tampered", true, true},
		{"stateless/untampered", false, false},
		{"stateless/tampered", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal.db")
			store, err := sqlitelog.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			parent, child := store.Session("parent"), store.Session("child")

			ctl := &cloningControl{snapshot: "snap-parent-1"}
			var p *placement.Placer
			if tc.memory {
				p = memorySubstratePlacer(ctl)
				fence, err := parent.NewFence()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := parent.Append(0, fence, api.Event{Kind: api.EventInput, Message: api.TextMessage("user", "hi")}); err != nil {
					t.Fatal(err)
				}
			} else {
				p = newLocalPlacer(t, echoagent.Harness{})
				if _, err := p.Exec(context.Background(), parent, "parent", []api.Message{*api.TextMessage("user", "hi")}, 0); err != nil {
					t.Fatal(err)
				}
			}
			head, err := parent.Head()
			if err != nil {
				t.Fatal(err)
			}

			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if tc.tamper {
				tamperParentInput(t, db)
			}
			before := parentRows(t, db)

			err = p.Fork(context.Background(), parent, "parent", []placement.ForkChild{{UID: "child", Log: child}}, head)
			if !tc.tamper {
				if err != nil {
					t.Fatalf("fork: %v", err)
				}
				if recs, _ := child.Read(1); len(recs) != int(head)+1 {
					t.Fatalf("child has %d records, want %d", len(recs), head+1)
				}
				return
			}
			if !errors.Is(err, controller.ErrJournalTampered) || !errors.Is(err, eventlog.ErrTampered) {
				t.Fatalf("fork err = %v, want ErrJournalTampered", err)
			}
			if len(ctl.calls) != 0 {
				t.Errorf("control plane reached: %v", ctl.calls)
			}
			if after := parentRows(t, db); !reflect.DeepEqual(after, before) {
				t.Errorf("parent journal changed: %d rows -> %d", len(before), len(after))
			}
			if recs, err := child.Read(1); err != nil || len(recs) != 0 {
				t.Errorf("child has %d records (err %v), want none", len(recs), err)
			}
		})
	}
}

// tamperParentInput rewrites the text of the parent's first INPUT, keeping its stored hashes.
func tamperParentInput(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, row := range parentRows(t, db) {
		pb := &v1.Event{}
		if err := proto.Unmarshal(row[4].([]byte), pb); err != nil {
			t.Fatal(err)
		}
		ev := wire.EventFromProto(pb)
		if ev.Kind != api.EventInput {
			continue
		}
		ev.Message = api.TextMessage(ev.Message.Role, "tampered")
		blob, err := proto.Marshal(wire.EventToProto(ev))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE events SET event = ? WHERE session = 'parent' AND seq = ?`, blob, row[0]); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatal("parent has no INPUT")
}

// parentRows is every stored column of the parent's events.
func parentRows(t *testing.T, db *sql.DB) [][]any {
	t.Helper()
	rows, err := db.Query(`SELECT seq, prev_hash, hash, fence, event FROM events WHERE session = 'parent' ORDER BY seq`)
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
