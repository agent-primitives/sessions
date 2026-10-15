package agentsessionsd

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aramase/agentsessions/api"
	"github.com/aramase/agentsessions/client"
	"github.com/aramase/agentsessions/controller"
	"github.com/aramase/agentsessions/harness/echoagent"
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
	var asked []string
	executors := func(name string) controller.ToolFunc {
		asked = append(asked, name)
		if name != "with" {
			return nil
		}
		return func(_ context.Context, scope controller.ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
			return api.ToolResult{ID: call.ID, Output: map[string]any{"text": call.Tool + "@" + scope.SessionUID[:4]}}, nil
		}
	}
	registry, closeBackends, err := harnessRegistry("test-model", remoteHarnesses{"with": with, "without": without}, echoagent.Model, nil, nil, nil, executors)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeBackends)
	if len(asked) != 2 || asked[0] != "with" || asked[1] != "without" {
		t.Fatalf("executors asked for %v, want the two -harness names in order", asked)
	}
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
	executors := func(name string, sessions *client.Client) controller.ToolFunc {
		asked = append(asked, name)
		if name != "tooler" {
			return nil
		}
		return func(ctx context.Context, scope controller.ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
			child, err := sessions.Exec(ctx, client.ExecOptions{Harness: "reply", Inputs: []string{"from " + scope.SessionUID[:4]}})
			if err != nil {
				return api.ToolResult{}, err
			}
			return api.ToolResult{ID: call.ID, Output: map[string]any{"text": child.Output + "@" + child.Session.GetMetadata().GetUid()}}, nil
		}
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
