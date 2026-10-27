package e2e

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/client"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/substrate"
)

func TestExecutionStatePending(t *testing.T) {
	f := newExecutionStateFixture(t)
	f.assertState(t, f.uid, v1.ExecState_EXEC_PENDING)
	f.reopen(t)
	f.assertState(t, f.uid, v1.ExecState_EXEC_PENDING)
	if records := f.records(t, f.uid); len(records) != 0 {
		t.Fatal("creating or reading a pending session appended execution events")
	}
}

func TestExecutionStateSuccessfulTurn(t *testing.T) {
	f := newExecutionStateFixture(t)
	turn := f.startExec(t, "first")
	f.assertState(t, f.uid, v1.ExecState_EXEC_RUNNING)
	if err := turn.finish(t, nil); err != nil {
		t.Fatal(err)
	}
	f.assertState(t, f.uid, v1.ExecState_EXEC_COMPLETED)
	f.reopen(t)
	f.assertState(t, f.uid, v1.ExecState_EXEC_COMPLETED)
	_ = f.records(t, f.uid)
}

func TestExecutionStateNewTurnAfterCompletion(t *testing.T) {
	f := newExecutionStateFixture(t)
	first := f.startExec(t, "first")
	if err := first.finish(t, nil); err != nil {
		t.Fatal(err)
	}
	f.assertState(t, f.uid, v1.ExecState_EXEC_COMPLETED)
	before := f.records(t, f.uid)

	second := f.startExec(t, "second")
	f.assertState(t, f.uid, v1.ExecState_EXEC_RUNNING)
	running := f.records(t, f.uid)
	if running[len(running)-1].Event.ExecutionID == before[len(before)-1].Event.ExecutionID {
		t.Fatal("new turn reused the completed execution's identity")
	}
	if err := second.finish(t, nil); err != nil {
		t.Fatal(err)
	}
	f.assertState(t, f.uid, v1.ExecState_EXEC_COMPLETED)
	after := f.records(t, f.uid)
	if !reflect.DeepEqual(before, after[:len(before)]) {
		t.Fatal("new turn rewrote the completed execution's audit prefix")
	}
}

func TestExecutionStateFailureSurvivesReopen(t *testing.T) {
	f := newExecutionStateFixture(t)
	before := f.failExecution(t)
	f.reopen(t)
	f.assertState(t, f.uid, v1.ExecState_EXEC_FAILED)
	if after := f.records(t, f.uid); !reflect.DeepEqual(before, after) {
		t.Fatal("reopening or reading a failed execution changed its journal")
	}
}

func TestExecutionStateRecovery(t *testing.T) {
	f := newExecutionStateFixture(t)
	failed := f.failExecution(t)
	f.reopen(t)
	f.assertState(t, f.uid, v1.ExecState_EXEC_FAILED)

	turn := f.startResume(t, f.uid)
	// Re-entering Run without a new journal event is not evidence of progress.
	f.assertState(t, f.uid, v1.ExecState_EXEC_FAILED)
	if after := f.records(t, f.uid); !reflect.DeepEqual(failed, after) {
		t.Fatal("recovery changed the journal before the model answered")
	}
	if err := turn.finish(t, nil); err != nil {
		t.Fatal(err)
	}
	f.assertState(t, f.uid, v1.ExecState_EXEC_COMPLETED)
	recovered := f.records(t, f.uid)
	if len(recovered) <= len(failed) || !reflect.DeepEqual(failed, recovered[:len(failed)]) {
		t.Fatal("recovery rewrote the failed execution's audit prefix")
	}
	for _, record := range recovered[len(failed):] {
		if record.Event.Kind != api.EventLifecycle && record.Event.ExecutionID != failed[len(failed)-1].Event.ExecutionID {
			t.Fatalf("recovered %s at seq %d belongs to a different execution", record.Event.Kind, record.Seq)
		}
	}
	f.reopen(t)
	f.assertState(t, f.uid, v1.ExecState_EXEC_COMPLETED)
}

