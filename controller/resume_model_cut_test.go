package controller_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/sqlitelog"
)

var (
	errProvider = errors.New("provider unavailable")
	errCrashed  = errors.New("process crashed")
)

// crashStore simulates a hard process death: once crashed, no further append becomes durable, so
// not even the best-effort ERROR event reaches the journal.
type crashStore struct {
	eventlog.Store
	crashed bool
}

func (s *crashStore) Append(expectedLastSeq, fence int64, ev api.Event) (eventlog.Record, error) {
	if s.crashed {
		return eventlog.Record{}, errCrashed
	}
	return s.Store.Append(expectedLastSeq, fence, ev)
}

// scriptedModel fails its first failures calls with errProvider, then answers like echoModel.
type scriptedModel struct {
	failures int
	calls    int
	onCall   func()
}

func (m *scriptedModel) call(ctx context.Context, req api.ModelRequest) (api.ModelResponse, error) {
	m.calls++
	if m.onCall != nil {
		m.onCall()
	}
	if m.calls <= m.failures {
		return api.ModelResponse{}, errProvider
	}
	return echoModel(ctx, req)
}

// retryHarness retries a failed model call once within the same Run, with the same request. It
// resumes turns run by echoHarness, so it describes itself with the same name and version.
type retryHarness struct{}

func (retryHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "echo", Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}}, nil
}

func (retryHarness) Run(ctx context.Context, s *api.Start, sink api.EventSink) error {
	req := api.ModelRequest{Model: "echo", Messages: []api.Message{s.Inputs[len(s.Inputs)-1]}}
	if _, err := sink.Model(ctx, req); err == nil {
		return nil
	}
	_, err := sink.Model(ctx, req)
	return err
}

// fallbackHarness handles a model error by emitting direct fallback output. Replay and recovery
// must return the model failure again rather than reinterpret that output as provider success.
type fallbackHarness struct {
	id      string
	crash   func()
	success *int
}

type swallowCorrelationHarness struct {
	first, later error
}

func (*swallowCorrelationHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "echo", Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}}, nil
}

func (h *swallowCorrelationHarness) Run(ctx context.Context, s *api.Start, sink api.EventSink) error {
	_, h.first = sink.Model(ctx, api.ModelRequest{Model: "echo", Messages: []api.Message{s.Inputs[0]}})
	h.later = sink.Output(ctx, "later")
	return nil
}

func (h fallbackHarness) Describe(context.Context) (api.Descriptor, error) {
	id := h.id
	if id == "" {
		id = "fallback"
	}
	return api.Descriptor{ID: id, Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}}, nil
}

func (h fallbackHarness) Run(ctx context.Context, s *api.Start, sink api.EventSink) error {
	_, err := sink.Model(ctx, api.ModelRequest{Model: "echo", Messages: []api.Message{s.Inputs[0]}})
	if err == nil {
		if h.success != nil {
			(*h.success)++
		}
		return errors.New("model unexpectedly succeeded")
	}
	if err := sink.Output(ctx, "fallback"); err != nil {
		return err
	}
	if h.crash != nil {
		h.crash()
	}
	return nil
}

func journalKinds(t *testing.T, log eventlog.Store) []api.EventKind {
	t.Helper()
	recs, err := log.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]api.EventKind, 0, len(recs))
	for _, r := range recs {
		kinds = append(kinds, r.Event.Kind)
	}
	return kinds
}

func assertKinds(t *testing.T, log eventlog.Store, want ...api.EventKind) {
	t.Helper()
	if got := journalKinds(t, log); !reflect.DeepEqual(got, want) {
		t.Fatalf("journal kinds = %v, want %v", got, want)
	}
}

