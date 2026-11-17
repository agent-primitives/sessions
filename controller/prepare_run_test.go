package controller_test

import (
	"context"
	"errors"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
)

type preparedRunHarness struct {
	executionConfigHarness
	prepareErr            error
	prepareValueOnErr     bool
	prepare, runs, closes int
	beforeRun             func()
	runErr                error
}

func (h *preparedRunHarness) PrepareRun(context.Context) (api.PreparedHarnessRun, error) {
	h.prepare++
	if h.prepareErr != nil {
		if h.prepareValueOnErr {
			return &testPreparedRun{h: h}, h.prepareErr
		}
		return nil, h.prepareErr
	}
	return &testPreparedRun{h: h}, nil
}

type testPreparedRun struct {
	h      *preparedRunHarness
	used   bool
	closed bool
}

func (r *testPreparedRun) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	if r.used {
		return errors.New("prepared run reused")
	}
	r.used = true
	r.h.runs++
	if r.h.beforeRun != nil {
		r.h.beforeRun()
	}
	if r.h.runErr != nil {
		return r.h.runErr
	}
	return r.h.executionConfigHarness.Run(ctx, start, sink)
}

func (r *testPreparedRun) Close() {
	if !r.closed {
		r.closed = true
		r.h.closes++
	}
}

func TestExecPreparesHarnessBeforeFirstAppend(t *testing.T) {
	log := eventlog.AsStore(eventlog.New())
	c, err := controller.New(log, echoModel)
	if err != nil {
		t.Fatal(err)
	}
	h := &preparedRunHarness{}
	h.beforeRun = func() {
		recs, err := log.Read(1)
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) != 2 || recs[0].Event.Kind != api.EventExecutionStart || recs[1].Event.Kind != api.EventInput {
			t.Fatalf("prepared Run saw records %+v, want committed start and input", recs)
		}
	}
	if err := c.Exec(t.Context(), h, []api.Message{msg("hello")}, 0); err != nil {
		t.Fatal(err)
	}
	if h.prepare != 1 || h.runs != 1 || h.closes != 1 {
		t.Fatalf("prepare/run/close=%d/%d/%d, want 1/1/1", h.prepare, h.runs, h.closes)
	}
}

func TestExecPrepareFailureLeavesJournalUntouched(t *testing.T) {
	log := eventlog.AsStore(eventlog.New())
	c, err := controller.New(log, echoModel)
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("connect failed before Start")
	h := &preparedRunHarness{prepareErr: cause}
	err = c.Exec(t.Context(), h, []api.Message{msg("hello")}, 0)
	if !errors.Is(err, controller.ErrHarnessPrepareFailed) || !errors.Is(err, cause) {
		t.Fatalf("Exec=%v, want preparation failure retaining %v", err, cause)
	}
	if head, headErr := log.Head(); headErr != nil || head != 0 {
		t.Fatalf("head=%d err=%v, want untouched journal", head, headErr)
	}
	if h.prepare != 1 || h.runs != 0 || h.closes != 0 {
		t.Fatalf("prepare/run/close=%d/%d/%d, want 1/0/0", h.prepare, h.runs, h.closes)
	}
}

func TestExecDefensivelyClosesPreparedValueReturnedWithError(t *testing.T) {
	log := eventlog.AsStore(eventlog.New())
	c, err := controller.New(log, echoModel)
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("prepare failed after allocation")
	h := &preparedRunHarness{prepareErr: cause, prepareValueOnErr: true}
	err = c.Exec(t.Context(), h, nil, 0)
	if !errors.Is(err, controller.ErrHarnessPrepareFailed) || !errors.Is(err, cause) {
		t.Fatalf("Exec=%v, want preparation failure retaining %v", err, cause)
	}
	if h.prepare != 1 || h.runs != 0 || h.closes != 1 {
		t.Fatalf("prepare/run/close=%d/%d/%d, want 1/0/1", h.prepare, h.runs, h.closes)
	}
}

func TestExecClosesPreparedRunWhenAppendFails(t *testing.T) {
	cause := errors.New("append failed")
	log := rejectAppendStore{Store: eventlog.AsStore(eventlog.New()), err: cause}
	c, err := controller.New(log, echoModel)
	if err != nil {
		t.Fatal(err)
	}
	h := &preparedRunHarness{}
	err = c.Exec(t.Context(), h, []api.Message{msg("hello")}, 0)
	if !errors.Is(err, cause) || errors.Is(err, controller.ErrHarnessPrepareFailed) {
		t.Fatalf("Exec=%v, want append failure %v", err, cause)
	}
	if h.prepare != 1 || h.runs != 0 || h.closes != 1 {
		t.Fatalf("prepare/run/close=%d/%d/%d, want 1/0/1", h.prepare, h.runs, h.closes)
	}
}

func TestExecClosesPreparedRunWhenInputAppendFails(t *testing.T) {
	cause := errors.New("input append failed")
	base := eventlog.AsStore(eventlog.New())
	log := &failInputStore{Store: base, failAt: 2, err: cause}
	c, err := controller.New(log, echoModel)
	if err != nil {
		t.Fatal(err)
	}
	h := &preparedRunHarness{}
	err = c.Exec(t.Context(), h, []api.Message{msg("one"), msg("two")}, 0)
	if !errors.Is(err, cause) || errors.Is(err, controller.ErrHarnessPrepareFailed) {
		t.Fatalf("Exec=%v, want input append failure %v", err, cause)
	}
	if h.prepare != 1 || h.runs != 0 || h.closes != 1 {
		t.Fatalf("prepare/run/close=%d/%d/%d, want 1/0/1", h.prepare, h.runs, h.closes)
	}
	recs, readErr := base.Read(1)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(recs) != 2 || recs[0].Event.Kind != api.EventExecutionStart || recs[1].Event.Kind != api.EventInput {
		t.Fatalf("records=%+v, want existing incomplete invocation", recs)
	}
}

func TestExecPreparedRunFailureRemainsDurable(t *testing.T) {
	log := eventlog.AsStore(eventlog.New())
	c, err := controller.New(log, echoModel)
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("Start send outcome unknown")
	h := &preparedRunHarness{runErr: cause}
	err = c.Exec(t.Context(), h, []api.Message{msg("hello")}, 0)
	if !errors.Is(err, cause) || errors.Is(err, controller.ErrHarnessPrepareFailed) {
		t.Fatalf("Exec=%v, want Run failure %v without preparation marker", err, cause)
	}
	recs, readErr := log.Read(1)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(recs) != 3 || recs[0].Event.Kind != api.EventExecutionStart || recs[1].Event.Kind != api.EventInput || recs[2].Event.Kind != api.EventError {
		t.Fatalf("records=%+v, want durable failed execution", recs)
	}
	if h.prepare != 1 || h.runs != 1 || h.closes != 1 {
		t.Fatalf("prepare/run/close=%d/%d/%d, want 1/1/1", h.prepare, h.runs, h.closes)
	}
}
