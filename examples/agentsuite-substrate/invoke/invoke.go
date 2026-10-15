// Package invoke executes AgentSuite agent-to-agent invocations for agentsessionsd. A harness asks
// for one with a host-mediated tool call (Tool, arguments {"agent", "task"}); the host runs it here:
// it checks the suite's invokes edge for the calling agent, creates a child session on the target
// agent's harness through the daemon's own Sessions API, runs one turn with the task, and returns
// the child's output as the tool result. The call and result are TOOL_CALL and TOOL_RESULT records
// in the caller's journal; the child's first EXECUTION_START config records the caller (see
// Provenance), so each journal names the other.
//
// The suite's bounds are enforced before any child session exists: an undeclared edge, a call that
// would exceed the edge's maxConcurrent for the calling session, and a chain deeper than the edge's
// maxDepth (kaimahi agentsuite/spec.md section 8.1) are refused with an error result. An edge the
// deployment marks for approval is refused unless the call arrives as REQUIRES_APPROVAL, so the
// harness cannot skip the host's approval gate by choosing its own mediation.
package invoke

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/kaimahi-agents/kaimahi/agentsuite"

	"github.com/aramase/agentsessions/api"
	v1 "github.com/aramase/agentsessions/api/genpb"
	"github.com/aramase/agentsessions/client"
	"github.com/aramase/agentsessions/controller"
)

// Tool is the tool name of an invocation call.
const Tool = "agentsuite.invoke"

// ConfigKey is the key of the invocation provenance in a child session's first execution config.
const ConfigKey = "agentsuite.invocation"

// Edge is one invokes entry of a calling agent, plus the deployment's approval choice.
type Edge struct {
	Target        string
	MaxConcurrent int
	MaxDepth      int
	// Approval makes the call to Target REQUIRES_APPROVAL: the host parks it until a decision.
	Approval bool
}

// Policy is the suite's invocation graph: the calling agent's id to its edges.
type Policy map[string][]Edge

// FromAgents builds the policy from the suite's agent manifests. approve names the targets whose
// invocations need approval; a name that no edge targets is refused, so a typo cannot leave a
// delegation ungated.
func FromAgents(agents []agentsuite.Agent, approve []string) (Policy, error) {
	p := Policy{}
	targeted := map[string]bool{}
	for _, a := range agents {
		for _, inv := range a.Invokes {
			e := Edge{Target: inv.Agent, MaxConcurrent: inv.MaxConcurrent, MaxDepth: inv.MaxDepth, Approval: slices.Contains(approve, inv.Agent)}
			if err := p.add(a.ID, e); err != nil {
				return nil, err
			}
			targeted[inv.Agent] = true
		}
	}
	for _, name := range approve {
		if !targeted[name] {
			return nil, fmt.Errorf("approval for %q: no invokes edge targets it", name)
		}
	}
	return p, nil
}

func (p Policy) add(caller string, e Edge) error {
	if caller == "" || e.Target == "" {
		return errors.New("an edge needs a caller and a target")
	}
	if e.MaxConcurrent < 1 || e.MaxDepth < 1 {
		return fmt.Errorf("edge %s -> %s: maxConcurrent and maxDepth must be positive", caller, e.Target)
	}
	if _, ok := p.edge(caller, e.Target); ok {
		return fmt.Errorf("edge %s -> %s is given more than once", caller, e.Target)
	}
	p[caller] = append(p[caller], e)
	return nil
}

func (p Policy) edge(caller, target string) (Edge, bool) {
	for _, e := range p[caller] {
		if e.Target == target {
			return e, true
		}
	}
	return Edge{}, false
}

// Flag renders the policy as the -agentsuite-invoke values Set parses, sorted.
func (p Policy) Flag() []string {
	var out []string
	for caller, edges := range p {
		for _, e := range edges {
			v := fmt.Sprintf("%s=%s:%d:%d", caller, e.Target, e.MaxConcurrent, e.MaxDepth)
			if e.Approval {
				v += ":approval"
			}
			out = append(out, v)
		}
	}
	slices.Sort(out)
	return out
}

// String implements flag.Value.
func (p Policy) String() string { return strings.Join(p.Flag(), ",") }