// execCutAtModelCall runs one turn whose model call is cut after MODEL_CALL is durable. With
// crash, the process dies inside the provider call and nothing after MODEL_CALL is written;
// otherwise the provider fails and the turn records ERROR.
func execCutAtModelCall(t *testing.T, log eventlog.Store, crash bool) {
	t.Helper()
	store := &crashStore{Store: log}
	model := &scriptedModel{failures: 1}
	if crash {
		model.onCall = func() { store.crashed = true }
	}
	c, err := controller.New(store, model.call)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Exec(t.Context(), &echoHarness{}, []api.Message{msg("hi")}, 0); !errors.Is(err, errProvider) {
		t.Fatalf("exec: want provider error, got %v", err)
	}
	if model.calls != 1 {
		t.Fatalf("exec invoked the provider %d times, want 1", model.calls)
	}
	want := []api.EventKind{api.EventExecutionStart, api.EventInput, api.EventModelCall}
	if !crash {
		want = append(want, api.EventError)
	}
	assertKinds(t, log, want...)
}

// resumeOnce resumes with a fresh controller (a new incarnation) and returns Resume's error.
func resumeOnce(t *testing.T, log eventlog.Store, har api.Harness, model *scriptedModel) error {
	t.Helper()
	c, err := controller.New(log, model.call)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := c.Resume(t.Context(), har)
	if !resumed {
		t.Fatalf("resume did not re-drive the interrupted turn (err=%v)", err)
	}
	return err
}

// assertRecovered checks the recovered turn holds one MODEL_CALL with one OUTPUT, verifies, and
// replays with zero provider calls.
func assertRecovered(t *testing.T, log eventlog.Store) {
	t.Helper()
	records, err := log.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]api.EventKind, 0, len(records))
	var calls, outputs int
	var callID string
	for _, record := range records {
		kinds = append(kinds, record.Event.Kind)
		switch record.Event.Kind {
		case api.EventModelCall:
			calls++
			if record.Event.ModelCall == nil || record.Event.ModelCall.ResultCorrelation != api.ModelResultCorrelationByID || record.Event.ModelCall.ID == "" {
				t.Fatalf("recorded model call lacks ID correlation: %+v", record.Event.ModelCall)
			}
			callID = record.Event.ModelCall.ID
		case api.EventOutput:
			outputs++
			if record.Event.ModelCallID == nil || *record.Event.ModelCallID != callID {
				t.Fatalf("recorded model result ID = %v, want %q", record.Event.ModelCallID, callID)
			}
		}
	}
	if calls != 1 || outputs != 1 {
		t.Fatalf("journal %v has %d MODEL_CALL and %d OUTPUT, want exactly one each", kinds, calls, outputs)
	}
	if kinds[len(kinds)-1] != api.EventEnd {
		t.Fatalf("journal %v does not end the recovered turn", kinds)
	}
	if err := log.Verify(); err != nil {
		t.Fatalf("verify: %v", err)
	}
	replay := &scriptedModel{}
	c, err := controller.New(log, replay.call)
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.Replay(t.Context(), &echoHarness{})
	if err != nil {
		t.Fatalf("replay of the recovered turn: %v", err)
	}
	if want := []string{"echo:hi"}; !reflect.DeepEqual(out, want) {
		t.Fatalf("replay outputs = %v, want %v", out, want)
	}
	if replay.calls != 0 {
		t.Fatalf("replay invoked the provider %d times", replay.calls)
	}
}

// A provider error between MODEL_CALL and OUTPUT: Resume re-drives the recorded call with exactly
// one live provider call and records its completion after that MODEL_CALL.
func TestResumeAfterModelCallProviderError(t *testing.T) {
	log := memStore(t)
	execCutAtModelCall(t, log, false)

	model := &scriptedModel{}
	if err := resumeOnce(t, log, &echoHarness{}, model); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if model.calls != 1 {
		t.Fatalf("resume invoked the provider %d times, want 1", model.calls)
	}
	assertKinds(t, log, api.EventExecutionStart, api.EventInput, api.EventModelCall, api.EventError,
		api.EventOutput, api.EventEnd)
	assertRecovered(t, log)
}

