package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aramase/agentsessions/api"
	v1 "github.com/aramase/agentsessions/api/genpb"
	"github.com/aramase/agentsessions/controller"
	"github.com/aramase/agentsessions/eventlog"
	"github.com/aramase/agentsessions/harness/echoagent"
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
	started := func(harness string) []api.Event {
		return []api.Event{
			{Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{InputCount: &count, Harness: harness}},
			{Kind: api.EventInput, Message: &api.Message{Role: "user", Parts: []api.Part{{Text: &api.TextPart{Text: "hi"}}}}},
		}
	}
	turn := func(harness string) []api.Event {
		return append(started(harness), api.Event{Kind: api.EventEnd, End: &api.HarnessEnd{State: "COMPLETED"}})
	}

	t.Run("last completed turn's harness", func(t *testing.T) {
		protos := journal(t, append(turn("first"), turn("triage")...)...)
		_, recs, harness, err := loadJournal("s", protos)
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) != 6 || harness != "triage" || completedTurns(recs) != 2 {
			t.Fatalf("got %d records, %d completed, on harness %q, want 6, 2 on %q", len(recs), completedTurns(recs), harness, "triage")
		}
	})

	// A turn that started on another harness (an override) and never ended is not replayed, so it
	// does not choose the harness.
	t.Run("an incomplete turn on another harness", func(t *testing.T) {
		protos := journal(t, append(turn("triage"), started("override")...)...)
		_, recs, harness, err := loadJournal("s", protos)
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) != 5 || harness != "triage" || completedTurns(recs) != 1 {
			t.Fatalf("got %d records, %d completed, on harness %q, want 5, 1 on %q", len(recs), completedTurns(recs), harness, "triage")
		}
	})

	// A turn that started but never ended is skipped by the controller's Replay, so a journal with
	// no completed turn would re-execute nothing; it is refused rather than reported as replayed.
	t.Run("no completed turn", func(t *testing.T) {
		protos := journal(t, started("triage")...)
		if _, _, _, err := loadJournal("s", protos); err == nil || !strings.Contains(err.Error(), "no completed turn") {
			t.Fatalf("error = %v, want a no-completed-turn error", err)
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

func TestModelCalls(t *testing.T) {
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"calls":7}`) })
	mux.HandleFunc("/error", func(w http.ResponseWriter, _ *http.Request) {
		// A JSON error body without "calls" would decode as 0 calls.
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"x"}`)
	})
	mux.HandleFunc("/stall-headers", func(http.ResponseWriter, *http.Request) { <-release })
	mux.HandleFunc("/stall-body", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"calls":`)
		w.(http.Flusher).Flush()
		<-release
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	defer close(release) // runs before srv.Close, so the stalled handlers return

	if n, err := modelCalls(context.Background(), ""); n != 0 || err != nil {
		t.Fatalf("no stats URL: got %d, %v; want 0, nil", n, err)
	}
	if n, err := modelCalls(context.Background(), srv.URL+"/stats"); n != 7 || err != nil {
		t.Fatalf("stats: got %d, %v; want 7, nil", n, err)
	}
	if _, err := modelCalls(context.Background(), srv.URL+"/error"); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("error status: err = %v, want the status reported", err)
	}
	// A counter that stalls, before its headers or in the middle of its body, ends with the
	// caller's context instead of hanging the replay.
	for _, path := range []string{"/stall-headers", "/stall-body"} {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		start := time.Now()
		_, err := modelCalls(ctx, srv.URL+path)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s: err = %v, want the context deadline", path, err)
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Errorf("%s: returned after %v", path, d)
		}
	}
}

// TestLoadJournalReplays runs the controller's Replay with the harness loadJournal picks, over a
// journal whose completed turn ran on "triage" and whose last turn started on "override" and never
// ended: the replay re-executes the completed turn with no model call instead of failing with
// ErrHarnessMismatch.
func TestLoadJournalReplays(t *testing.T) {
	ctx := context.Background()
	live := eventlog.New()
	c, err := controller.New(eventlog.AsStore(live), echoagent.Model, controller.WithSessionUID("s"), controller.WithHarness("triage"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Exec(ctx, echoagent.Harness{}, []api.Message{*api.TextMessage("user", "ping")}, 0); err != nil {
		t.Fatal(err)
	}
	count := int64(1)
	if _, err := live.Append(live.Head(), live.NewFence(), api.Event{
		Kind:           api.EventExecutionStart,
		ExecutionID:    "incomplete-override",
		ExecutionStart: &api.ExecutionStart{InputCount: &count, Harness: "override"},
	}); err != nil {
		t.Fatal(err)
	}
	var protos []*v1.LogRecord
	for _, r := range live.Snapshot() {
		protos = append(protos, eventlog.RecordToProto(r))
	}

	log, _, harness, err := loadJournal("s", protos)
	if err != nil {
		t.Fatal(err)
	}
	if harness != "triage" {
		t.Fatalf("harness = %q, want triage", harness)
	}
	r, err := controller.New(eventlog.AsStore(log), echoagent.Model, controller.WithSessionUID("s"), controller.WithHarness(harness))
	if err != nil {
		t.Fatal(err)
	}
	outputs, err := r.Replay(ctx, echoagent.Harness{})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(outputs) != 1 || outputs[0] != "echo:ping" || r.ModelInvocations() != 0 {
		t.Fatalf("outputs %v with %d model calls, want [echo:ping] with 0", outputs, r.ModelInvocations())
	}

	// The harness of the incomplete turn is the one the old loader picked; Replay refuses it.
	m, err := controller.New(eventlog.AsStore(log), echoagent.Model, controller.WithSessionUID("s"), controller.WithHarness("override"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Replay(ctx, echoagent.Harness{}); !errors.Is(err, controller.ErrHarnessMismatch) {
		t.Fatalf("Replay on the override = %v, want ErrHarnessMismatch", err)
	}
}
