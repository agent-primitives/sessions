package controller_test

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
	"github.com/agent-primitives/sessions/sqlitelog"
	"github.com/agent-primitives/sessions/wire"
)

// countingHarness is the echo harness with every call into it counted.
type countingHarness struct{ describes, runs int }

func (h *countingHarness) Describe(ctx context.Context) (api.Descriptor, error) {
	h.describes++
	return echoagent.Harness{}.Describe(ctx)
}

func (h *countingHarness) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	h.runs++
	return echoagent.Harness{}.Run(ctx, start, sink)
}

// modernRecords is one echo turn written by the current controller, cut after the last record of
// kind through ("" keeps the whole completed turn).
func modernRecords(t *testing.T, through api.EventKind) []eventlog.Record {
	t.Helper()
	log := eventlog.AsStore(eventlog.New())
	c, err := controller.New(log, echoagent.Model, controller.WithSessionUID("modern-session"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Exec(t.Context(), echoagent.Harness{}, []api.Message{msg("hello")}, 0); err != nil {
		t.Fatal(err)
	}
	records, err := log.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	if through == "" {
		return records
	}
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].Event.Kind == through {
			return records[:i+1]
		}
	}
	t.Fatalf("no %v record in %v", through, records)
	return nil
}

// tamperedJournal copies records into a SQLite journal, rewrites the message text of the last
// record of kind tamper ("" tampers nothing) without touching its stored hashes, as a bad manual edit
// or a corrupting migration would, and reopens the database.
func tamperedJournal(t *testing.T, records []eventlog.Record, tamper api.EventKind) eventlog.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal.db")
	store, err := sqlitelog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	log := store.Session("session")
	var fence int64
	for _, record := range records {
		for fence < record.Fence {
			if fence, err = log.NewFence(); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := log.Append(record.Seq-1, fence, record.Event); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if tamper != "" {
		seq := int64(0)
		for _, record := range records {
			if record.Event.Kind == tamper {
				seq = record.Seq
			}
		}
		if seq == 0 {
			t.Fatalf("no %v record to tamper", tamper)
		}
		tamperMessage(t, path, "session", seq)
	}
	store, err = sqlitelog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store.Session("session")
}

// tamperMessage replaces the stored message text at seq, keeping prev_hash and hash.
func tamperMessage(t *testing.T, path, session string, seq int64) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var blob []byte
	if err := db.QueryRow(`SELECT event FROM events WHERE session = ? AND seq = ?`, session, seq).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	pb := &v1.Event{}
	if err := proto.Unmarshal(blob, pb); err != nil {
		t.Fatal(err)
	}
	ev := wire.EventFromProto(pb)
	if ev.Message == nil {
		t.Fatalf("seq %d has no message", seq)
	}
	ev.Message = api.TextMessage(ev.Message.Role, "tampered "+ev.Message.Text())
	if blob, err = proto.Marshal(wire.EventToProto(ev)); err != nil {
		t.Fatal(err)
	}
	if res, err := db.Exec(`UPDATE events SET event = ? WHERE session = ? AND seq = ?`, blob, session, seq); err != nil {
		t.Fatal(err)
	} else if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("tampered %d rows", n)
	}
}

func assertJournalTampered(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, controller.ErrJournalTampered) || !errors.Is(err, sqlitelog.ErrTampered) || !errors.Is(err, eventlog.ErrTampered) {
		t.Fatalf("err = %v, want ErrJournalTampered wrapping sqlitelog.ErrTampered", err)
	}
}