// A hard crash during the provider call leaves MODEL_CALL as the journal's last event, with no
// ERROR. Resume completes the turn the same way.
func TestResumeAfterCrashDuringModelCall(t *testing.T) {
	log := memStore(t)
	execCutAtModelCall(t, log, true)

	model := &scriptedModel{}
	if err := resumeOnce(t, log, &echoHarness{}, model); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if model.calls != 1 {
		t.Fatalf("resume invoked the provider %d times, want 1", model.calls)
	}
	assertKinds(t, log, api.EventExecutionStart, api.EventInput, api.EventModelCall, api.EventOutput, api.EventEnd)
	assertRecovered(t, log)
}

// Recovery re-drives the cut call through completeModel, outside liveSink.Model; it must still
// get exactly one model_call span.
func TestResumeRedriveOfModelCallIsTraced(t *testing.T) {
	log := memStore(t)
	execCutAtModelCall(t, log, true)
	rec := controller.RecordSpans(t)
	if err := resumeOnce(t, log, &echoHarness{}, &scriptedModel{}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if n := controller.SpanCount(rec, "controller.model_call"); n != 1 {
		t.Fatalf("controller.model_call spans = %d, want 1", n)
	}
}

// ERROR events left by failed resumes stay in the journal as audit records but are not effects:
// they neither consume the pending MODEL_CALL nor block a later resume from completing it.
func TestResumeAfterFailedResumesAtModelCall(t *testing.T) {
	log := memStore(t)
	execCutAtModelCall(t, log, false)

	model := &scriptedModel{failures: 2}
	for i := range 2 {
		if err := resumeOnce(t, log, &echoHarness{}, model); !errors.Is(err, errProvider) {
			t.Fatalf("failed resume %d: want provider error, got %v", i+1, err)
		}
		if model.calls != i+1 {
			t.Fatalf("after failed resume %d the provider saw %d calls, want %d", i+1, model.calls, i+1)
		}
	}
	assertKinds(t, log, api.EventExecutionStart, api.EventInput, api.EventModelCall,
		api.EventError, api.EventError, api.EventError)

	if err := resumeOnce(t, log, &echoHarness{}, model); err != nil {
		t.Fatalf("resume after two failed resumes: %v", err)
	}
	if model.calls != 3 {
		t.Fatalf("provider saw %d calls across three resumes, want 3", model.calls)
	}
	assertKinds(t, log, api.EventExecutionStart, api.EventInput, api.EventModelCall,
		api.EventError, api.EventError, api.EventError, api.EventOutput, api.EventEnd)
	assertRecovered(t, log)
}

// A failed re-drive is a failed model call, recorded exactly as on the live path: the recorded
// MODEL_CALL stays unanswered and a harness retry within the same Run records its own MODEL_CALL
// and OUTPUT. No MODEL_CALL gets two OUTPUTs, and the turn replays with zero provider calls.
func TestResumeRetryAfterFailedRedrive(t *testing.T) {
	log := memStore(t)
	execCutAtModelCall(t, log, false)

	model := &scriptedModel{failures: 1}
	if err := resumeOnce(t, log, retryHarness{}, model); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if model.calls != 2 {
		t.Fatalf("provider saw %d calls, want 2 (failed re-drive, retry)", model.calls)
	}
	assertKinds(t, log, api.EventExecutionStart, api.EventInput, api.EventModelCall, api.EventError,
		api.EventModelCall, api.EventOutput, api.EventEnd)

	replay := &scriptedModel{}
	c, err := controller.New(log, replay.call)
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.Replay(t.Context(), retryHarness{})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if want := []string{"echo:hi"}; !reflect.DeepEqual(out, want) || replay.calls != 0 {
		t.Fatalf("replay outputs = %v with %d provider calls, want %v with 0", out, replay.calls, want)
	}
}

func TestModelResultIDsDistinguishProviderResultsFromFallbackOutput(t *testing.T) {
	log := memStore(t)
	store := &crashStore{Store: log}
	model := &scriptedModel{failures: 1}
	c, err := controller.New(store, model.call)
	if err != nil {
		t.Fatal(err)
	}
	h := fallbackHarness{crash: func() { store.crashed = true }}
	if err := c.Exec(t.Context(), h, []api.Message{msg("hi")}, 0); !errors.Is(err, errCrashed) {
		t.Fatalf("Exec = %v, want crash after fallback", err)
	}
	recs, err := log.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 4 || recs[2].Event.ModelCall == nil || recs[2].Event.ModelCall.ResultCorrelation != api.ModelResultCorrelationByID ||
		recs[3].Event.Kind != api.EventOutput || recs[3].Event.ModelCallID != nil {
		t.Fatalf("journal does not distinguish call and direct fallback: %+v", recs)
	}

	success := 0
	recoveryModel := &scriptedModel{}
	if err := resumeOnce(t, log, fallbackHarness{success: &success}, recoveryModel); err != nil {
		t.Fatalf("Resume = %v", err)
	}
	if recoveryModel.calls != 0 || success != 0 {
		t.Fatalf("Resume treated fallback as model success: provider=%d success-path=%d", recoveryModel.calls, success)
	}
	assertKinds(t, log, api.EventExecutionStart, api.EventInput, api.EventModelCall, api.EventOutput, api.EventEnd)

	replayModel := &scriptedModel{}
	replay, err := controller.New(log, replayModel.call)
	if err != nil {
		t.Fatal(err)
	}
	out, err := replay.Replay(t.Context(), fallbackHarness{success: &success})
	if err != nil || !reflect.DeepEqual(out, []string{"fallback"}) || replayModel.calls != 0 || success != 0 {
		t.Fatalf("Replay = outputs %v err %v provider=%d success-path=%d", out, err, replayModel.calls, success)
	}
}

func TestModelResultCorrelationIsExact(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*api.Event, *api.Event)
	}{
		{name: "wrong result ID", change: func(_ *api.Event, output *api.Event) { output.ModelCallID = stringPointer("other") }},
		{name: "empty result ID", change: func(_ *api.Event, output *api.Event) { output.ModelCallID = stringPointer("") }},
		{name: "empty call ID", change: func(call, _ *api.Event) { call.ModelCall.ID = "" }},
		{name: "unknown mode", change: func(call, _ *api.Event) { call.ModelCall.ResultCorrelation = 99 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := memStore(t)
			c, err := controller.New(source, echoModel)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Exec(t.Context(), &echoHarness{}, []api.Message{msg("hi")}, 0); err != nil {
				t.Fatal(err)
			}
			records, err := source.Read(1)
			if err != nil {
				t.Fatal(err)
			}
			tc.change(&records[2].Event, &records[3].Event)

			log := memStore(t)
			fence, err := log.NewFence()
			if err != nil {
				t.Fatal(err)
			}
			for i, record := range records {
				if _, err := log.Append(int64(i), fence, record.Event); err != nil {
					t.Fatal(err)
				}
			}
			replay, err := controller.New(log, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := replay.Replay(t.Context(), &echoHarness{}); !errors.Is(err, controller.ErrReplayDiverged) {
				t.Fatalf("Replay = %v, want ErrReplayDiverged", err)
			}
		})
	}
}

