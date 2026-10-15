package agentsessionsd

import (
	"context"
	"errors"
	"net"
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