// A tampered INPUT or OUTPUT, in a journal the current controller wrote and in one the v0.1.2
// controller wrote, refuses Replay, Resume and Exec before the harness is described or run, a model
// is called, or anything is appended. The same journals untampered are served, resumed and extended.
func TestTamperedJournalRefusedBeforeEffects(t *testing.T) {
	type journals struct {
		completed, pendingInput, pendingOutput func(*testing.T) []eventlog.Record
	}
	generations := map[string]journals{
		"modern": {
			completed:     func(t *testing.T) []eventlog.Record { return modernRecords(t, "") },
			pendingInput:  func(t *testing.T) []eventlog.Record { return modernRecords(t, api.EventInput) },
			pendingOutput: func(t *testing.T) []eventlog.Record { return modernRecords(t, api.EventOutput) },
		},
		"v0.1.2": {
			completed:     func(t *testing.T) []eventlog.Record { return v012Records(t, "completed") },
			pendingInput:  func(t *testing.T) []eventlog.Record { return v012Records(t, "crashed-input") },
			pendingOutput: func(t *testing.T) []eventlog.Record { return v012Records(t, "crashed-output") },
		},
	}
	for generation, fixture := range generations {
		for _, tamper := range []api.EventKind{"", api.EventInput, api.EventOutput} {
			name := "untampered"
			if tamper != "" {
				name = "tampered-" + string(tamper)
			}
			for _, operation := range []string{"Replay", "Resume", "Exec"} {
				t.Run(generation+"/"+name+"/"+operation, func(t *testing.T) {
					var records []eventlog.Record
					switch {
					case operation != "Resume":
						records = fixture.completed(t)
					case tamper == api.EventOutput:
						records = fixture.pendingOutput(t)
					default:
						records = fixture.pendingInput(t)
					}
					log := tamperedJournal(t, records, tamper)
					before, err := log.Read(1)
					if err != nil {
						t.Fatal(err)
					}
					models := 0
					model := func(ctx context.Context, req api.ModelRequest) (api.ModelResponse, error) {
						models++
						return echoagent.Model(ctx, req)
					}
					c, err := v012Controller(log, model)
					if err != nil {
						t.Fatal(err)
					}
					har := &countingHarness{}
					switch operation {
					case "Replay":
						_, err = c.Replay(t.Context(), har)
					case "Resume":
						var resumed bool
						resumed, err = c.Resume(t.Context(), har)
						if tamper == "" && !resumed {
							t.Fatal("untampered pending turn was not resumed")
						}
					case "Exec":
						err = c.Exec(t.Context(), har, []api.Message{msg("next")}, int64(len(before)))
					}
					after, readErr := log.Read(1)
					if readErr != nil {
						t.Fatal(readErr)
					}
					if tamper == "" {
						if err != nil {
							t.Fatalf("untampered %s: %v", operation, err)
						}
						if har.runs == 0 || operation != "Replay" && len(after) <= len(before) {
							t.Fatalf("untampered %s did no work: runs=%d records %d -> %d", operation, har.runs, len(before), len(after))
						}
						if err := log.Verify(); err != nil {
							t.Fatal(err)
						}
						return
					}
					assertJournalTampered(t, err)
					if har.describes != 0 || har.runs != 0 || models != 0 {
						t.Errorf("effects before refusal: describes=%d runs=%d models=%d", har.describes, har.runs, models)
					}
					if !reflect.DeepEqual(after, before) {
						t.Errorf("journal changed: %d records -> %d", len(before), len(after))
					}
				})
			}
		}
	}
}