func TestExecutionStateAcrossSuspendResume(t *testing.T) {
	f := newExecutionStateFixture(t)
	turn := f.startExec(t, "first")
	if err := turn.finish(t, nil); err != nil {
		t.Fatal(err)
	}
	f.assertState(t, f.uid, v1.ExecState_EXEC_COMPLETED)
	suspended, err := f.client.Suspend(f.ctx, f.uid)
	if err != nil {
		t.Fatal(err)
	}
	if suspended.GetExecState() != v1.ExecState_EXEC_COMPLETED || suspended.GetComputeState() != v1.ComputeState_COMPUTE_COLD {
		t.Fatal("Suspend did not preserve COMPLETED while moving compute to COLD")
	}
	f.assertState(t, f.uid, v1.ExecState_EXEC_COMPLETED)

	done := f.run(t, func(ctx context.Context) error {
		current, err := f.client.Resume(ctx, f.uid, false)
		if err == nil && (current.GetExecState() != v1.ExecState_EXEC_COMPLETED || current.GetComputeState() != v1.ComputeState_COMPUTE_LIVE) {
			return errors.New("Resume did not preserve COMPLETED while moving compute to LIVE")
		}
		return err
	})
	select {
	case <-f.modelCalls:
		t.Fatal("resuming a completed execution invoked the model")
	case <-done.done:
		if done.err != nil {
			t.Fatal(done.err)
		}
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	f.assertState(t, f.uid, v1.ExecState_EXEC_COMPLETED)
	_ = f.records(t, f.uid)
}

func TestExecutionStateMidExecutionFork(t *testing.T) {
	f := newExecutionStateFixture(t)
	turn := f.startExec(t, "first")
	parked := f.records(t, f.uid)
	cut := parked[len(parked)-1]
	if cut.Event.Kind != api.EventModelCall {
		t.Fatalf("parked journal tail=%s, want MODEL_CALL", cut.Event.Kind)
	}
	if err := turn.finish(t, nil); err != nil {
		t.Fatal(err)
	}
	f.assertState(t, f.uid, v1.ExecState_EXEC_COMPLETED)
	children, err := f.client.Fork(f.ctx, f.uid, client.ForkOptions{AtSeq: cut.Seq, Count: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0].GetExecState() != v1.ExecState_EXEC_RUNNING {
		t.Fatal("mid-execution fork must report RUNNING before a controller resumes it")
	}
	child := children[0].GetMetadata().GetUid()
	t.Cleanup(func() { stopQuietly(t, f.backend, child) })
	parent := f.records(t, f.uid)
	inherited := f.records(t, child)
	if len(inherited) < len(parked) || !reflect.DeepEqual(parked, inherited[:len(parked)]) {
		t.Fatal("fork did not preserve the unfinished journal prefix")
	}
	f.reopen(t)
	f.assertState(t, child, v1.ExecState_EXEC_RUNNING)
	f.assertState(t, f.uid, v1.ExecState_EXEC_COMPLETED)
	turn = f.startResume(t, child)
	f.assertState(t, child, v1.ExecState_EXEC_RUNNING)
	if err := turn.finish(t, nil); err != nil {
		t.Fatal(err)
	}
	f.assertState(t, child, v1.ExecState_EXEC_COMPLETED)
	f.assertState(t, f.uid, v1.ExecState_EXEC_COMPLETED)
	if after := f.records(t, f.uid); !reflect.DeepEqual(parent, after) {
		t.Fatal("resuming the child changed its parent's journal")
	}
	_ = f.records(t, child)
}

type executionStateFixture struct {
	*sessionAPI
	ctx        context.Context
	uid        string
	backend    *substrate.Backend
	modelCalls chan chan error
}

func newExecutionStateFixture(t *testing.T) *executionStateFixture {
	t.Helper()
	cluster := newFixture(t, env("SUBSTRATE_ATESPACE", "e2e-execution-state"))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)
	desc := api.Descriptor{ID: "echo", Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}}
	backend := cluster.backend(t, echoTemplateSpec, desc)
	modelCalls := make(chan chan error)
	model := func(ctx context.Context, req api.ModelRequest) (api.ModelResponse, error) {
		release := make(chan error, 1)
		select {
		case modelCalls <- release:
		case <-ctx.Done():
			return api.ModelResponse{}, ctx.Err()
		}
		select {
		case err := <-release:
			if err != nil {
				return api.ModelResponse{}, err
			}
			return echoagent.Model(ctx, req)
		case <-ctx.Done():
			return api.ModelResponse{}, ctx.Err()
		}
	}
	registry, err := placement.NewRegistry("echo", map[string]*placement.Placer{"echo": placement.New(backend, model)})
	if err != nil {
		t.Fatal(err)
	}
	a := newSessionAPI(t, registry)
	created, err := a.client.CreateSession(ctx, &v1.Session{Metadata: &v1.ResourceMetadata{Project: "execution-state"}})
	if err != nil {
		t.Fatal(err)
	}
	uid := created.GetMetadata().GetUid()
	t.Cleanup(func() { stopQuietly(t, backend, uid) })
	if created.GetExecState() != v1.ExecState_EXEC_PENDING {
		t.Fatalf("Create state=%s, want PENDING", created.GetExecState())
	}
	return &executionStateFixture{sessionAPI: a, ctx: ctx, uid: uid, backend: backend, modelCalls: modelCalls}
}

