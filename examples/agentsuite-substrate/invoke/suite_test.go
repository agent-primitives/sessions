package invoke_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"

	"github.com/aramase/agentsessions/api"
	v1 "github.com/aramase/agentsessions/api/genpb"
	"github.com/aramase/agentsessions/client"
	"github.com/aramase/agentsessions/controller"
	"github.com/aramase/agentsessions/examples/agentsuite-substrate/agent"
	"github.com/aramase/agentsessions/examples/agentsuite-substrate/invoke"
	"github.com/aramase/agentsessions/examples/agentsuite-substrate/render"
	"github.com/aramase/agentsessions/placement"
	"github.com/aramase/agentsessions/runtime/local"
	"github.com/aramase/agentsessions/session"
	"github.com/aramase/agentsessions/sqlitelog"
)

// The demo's suite (the version that pins gpt-5.4-mini) and its binding, whose approval edge
// coordinator -> reviewer gates the reviewer call.
const (
	suiteDir    = "../suites/coordinator-workers-v2"
	bindingFile = "../binding-coordinator.json"
)

// loadAgent stages one agent of the vendored suite the way its image does (agent.json and
// instructions.md) and loads it with the harness's own loader.
func loadAgent(t *testing.T, id string) agent.Harness {
	t.Helper()
	comp, err := render.Load(suiteDir, id, "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agent.json"), comp.AgentJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "instructions.md"), comp.Instructions, 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := agent.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// scriptedModel plays the three agents: the coordinator delegates to writer, then to reviewer,
// then answers; writer drafts; reviewer approves. It counts live calls per agent and records the
// model ids requested.
type scriptedModel struct {
	mu     sync.Mutex
	calls  map[string]int
	models map[string]bool
}

func (m *scriptedModel) model(_ context.Context, req api.ModelRequest) (api.ModelResponse, error) {
	text := func(msg api.Message) string {
		var b strings.Builder
		for _, p := range msg.Parts {
			if p.Text != nil {
				b.WriteString(p.Text.Text)
			}
		}
		return b.String()
	}
	system, last := text(req.Messages[0]), text(req.Messages[len(req.Messages)-1])
	var who, reply string
	switch {
	case strings.HasPrefix(system, "Delegate writing tasks"):
		who = "coordinator"
		switch {
		case strings.HasPrefix(last, "Reply from writer:"):
			reply = `{"invoke": "reviewer", "task": "Review this draft: DRAFT(two lines on tides)"}`
		case strings.HasPrefix(last, "Reply from reviewer:"):
			reply = "FINAL: the draft, approved by the reviewer."
		case strings.HasPrefix(last, "The delegation to reviewer did not run:"):
			reply = "FINAL: the draft, without a review."
		default:
			reply = `{"invoke": "writer", "task": "two lines on tides"}`
		}
	case strings.HasPrefix(system, "Write clearly."):
		who, reply = "writer", "DRAFT("+last+")"
	case strings.HasPrefix(system, "Review the writer output"):
		who, reply = "reviewer", "REVIEW: clear and correct."
	default:
		who, reply = "unknown", "?"
	}
	m.mu.Lock()
	m.calls[who]++
	if m.models == nil {
		m.models = map[string]bool{}
	}
	m.models[req.Model] = true
	m.mu.Unlock()
	return api.ModelResponse{Message: *api.TextMessage("assistant", reply)}, nil
}

func (m *scriptedModel) count(who string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[who]
}

// lateSessions lets the executor be built before the client of the server it lives in.
type lateSessions struct{ c *client.Client }

func (l *lateSessions) GetSession(ctx context.Context, uid string) (*v1.Session, error) {
	return l.c.GetSession(ctx, uid)
}

func (l *lateSessions) CreateSession(ctx context.Context, s *v1.Session) (*v1.Session, error) {
	return l.c.CreateSession(ctx, s)
}

func (l *lateSessions) Exec(ctx context.Context, o client.ExecOptions) (*client.TurnResult, error) {
	return l.c.Exec(ctx, o)
}

func (l *lateSessions) Replay(ctx context.Context, uid string, from, to int64) ([]*v1.LogRecord, error) {
	return l.c.Replay(ctx, uid, from, to)
}

func (l *lateSessions) ListSessions(ctx context.Context, project string) ([]*v1.Session, error) {
	return l.c.ListSessions(ctx, project)
}