// Set implements flag.Value for one caller=target:maxConcurrent:maxDepth[:approval] edge.
func (p Policy) Set(value string) error {
	caller, rest, ok := strings.Cut(value, "=")
	fields := strings.Split(rest, ":")
	if !ok || len(fields) < 3 || len(fields) > 4 {
		return fmt.Errorf("want caller=target:maxConcurrent:maxDepth[:approval], got %q", value)
	}
	conc, err1 := strconv.Atoi(fields[1])
	depth, err2 := strconv.Atoi(fields[2])
	if err1 != nil || err2 != nil {
		return fmt.Errorf("edge %q: maxConcurrent and maxDepth must be integers", value)
	}
	e := Edge{Target: fields[0], MaxConcurrent: conc, MaxDepth: depth}
	if len(fields) == 4 {
		if fields[3] != "approval" {
			return fmt.Errorf("edge %q: unknown option %q; the only option is approval", value, fields[3])
		}
		e.Approval = true
	}
	return p.add(caller, e)
}

// Args are the arguments of an invocation call.
type Args struct {
	Agent string `json:"agent"`
	Task  string `json:"task"`
}

// Provenance is what a child session's first execution config records under ConfigKey.
type Provenance struct {
	ParentSession string `json:"parentSession"`
	Caller        string `json:"caller"`
	ToolCallID    string `json:"toolCallId"`
	Target        string `json:"target"`
	// Depth is the child's depth in the chain: a session invoked by a root session is depth 1.
	Depth int `json:"depth"`
	// Remaining is how many more invocation edges the chain below the child may use.
	Remaining int `json:"remaining"`
}

// Sessions is the part of the Sessions client the executor uses.
type Sessions interface {
	CreateSession(ctx context.Context, s *v1.Session) (*v1.Session, error)
	Exec(ctx context.Context, opts client.ExecOptions) (*client.TurnResult, error)
	Replay(ctx context.Context, uid string, fromSeq, toSeq int64) ([]*v1.LogRecord, error)
}

// Executor runs invocations for the harnesses of one daemon.
type Executor struct {
	policy   Policy
	sessions Sessions

	mu       sync.Mutex
	inflight map[inflightKey]int
}

type inflightKey struct{ session, target string }

// New returns an executor for policy that drives child sessions through sessions.
func New(policy Policy, sessions Sessions) *Executor {
	return &Executor{policy: policy, sessions: sessions, inflight: map[inflightKey]int{}}
}

// For returns the tool executor for the harness serving agent caller, or nil when the agent
// declares no invokes edge (its host-mediated calls then fail as with no executor).
func (x *Executor) For(caller string) controller.ToolFunc {
	if len(x.policy[caller]) == 0 {
		return nil
	}
	return func(ctx context.Context, scope controller.ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
		return x.invoke(ctx, caller, scope.SessionUID, call)
	}
}

// refuse is a handled refusal: a tool error result the calling agent's model sees, not a failed
// turn.
func refuse(call api.ToolCall, format string, a ...any) (api.ToolResult, error) {
	msg := "invocation refused: " + fmt.Sprintf(format, a...)
	return api.ToolResult{ID: call.ID, IsError: true, Error: msg, Content: api.TextMessage("tool", msg).Parts}, nil
}

