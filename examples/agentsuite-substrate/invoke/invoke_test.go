package invoke

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kaimahi-agents/kaimahi/agentsuite"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/aramase/agentsessions/api"
	v1 "github.com/aramase/agentsessions/api/genpb"
	"github.com/aramase/agentsessions/client"
	"github.com/aramase/agentsessions/controller"
	"github.com/aramase/agentsessions/wire"
)

// fakeSessions records child sessions and their first exec config, and keeps a journal per session:
// each Exec appends an EXECUTION_START with its config and, once the turn finishes, its OUTPUT and
// END; Resume finishes an interrupted turn the way the host's re-drive would. A session without a
// journal serves one EXECUTION_START carrying its configs entry (the calling sessions of the tests).
// Every calling session is in project "proj-<uid>".
type fakeSessions struct {
	mu       sync.Mutex
	created  []*v1.Session
	configs  map[string][]byte
	journals map[string][]*v1.LogRecord
	cold     map[string]bool
	execs    int
	// suspended and resumed list the Suspend and Resume calls, in order.
	suspended, resumed []string
	// block, when set, holds every Exec until it is closed or the Exec's ctx ends; started reports
	// each Exec.
	block   chan struct{}
	started chan string
	execErr error
	// execErrBefore fails the next Exec before it journals anything.
	execErrBefore error
	// execAborted refuses that many Execs with ABORTED before they journal anything, as the host
	// does while the child's actor is mid-transition.
	execAborted int
	suspendErr  error
	// execCtxErr records the ctx error each blocked Exec ended with.
	execCtxErr []error
}

func newFake() *fakeSessions {
	return &fakeSessions{configs: map[string][]byte{}, journals: map[string][]*v1.LogRecord{}, cold: map[string]bool{}}
}

func (f *fakeSessions) GetSession(_ context.Context, uid string) (*v1.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.created {
		if s.GetMetadata().GetUid() == uid {
			return f.view(s), nil
		}
	}
	if strings.HasPrefix(uid, "sess-child-") {
		return nil, status.Errorf(codes.NotFound, "session %q not found", uid)
	}
	return &v1.Session{Metadata: &v1.ResourceMetadata{Project: "proj-" + uid, Uid: uid}}, nil
}

func (f *fakeSessions) ListSessions(_ context.Context, project string) ([]*v1.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*v1.Session
	for _, s := range f.created {
		if s.GetMetadata().GetProject() == project {
			out = append(out, f.view(s))
		}
	}
	return out, nil
}

// view is a copy of s with its compute state, as the Sessions API reports it.
func (f *fakeSessions) view(s *v1.Session) *v1.Session {
	out := proto.Clone(s).(*v1.Session)
	out.ComputeState = v1.ComputeState_COMPUTE_LIVE
	if f.cold[s.GetMetadata().GetUid()] {
		out.ComputeState = v1.ComputeState_COMPUTE_COLD
	}
	return out
}

func (f *fakeSessions) CreateSession(_ context.Context, s *v1.Session) (*v1.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &v1.Session{Metadata: &v1.ResourceMetadata{Project: s.GetMetadata().GetProject(), Name: s.GetMetadata().GetName(), Uid: fmt.Sprintf("sess-child-%d", len(f.created)+1)}, Harness: s.GetHarness(), Labels: s.GetLabels()}
	f.created = append(f.created, out)
	return out, nil
}

func (f *fakeSessions) record(uid, exec string, ev *v1.Event) {
	ev.ExecutionId = exec
	f.journals[uid] = append(f.journals[uid], &v1.LogRecord{Seq: int64(len(f.journals[uid]) + 1), Event: ev})
}

func (f *fakeSessions) finish(uid, exec, output string) {
	f.record(uid, exec, &v1.Event{Kind: v1.EventKind_EVENT_OUTPUT, Body: &v1.Event_Message{Message: wire.MessageToProto(api.TextMessage("assistant", output))}})
	f.record(uid, exec, &v1.Event{Kind: v1.EventKind_EVENT_END, Body: &v1.Event_End{End: &v1.HarnessEnd{State: "COMPLETED"}}})
}

