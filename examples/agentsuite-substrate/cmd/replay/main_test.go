package main

import (
	"strings"
	"testing"

	"github.com/aramase/agentsessions/api"
	v1 "github.com/aramase/agentsessions/api/genpb"
	"github.com/aramase/agentsessions/eventlog"
)

func journal(t *testing.T, evs ...api.Event) []*v1.LogRecord {
	t.Helper()
	log := eventlog.New()
	var out []*v1.LogRecord
	for _, ev := range evs {
		rec, err := log.Append(log.Head(), 0, ev)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, eventlog.RecordToProto(rec))
	}
	return out
}

func TestLoadJournal(t *testing.T) {
	count := int64(1)
	turn := func(harness string) []api.Event {
		return []api.Event{
			{Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{InputCount: &count, Harness: harness}},
			{Kind: api.EventInput, Message: &api.Message{Role: "user", Parts: []api.Part{{Text: &api.TextPart{Text: "hi"}}}}},
		}
	}

	t.Run("last turn's harness", func(t *testing.T) {
		protos := journal(t, append(turn("first"), turn("triage")...)...)
		_, recs, harness, err := loadJournal("s", protos)
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) != 4 || harness != "triage" {
			t.Fatalf("got %d records on harness %q, want 4 on %q", len(recs), harness, "triage")
		}
	})

	// A created session that never ran has no records; it is refused, not indexed.
	t.Run("no records", func(t *testing.T) {
		if _, _, _, err := loadJournal("s", nil); err == nil || !strings.Contains(err.Error(), "no journal records") {
			t.Fatalf("error = %v, want a no-records error", err)
		}
	})

	t.Run("tampered", func(t *testing.T) {
		protos := journal(t, turn("triage")...)
		protos[1].ContentHash = strings.Repeat("0", len(protos[1].ContentHash))
		if _, _, _, err := loadJournal("s", protos); err == nil || !strings.Contains(err.Error(), "does not verify") {
			t.Fatalf("error = %v, want a verification failure", err)
		}
	})
}
