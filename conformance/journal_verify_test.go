package conformance_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/sqlitelog"
	"github.com/agent-primitives/sessions/wire"
)

// CHAIN-12: an OUTPUT changed in storage without its content_hash is not served by reconstruction,
// re-driven by recovery, or extended by a new execution. Each refuses with the store's tamper error
// before the model is called or the journal grows.
func TestTamperedJournalRefusedBeforeUse(t *testing.T) {
	s, path := openFile(t)
	m := &countModel{}
	c, err := controller.New(s.Session("s"), m.call)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Exec(context.Background(), echoagent.Harness{}, []api.Message{*api.TextMessage("user", "hi")}, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var seq int64
	var blob []byte
	rows, err := db.Query(`SELECT seq, event FROM events WHERE session = 's' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var b []byte
		var n int64
		if err := rows.Scan(&n, &b); err != nil {
			t.Fatal(err)
		}
		pb := &v1.Event{}
		if err := proto.Unmarshal(b, pb); err != nil {
			t.Fatal(err)
		}
		if pb.GetKind() == v1.EventKind_EVENT_OUTPUT {
			seq, blob = n, b
		}
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	pb := &v1.Event{}
	if err := proto.Unmarshal(blob, pb); err != nil {
		t.Fatal(err)
	}
	ev := wire.EventFromProto(pb)
	ev.Message = api.TextMessage("assistant", "not what the model said")
	if blob, err = proto.Marshal(wire.EventToProto(ev)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE events SET event = ? WHERE session = 's' AND seq = ?`, blob, seq); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := sqlitelog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	log := s2.Session("s")
	head, err := log.Head()
	if err != nil {
		t.Fatal(err)
	}
	calls := m.n
	for _, op := range []string{"Replay", "Resume", "Exec"} {
		fresh, err := controller.New(log, m.call)
		if err != nil {
			t.Fatal(err)
		}
		switch op {
		case "Replay":
			_, err = fresh.Replay(context.Background(), echoagent.Harness{})
		case "Resume":
			_, err = fresh.Resume(context.Background(), echoagent.Harness{})
		case "Exec":
			err = fresh.Exec(context.Background(), echoagent.Harness{}, []api.Message{*api.TextMessage("user", "again")}, head)
		}
		if !errors.Is(err, sqlitelog.ErrTampered) {
			t.Errorf("%s = %v, want sqlitelog.ErrTampered", op, err)
		}
	}
	if m.n != calls {
		t.Errorf("model called %d times on a tampered journal", m.n-calls)
	}
	if got, err := log.Head(); err != nil || got != head {
		t.Errorf("head = %d, %v; want %d", got, err, head)
	}
}