func (x *Executor) invoke(ctx context.Context, caller, session string, call api.ToolCall) (api.ToolResult, error) {
	if call.Tool != Tool {
		return refuse(call, "unknown tool %q; the host executes %s only", call.Tool, Tool)
	}
	raw, err := json.Marshal(call.Args)
	if err != nil {
		return refuse(call, "arguments: %v", err)
	}
	var args Args
	if err := json.Unmarshal(raw, &args); err != nil || args.Agent == "" || strings.TrimSpace(args.Task) == "" {
		return refuse(call, `want arguments {"agent": "<id>", "task": "<text>"}`)
	}
	edge, ok := x.policy.edge(caller, args.Agent)
	if !ok {
		return refuse(call, "agent %q declares no invokes edge to %q", caller, args.Agent)
	}
	if edge.Approval && call.Mediation != api.MediationRequiresApproval {
		return refuse(call, "the deployment requires approval for %s -> %s, and the call is %s", caller, args.Agent, call.Mediation)
	}
	remaining, depth, err := x.budget(ctx, session)
	if err != nil {
		return api.ToolResult{}, err
	}
	if remaining < 1 {
		return refuse(call, "%s -> %s exceeds maxDepth: session %s is at depth %d and its chain allows no deeper invocation", caller, args.Agent, session, depth)
	}
	release, ok := x.acquire(session, edge)
	if !ok {
		return refuse(call, "%s -> %s exceeds maxConcurrent %d for session %s", caller, args.Agent, edge.MaxConcurrent, session)
	}
	defer release()

	prov := Provenance{
		ParentSession: session, Caller: caller, ToolCallID: call.ID, Target: args.Agent,
		Depth: depth + 1, Remaining: min(remaining, edge.MaxDepth) - 1,
	}
	config, err := json.Marshal(map[string]Provenance{ConfigKey: prov})
	if err != nil {
		return api.ToolResult{}, err
	}
	child, err := x.sessions.CreateSession(ctx, &v1.Session{
		Metadata: &v1.ResourceMetadata{Name: caller + "-" + args.Agent},
		Harness:  args.Agent,
		Labels:   map[string]string{"agentsuite.invoked-by": session, "agentsuite.edge": caller + "-to-" + args.Agent},
	})
	if err != nil {
		return api.ToolResult{}, fmt.Errorf("invoke %s: create child session: %w", args.Agent, err)
	}
	uid := child.GetMetadata().GetUid()
	turn, err := x.sessions.Exec(ctx, client.ExecOptions{Session: uid, Inputs: []string{args.Task}, Config: config})
	if err != nil {
		// The child exists and its journal holds what happened; the caller's model gets the failure.
		msg := fmt.Sprintf("invocation of %s failed in child session %s: %v", args.Agent, uid, err)
		return api.ToolResult{ID: call.ID, IsError: true, Error: msg, Output: map[string]any{"session": uid, "agent": args.Agent},
			Content: api.TextMessage("tool", msg).Parts}, nil
	}
	return api.ToolResult{
		ID:      call.ID,
		Output:  map[string]any{"session": uid, "agent": args.Agent, "output": turn.Output},
		Content: api.TextMessage("tool", turn.Output).Parts,
	}, nil
}

// acquire takes one of edge.MaxConcurrent slots for the calling session's invocations of the
// target. The count is in memory: a call in flight dies with the daemon.
func (x *Executor) acquire(session string, edge Edge) (func(), bool) {
	k := inflightKey{session, edge.Target}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.inflight[k] >= edge.MaxConcurrent {
		return nil, false
	}
	x.inflight[k]++
	return func() {
		x.mu.Lock()
		defer x.mu.Unlock()
		if x.inflight[k]--; x.inflight[k] == 0 {
			delete(x.inflight, k)
		}
	}, true
}

// budget reads the calling session's place in an invocation chain from its journal: the
// provenance in its first EXECUTION_START config, written by the invocation that created it. A
// session without one was started by a client, so it is a root: depth 0 and no chain bound beyond
// the edge's own maxDepth.
func (x *Executor) budget(ctx context.Context, session string) (remaining, depth int, err error) {
	recs, err := x.sessions.Replay(ctx, session, 1, 0)
	if err != nil {
		return 0, 0, fmt.Errorf("invoke: read the calling session's journal: %w", err)
	}
	for _, r := range recs {
		start := r.GetEvent().GetExecutionStart()
		if start == nil {
			continue
		}
		var cfg map[string]json.RawMessage
		if len(start.GetConfig()) == 0 || json.Unmarshal(start.GetConfig(), &cfg) != nil || cfg[ConfigKey] == nil {
			return math.MaxInt, 0, nil
		}
		var prov Provenance
		if err := json.Unmarshal(cfg[ConfigKey], &prov); err != nil {
			return 0, 0, fmt.Errorf("invoke: session %s: invocation provenance: %w", session, err)
		}
		return prov.Remaining, prov.Depth, nil
	}
	return math.MaxInt, 0, nil
}