func (l *lateSessions) Suspend(ctx context.Context, uid string) (*v1.Session, error) {
	return l.c.Suspend(ctx, uid)
}

func (l *lateSessions) Resume(ctx context.Context, uid string, boot bool) (*v1.Session, error) {
	return l.c.Resume(ctx, uid, boot)
}

// crashOnce wraps the coordinator's executor: the first call runs (the child session and its turn
// happen) and then fails as a host that died before journaling the result would, so the caller's
// journal holds the TOOL_CALL and no TOOL_RESULT.
type crashOnce struct {
	mu      sync.Mutex
	crashed bool
}

func (c *crashOnce) wrap(tool controller.ToolFunc) controller.ToolFunc {
	return func(ctx context.Context, scope controller.ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
		res, err := tool(ctx, scope, call)
		c.mu.Lock()
		defer c.mu.Unlock()
		if err == nil && !c.crashed {
			c.crashed = true
			return api.ToolResult{}, errors.New("host crashed before the tool result was journaled")
		}
		return res, err
	}
}

// suiteServer serves the three agents as stateless harnesses on local backends, the coordinator
// with the invoke executor over the server's own Sessions API, as agentsessionsd does on substrate.
// crash, when set, wraps the coordinator's executor.
func suiteServer(t *testing.T, model *scriptedModel, crash ...*crashOnce) *client.Client {
	t.Helper()
	agents, err := render.Agents(suiteDir)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := render.LoadBinding(bindingFile)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := invoke.FromAgents(agents, binding.Gates())
	if err != nil {
		t.Fatal(err)
	}
	sessions := &lateSessions{}
	x := invoke.New(policy, sessions)
	placers := map[string]*placement.Placer{}
	for _, id := range []string{"coordinator", "writer", "reviewer"} {
		h := loadAgent(t, id)
		if targets := binding.ApprovalTargets(id); len(targets) > 0 {
			if h, err = h.WithApproval(targets); err != nil {
				t.Fatal(err)
			}
		}
		b := local.New(h)
		t.Cleanup(func() { _ = b.Close() })
		var opts []placement.Option
		if tool := x.For(id); tool != nil {
			for _, c := range crash {
				tool = c.wrap(tool)
			}
			opts = append(opts, placement.WithToolExecutor(tool))
		}
		placers[id] = placement.New(b, controller.ModelFunc(model.model), opts...)
	}
	registry, err := placement.NewRegistry("coordinator", placers)
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	v1.RegisterSessionsServer(srv, session.NewService(store, registry))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	c, err := client.Dial("passthrough:///bufnet", client.WithDialOptions(
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) })))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	sessions.c = c
	return c
}

// children returns the sessions the invocation executor created for parent, by target harness.
func children(t *testing.T, ctx context.Context, c *client.Client, parent string) map[string][]*v1.Session {
	t.Helper()
	all, err := c.ListSessions(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]*v1.Session{}
	for _, s := range all {
		if s.GetLabels()["agentsuite.invoked-by"] == parent {
			out[s.GetHarness()] = append(out[s.GetHarness()], s)
		}
	}
	return out
}

type journalSummary struct {
	kinds     []string
	results   []*v1.ToolResult
	lastOut   string
	startCfgs [][]byte
}

