package invoke

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/kaimahi-agents/kaimahi/agentsuite"

	"github.com/aramase/agentsessions/api"
	v1 "github.com/aramase/agentsessions/api/genpb"
	"github.com/aramase/agentsessions/client"
	"github.com/aramase/agentsessions/controller"
)

// fakeSessions records child sessions and their first exec config, and serves each session's
// journal as one EXECUTION_START carrying that config.
type fakeSessions struct {
	mu      sync.Mutex
	created []*v1.Session
	configs map[string][]byte
	// block, when set, holds every Exec until it is closed; started reports each Exec.
	block   chan struct{}
	started chan string
	execErr error
}

func newFake() *fakeSessions { return &fakeSessions{configs: map[string][]byte{}} }

func (f *fakeSessions) CreateSession(_ context.Context, s *v1.Session) (*v1.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &v1.Session{Metadata: &v1.ResourceMetadata{Name: s.GetMetadata().GetName(), Uid: fmt.Sprintf("sess-child-%d", len(f.created)+1)}, Harness: s.GetHarness(), Labels: s.GetLabels()}
	f.created = append(f.created, out)
	return out, nil
}

func (f *fakeSessions) Exec(_ context.Context, o client.ExecOptions) (*client.TurnResult, error) {
	f.mu.Lock()
	f.configs[o.Session] = o.Config
	block, started := f.block, f.started
	f.mu.Unlock()
	if started != nil {
		started <- o.Session
	}
	if block != nil {
		<-block
	}
	if f.execErr != nil {
		return nil, f.execErr
	}
	return &client.TurnResult{Output: "done: " + strings.Join(o.Inputs, " ")}, nil
}

func (f *fakeSessions) Replay(_ context.Context, uid string, _, _ int64) ([]*v1.LogRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if uid == "missing" {
		return nil, errors.New("not found")
	}
	cfg := f.configs[uid]
	return []*v1.LogRecord{
		{Seq: 1, Event: &v1.Event{Body: &v1.Event_ExecutionStart{ExecutionStart: &v1.ExecutionStart{Config: cfg}}}},
	}, nil
}

func (f *fakeSessions) setConfig(uid string, p Provenance) {
	b, _ := json.Marshal(map[string]Provenance{ConfigKey: p})
	f.mu.Lock()
	f.configs[uid] = b
	f.mu.Unlock()
}

func call(agent, task string, mediation api.Mediation) api.ToolCall {
	return api.ToolCall{ID: "call-1", Tool: Tool, Args: map[string]any{"agent": agent, "task": task}, Mediation: mediation, IdempotencyKey: "call-1"}
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
// 1; the workers declare none.
func suitePolicy(t *testing.T, approve ...string) Policy {
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
	p, err := FromAgents(agents, approve)
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
	if _, err := FromAgents(nil, []string{"reviewer"}); err == nil || !strings.Contains(err.Error(), "no invokes edge targets it") {
		t.Fatalf("approval for an untargeted agent: err = %v", err)
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
	res := run(t, fn, "sess-root", call("writer", "write a haiku", api.MediationControllerMediated))
	if res.IsError || res.ID != "call-1" {
		t.Fatalf("result = %+v", res)
	}
	if res.Output["session"] != "sess-child-1" || res.Output["agent"] != "writer" || res.Output["output"] != "done: write a haiku" {
		t.Fatalf("output = %v", res.Output)
	}
	child := f.created[0]
	if child.GetHarness() != "writer" || child.GetLabels()["agentsuite.invoked-by"] != "sess-root" || child.GetMetadata().GetName() != "coordinator-writer" {
		t.Fatalf("child = %+v", child)
	}
	var cfg map[string]Provenance
	if err := json.Unmarshal(f.configs["sess-child-1"], &cfg); err != nil {
		t.Fatal(err)
	}
	want := Provenance{ParentSession: "sess-root", Caller: "coordinator", ToolCallID: "call-1", Target: "writer", Depth: 1, Remaining: 0}
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

// maxConcurrent counts the calling session's in-flight invocations of the target: a second call
// while the first runs is refused, a call from another session or to another target is not, and the
// slot frees when the first returns.
func TestInvokeEnforcesMaxConcurrent(t *testing.T) {
	f := newFake()
	block := make(chan struct{})
	f.block, f.started = block, make(chan string, 4)
	fn := New(suitePolicy(t), f).For("coordinator")
	first := make(chan api.ToolResult, 1)
	go func() {
		res, _ := fn(context.Background(), controller.ToolCallContext{SessionUID: "sess-root"}, call("writer", "one", api.MediationControllerMediated))
		first <- res
	}()
	<-f.started
	f.mu.Lock()
	f.block = nil // later calls do not wait
	f.mu.Unlock()
	res := run(t, fn, "sess-root", call("writer", "two", api.MediationControllerMediated))
	if !res.IsError || !strings.Contains(res.Error, "exceeds maxConcurrent 1 for session sess-root") {
		t.Fatalf("second concurrent call: %+v, want the maxConcurrent refusal", res)
	}
	if res := run(t, fn, "sess-other", call("writer", "other session", api.MediationControllerMediated)); res.IsError {
		t.Fatalf("another session's call: %+v", res)
	}
	if res := run(t, fn, "sess-root", call("reviewer", "other target", api.MediationControllerMediated)); res.IsError {
		t.Fatalf("another target: %+v", res)
	}
	close(f.started)
	f.mu.Lock()
	f.started = nil
	f.mu.Unlock()
	close(block)
	if res := <-first; res.IsError {
		t.Fatalf("first call: %+v", res)
	}
	if res := run(t, fn, "sess-root", call("writer", "three", api.MediationControllerMediated)); res.IsError {
		t.Fatalf("after the first returned: %+v", res)
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