func (f *fakeSessions) Exec(ctx context.Context, o client.ExecOptions) (*client.TurnResult, error) {
	f.mu.Lock()
	if err := f.execErrBefore; err != nil {
		f.execErrBefore = nil
		f.mu.Unlock()
		return nil, err
	}
	if f.execAborted > 0 {
		f.execAborted--
		f.mu.Unlock()
		return nil, status.Error(codes.Aborted, "substrate: actor is busy with another operation: actor is mid-transition")
	}
	if f.cold[o.Session] {
		f.mu.Unlock()
		return nil, fmt.Errorf("session %s is suspended", o.Session)
	}
	f.execs++
	exec := fmt.Sprintf("exec-%d", f.execs)
	if _, ok := f.configs[o.Session]; !ok {
		f.configs[o.Session] = o.Config
	}
	f.record(o.Session, exec, &v1.Event{Kind: v1.EventKind_EVENT_EXECUTION_START, Body: &v1.Event_ExecutionStart{ExecutionStart: &v1.ExecutionStart{Config: o.Config}}})
	block, started := f.block, f.started
	f.mu.Unlock()
	if started != nil {
		started <- o.Session
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			f.mu.Lock()
			f.execCtxErr = append(f.execCtxErr, ctx.Err())
			f.mu.Unlock()
			return nil, ctx.Err()
		}
	}
	if f.execErr != nil {
		// As the controller does: a failed turn ends with ERROR and no END, after any output.
		f.mu.Lock()
		f.record(o.Session, exec, &v1.Event{Kind: v1.EventKind_EVENT_OUTPUT, Body: &v1.Event_Message{Message: wire.MessageToProto(api.TextMessage("assistant", "partial"))}})
		f.record(o.Session, exec, &v1.Event{Kind: v1.EventKind_EVENT_ERROR, Body: &v1.Event_Error{Error: &v1.Error{Description: f.execErr.Error()}}})
		f.mu.Unlock()
		return nil, f.execErr
	}
	out := "done: " + strings.Join(o.Inputs, " ")
	f.mu.Lock()
	f.finish(o.Session, exec, out)
	f.mu.Unlock()
	return &client.TurnResult{Output: out}, nil
}

func (f *fakeSessions) Replay(_ context.Context, uid string, _, _ int64) ([]*v1.LogRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if uid == "missing" {
		return nil, errors.New("not found")
	}
	if j := f.journals[uid]; len(j) > 0 {
		return slices.Clone(j), nil
	}
	cfg := f.configs[uid]
	return []*v1.LogRecord{
		{Seq: 1, Event: &v1.Event{Body: &v1.Event_ExecutionStart{ExecutionStart: &v1.ExecutionStart{Config: cfg}}}},
	}, nil
}

func (f *fakeSessions) Suspend(_ context.Context, uid string) (*v1.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.suspended = append(f.suspended, uid)
	if f.suspendErr != nil {
		return nil, f.suspendErr
	}
	f.cold[uid] = true
	return &v1.Session{}, nil
}

// Resume wakes the session and, as the host's crash recovery does, finishes its last turn when it
// has an EXECUTION_START and no END or ERROR.
func (f *fakeSessions) Resume(_ context.Context, uid string, _ bool) (*v1.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumed = append(f.resumed, uid)
	delete(f.cold, uid)
	j := f.journals[uid]
	var exec string
	for _, r := range j {
		switch r.GetEvent().GetKind() {
		case v1.EventKind_EVENT_EXECUTION_START:
			exec = r.GetEvent().GetExecutionId()
		case v1.EventKind_EVENT_END, v1.EventKind_EVENT_ERROR:
			exec = ""
		}
	}
	if exec != "" {
		f.finish(uid, exec, "re-driven")
	}
	return &v1.Session{}, nil
}

func (f *fakeSessions) setConfig(uid string, p Provenance) {
	b, _ := json.Marshal(map[string]Provenance{ConfigKey: p})
	f.mu.Lock()
	f.configs[uid] = b
	f.mu.Unlock()
}

// calls numbers the calls call makes, so two calls are two invocations unless a test reuses one.
var calls atomic.Int64

func call(agent, task string, mediation api.Mediation) api.ToolCall {
	id := fmt.Sprintf("call-%d", calls.Add(1))
	return api.ToolCall{ID: id, Tool: Tool, Args: map[string]any{"agent": agent, "task": task}, Mediation: mediation, IdempotencyKey: id}
}

// followUp is a call that sends task to the existing child session.
func followUp(agent, session, task string) api.ToolCall {
	c := call(agent, task, api.MediationControllerMediated)
	c.Args["session"] = session
	return c
}

func run(t *testing.T, fn controller.ToolFunc, session string, c api.ToolCall) api.ToolResult {
	t.Helper()
	res, err := fn(t.Context(), controller.ToolCallContext{SessionUID: session}, c)
	if err != nil {
		t.Fatalf("executor error: %v", err)
	}
	return res
}