func journal(t *testing.T, ctx context.Context, c *client.Client, uid string) journalSummary {
	t.Helper()
	recs, err := c.Replay(ctx, uid, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	var j journalSummary
	for _, r := range recs {
		ev := r.GetEvent()
		kind := strings.TrimPrefix(ev.GetKind().String(), "EVENT_")
		if lc := ev.GetLifecycle(); lc != nil {
			kind += ":" + strings.TrimPrefix(lc.GetKind().String(), "LIFECYCLE_")
		}
		j.kinds = append(j.kinds, kind)
		if res := ev.GetResult(); res != nil {
			j.results = append(j.results, res)
		}
		if start := ev.GetExecutionStart(); start != nil {
			j.startCfgs = append(j.startCfgs, start.GetConfig())
		}
		if m := ev.GetMessage(); m != nil && kind == "OUTPUT" {
			var b strings.Builder
			for _, p := range m.GetParts() {
				b.WriteString(p.GetText().GetText())
			}
			j.lastOut = b.String()
		}
	}
	return j
}

func pendingDecision(t *testing.T, ctx context.Context, c *client.Client, uid string, approved bool) api.ApprovalDecision {
	t.Helper()
	sess, err := c.GetSession(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	ref := sess.GetPendingApproval()
	if sess.GetExecState() != v1.ExecState_EXEC_AWAITING || ref == nil {
		t.Fatalf("session %s: exec_state %v, pending %v; want AWAITING with a pending approval", uid, sess.GetExecState(), ref)
	}
	return api.ApprovalDecision{
		ExecutionID: ref.GetExecutionId(), ToolCallID: ref.GetToolCallId(), RequestSeq: ref.GetRequestSeq(),
		Approved: approved, Reason: "test", Identity: api.IdentityRef{Principal: "operator"},
	}
}

func contains(kinds []string, want ...string) bool {
	i := 0
	for _, k := range kinds {
		if i < len(want) && k == want[i] {
			i++
		}
	}
	return i == len(want)
}

// One coordinator request delegates to writer (a child session), then parks at the gated reviewer
// call. Approval resumes the turn: the recorded writer result and model replies are served from the
// journal (writer runs once, coordinator's first two model calls are not repeated), the reviewer
// child runs, and the coordinator answers. Both children name the parent in their first execution
// config.
func TestCoordinatorApproved(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	model := &scriptedModel{calls: map[string]int{}}
	c := suiteServer(t, model)
	turn, err := c.Exec(ctx, client.ExecOptions{Harness: "coordinator", Inputs: []string{"Two lines on tides, reviewed."}})
	if err != nil {
		t.Fatal(err)
	}
	uid := turn.Session.GetMetadata().GetUid()
	kids := children(t, ctx, c, uid)
	if len(kids["writer"]) != 1 || len(kids["reviewer"]) != 0 {
		t.Fatalf("children before the decision = %v, want one writer and no reviewer", kids)
	}
	decision := pendingDecision(t, ctx, c, uid, true)
	if decision.ToolCallID != turn.Session.GetPendingApproval().GetToolCallId() || !strings.HasSuffix(decision.ToolCallID, "-invoke-2") {
		t.Fatalf("pending call = %q, want the second delegation", decision.ToolCallID)
	}
	if _, err := c.Approve(ctx, uid, decision); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Resume(ctx, uid, false); err != nil {
		t.Fatal(err)
	}
	j := journal(t, ctx, c, uid)
	if !contains(j.kinds, "EXECUTION_START", "INPUT", "TOOL_CALL", "TOOL_RESULT", "TOOL_CALL", "APPROVAL_REQUEST", "LIFECYCLE:SUSPEND", "APPROVAL_RESULT", "TOOL_RESULT", "OUTPUT") {
		t.Fatalf("coordinator journal = %v", j.kinds)
	}
	if j.lastOut != "FINAL: the draft, approved by the reviewer." {
		t.Fatalf("final output = %q", j.lastOut)
	}
	kids = children(t, ctx, c, uid)
	if len(kids["writer"]) != 1 || len(kids["reviewer"]) != 1 {
		t.Fatalf("children = %v, want one writer and one reviewer", kids)
	}
	review := j.results[len(j.results)-1]
	if review.GetOutput().GetFields()["session"].GetStringValue() != kids["reviewer"][0].GetMetadata().GetUid() || review.GetApprovalDecisionSeq() == 0 {
		t.Fatalf("reviewer receipt = %v", review)
	}
	for target, sessions := range kids {
		cj := journal(t, ctx, c, sessions[0].GetMetadata().GetUid())
		var cfg map[string]invoke.Provenance
		if err := json.Unmarshal(cj.startCfgs[0], &cfg); err != nil {
			t.Fatal(err)
		}
		if p := cfg[invoke.ConfigKey]; p.ParentSession != uid || p.Caller != "coordinator" || p.Target != target || p.Depth != 1 || p.Remaining != 0 {
			t.Fatalf("%s provenance = %+v", target, p)
		}
	}
	if got := [3]int{model.count("coordinator"), model.count("writer"), model.count("reviewer")}; got != [3]int{3, 1, 1} {
		t.Fatalf("live model calls (coordinator, writer, reviewer) = %v, want [3 1 1]", got)
	}
	// Each child was suspended after its turn: none holds compute once it has answered.
	for target, sessions := range kids {
		s, err := c.GetSession(ctx, sessions[0].GetMetadata().GetUid())
		if err != nil {
			t.Fatal(err)
		}
		if s.GetComputeState() != v1.ComputeState_COMPUTE_COLD {
			t.Fatalf("%s child compute = %v, want COLD", target, s.GetComputeState())
		}
	}
	// Every agent requests the model the suite pins, which the binding serves.
	if len(model.models) != 1 || !model.models["gpt-5.4-mini"] {
		t.Fatalf("models requested = %v, want only gpt-5.4-mini", model.models)
	}
}

// A denial is journaled as APPROVAL_RESULT and an APPROVAL_DENIED receipt; no reviewer session is
// created, and the coordinator finishes without the review.
func TestCoordinatorDenied(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	model := &scriptedModel{calls: map[string]int{}}
	c := suiteServer(t, model)
	turn, err := c.Exec(ctx, client.ExecOptions{Harness: "coordinator", Inputs: []string{"Two lines on tides, reviewed."}})
	if err != nil {
		t.Fatal(err)
	}
	uid := turn.Session.GetMetadata().GetUid()
	if _, err := c.Approve(ctx, uid, pendingDecision(t, ctx, c, uid, false)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Resume(ctx, uid, false); err != nil {
		t.Fatal(err)
	}
	j := journal(t, ctx, c, uid)
	if !contains(j.kinds, "APPROVAL_REQUEST", "LIFECYCLE:SUSPEND", "APPROVAL_RESULT", "TOOL_RESULT", "OUTPUT") {
		t.Fatalf("coordinator journal = %v", j.kinds)
	}
	denied := j.results[len(j.results)-1]
	if denied.GetCode() != v1.ToolResult_APPROVAL_DENIED || !denied.GetIsError() {
		t.Fatalf("receipt = %v, want APPROVAL_DENIED", denied)
	}
	if j.lastOut != "FINAL: the draft, without a review." {
		t.Fatalf("final output = %q", j.lastOut)
	}
	if kids := children(t, ctx, c, uid); len(kids["writer"]) != 1 || len(kids["reviewer"]) != 0 {
		t.Fatalf("children = %v, want the writer only", kids)
	}
	if model.count("reviewer") != 0 {
		t.Fatal("the reviewer's model was called after a denial")
	}
}

// The host "crashes" after the writer child's turn and before the coordinator's TOOL_RESULT is
// journaled. Resume re-drives the coordinator's turn under the recorded call: the executor finds the
// writer child by the call's key, serves its recorded turn, and the turn goes on to the gated
// reviewer call. One writer session, one writer model call.
func TestCoordinatorCrashMidDelegation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	model := &scriptedModel{calls: map[string]int{}}
	c := suiteServer(t, model, &crashOnce{})
	turn, err := c.Exec(ctx, client.ExecOptions{Harness: "coordinator", Inputs: []string{"Two lines on tides, reviewed."}})
	if err == nil {
		t.Fatal("the crashed turn succeeded")
	}
	uid := turn.Session.GetMetadata().GetUid()
	j := journal(t, ctx, c, uid)
	if !contains(j.kinds, "TOOL_CALL") || len(j.results) != 0 {
		t.Fatalf("after the crash the journal = %v, want the writer call without a result", j.kinds)
	}
	if kids := children(t, ctx, c, uid); len(kids["writer"]) != 1 {
		t.Fatalf("children after the crash = %v", kids)
	}
	if _, err := c.Resume(ctx, uid, false); err != nil {
		t.Fatal(err)
	}
	j = journal(t, ctx, c, uid)
	if !contains(j.kinds, "TOOL_CALL", "TOOL_RESULT", "TOOL_CALL", "APPROVAL_REQUEST") || len(j.results) != 1 {
		t.Fatalf("after the resume the journal = %v, want the writer result, then the gated reviewer call", j.kinds)
	}
	kids := children(t, ctx, c, uid)
	if len(kids["writer"]) != 1 || model.count("writer") != 1 {
		t.Fatalf("children = %v and %d writer model calls, want one writer child that ran once", kids, model.count("writer"))
	}
	lc := j.results[0].GetOutput().GetFields()["lifecycle"].GetStructValue().GetFields()
	if j.results[0].GetOutput().GetFields()["session"].GetStringValue() != kids["writer"][0].GetMetadata().GetUid() ||
		lc["origin"].GetStringValue() != "retry" || lc["turn"].GetStringValue() != "recorded" {
		t.Fatalf("writer result = %v, want the first child's recorded turn", j.results[0])
	}
}