type executionStateTurn struct {
	ctx     context.Context
	release chan error
	done    chan struct{}
	err     error
}

func (f *executionStateFixture) run(t *testing.T, run func(context.Context) error) *executionStateTurn {
	t.Helper()
	ctx, cancel := context.WithCancel(f.ctx)
	turn := &executionStateTurn{ctx: ctx, done: make(chan struct{})}
	go func() {
		turn.err = run(ctx)
		close(turn.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-turn.done:
		case <-time.After(10 * time.Second):
			t.Error("execution did not return after cancellation")
		}
	})
	return turn
}

func (f *executionStateFixture) park(t *testing.T, run func(context.Context) error) *executionStateTurn {
	t.Helper()
	turn := f.run(t, run)
	select {
	case turn.release = <-f.modelCalls:
	case <-turn.done:
		t.Fatalf("execution returned before reaching the remote harness's model call: %v", turn.err)
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	return turn
}

func (f *executionStateFixture) startExec(t *testing.T, input string) *executionStateTurn {
	t.Helper()
	return f.park(t, func(ctx context.Context) error {
		turn, err := f.client.Exec(ctx, client.ExecOptions{Session: f.uid, Inputs: []string{input}})
		if err == nil && turn.Output != "echo:"+input {
			return errors.New("unexpected execution output")
		}
		return err
	})
}

func (f *executionStateFixture) startResume(t *testing.T, uid string) *executionStateTurn {
	t.Helper()
	return f.park(t, func(ctx context.Context) error {
		current, err := f.client.Resume(ctx, uid, false)
		if err == nil && current.GetExecState() != v1.ExecState_EXEC_COMPLETED {
			return errors.New("Resume response did not report COMPLETED")
		}
		return err
	})
}

func (turn *executionStateTurn) finish(t *testing.T, modelErr error) error {
	t.Helper()
	turn.release <- modelErr
	select {
	case <-turn.done:
		return turn.err
	case <-turn.ctx.Done():
		t.Fatal(turn.ctx.Err())
		return turn.ctx.Err()
	}
}

func (f *executionStateFixture) failExecution(t *testing.T) []eventlog.Record {
	t.Helper()
	turn := f.startExec(t, "fail")
	f.assertState(t, f.uid, v1.ExecState_EXEC_RUNNING)
	if err := turn.finish(t, errors.New("scripted model failure")); status.Code(err) != codes.Internal {
		t.Fatalf("model failure returned %v, want Internal", err)
	}
	f.assertState(t, f.uid, v1.ExecState_EXEC_FAILED)
	records := f.records(t, f.uid)
	if tail := records[len(records)-1]; tail.Event.Kind != api.EventError || tail.Event.ExecutionID == "" {
		t.Fatal("failed execution did not record its own ERROR")
	}
	return records
}

func (f *executionStateFixture) assertState(t *testing.T, uid string, want v1.ExecState) {
	t.Helper()
	current, err := f.client.GetSession(f.ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if current.GetExecState() != want {
		t.Fatalf("Get state=%s, want %s", current.GetExecState(), want)
	}
	all, err := f.client.ListSessions(f.ctx, "execution-state")
	if err != nil {
		t.Fatal(err)
	}
	for _, listed := range all {
		if listed.GetMetadata().GetUid() == uid {
			if !proto.Equal(current, listed) {
				t.Fatal("Get and List returned different Session metadata")
			}
			return
		}
	}
	t.Fatal("List omitted the session")
}
