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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"google.golang.org/grpc"

	"github.com/aramase/agentsessions/client"
	"github.com/aramase/agentsessions/controller"
	"github.com/aramase/agentsessions/examples/agentsuite-substrate/agent"
	"github.com/aramase/agentsessions/examples/agentsuite-substrate/invoke"
	ateadapter "github.com/aramase/agentsessions/integrations/substrate"
	"github.com/aramase/agentsessions/internal/agentsessionsd"
	"github.com/aramase/agentsessions/runtime/substrate"
)

// InvokesEnv names the environment variable holding the suite's invocation edges.
const InvokesEnv = "AGENTSUITE_INVOKES"

// MembersEnv names the environment variable that serves the suite's agents from one shared harness
// template: base64 of a JSON object mapping each agent id (its harness name) to its agent.Member,
// as emit members prints it. The daemon adds the member to every new turn's execution config on
// that harness, where the shared harness reads it.
const MembersEnv = "AGENTSUITE_MEMBERS_JSON"

func main() {
	policy, err := parsePolicy(os.Getenv(InvokesEnv))
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentsessionsd: %s: %v\n", InvokesEnv, err)
		os.Exit(1)
	}
	opts := []agentsessionsd.Option{
		agentsessionsd.WithSubstrate(func(conn grpc.ClientConnInterface) substrate.ControlClient { return ateadapter.New(conn) }),
		agentsessionsd.WithToolExecutors(executors(policy)),
	}
	if v := os.Getenv(MembersEnv); v != "" {
		members, err := parseMembers(v)
		if err != nil {
			fmt.Fprintf(os.Stderr, "agentsessionsd: %s: %v\n", MembersEnv, err)
			os.Exit(1)
		}
		opts = append(opts, agentsessionsd.WithExecConfig(agent.Inject(members)))
	}
	agentsessionsd.Main(opts...)
}

// parseMembers decodes a MembersEnv value and checks that each member's instructions match the
// digest it carries, under its own id.
func parseMembers(v string) (map[string]agent.Member, error) {
	raw, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil, fmt.Errorf("not base64: %w", err)
	}
	var members map[string]agent.Member
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return nil, errors.New("no members")
	}
	for id, m := range members {
		if m.ID != id || m.Model == "" || agent.Digest(m.Instructions) != m.InstructionsDigest {
			return nil, fmt.Errorf("member %q: want its own id, a model, and instructions matching instructionsDigest", id)
		}
	}
	return members, nil
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
// count lives in one place. It logs each invocation's child lifecycle (created, reused, woken,
// suspended, and how long each took) to stderr.
func executors(policy invoke.Policy) agentsessionsd.ToolExecutorFunc {
	var x *invoke.Executor
	return func(harness string, sessions *client.Client) controller.ToolFunc {
		if x == nil {
			x = invoke.New(policy, sessions, invoke.WithLogger(slog.New(slog.NewTextHandler(os.Stderr, nil))))
		}
		return x.For(harness)
	}
}