// The vendored suite's coordinator declares writer and reviewer, each maxConcurrent 1 and maxDepth
// 1; the workers declare none. gated names the coordinator's targets whose calls need approval.
func suitePolicy(t *testing.T, gated ...string) Policy {
	t.Helper()
	report, err := agentsuite.ValidateDirectory("../suites/coordinator-workers")
	if err != nil {
		t.Fatal(err)
	}
	if report.Name != "coordinator-workers" {
		t.Fatalf("suite = %q", report.Name)
	}
	var agents []agentsuite.Agent
	for _, id := range []string{"coordinator", "writer", "reviewer"} {
		raw, err := os.ReadFile("../suites/coordinator-workers/agents/" + id + ".json")
		if err != nil {
			t.Fatal(err)
		}
		var a agentsuite.Agent
		if err := agentsuite.Unmarshal(raw, &a); err != nil {
			t.Fatal(err)
		}
		agents = append(agents, a)
	}
	var gates []Gate
	for _, g := range gated {
		gates = append(gates, Gate{Caller: "coordinator", Agent: g})
	}
	p, err := FromAgents(agents, gates)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPolicyFromSuite(t *testing.T) {
	p := suitePolicy(t, "reviewer")
	want := []string{"coordinator=reviewer:1:1:approval", "coordinator=writer:1:1"}
	if got := p.Flag(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("policy = %v, want %v", got, want)
	}
	// The flag form parses back to the same policy.
	back := Policy{}
	for _, v := range p.Flag() {
		if err := back.Set(v); err != nil {
			t.Fatal(err)
		}
	}
	if back.String() != p.String() {
		t.Fatalf("round trip = %q, want %q", back.String(), p.String())
	}
	for _, g := range []Gate{{Caller: "coordinator", Agent: "publisher"}, {Caller: "writer", Agent: "reviewer"}, {Caller: "reviewer", Agent: "coordinator"}} {
		if _, err := FromAgents(suiteAgents(t), []Gate{g}); err == nil || !strings.Contains(err.Error(), "the suite declares no such invokes edge") {
			t.Fatalf("gate %+v on an undeclared edge: err = %v", g, err)
		}
	}
}

// suiteAgents returns the vendored suite's agent manifests.
func suiteAgents(t *testing.T) []agentsuite.Agent {
	t.Helper()
	var agents []agentsuite.Agent
	for _, id := range []string{"coordinator", "writer", "reviewer"} {
		raw, err := os.ReadFile("../suites/coordinator-workers/agents/" + id + ".json")
		if err != nil {
			t.Fatal(err)
		}
		var a agentsuite.Agent
		if err := agentsuite.Unmarshal(raw, &a); err != nil {
			t.Fatal(err)
		}
		agents = append(agents, a)
	}
	return agents
}

// A gate names one edge, caller -> agent: when two agents invoke the same target, gating one
// caller's edge leaves the other's ungated.
func TestGateIsPerEdge(t *testing.T) {
	agents := []agentsuite.Agent{
		{ID: "a", Invokes: []agentsuite.AgentInvoke{{Agent: "c", MaxConcurrent: 1, MaxDepth: 1}}},
		{ID: "b", Invokes: []agentsuite.AgentInvoke{{Agent: "c", MaxConcurrent: 2, MaxDepth: 1}}},
		{ID: "c"},
	}
	p, err := FromAgents(agents, []Gate{{Caller: "a", Agent: "c"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := p.String(), "a=c:1:1:approval,b=c:2:1"; got != want {
		t.Fatalf("policy = %q, want %q", got, want)
	}
}

func TestPolicySetRefusesMalformedEdges(t *testing.T) {
	for _, v := range []string{"coordinator", "coordinator=writer", "coordinator=writer:1", "coordinator=writer:x:1", "coordinator=writer:0:1", "coordinator=writer:1:0", "coordinator=writer:1:1:gate", "=writer:1:1"} {
		if err := (Policy{}).Set(v); err == nil {
			t.Errorf("Set(%q) accepted", v)
		}
	}
	p := Policy{}
	if err := p.Set("a=b:1:1"); err != nil {
		t.Fatal(err)
	}
	if err := p.Set("a=b:2:2"); err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("duplicate edge: err = %v", err)
	}
}

// A declared edge creates one child session on the target's harness, runs the task as its turn
// and returns its output with the child's uid. The child's first execution config records the
// caller, so the child's journal names the parent.
func TestInvokeCreatesChildSession(t *testing.T) {
	f := newFake()
	fn := New(suitePolicy(t), f).For("coordinator")
	c := call("writer", "write a haiku", api.MediationControllerMediated)
	res := run(t, fn, "sess-root", c)
	if res.IsError || res.ID != c.ID {
		t.Fatalf("result = %+v", res)
	}
	if res.Output["session"] != "sess-child-1" || res.Output["agent"] != "writer" || res.Output["output"] != "done: write a haiku" {
		t.Fatalf("output = %v", res.Output)
	}
	child := f.created[0]
	key := Key("sess-root", c.ID)
	if child.GetHarness() != "writer" || child.GetLabels()[LabelInvokedBy] != "sess-root" || child.GetLabels()[LabelKey] != key ||
		child.GetMetadata().GetName() != "coordinator-writer-"+key[:12] {
		t.Fatalf("child = %+v", child)
	}
	var cfg map[string]Provenance
	if err := json.Unmarshal(f.configs["sess-child-1"], &cfg); err != nil {
		t.Fatal(err)
	}
	want := Provenance{ParentSession: "sess-root", Caller: "coordinator", ToolCallID: c.ID, Target: "writer", Depth: 1, Remaining: 0}
	if cfg[ConfigKey] != want {
		t.Fatalf("provenance = %+v, want %+v", cfg[ConfigKey], want)
	}
}

// Invocation permission is directional and not transitive: an agent with no edges gets no
// executor, and an edge to another target does not permit this one. No child session is created.
func TestInvokeRefusesUndeclaredEdges(t *testing.T) {
	f := newFake()
	x := New(suitePolicy(t), f)
	if x.For("writer") != nil || x.For("reviewer") != nil {
		t.Fatal("workers declare no invokes and must get no executor")
	}
	fn := x.For("coordinator")
	for _, c := range []api.ToolCall{
		call("coordinator", "loop", api.MediationControllerMediated),
		call("translator", "x", api.MediationControllerMediated),
	} {
		res := run(t, fn, "sess-root", c)
		if !res.IsError || !strings.Contains(res.Error, "declares no invokes edge") {
			t.Fatalf("%v: result = %+v, want the edge refusal", c.Args, res)
		}
	}
	for _, c := range []api.ToolCall{
		{ID: "c", Tool: "shell", Args: map[string]any{"agent": "writer", "task": "x"}},
		{ID: "c", Tool: Tool, Args: map[string]any{"agent": "writer"}},
		{ID: "c", Tool: Tool, Args: map[string]any{"agent": "writer", "task": 7}},
	} {
		if res := run(t, fn, "sess-root", c); !res.IsError || !strings.HasPrefix(res.Error, "invocation refused") {
			t.Fatalf("%+v: result = %+v, want a refusal", c, res)
		}
	}
	if len(f.created) != 0 {
		t.Fatalf("refused calls created sessions: %v", f.created)
	}
}

// An edge the deployment gates is refused unless the call arrives as REQUIRES_APPROVAL, so a
// harness cannot skip the gate by sending it CONTROLLER_MEDIATED. The executor runs only after the
// host's approval (the controller calls it for an approved call only).
func TestInvokeRequiresApprovalMediationOnGatedEdge(t *testing.T) {
	f := newFake()
	fn := New(suitePolicy(t, "reviewer"), f).For("coordinator")
	res := run(t, fn, "sess-root", call("reviewer", "review", api.MediationControllerMediated))
	if !res.IsError || !strings.Contains(res.Error, "requires approval for coordinator -> reviewer") {
		t.Fatalf("result = %+v, want the approval refusal", res)
	}
	if len(f.created) != 0 {
		t.Fatal("an ungated call to a gated edge created a session")
	}
	if res := run(t, fn, "sess-root", call("reviewer", "review", api.MediationRequiresApproval)); res.IsError {
		t.Fatalf("approved call: %+v", res)
	}
}

// maxConcurrent counts the declaring agent's in-flight invocations of the target across all of its
// sessions (kaimahi agentsuite/spec.md 8.1: "concurrent invocations of this target by the declaring
// agent"): while one coordinator session's writer call runs, another coordinator session's writer
// call is refused, a call to another target or by another agent is not, and the slot frees when the
// first returns.
func TestInvokeEnforcesMaxConcurrent(t *testing.T) {
	p := suitePolicy(t)
	if err := p.Set("planner=writer:1:1"); err != nil {
		t.Fatal(err)
	}
	f := newFake()
	block := make(chan struct{})
	f.block, f.started = block, make(chan string, 4)
	x := New(p, f)
	fn := x.For("coordinator")
	first := make(chan api.ToolResult, 1)
	go func() {
		res, _ := fn(context.Background(), controller.ToolCallContext{SessionUID: "sess-root-a"}, call("writer", "one", api.MediationControllerMediated))
		first <- res
	}()
	<-f.started
	f.mu.Lock()
	f.block = nil // later calls do not wait
	f.mu.Unlock()
	for _, session := range []string{"sess-root-b", "sess-root-a"} {
		res := run(t, fn, session, call("writer", "two", api.MediationControllerMediated))
		if !res.IsError || !strings.Contains(res.Error, "coordinator -> writer exceeds maxConcurrent 1: agent coordinator already has 1 invocation(s) of writer in flight") {
			t.Fatalf("%s: concurrent call: %+v, want the maxConcurrent refusal", session, res)
		}
	}
	if res := run(t, fn, "sess-root-b", call("reviewer", "other target", api.MediationControllerMediated)); res.IsError {
		t.Fatalf("another target: %+v", res)
	}
	if res := run(t, x.For("planner"), "sess-planner", call("writer", "other agent", api.MediationControllerMediated)); res.IsError {
		t.Fatalf("another declaring agent: %+v", res)
	}
	close(f.started)
	f.mu.Lock()
	f.started = nil
	f.mu.Unlock()
	close(block)
	if res := <-first; res.IsError {
		t.Fatalf("first call: %+v", res)
	}
	if res := run(t, fn, "sess-root-b", call("writer", "three", api.MediationControllerMediated)); res.IsError {
		t.Fatalf("after the first returned: %+v", res)
	}
}

// The child session lands in the calling session's project, and a child turn that outlives the
// executor's timeout ends with a handled error result and frees the maxConcurrent slot.
func TestInvokeChildProjectAndTimeout(t *testing.T) {
	f := newFake()
	fn := New(suitePolicy(t), f, WithTimeout(50*time.Millisecond)).For("coordinator")
	if res := run(t, fn, "sess-root", call("writer", "w", api.MediationControllerMediated)); res.IsError {
		t.Fatal(res.Error)
	}
	if got := f.created[0].GetMetadata().GetProject(); got != "proj-sess-root" {
		t.Fatalf("child project = %q, want the caller's proj-sess-root", got)
	}
	f.block = make(chan struct{}) // never closed: the child turn hangs
	res := run(t, fn, "sess-root", call("writer", "hang", api.MediationControllerMediated))
	if !res.IsError || !strings.Contains(res.Error, "failed in child session sess-child-2") || !strings.Contains(res.Error, context.DeadlineExceeded.Error()) {
		t.Fatalf("hung child: %+v, want a deadline error result", res)
	}
	if len(f.execCtxErr) != 1 || !errors.Is(f.execCtxErr[0], context.DeadlineExceeded) {
		t.Fatalf("child Exec ctx errors = %v, want one deadline", f.execCtxErr)
	}
	f.block = nil
	if res := run(t, fn, "sess-root", call("writer", "after", api.MediationControllerMediated)); res.IsError {
		t.Fatalf("after the timeout the slot is free: %+v", res)
	}
}

// maxDepth bounds the chain through an edge: the directly invoked target is depth 1, so under a
// maxDepth 1 edge the child may not invoke again even through an edge of its own, while a maxDepth
// 2 edge lets the child make exactly one more hop.
func TestInvokeEnforcesMaxDepth(t *testing.T) {
	p := Policy{}
	for _, v := range []string{"coordinator=writer:1:1", "coordinator=planner:1:2", "writer=reviewer:1:5", "planner=reviewer:1:5", "reviewer=writer:1:5"} {
		if err := p.Set(v); err != nil {
			t.Fatal(err)
		}
	}
	f := newFake()
	x := New(p, f)
	// coordinator -> writer (maxDepth 1): the writer child has no budget left.
	if res := run(t, x.For("coordinator"), "sess-root", call("writer", "w", api.MediationControllerMediated)); res.IsError {
		t.Fatal(res.Error)
	}
	res := run(t, x.For("writer"), "sess-child-1", call("reviewer", "r", api.MediationControllerMediated))
	if !res.IsError || !strings.Contains(res.Error, "exceeds maxDepth: session sess-child-1 is at depth 1") {
		t.Fatalf("depth-2 call under a maxDepth 1 edge: %+v, want the maxDepth refusal", res)
	}
	// coordinator -> planner (maxDepth 2): the planner child may make one more hop, and that
	// grandchild none, although its own edge allows 5.
	if res := run(t, x.For("coordinator"), "sess-root", call("planner", "p", api.MediationControllerMediated)); res.IsError {
		t.Fatal(res.Error)
	}
	planner := f.created[1].GetMetadata().GetUid()
	if res := run(t, x.For("planner"), planner, call("reviewer", "r", api.MediationControllerMediated)); res.IsError {
		t.Fatalf("depth-2 call under a maxDepth 2 edge: %+v", res)
	}
	grandchild := f.created[2].GetMetadata().GetUid()
	var cfg map[string]Provenance
	if err := json.Unmarshal(f.configs[grandchild], &cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg[ConfigKey]; got.Depth != 2 || got.Remaining != 0 || got.ParentSession != planner {
		t.Fatalf("grandchild provenance = %+v", got)
	}
	if res := run(t, x.For("reviewer"), grandchild, call("writer", "w", api.MediationControllerMediated)); !res.IsError || !strings.Contains(res.Error, "exceeds maxDepth") {
		t.Fatalf("depth-3 call: %+v, want the maxDepth refusal", res)
	}
	if len(f.created) != 3 {
		t.Fatalf("created %d sessions, want 3", len(f.created))
	}
}

// A child turn that fails is a handled error result naming the child, so the caller's model can
// continue; a journal the executor cannot read is an executor error, never an unbounded call.
func TestInvokeErrors(t *testing.T) {
	f := newFake()
	f.execErr = errors.New("harness unavailable")
	fn := New(suitePolicy(t), f).For("coordinator")
	res := run(t, fn, "sess-root", call("writer", "w", api.MediationControllerMediated))
	if !res.IsError || !strings.Contains(res.Error, "failed in child session sess-child-1") || res.Output["session"] != "sess-child-1" {
		t.Fatalf("result = %+v", res)
	}
	if _, err := fn(t.Context(), controller.ToolCallContext{SessionUID: "missing"}, call("writer", "w", api.MediationControllerMediated)); err == nil {
		t.Fatal("an unreadable calling journal must fail the call")
	}
	f.setConfig("sess-bad", Provenance{})
	f.mu.Lock()
	f.configs["sess-bad"] = []byte(`{"agentsuite.invocation": "x"}`)
	f.mu.Unlock()
	if _, err := fn(t.Context(), controller.ToolCallContext{SessionUID: "sess-bad"}, call("writer", "w", api.MediationControllerMediated)); err == nil {
		t.Fatal("malformed provenance must fail the call")
	}
}

func lifecycle(t *testing.T, res api.ToolResult) map[string]any {
	t.Helper()
	lc, ok := res.Output["lifecycle"].(map[string]any)
	if !ok {
		t.Fatalf("no lifecycle in %v", res.Output)
	}
	return lc
}

// After its turn, a child is suspended, also when the turn failed; a failed suspend is reported in
// the result and does not turn a done delegation into an error.
func TestInvokeSuspendsChildAfterItsTurn(t *testing.T) {
	f := newFake()
	fn := New(suitePolicy(t), f).For("coordinator")
	res := run(t, fn, "sess-root", call("writer", "w", api.MediationControllerMediated))
	if lc := lifecycle(t, res); res.IsError || lc["origin"] != "new" || lc["turn"] != "ran" || lc["suspended"] != true || lc["suspendMs"] == nil || lc["wakeMs"] != nil {
		t.Fatalf("result = %+v", res)
	}
	if !slices.Equal(f.suspended, []string{"sess-child-1"}) || len(f.resumed) != 0 {
		t.Fatalf("suspended %v, resumed %v; want the child suspended once and nothing resumed", f.suspended, f.resumed)
	}

	f.execErr = errors.New("harness unavailable")
	if res := run(t, fn, "sess-root", call("writer", "w", api.MediationControllerMediated)); !res.IsError || lifecycle(t, res)["suspended"] != true {
		t.Fatalf("failed turn: %+v, want an error result with the child suspended", res)
	}
	f.execErr, f.suspendErr = nil, errors.New("ate-api unavailable")
	res = run(t, fn, "sess-root", call("writer", "w", api.MediationControllerMediated))
	if lc := lifecycle(t, res); res.IsError || lc["suspended"] != false || lc["suspendError"] != "ate-api unavailable" {
		t.Fatalf("failed suspend: %+v, want the output with suspended false", res)
	}
}

// A follow-up names an earlier child: the executor wakes it, runs the task there and suspends it
// again, without creating a session. The turn's execution config names the follow-up's call.
func TestInvokeFollowUpWakesTheSameChild(t *testing.T) {
	f := newFake()
	fn := New(suitePolicy(t), f).For("coordinator")
	first := run(t, fn, "sess-root", call("writer", "draft", api.MediationControllerMediated))
	child := first.Output["session"].(string)
	c := followUp("writer", child, "revise")
	res := run(t, fn, "sess-root", c)
	lc := lifecycle(t, res)
	if res.IsError || res.Output["session"] != child || res.Output["output"] != "done: revise" || lc["origin"] != "follow-up" || lc["wakeMs"] == nil || lc["suspended"] != true {
		t.Fatalf("follow-up = %+v", res)
	}
	if len(f.created) != 1 || !slices.Equal(f.resumed, []string{child}) || !slices.Equal(f.suspended, []string{child, child}) {
		t.Fatalf("created %d, resumed %v, suspended %v; want one child woken once and suspended after each turn", len(f.created), f.resumed, f.suspended)
	}
	recs, _ := f.Replay(t.Context(), child, 1, 0)
	var ids []string
	for _, r := range recs {
		if p, ok := parseProvenance(r.GetEvent().GetExecutionStart().GetConfig()); ok {
			ids = append(ids, p.ToolCallID)
		}
	}
	if len(ids) != 2 || ids[1] != c.ID {
		t.Fatalf("the child's executions name calls %v, want the first call then %s", ids, c.ID)
	}
}

// A follow-up may only name a child of the calling session on the named agent; anything else is
// refused before a turn runs.
func TestInvokeFollowUpRefusesOtherSessions(t *testing.T) {
	f := newFake()
	x := New(suitePolicy(t), f)
	writer := run(t, x.For("coordinator"), "sess-root", call("writer", "draft", api.MediationControllerMediated)).Output["session"].(string)
	reviewer := run(t, x.For("coordinator"), "sess-other", call("reviewer", "review", api.MediationControllerMediated)).Output["session"].(string)
	execs := f.execs
	for name, c := range map[string]struct {
		caller string
		call   api.ToolCall
	}{
		"another agent's child":       {"sess-root", followUp("reviewer", writer, "x")},
		"another caller's child":      {"sess-root", followUp("reviewer", reviewer, "x")},
		"no such session":             {"sess-root", followUp("writer", "sess-child-99", "x")},
		"a session it did not invoke": {"sess-root", followUp("writer", "sess-root", "x")},
	} {
		res := run(t, x.For("coordinator"), c.caller, c.call)
		if !res.IsError || !strings.Contains(res.Error, "is not a") {
			t.Errorf("%s: %+v, want a refusal", name, res)
		}
	}
	if f.execs != execs || len(f.resumed) != 0 {
		t.Fatalf("refused follow-ups ran %d turns and woke %v", f.execs-execs, f.resumed)
	}
}

// A call re-driven after a host crash reaches the child the first attempt created, whatever the
// crash interrupted: the child creation (no turn yet: run it now), the child's turn (re-driven with
// Resume), or the result (the child's turn is served from its journal). Each retry runs on a new
// executor, as a restarted host would, and no case creates a second child or runs the task twice.
func TestInvokeRetryAfterCrashReachesTheSameChild(t *testing.T) {
	for _, tc := range []struct {
		name  string
		crash func(t *testing.T, f *fakeSessions, fn controller.ToolFunc, c api.ToolCall)
		turn  string
		execs int
		out   string
		// failed, when set, is the error the retry must report.
		failed string
	}{
		{
			name: "after create, before the turn",
			crash: func(t *testing.T, f *fakeSessions, fn controller.ToolFunc, c api.ToolCall) {
				f.execErrBefore = errors.New("host lost")
				run(t, fn, "sess-root", c)
			},
			turn: "ran", execs: 1, out: "done: w",
		},
		{
			name: "during the child's turn",
			crash: func(t *testing.T, f *fakeSessions, fn controller.ToolFunc, c api.ToolCall) {
				f.block, f.started = make(chan struct{}), make(chan string, 1)
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan struct{})
				go func() {
					defer close(done)
					_, _ = fn(ctx, controller.ToolCallContext{SessionUID: "sess-root"}, c)
				}()
				<-f.started
				cancel() // the host dies mid-turn; its result is never journaled
				<-done
				f.block, f.started = nil, nil
			},
			turn: "re-driven", execs: 1, out: "re-driven",
		},
		{
			name: "after the child's turn, before the caller's result",
			crash: func(t *testing.T, f *fakeSessions, fn controller.ToolFunc, c api.ToolCall) {
				run(t, fn, "sess-root", c) // the result is dropped as a crash would drop it
			},
			turn: "recorded", execs: 1, out: "done: w",
		},
		{
			// The turn failed after some output: its ERROR is terminal, so the retry reports the
			// failure instead of re-driving the turn or serving the partial output as a success.
			name: "after the child's turn failed, before the caller's result",
			crash: func(t *testing.T, f *fakeSessions, fn controller.ToolFunc, c api.ToolCall) {
				f.execErr = errors.New("model unavailable")
				run(t, fn, "sess-root", c)
				f.execErr = nil
			},
			turn: "recorded", execs: 1, failed: "model unavailable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			c := call("writer", "w", api.MediationControllerMediated)
			tc.crash(t, f, New(suitePolicy(t), f).For("coordinator"), c)
			if len(f.created) != 1 {
				t.Fatalf("the first attempt created %d children", len(f.created))
			}
			resumedBefore := len(f.resumed)
			res := run(t, New(suitePolicy(t), f).For("coordinator"), "sess-root", c)
			lc := lifecycle(t, res)
			if res.Output["session"] != "sess-child-1" || lc["origin"] != "retry" || lc["turn"] != tc.turn || lc["suspended"] != true {
				t.Fatalf("retry = %+v", res)
			}
			if tc.failed != "" {
				if !res.IsError || !strings.Contains(res.Error, tc.failed) || len(f.resumed) != resumedBefore {
					t.Fatalf("retry of a failed turn = %+v, resumed %v; want its failure, not a re-drive", res, f.resumed[resumedBefore:])
				}
			} else if res.IsError || res.Output["output"] != tc.out {
				t.Fatalf("retry = %+v", res)
			}
			if len(f.created) != 1 || f.execs != tc.execs {
				t.Fatalf("after the retry: %d children and %d turns, want 1 child and %d turns", len(f.created), f.execs, tc.execs)
			}
		})
	}
}

// On micro-VMs a host killed right after CreateSession leaves the child's actor mid-creation, and
// the retry's Exec is refused with ABORTED until the actor settles: the executor tries again (and
// counts the refusals) instead of failing the delegation, and a non-ABORTED error is not retried.
// When ABORTED outlasts the call's deadline, the delegation fails with the last refusal.
func TestInvokeRetriesAnAbortedChildStep(t *testing.T) {
	f := newFake()
	f.execAborted = 2
	x := New(suitePolicy(t), f)
	x.abortWait = time.Millisecond
	res := run(t, x.For("coordinator"), "sess-root", call("writer", "w", api.MediationControllerMediated))
	lc := lifecycle(t, res)
	if res.IsError || res.Output["output"] != "done: w" || lc["aborted"] != 2 || lc["turn"] != "ran" || lc["suspended"] != true {
		t.Fatalf("result = %+v", res)
	}
	if len(f.created) != 1 || f.execs != 1 {
		t.Fatalf("%d children and %d turns, want 1 and 1", len(f.created), f.execs)
	}

	f.execErrBefore = errors.New("harness unavailable")
	if res := run(t, x.For("coordinator"), "sess-root", call("writer", "w", api.MediationControllerMediated)); !res.IsError || lifecycle(t, res)["aborted"] != nil {
		t.Fatalf("a non-ABORTED error: %+v, want one failed try", res)
	}

	f.execAborted = math.MaxInt
	x = New(suitePolicy(t), f, WithTimeout(50*time.Millisecond))
	x.abortWait = time.Millisecond
	res = run(t, x.For("coordinator"), "sess-root", call("writer", "w", api.MediationControllerMediated))
	if !res.IsError || !strings.Contains(res.Error, "mid-transition") || !strings.Contains(res.Error, "gave up after") {
		t.Fatalf("ABORTED past the deadline: %+v, want the last refusal", res)
	}
}

// The key separates its inputs, and a child carrying a call's key for another caller or harness
// fails the call rather than being reused.
func TestKeyAndForeignKeyHolder(t *testing.T) {
	if Key("a", "bc") == Key("ab", "c") || Key("s", "c") != Key("s", "c") || len(Key("s", "c")) != 32 {
		t.Fatal("Key must be a stable 32-hex digest that separates session and call id")
	}
	f := newFake()
	c := call("writer", "w", api.MediationControllerMediated)
	_, _ = f.CreateSession(t.Context(), &v1.Session{
		Metadata: &v1.ResourceMetadata{Project: "proj-sess-root"}, Harness: "reviewer",
		Labels: map[string]string{LabelKey: Key("sess-root", c.ID), LabelInvokedBy: "sess-root"},
	})
	if _, err := New(suitePolicy(t), f).For("coordinator")(t.Context(), controller.ToolCallContext{SessionUID: "sess-root"}, c); err == nil {
		t.Fatal("a session holding the key on another harness must fail the call")
	}
}