// The in-memory store gets the same check: an event changed after it was appended, here through an
// alias of the recorded message, fails before Replay serves it or Outputs returns it.
func TestTamperedInMemoryJournalRefused(t *testing.T) {
	inner := eventlog.New()
	log := eventlog.AsStore(inner)
	c, err := controller.New(log, echoagent.Model)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Exec(t.Context(), echoagent.Harness{}, []api.Message{msg("hello")}, 0); err != nil {
		t.Fatal(err)
	}
	records := inner.Snapshot()
	for i := range records {
		if records[i].Event.Kind == api.EventOutput {
			changed := *records[i].Event.Message
			changed.Parts = []api.Part{{Text: &api.TextPart{Text: "tampered"}}}
			records[i].Event.Message = &changed
		}
	}
	tampered := eventlog.NewFrom(records)
	if err := tampered.Verify(); !errors.Is(err, eventlog.ErrTampered) {
		t.Fatalf("Verify = %v, want ErrTampered", err)
	}
	fresh, err := controller.New(eventlog.AsStore(tampered), func(context.Context, api.ModelRequest) (api.ModelResponse, error) {
		t.Fatal("model called")
		return api.ModelResponse{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	har := &countingHarness{}
	if _, err := fresh.Replay(t.Context(), har); !errors.Is(err, controller.ErrJournalTampered) || !errors.Is(err, eventlog.ErrTampered) {
		t.Fatalf("Replay = %v", err)
	}
	if out, err := fresh.Outputs(); !errors.Is(err, controller.ErrJournalTampered) || out != nil {
		t.Fatalf("Outputs = %v, %v", out, err)
	}
	if har.describes != 0 || har.runs != 0 {
		t.Errorf("harness reached: describes=%d runs=%d", har.describes, har.runs)
	}
}

// A controller verifies its journal once and then only what was appended since; a record appended
// by another writer, a shorter journal, or a changed verified head is still refused.
func TestJournalVerificationCachedHead(t *testing.T) {
	for _, change := range []string{"tampered-tail", "truncated", "rehashed-head"} {
		t.Run(change, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal.db")
			store, err := sqlitelog.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			log := store.Session("session")
			writer, err := controller.New(log, echoagent.Model)
			if err != nil {
				t.Fatal(err)
			}
			if err := writer.Exec(t.Context(), echoagent.Harness{}, []api.Message{msg("one")}, 0); err != nil {
				t.Fatal(err)
			}
			reader, err := controller.New(log, echoagent.Model)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reader.Replay(t.Context(), echoagent.Harness{}); err != nil {
				t.Fatal(err)
			}
			head, err := log.Head()
			if err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			switch change {
			case "tampered-tail":
				other, err := controller.New(log, echoagent.Model)
				if err != nil {
					t.Fatal(err)
				}
				if err := other.Exec(t.Context(), echoagent.Harness{}, []api.Message{msg("two")}, head); err != nil {
					t.Fatal(err)
				}
				var seq int64
				recs, err := log.Read(head + 1)
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range recs {
					if r.Event.Kind == api.EventInput {
						seq = r.Seq
					}
				}
				tamperMessage(t, path, "session", seq)
			case "truncated":
				if _, err := db.Exec(`DELETE FROM events WHERE session = 'session' AND seq = ?`, head); err != nil {
					t.Fatal(err)
				}
			case "rehashed-head":
				if _, err := db.Exec(`UPDATE events SET hash = 'rewritten' WHERE session = 'session' AND seq = ?`, head); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := reader.Replay(t.Context(), echoagent.Harness{}); !errors.Is(err, controller.ErrJournalTampered) || !errors.Is(err, sqlitelog.ErrTampered) {
				t.Fatalf("Replay after %s = %v", change, err)
			}
		})
	}
}

// Fork re-hashes the copied prefix into a fresh chain, so it must refuse a changed parent record
// before the child is fenced or written; a change after the fork point is not copied and does not
// block the fork.
func TestForkRefusesTamperedPrefix(t *testing.T) {
	records := modernRecords(t, "")
	var input, output int64
	for _, r := range records {
		switch r.Event.Kind {
		case api.EventInput:
			input = r.Seq
		case api.EventOutput:
			output = r.Seq
		}
	}
	parent := tamperedJournal(t, records, api.EventOutput)
	for _, tc := range []struct {
		name  string
		atSeq int64
		ok    bool
	}{
		{"tampered record copied", output, false},
		{"tampered record after the fork point", input, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			child := eventlog.AsStore(eventlog.New())
			err := controller.Fork(parent, child, tc.atSeq)
			if tc.ok {
				if err != nil {
					t.Fatal(err)
				}
				if err := child.Verify(); err != nil {
					t.Fatal(err)
				}
				return
			}
			assertJournalTampered(t, err)
			if head, _ := child.Head(); head != 0 {
				t.Errorf("child head = %d", head)
			}
			if fence, _ := child.NewFence(); fence != 1 {
				t.Errorf("child was fenced before the refusal: next fence %d", fence)
			}
		})
	}
}