func TestResumeModelResultCorrelationIsExactAndSticky(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*api.Event, *api.Event)
	}{
		{name: "wrong result ID", change: func(_ *api.Event, output *api.Event) { output.ModelCallID = stringPointer("other") }},
		{name: "empty result ID", change: func(_ *api.Event, output *api.Event) { output.ModelCallID = stringPointer("") }},
		{name: "empty call ID", change: func(call, _ *api.Event) { call.ModelCall.ID = "" }},
		{name: "unknown mode", change: func(call, _ *api.Event) { call.ModelCall.ResultCorrelation = 99 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := memStore(t)
			store := &crashStore{Store: source}
			c, err := controller.New(store, echoModel)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Exec(t.Context(), &crashAfterModelHarness{crash: func() { store.crashed = true }}, []api.Message{msg("hi")}, 0); !errors.Is(err, errCrashed) {
				t.Fatal(err)
			}
			records, err := source.Read(1)
			if err != nil {
				t.Fatal(err)
			}
			tc.change(&records[2].Event, &records[3].Event)
			log := memStore(t)
			fence, err := log.NewFence()
			if err != nil {
				t.Fatal(err)
			}
			for i, record := range records {
				if _, err := log.Append(int64(i), fence, record.Event); err != nil {
					t.Fatal(err)
				}
			}
			model := &scriptedModel{}
			err = resumeOnce(t, log, &echoHarness{}, model)
			if !errors.Is(err, controller.ErrReplayDiverged) || model.calls != 0 {
				t.Fatalf("Resume = %v provider=%d, want divergence without provider", err, model.calls)
			}
			assertKinds(t, log, api.EventExecutionStart, api.EventInput, api.EventModelCall, api.EventOutput, api.EventError)
		})
	}
}

