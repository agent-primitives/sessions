package agentsessionsd

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/client"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/harness/echoagent"
)

// toolHarness makes one CONTROLLER_MEDIATED tool call per turn and outputs the result's text.
type toolHarness struct{}

func (toolHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "tooler", Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}}, nil
}

func (toolHarness) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	res, err := sink.ToolCall(ctx, api.ToolCall{ID: start.ExecutionID + "-1", Tool: "probe", Mediation: api.MediationControllerMediated, IdempotencyKey: start.ExecutionID + "-1"})
	if err != nil {
		return err
	}
	if res.IsError {
		return sink.Output(ctx, "error:"+res.Error)
	}
	out, _ := res.Output["text"].(string)
	return sink.Output(ctx, "tool:"+out)
}

// The executors function is asked for every -harness name and installs the executor it returns on
// that harness only: the named harness's host-mediated call runs it, and another harness without one
// fails its call instead of borrowing it.
func TestHarnessRegistryInstallsExecutorsByName(t *testing.T) {
	with := serveHarness(t, toolHarness{})
	without := serveHarness(t, toolHarness{})
	remotes := remoteHarnesses{"with": with, "without": without}
	var asked []string
	executors, err := toolExecutors(remotes, func(name string, _ *client.Client) (ToolExecutor, error) {
		asked = append(asked, name)
		if name != "with" {
			return ToolExecutor{}, nil
		}
		return ToolExecutor{Call: func(_ context.Context, scope controller.ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
			return api.ToolResult{ID: call.ID, Output: map[string]any{"text": call.Tool + "@" + scope.SessionUID[:4]}}, nil
		}}, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) != 2 || asked[0] != "with" || asked[1] != "without" {
		t.Fatalf("executors asked for %v, want the two -harness names in order", asked)
	}
	if _, ok := executors["without"]; ok || len(executors) != 1 {
		t.Fatalf("executors = %v, want only the harness that has one", executors)
	}
	registry, closeBackends, err := harnessRegistry("test-model", remotes, echoagent.Model, nil, nil, executors, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeBackends)
	c := newTestClient(t, registry)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	turn, err := c.Exec(ctx, client.ExecOptions{Harness: "with", Inputs: []string{"hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "tool:probe@" + turn.Session.GetMetadata().GetUid()[:4]; turn.Output != want {
		t.Fatalf("output = %q, want %q", turn.Output, want)
	}
	if _, err := c.Exec(ctx, client.ExecOptions{Harness: "without", Inputs: []string{"hi"}}); err == nil {
		t.Fatal("a harness without an executor must fail its host-mediated call")
	} else if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exec without an executor hung: %v", err)
	}
}

// The registry passes each harness's durable-dedup declaration to its Placer: after an executor
// failure leaves a trailing intent, Resume re-drives it on the harness that declares the tool and
// refuses it, FAILED_PRECONDITION and without running the executor, on the one that declares none.
func TestHarnessRegistryPassesDurableDedupDeclarations(t *testing.T) {
	remotes := remoteHarnesses{"declared": serveHarness(t, toolHarness{}), "undeclared": serveHarness(t, toolHarness{})}
	var failing atomic.Bool
	var runs atomic.Int32
	executors, err := toolExecutors(remotes, func(name string, _ *client.Client) (ToolExecutor, error) {
		tool := ToolExecutor{Call: func(_ context.Context, _ controller.ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
			runs.Add(1)
			if failing.Load() {
				return api.ToolResult{}, errors.New("probe: transport down")
			}
			return api.ToolResult{Output: map[string]any{"text": call.Tool}}, nil
		}}
		if name == "declared" {
			tool.DurableDedup = func(tool string) bool { return tool == "probe" }
		}
		return tool, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry, closeBackends, err := harnessRegistry("test-model", remotes, echoagent.Model, nil, nil, executors, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeBackends)
	c := newTestClient(t, registry)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for _, name := range []string{"declared", "undeclared"} {
		failing.Store(true)
		var uid string
		if _, err := c.Exec(ctx, client.ExecOptions{Harness: name, Inputs: []string{"hi"}, OnSession: func(s *v1.Session) { uid = s.GetMetadata().GetUid() }}); err == nil || uid == "" {
			t.Fatalf("%s: Exec = %v (session %q), want the executor failure", name, err, uid)
		}
		failing.Store(false)
		before := runs.Load()
		_, err := c.Resume(ctx, uid, false)
		if name == "declared" {
			if err != nil || runs.Load() != before+1 {
				t.Fatalf("%s: Resume = %v after %d executor runs, want one re-drive", name, err, runs.Load()-before)
			}
			continue
		}
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "repeating it could repeat the action") || runs.Load() != before {
			t.Fatalf("%s: Resume = %v after %d executor runs, want FailedPrecondition and none", name, err, runs.Load()-before)
		}
	}
}

// An executor that cannot be built fails the start: Run returns its error, naming the harness, as a
// plain error (Main's status 1) before it opens the journal or serves, and asks no later name.
func TestRunToolExecutorErrorFailsStartup(t *testing.T) {
	errConfig := errors.New("probe: no endpoint configured")
	var asked []string
	executors := func(name string, _ *client.Client) (ToolExecutor, error) {
		asked = append(asked, name)
		if name == "broken" {
			return ToolExecutor{}, errConfig
		}
		return ToolExecutor{Call: func(context.Context, controller.ToolCallContext, api.ToolCall) (api.ToolResult, error) {
			return api.ToolResult{}, nil
		}}, nil
	}
	journal := filepath.Join(t.TempDir(), "journal.db")
	listened := false
	// A cancelled context makes a regression that serves return nil at once instead of hanging.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Run([]string{"-journal", journal, "-addr", "127.0.0.1:0", "-model", "m",
		"-harness", "alpha=127.0.0.1:9000", "-harness", "broken=127.0.0.1:9001", "-harness", "zulu=127.0.0.1:9002"},
		WithToolExecutors(executors), withContext(ctx), withListening(func(string) { listened = true }))
	if !errors.Is(err, errConfig) {
		t.Fatalf("Run error = %v, want the executor's error", err)
	}
	if errors.Is(err, ErrUsage) {
		t.Errorf("Run error = %v is ErrUsage; an executor error is a configuration error (status 1)", err)
	}
	if !strings.Contains(err.Error(), `harness "broken"`) {
		t.Errorf("Run error = %v, want it to name the harness", err)
	}
	if listened {
		t.Error("Run served the Sessions API before reporting the executor error")
	}
	if !slices.Equal(asked, []string{"alpha", "broken"}) {
		t.Errorf("executors asked for %v, want alpha then broken and nothing after the error", asked)
	}
	if _, statErr := os.Stat(journal); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a refused start left a journal behind: %v", statErr)
	}
}

