// Command agentsessionsd is the substrate build of agentsessionsd with AgentSuite invocations: on
// top of everything integrations/substrate/cmd/agentsessionsd serves, a harness whose agent
// declares invokes edges gets the host executor of package invoke, which runs each agentsuite.invoke
// call as a child session on the target agent's harness.
//
// The suite's edges come from AGENTSUITE_INVOKES, comma-separated
// caller=target:maxConcurrent:maxDepth[:approval] values as emit invokes prints them from the suite
// and the binding's approval edges. The
// harness names are the agent ids. A malformed value refuses the start.
package main

import (
	"fmt"
	"os"
	"strings"

	"google.golang.org/grpc"

	"github.com/aramase/agentsessions/client"
	"github.com/aramase/agentsessions/controller"
	"github.com/aramase/agentsessions/examples/agentsuite-substrate/invoke"
	ateadapter "github.com/aramase/agentsessions/integrations/substrate"
	"github.com/aramase/agentsessions/internal/agentsessionsd"
	"github.com/aramase/agentsessions/runtime/substrate"
)

// InvokesEnv names the environment variable holding the suite's invocation edges.
const InvokesEnv = "AGENTSUITE_INVOKES"

func main() {
	policy, err := parsePolicy(os.Getenv(InvokesEnv))
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentsessionsd: %s: %v\n", InvokesEnv, err)
		os.Exit(1)
	}
	agentsessionsd.Main(
		agentsessionsd.WithSubstrate(func(conn grpc.ClientConnInterface) substrate.ControlClient { return ateadapter.New(conn) }),
		agentsessionsd.WithToolExecutors(executors(policy)),
	)
}

func parsePolicy(v string) (invoke.Policy, error) {
	p := invoke.Policy{}
	if strings.TrimSpace(v) == "" {
		return p, nil
	}
	for _, edge := range strings.Split(v, ",") {
		if err := p.Set(strings.TrimSpace(edge)); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// executors builds one Executor over the daemon's own Sessions client, so every caller's in-flight
// count lives in one place.
func executors(policy invoke.Policy) agentsessionsd.ToolExecutorFunc {
	var x *invoke.Executor
	return func(harness string, sessions *client.Client) controller.ToolFunc {
		if x == nil {
			x = invoke.New(policy, sessions)
		}
		return x.For(harness)
	}
}
