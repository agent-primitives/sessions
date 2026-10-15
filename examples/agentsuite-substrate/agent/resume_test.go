package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
)

// endFailStore refuses to append END while failEnd is set, which leaves a turn whose effects are
// journaled but which never ended: what a host that dies after Run returns, or an END append
// refused by a fence takeover or an I/O error, leaves behind.
type endFailStore struct {
	eventlog.Store
	failEnd bool
}

func (s *endFailStore) Append(expectedLastSeq, fence int64, ev api.Event) (eventlog.Record, error) {
	if s.failEnd && ev.Kind == api.EventEnd {
		return eventlog.Record{}, errors.New("journal down")
	}
	return s.Store.Append(expectedLastSeq, fence, ev)
}

// countingModel answers reply-<n> on the nth call, with usage, and records each request.
type countingModel struct {
	requests []api.ModelRequest
}

func (m *countingModel) call(_ context.Context, req api.ModelRequest) (api.ModelResponse, error) {
	m.requests = append(m.requests, req)
	return api.ModelResponse{
		Message: *api.TextMessage("assistant", fmt.Sprintf("reply-%d", len(m.requests))),
		Usage:   api.Usage{Model: req.Model, InputTokens: 10, OutputTokens: 2},
	}, nil
}

func texts(messages []api.Message) []string {
	var got []string
	for _, m := range messages {
		got = append(got, m.Role+":"+m.Text())
	}
	return got
}

// A memory-tier turn already in RAM whose END the journal does not hold is resumed by the host
// running the same execution again. The harness rebuilds it from the transcript held before the
// turn, so the request matches the recorded MODEL_CALL, the recorded completion is served, and RAM
// holds the turn once for the next one.
func TestMemoryTierResumesTurnWhoseEndWasLost(t *testing.T) {
	store := &endFailStore{Store: eventlog.AsStore(eventlog.New()), failEnd: true}
	model := &countingModel{}
	h := Harness{ID: "writer", Model: "m", Instructions: "write"}.WithMemory()

	c, err := controller.New(store, model.call)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Exec(t.Context(), h, []api.Message{*api.TextMessage("user", "first")}, 0); err == nil {
		t.Fatal("exec with END refused: err = nil")
	}
	if got := len(h.memory.messages); got != 2 {
		t.Fatalf("RAM holds %d messages after the turn, want 2 (the turn ran before END failed)", got)
	}

	// A new incarnation, as after the host died, resumes the open turn on the same actor.
	store.failEnd = false
	c, err = controller.New(store, model.call)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := c.Resume(t.Context(), h)
	if err != nil || !resumed {
		t.Fatalf("resume: resumed = %v, err = %v; want the open turn completed", resumed, err)
	}
	if len(model.requests) != 1 {
		t.Fatalf("provider saw %d calls, want 1 (resume serves the recorded completion)", len(model.requests))
	}

	head, err := store.Head()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Exec(t.Context(), h, []api.Message{*api.TextMessage("user", "second")}, head); err != nil {
		t.Fatalf("next turn: %v", err)
	}
	got := texts(model.requests[len(model.requests)-1].Messages)
	want := []string{"system:write", "user:first", "assistant:reply-1", "user:second"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("next turn's messages = %v, want %v", got, want)
	}
	if h.memory.turns != 2 {
		t.Fatalf("RAM counts %d turns, want 2", h.memory.turns)
	}
	if err := store.Verify(); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// Running an execution again replaces that turn rather than adding to it, keeps RAM as it was when
// the re-run fails, and leaves a new execution ID building on the replaced turn.
func TestMemoryTierRerunReplacesTheTurn(t *testing.T) {
	h := Harness{ID: "writer", Model: "m", Instructions: "write"}.WithMemory()
	sink := &replySink{reply: "one"}
	run := func(id, input string) error {
		return h.Run(t.Context(), &api.Start{ExecutionID: id, Inputs: []api.Message{*api.TextMessage("user", input)}}, sink)
	}
	if err := run("e1", "first"); err != nil {
		t.Fatal(err)
	}
	sink.fail = true
	if err := run("e1", "first"); err == nil {
		t.Fatal("failed re-run: err = nil")
	}
	if got, want := texts(h.memory.messages), []string{"user:first", "assistant:one"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("RAM after a failed re-run = %v, want %v", got, want)
	}
	sink.fail = false
	sink.reply = "one again"
	if err := run("e1", "first"); err != nil {
		t.Fatal(err)
	}
	if got, want := texts(sink.requests[len(sink.requests)-1].Messages), []string{"system:write", "user:first"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("re-run request = %v, want %v", got, want)
	}
	sink.reply = "two"
	if err := run("e2", "second"); err != nil {
		t.Fatal(err)
	}
	want := []string{"system:write", "user:first", "assistant:one again", "user:second"}
	if got := texts(sink.requests[len(sink.requests)-1].Messages); !reflect.DeepEqual(got, want) {
		t.Fatalf("next turn's request = %v, want %v", got, want)
	}
	if h.memory.turns != 2 {
		t.Fatalf("RAM counts %d turns, want 2", h.memory.turns)
	}
}