// replyHarness outputs "reply:" plus its first input's text, without a model call.
type replyHarness struct{}

func (replyHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "reply", Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}}, nil
}

func (replyHarness) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	var text string
	if len(start.Inputs) > 0 {
		text = start.Inputs[0].Text()
	}
	return sink.Output(ctx, "reply:"+text)
}

// Through Run, an executor's sessions client reaches the daemon's own Sessions listener: the
// tooler harness's host-mediated call runs a turn in a child session on the reply harness over that
// loopback client, and the child's output comes back as the tool result. The wildcard bind checks
// the dial address is the loopback form, not the bind address.
func TestRunToolExecutorsReachTheDaemonOverLoopback(t *testing.T) {
	tooler := serveHarness(t, toolHarness{})
	reply := serveHarness(t, replyHarness{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	addrs := make(chan string, 1)
	var asked []string
	executors := func(name string, sessions *client.Client) (ToolExecutor, error) {
		asked = append(asked, name)
		if name != "tooler" {
			return ToolExecutor{}, nil
		}
		return ToolExecutor{Call: func(ctx context.Context, scope controller.ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
			child, err := sessions.Exec(ctx, client.ExecOptions{Harness: "reply", Inputs: []string{"from " + scope.SessionUID[:4]}})
			if err != nil {
				return api.ToolResult{}, err
			}
			return api.ToolResult{ID: call.ID, Output: map[string]any{"text": child.Output + "@" + child.Session.GetMetadata().GetUid()}}, nil
		}}, nil
	}
	done := make(chan error, 1)
	go func() {
		done <- Run([]string{"-journal", t.TempDir() + "/journal.db", "-addr", "0.0.0.0:0", "-model", "m", "-harness", "tooler=" + tooler, "-harness", "reply=" + reply},
			WithToolExecutors(executors), withContext(ctx), withListening(func(addr string) { addrs <- addr }))
	}()
	var addr string
	select {
	case addr = <-addrs:
	case err := <-done:
		t.Fatalf("Run returned before serving: %v", err)
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.Dial("127.0.0.1:" + port)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	tctx, tcancel := context.WithTimeout(ctx, 10*time.Second)
	defer tcancel()
	turn, err := c.Exec(tctx, client.ExecOptions{Harness: "tooler", Inputs: []string{"hi"}})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	parent := turn.Session.GetMetadata().GetUid()
	got, ok := strings.CutPrefix(turn.Output, "tool:reply:from "+parent[:4]+"@")
	if !ok || got == "" || got == parent {
		t.Fatalf("output = %q, want the reply child's output and its own session uid", turn.Output)
	}
	if _, err := c.GetSession(tctx, got); err != nil {
		t.Fatalf("the child session %s is not in the daemon's journal: %v", got, err)
	}
	if !slices.Contains(asked, "tooler") {
		t.Fatalf("executors asked for %v, want tooler", asked)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// withContext makes ctx the parent of Run's signal context.
func withContext(ctx context.Context) Option { return func(o *options) { o.ctx = ctx } }

// withListening reports the Sessions listener's address once Run serves.
func withListening(f func(addr string)) Option { return func(o *options) { o.listening = f } }

func TestLoopbackDialAddr(t *testing.T) {
	for _, tc := range []struct {
		bind net.TCPAddr
		want string
	}{
		{net.TCPAddr{Port: 8080}, "127.0.0.1:8080"},
		{net.TCPAddr{IP: net.IPv4zero, Port: 8080}, "127.0.0.1:8080"},
		{net.TCPAddr{IP: net.IPv6unspecified, Port: 8080}, "[::1]:8080"},
		{net.TCPAddr{IP: net.IPv4(10, 0, 0, 7), Port: 9}, "10.0.0.7:9"},
	} {
		if got := loopbackDialAddr(&tc.bind); got != tc.want {
			t.Errorf("loopbackDialAddr(%v) = %q, want %q", tc.bind, got, tc.want)
		}
	}
}

// gatedHarness makes one REQUIRES_APPROVAL tool call per turn and outputs the result.
type gatedHarness struct{}

func (gatedHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "gated", Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}}, nil
}

func (gatedHarness) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	res, err := sink.ToolCall(ctx, api.ToolCall{ID: start.ExecutionID + "-1", Tool: "publish", Mediation: api.MediationRequiresApproval, IdempotencyKey: start.ExecutionID + "-1"})
	if err != nil {
		return err
	}
	if res.IsError {
		return sink.Output(ctx, "error:"+res.Error)
	}
	out, _ := res.Output["text"].(string)
	return sink.Output(ctx, "tool:"+out)
}

// An executor installed through Run does not bypass the approval gate: a REQUIRES_APPROVAL call
// parks the turn without running it, a denial never runs it, and only an approval followed by
// Resume runs it, once.
func TestRunToolExecutorsKeepTheApprovalGate(t *testing.T) {
	gated := serveHarness(t, gatedHarness{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	addrs := make(chan string, 1)
	var runs atomic.Int32
	executors := func(name string, _ *client.Client) (ToolExecutor, error) {
		if name != "gated" {
			return ToolExecutor{}, nil
		}
		return ToolExecutor{Call: func(_ context.Context, _ controller.ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
			runs.Add(1)
			return api.ToolResult{ID: call.ID, Output: map[string]any{"text": "published"}}, nil
		}}, nil
	}
	done := make(chan error, 1)
	go func() {
		done <- Run([]string{"-journal", t.TempDir() + "/journal.db", "-addr", "127.0.0.1:0", "-model", "m", "-harness", "gated=" + gated},
			WithToolExecutors(executors), withContext(ctx), withListening(func(addr string) { addrs <- addr }))
	}()
	var addr string
	select {
	case addr = <-addrs:
	case err := <-done:
		t.Fatalf("Run returned before serving: %v", err)
	}
	c, err := client.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	tctx, tcancel := context.WithTimeout(ctx, 10*time.Second)
	defer tcancel()

	for _, approved := range []bool{false, true} {
		before := runs.Load()
		turn, err := c.Exec(tctx, client.ExecOptions{Harness: "gated", Inputs: []string{"hi"}})
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		uid := turn.Session.GetMetadata().GetUid()
		ref := turn.Session.GetPendingApproval()
		if ref == nil || ref.GetRequestSeq() <= 0 {
			t.Fatalf("approved=%v: session after the gated call = %v, want a pending approval", approved, turn.Session)
		}
		if got := runs.Load(); got != before {
			t.Fatalf("approved=%v: executor ran %d times before a decision, want 0", approved, got-before)
		}
		if _, err := c.Approve(tctx, uid, api.ApprovalDecision{ExecutionID: ref.GetExecutionId(), ToolCallID: ref.GetToolCallId(), RequestSeq: ref.GetRequestSeq(), Approved: approved, Reason: "test"}); err != nil {
			t.Fatalf("approved=%v: approve: %v", approved, err)
		}
		if got := runs.Load(); got != before {
			t.Fatalf("approved=%v: Approve ran the executor %d times; only Resume continues the turn", approved, got-before)
		}
		resumed, err := c.Resume(tctx, uid, false)
		if err != nil {
			t.Fatalf("approved=%v: resume: %v", approved, err)
		}
		if resumed.GetPendingApproval() != nil {
			t.Fatalf("approved=%v: session after Resume still has a pending approval: %v", approved, resumed)
		}
		want := int32(0)
		if approved {
			want = 1
		}
		if got := runs.Load() - before; got != want {
			t.Fatalf("approved=%v: executor ran %d times, want %d", approved, got, want)
		}
		records, err := c.Replay(tctx, uid, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		var output string
		for _, r := range records {
			if ev := r.GetEvent(); ev.GetKind() == v1.EventKind_EVENT_OUTPUT {
				for _, p := range ev.GetMessage().GetParts() {
					output += p.GetText().GetText()
				}
			}
		}
		if approved && output != "tool:published" || !approved && !strings.HasPrefix(output, "error:") {
			t.Fatalf("approved=%v: turn output = %q", approved, output)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}