func TestModelResultCorrelationDivergenceStaysSticky(t *testing.T) {
	log := memStore(t)
	c, err := controller.New(log, echoModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Exec(t.Context(), &echoHarness{}, []api.Message{msg("hi")}, 0); err != nil {
		t.Fatal(err)
	}
	records, err := log.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	records[3].Event.ModelCallID = stringPointer("wrong")
	tampered := memStore(t)
	fence, err := tampered.NewFence()
	if err != nil {
		t.Fatal(err)
	}
	for i, record := range records {
		if _, err := tampered.Append(int64(i), fence, record.Event); err != nil {
			t.Fatal(err)
		}
	}
	h := &swallowCorrelationHarness{}
	replay, err := controller.New(tampered, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replay.Replay(t.Context(), h); !errors.Is(err, controller.ErrReplayDiverged) ||
		!errors.Is(h.first, controller.ErrReplayDiverged) || h.later != h.first {
		t.Fatalf("Replay=%v first=%v later=%v, want the same sticky divergence", err, h.first, h.later)
	}
}

func TestLegacyPositionalModelResultStillReplays(t *testing.T) {
	source := memStore(t)
	c, err := controller.New(source, echoModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Exec(t.Context(), &echoHarness{}, []api.Message{msg("hi")}, 0); err != nil {
		t.Fatal(err)
	}
	records, err := source.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	records[2].Event.ModelCall.ResultCorrelation = api.ModelResultCorrelationUnspecified
	records[3].Event.ModelCallID = nil
	legacy := memStore(t)
	fence, err := legacy.NewFence()
	if err != nil {
		t.Fatal(err)
	}
	for i, record := range records {
		if _, err := legacy.Append(int64(i), fence, record.Event); err != nil {
			t.Fatal(err)
		}
	}
	replay, err := controller.New(legacy, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := replay.Replay(t.Context(), &echoHarness{})
	if err != nil || !reflect.DeepEqual(out, []string{"echo:hi"}) {
		t.Fatalf("legacy Replay = %v, %v", out, err)
	}
}

func TestFailedLegacyRedriveRefusesImmediateFallback(t *testing.T) {
	log := memStore(t)
	execCutAtModelCall(t, log, true)
	records, err := log.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	if records[2].Event.ModelCall == nil {
		t.Fatal("missing recorded model call")
	}
	records[2].Event.ModelCall.ResultCorrelation = api.ModelResultCorrelationUnspecified
	legacy := memStore(t)
	fence, err := legacy.NewFence()
	if err != nil {
		t.Fatal(err)
	}
	for i, record := range records {
		if _, err := legacy.Append(int64(i), fence, record.Event); err != nil {
			t.Fatal(err)
		}
	}
	model := &scriptedModel{failures: 1}
	err = resumeOnce(t, legacy, fallbackHarness{id: "echo"}, model)
	if !errors.Is(err, controller.ErrReplayDiverged) || model.calls != 1 {
		t.Fatalf("Resume = %v provider=%d, want divergence after one failed re-drive", err, model.calls)
	}
	assertKinds(t, legacy, api.EventExecutionStart, api.EventInput, api.EventModelCall, api.EventError)
}

func TestFailedLegacyRedriveAllowsFreshCorrelatedRetry(t *testing.T) {
	log := memStore(t)
	execCutAtModelCall(t, log, true)
	records, err := log.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	records[2].Event.ModelCall.ResultCorrelation = api.ModelResultCorrelationUnspecified
	legacy := memStore(t)
	fence, err := legacy.NewFence()
	if err != nil {
		t.Fatal(err)
	}
	for i, record := range records {
		if _, err := legacy.Append(int64(i), fence, record.Event); err != nil {
			t.Fatal(err)
		}
	}
	model := &scriptedModel{failures: 1}
	if err := resumeOnce(t, legacy, retryHarness{}, model); err != nil {
		t.Fatalf("Resume = %v", err)
	}
	records, err = legacy.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	var calls []*api.ModelCall
	var resultID string
	for _, record := range records {
		if record.Event.ModelCall != nil {
			calls = append(calls, record.Event.ModelCall)
		}
		if record.Event.ModelCallID != nil {
			resultID = *record.Event.ModelCallID
		}
	}
	if len(calls) != 2 || calls[0].ResultCorrelation != api.ModelResultCorrelationUnspecified ||
		calls[1].ResultCorrelation != api.ModelResultCorrelationByID || calls[0].ID == calls[1].ID || resultID != calls[1].ID {
		t.Fatalf("retry correlation calls=%+v resultID=%q", calls, resultID)
	}
}

func stringPointer(value string) *string { return &value }

// A re-issued request whose input hash differs from the recorded MODEL_CALL is divergence: no
// provider call, nothing recorded for the call, and the turn stays recoverable by a matching
// harness.
func TestResumeAtModelCallInputHashMismatch(t *testing.T) {
	log := memStore(t)
	execCutAtModelCall(t, log, false)

	model := &scriptedModel{}
	err := resumeOnce(t, log, &echoHarness{flaky: true}, model)
	if !errors.Is(err, controller.ErrReplayDiverged) {
		t.Fatalf("want ErrReplayDiverged for a model input hash mismatch, got %v", err)
	}
	if model.calls != 0 {
		t.Fatalf("divergent resume invoked the provider %d times", model.calls)
	}
	assertKinds(t, log, api.EventExecutionStart, api.EventInput, api.EventModelCall, api.EventError, api.EventError)

	if err := resumeOnce(t, log, &echoHarness{}, model); err != nil {
		t.Fatalf("matching resume after a divergent one: %v", err)
	}
	if model.calls != 1 {
		t.Fatalf("provider saw %d calls, want 1", model.calls)
	}
	assertRecovered(t, log)
}

// Forking at a MODEL_CALL copies a call whose completion the parent did record (or may have
// recorded after the fork point). The child cannot see that, so its Resume treats the call as cut
// and invokes the provider once. This is intended: each such fork costs one provider call.
func TestForkAtModelCallResumeRedrivesOnce(t *testing.T) {
	s, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	parent, child := s.Session("parent"), s.Session("child")

	parentModel := &scriptedModel{}
	pc, err := controller.New(parent, parentModel.call)
	if err != nil {
		t.Fatal(err)
	}
	if err := pc.Exec(t.Context(), &echoHarness{}, []api.Message{msg("hi")}, 0); err != nil {
		t.Fatalf("parent exec: %v", err)
	}
	assertKinds(t, parent, api.EventExecutionStart, api.EventInput, api.EventModelCall, api.EventOutput, api.EventEnd)
	const modelCallSeq = 3
	if err := controller.Fork(parent, child, modelCallSeq); err != nil {
		t.Fatalf("fork: %v", err)
	}
	assertKinds(t, child, api.EventExecutionStart, api.EventInput, api.EventModelCall, api.EventLifecycle)

	childModel := &scriptedModel{}
	if err := resumeOnce(t, child, &echoHarness{}, childModel); err != nil {
		t.Fatalf("child resume: %v", err)
	}
	if childModel.calls != 1 {
		t.Fatalf("child resume invoked the provider %d times, want 1", childModel.calls)
	}
	if parentModel.calls != 1 {
		t.Fatalf("parent provider calls = %d, want 1 (the child must not touch the parent)", parentModel.calls)
	}
	assertKinds(t, child, api.EventExecutionStart, api.EventInput, api.EventModelCall, api.EventLifecycle,
		api.EventOutput, api.EventEnd)
	assertRecovered(t, child)
}

// usageAfterFailedModelHarness handles a model failure, reports usage, then the process dies
// before END. The unanswered MODEL_CALL is followed by a recorded effect, so it is not the crash
// point and Resume must not re-drive it.
type usageAfterFailedModelHarness struct{ crash func() }

func (usageAfterFailedModelHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "usage-after-failure", Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}}, nil
}

func (h usageAfterFailedModelHarness) Run(ctx context.Context, s *api.Start, sink api.EventSink) error {
	if _, err := sink.Model(ctx, api.ModelRequest{Model: "echo", Messages: []api.Message{s.Inputs[0]}}); err == nil {
		return errors.New("expected the model call to fail")
	}
	if err := sink.Usage(ctx, api.Usage{Model: "echo"}); err != nil {
		return err
	}
	if h.crash != nil {
		h.crash()
	}
	return nil
}

func TestResumeDoesNotRedriveNonTrailingModelCall(t *testing.T) {
	log := memStore(t)
	store := &crashStore{Store: log}
	model := &scriptedModel{failures: 1}
	c, err := controller.New(store, model.call)
	if err != nil {
		t.Fatal(err)
	}
	har := usageAfterFailedModelHarness{crash: func() { store.crashed = true }}
	if err := c.Exec(t.Context(), har, []api.Message{msg("hi")}, 0); !errors.Is(err, errCrashed) {
		t.Fatalf("exec: want crash, got %v", err)
	}
	assertKinds(t, log, api.EventExecutionStart, api.EventInput, api.EventModelCall, api.EventUsage)

	// The harness sees the same model failure it saw live, handles it the same way, and the turn
	// completes from the recorded effects alone.
	resume := &scriptedModel{}
	if err := resumeOnce(t, log, usageAfterFailedModelHarness{}, resume); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resume.calls != 0 {
		t.Fatalf("resume re-drove a MODEL_CALL that is not the crash point (%d provider calls)", resume.calls)
	}
	assertKinds(t, log, api.EventExecutionStart, api.EventInput, api.EventModelCall, api.EventUsage, api.EventEnd)
}

// A turn whose OUTPUT was recorded before the cut is served on Resume with zero provider calls.
func TestResumeAfterRecordedModelOutputMakesNoCall(t *testing.T) {
	log := memStore(t)
	store := &crashStore{Store: log}
	c, err := controller.New(store, echoModel)
	if err != nil {
		t.Fatal(err)
	}
	// Die right after OUTPUT commits, before END.
	har := &crashAfterModelHarness{crash: func() { store.crashed = true }}
	if err := c.Exec(t.Context(), har, []api.Message{msg("hi")}, 0); !errors.Is(err, errCrashed) {
		t.Fatalf("exec: want crash, got %v", err)
	}
	assertKinds(t, log, api.EventExecutionStart, api.EventInput, api.EventModelCall, api.EventOutput)

	model := &scriptedModel{}
	if err := resumeOnce(t, log, &echoHarness{}, model); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if model.calls != 0 {
		t.Fatalf("resume invoked the provider %d times for a recorded completion", model.calls)
	}
	assertKinds(t, log, api.EventExecutionStart, api.EventInput, api.EventModelCall, api.EventOutput, api.EventEnd)
	assertRecovered(t, log)
}

// crashAfterModelHarness behaves like echoHarness, then the process dies before END.
type crashAfterModelHarness struct{ crash func() }

func (*crashAfterModelHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "echo", Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}}, nil
}

func (h *crashAfterModelHarness) Run(ctx context.Context, s *api.Start, sink api.EventSink) error {
	if err := (&echoHarness{}).Run(ctx, s, sink); err != nil {
		return err
	}
	h.crash()
	return nil
}
