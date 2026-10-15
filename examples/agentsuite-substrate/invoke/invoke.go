// Package invoke executes AgentSuite agent-to-agent invocations for agentsessionsd. A harness asks
// for one with a host-mediated tool call (Tool, arguments {"agent", "task"}); the host runs it here:
// it checks the suite's invokes edge for the calling agent, creates a child session on the target
// agent's harness through the daemon's own Sessions API, runs one turn with the task, and returns
// the child's output as the tool result. The call and result are TOOL_CALL and TOOL_RESULT records
// in the caller's journal; the child's first EXECUTION_START config records the caller (see
// Provenance), so each journal names the other.
//
// A child's actor is suspended after each of its turns, so a delegation does not hold a worker once
// it has answered. A call that names a child (Args.Session) is a follow-up: the executor wakes that
// child with Resume, runs the task there and suspends it again.
//
// Child creation is idempotent per call: the child carries Key(calling session, tool call id) as a
// label, and the executor looks for it before it creates one. A call re-driven after a host crash
// (the caller's TOOL_CALL is journaled, its TOOL_RESULT is not) therefore reaches the same child,
// and the child's journal says whether that call's turn there finished (served from the journal),
// was interrupted (re-driven with Resume) or never started (run now).
//
// The suite's bounds are enforced before any child session exists: an undeclared edge, a call that
// would exceed the edge's maxConcurrent (in-flight invocations of the target by the declaring agent,
// across all of its sessions on this daemon), and a chain deeper than the edge's maxDepth (kaimahi
// agentsuite/spec.md section 8.1) are refused with an error result. An edge the
// deployment marks for approval is refused unless the call arrives as REQUIRES_APPROVAL, so the
// harness cannot skip the host's approval gate by choosing its own mediation.
package invoke

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kaimahi-agents/kaimahi/agentsuite"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/aramase/agentsessions/api"
	v1 "github.com/aramase/agentsessions/api/genpb"
	"github.com/aramase/agentsessions/client"
	"github.com/aramase/agentsessions/controller"
	"github.com/aramase/agentsessions/wire"
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

// Gate names one invokes edge, caller -> agent, whose calls need approval. The deployment chooses
// the gates (the AgentSessionsBinding's approval.invokes); the suite declares the edges.
type Gate struct {
	Caller string `json:"caller"`
	Agent  string `json:"agent"`
}

// FromAgents builds the policy from the suite's agent manifests. gates are the edges whose
// invocations need approval; a gate on an edge the suite does not declare is refused, so a typo
// cannot leave a delegation ungated.
func FromAgents(agents []agentsuite.Agent, gates []Gate) (Policy, error) {
	p := Policy{}
	for _, a := range agents {
		for _, inv := range a.Invokes {
			e := Edge{Target: inv.Agent, MaxConcurrent: inv.MaxConcurrent, MaxDepth: inv.MaxDepth, Approval: slices.Contains(gates, Gate{Caller: a.ID, Agent: inv.Agent})}
			if err := p.add(a.ID, e); err != nil {
				return nil, err
			}
		}
	}
	for _, g := range gates {
		if _, ok := p.edge(g.Caller, g.Agent); !ok {
			return nil, fmt.Errorf("approval for %s -> %s: the suite declares no such invokes edge", g.Caller, g.Agent)
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
	// Session, when set, sends the task to a child session an earlier call of the same calling
	// session created on Agent (a follow-up), instead of creating a new one. The executor wakes the
	// child, runs the turn there and suspends it again.
	Session string `json:"session,omitempty"`
}

// Labels the executor sets on a child session. They are client-set metadata, not provenance: the
// tamper-evident link is the child's EXECUTION_START config and the caller's TOOL_RESULT.
const (
	// LabelInvokedBy is the calling session's uid.
	LabelInvokedBy = "agentsuite.invoked-by"
	// LabelEdge is <caller>-to-<target>.
	LabelEdge = "agentsuite.edge"
	// LabelKey is the child's idempotency key (Key): the executor finds an existing child by it
	// before it creates one.
	LabelKey = "agentsuite.invocation-key"
)

// Key is the idempotency key of the child session an invocation creates: a digest of the calling
// session's uid and the tool call's id. The call id is stable across replay and resume (the harness
// derives it from the execution id), so a re-driven call after a host crash computes the same key.
// The digest keeps the label short; the inputs are separated by a NUL so that no two (session, call)
// pairs share an encoding.
func Key(parent, callID string) string {
	sum := sha256.Sum256([]byte(parent + "\x00" + callID))
	return hex.EncodeToString(sum[:16])
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
	GetSession(ctx context.Context, uid string) (*v1.Session, error)
	ListSessions(ctx context.Context, project string) ([]*v1.Session, error)
	CreateSession(ctx context.Context, s *v1.Session) (*v1.Session, error)
	Exec(ctx context.Context, opts client.ExecOptions) (*client.TurnResult, error)
	Replay(ctx context.Context, uid string, fromSeq, toSeq int64) ([]*v1.LogRecord, error)
	Suspend(ctx context.Context, uid string) (*v1.Session, error)
	Resume(ctx context.Context, uid string, boot bool) (*v1.Session, error)
}

// DefaultTimeout bounds one invocation's child turn when New is given no WithTimeout.
const DefaultTimeout = 5 * time.Minute

// Executor runs invocations for the harnesses of one daemon.
type Executor struct {
	policy   Policy
	sessions Sessions
	timeout  time.Duration

	mu       sync.Mutex
	inflight map[inflightKey]int
	// createMu serializes the find-or-create of a child by its key on this daemon, so two
	// re-drives of one call here cannot both miss the lookup and create two children. Another
	// daemon on the same journal is not covered: that needs a key the Sessions API enforces.
	createMu sync.Mutex
	logger   *slog.Logger
}

// inflightKey is what maxConcurrent counts: the declaring (calling) agent's invocations of target,
// whichever of its sessions makes them.
type inflightKey struct{ caller, target string }

// Option configures an Executor.
type Option func(*Executor)

// WithTimeout bounds each invocation's child session creation and turn, so a hung worker cannot
// hold the caller's turn and its maxConcurrent slot past d. d must be positive.
func WithTimeout(d time.Duration) Option {
	return func(x *Executor) { x.timeout = d }
}

// WithLogger logs each invocation's child lifecycle (created or reused, wake, turn and suspend
// times). Without it nothing is logged.
func WithLogger(l *slog.Logger) Option {
	return func(x *Executor) { x.logger = l }
}

// New returns an executor for policy that drives child sessions through sessions.
func New(policy Policy, sessions Sessions, opts ...Option) *Executor {
	x := &Executor{policy: policy, sessions: sessions, timeout: DefaultTimeout, inflight: map[inflightKey]int{}}
	for _, o := range opts {
		o(x)
	}
	if x.timeout <= 0 {
		x.timeout = DefaultTimeout
	}
	if x.logger == nil {
		x.logger = slog.New(slog.DiscardHandler)
	}
	return x
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
	release, ok := x.acquire(caller, edge)
	if !ok {
		return refuse(call, "%s -> %s exceeds maxConcurrent %d: agent %s already has %d invocation(s) of %s in flight", caller, args.Agent, edge.MaxConcurrent, caller, edge.MaxConcurrent, args.Agent)
	}
	defer release()

	// The child lives in the caller's project, and its creation and turn are bounded by the
	// executor's timeout (and by the caller's own deadline, when ctx carries an earlier one).
	ctx, cancel := context.WithTimeout(ctx, x.timeout)
	defer cancel()
	parent, err := x.sessions.GetSession(ctx, session)
	if err != nil {
		return api.ToolResult{}, fmt.Errorf("invoke %s: read the calling session: %w", args.Agent, err)
	}
	project := parent.GetMetadata().GetProject()
	var child *v1.Session
	origin := "new"
	if args.Session != "" {
		var why string
		if child, why, err = x.followUp(ctx, session, project, args); err != nil {
			return api.ToolResult{}, err
		}
		if why != "" {
			return refuse(call, "%s", why)
		}
		origin = "follow-up"
	} else {
		var created bool
		if child, created, err = x.child(ctx, caller, session, project, args.Agent, call.ID); err != nil {
			return api.ToolResult{}, err
		}
		if !created {
			origin = "retry"
		}
	}
	uid := child.GetMetadata().GetUid()

	prov := Provenance{
		ParentSession: session, Caller: caller, ToolCallID: call.ID, Target: args.Agent,
		Depth: depth + 1, Remaining: min(remaining, edge.MaxDepth) - 1,
	}
	config, err := json.Marshal(map[string]Provenance{ConfigKey: prov})
	if err != nil {
		return api.ToolResult{}, err
	}
	out, lc, turnErr := x.turn(ctx, child, call.ID, args.Task, config)
	lc["origin"] = origin
	// The child's turn is over, done or failed: suspend its actor so it does not hold a worker
	// until someone deletes it. A follow-up call wakes it again. A failed suspend does not undo
	// the turn; the result says the child is still running.
	x.suspend(ctx, uid, lc)
	x.logger.Info("agentsuite invocation", "caller", caller, "session", session, "tool_call", call.ID, "target", args.Agent, "child", uid, "lifecycle", lc)
	output := map[string]any{"session": uid, "agent": args.Agent, "lifecycle": lc}
	if turnErr != nil {
		// The child exists and its journal holds what happened; the caller's model gets the failure.
		msg := fmt.Sprintf("invocation of %s failed in child session %s: %v", args.Agent, uid, turnErr)
		return api.ToolResult{ID: call.ID, IsError: true, Error: msg, Output: output, Content: api.TextMessage("tool", msg).Parts}, nil
	}
	output["output"] = out
	return api.ToolResult{ID: call.ID, Output: output, Content: api.TextMessage("tool", out).Parts}, nil
}

// child finds the child session an earlier attempt of this call created (by its LabelKey), or
// creates it. A host that crashed after CreateSession and before the caller's TOOL_RESULT re-drives
// the same call on recovery; the lookup makes that retry reach the same child instead of a second
// one. created reports whether this call created it.
func (x *Executor) child(ctx context.Context, caller, parent, project, target, callID string) (_ *v1.Session, created bool, _ error) {
	key := Key(parent, callID)
	x.createMu.Lock()
	defer x.createMu.Unlock()
	all, err := x.sessions.ListSessions(ctx, project)
	if err != nil {
		return nil, false, fmt.Errorf("invoke %s: look up the child of call %s: %w", target, callID, err)
	}
	for _, s := range all {
		if s.GetLabels()[LabelKey] != key {
			continue
		}
		if s.GetLabels()[LabelInvokedBy] != parent || s.GetHarness() != target {
			return nil, false, fmt.Errorf("invoke %s: session %s carries call %s's key but another caller or harness", target, s.GetMetadata().GetUid(), callID)
		}
		return s, false, nil
	}
	s, err := x.sessions.CreateSession(ctx, &v1.Session{
		Metadata: &v1.ResourceMetadata{Project: project, Name: caller + "-" + target + "-" + key[:12]},
		Harness:  target,
		Labels:   map[string]string{LabelInvokedBy: parent, LabelEdge: caller + "-to-" + target, LabelKey: key},
	})
	if err != nil {
		return nil, false, fmt.Errorf("invoke %s: create child session: %w", target, err)
	}
	return s, true, nil
}

// followUp checks that args.Session is a child the calling session created on args.Agent: its
// labels and its first execution's provenance name the caller and the target, and it is in the
// caller's project. why is the refusal the caller's model sees; err is a failure to check.
func (x *Executor) followUp(ctx context.Context, parent, project string, args Args) (_ *v1.Session, why string, _ error) {
	notMine := fmt.Sprintf("session %q is not a %s session this session invoked", args.Session, args.Agent)
	s, err := x.sessions.GetSession(ctx, args.Session)
	if status.Code(err) == codes.NotFound {
		return nil, notMine, nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("invoke %s: read session %s: %w", args.Agent, args.Session, err)
	}
	if s.GetHarness() != args.Agent || s.GetLabels()[LabelInvokedBy] != parent || s.GetMetadata().GetProject() != project {
		return nil, notMine, nil
	}
	prov, ok, err := x.provenance(ctx, args.Session)
	if err != nil {
		return nil, "", err
	}
	if !ok || prov.ParentSession != parent || prov.Target != args.Agent {
		return nil, notMine, nil
	}
	return s, "", nil
}

// turn runs the call's task in child, unless the child's journal shows an earlier attempt of the
// same call (its EXECUTION_START names this tool call id): a finished one is served from the
// child's journal (its output, or its failure for a turn that ended with ERROR), an interrupted
// one is re-driven with Resume. lc records what happened and how long each step took, in
// milliseconds.
func (x *Executor) turn(ctx context.Context, child *v1.Session, callID, task string, config []byte) (string, map[string]any, error) {
	uid := child.GetMetadata().GetUid()
	lc := map[string]any{}
	rec, err := x.recorded(ctx, uid, callID)
	if err != nil {
		return "", lc, err
	}
	switch {
	case rec.ended:
		lc["turn"] = "recorded"
	case rec.started:
		start := time.Now()
		if _, err := x.sessions.Resume(ctx, uid, false); err != nil {
			return "", lc, fmt.Errorf("re-drive the interrupted turn: %w", err)
		}
		lc["turn"], lc["resumeMs"] = "re-driven", ms(start)
		if rec, err = x.recorded(ctx, uid, callID); err != nil {
			return "", lc, err
		}
		if !rec.ended {
			return "", lc, errors.New("the re-driven turn did not end")
		}
	default:
		if child.GetComputeState() == v1.ComputeState_COMPUTE_COLD {
			start := time.Now()
			if _, err := x.sessions.Resume(ctx, uid, false); err != nil {
				return "", lc, fmt.Errorf("wake the child: %w", err)
			}
			lc["wakeMs"] = ms(start)
		}
		start := time.Now()
		turn, err := x.sessions.Exec(ctx, client.ExecOptions{Session: uid, Inputs: []string{task}, Config: config})
		lc["turn"], lc["turnMs"] = "ran", ms(start)
		if err != nil {
			return "", lc, err
		}
		return turn.Output, lc, nil
	}
	if rec.failed != "" {
		return "", lc, errors.New(rec.failed)
	}
	return rec.output, lc, nil
}

// suspend frees the child's worker unless it is already suspended, and records the outcome in lc.
// It runs even after the caller's deadline, with its own short bound, so a timed-out invocation
// does not leave its micro-VM running.
func (x *Executor) suspend(ctx context.Context, uid string, lc map[string]any) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), suspendTimeout)
	defer cancel()
	if s, err := x.sessions.GetSession(ctx, uid); err == nil && s.GetComputeState() == v1.ComputeState_COMPUTE_COLD {
		lc["suspended"] = true
		return
	}
	start := time.Now()
	if _, err := x.sessions.Suspend(ctx, uid); err != nil {
		lc["suspended"], lc["suspendError"] = false, err.Error()
		return
	}
	lc["suspended"], lc["suspendMs"] = true, ms(start)
}

// suspendTimeout bounds the suspend after a child's turn.
const suspendTimeout = time.Minute

func ms(start time.Time) int64 { return time.Since(start).Milliseconds() }

// attempt is what a child's journal holds of one call's turn there. A turn ends with END (the
// controller only writes COMPLETED) or, when it failed, with ERROR and no END; either is terminal,
// so a retry serves it instead of running the call again. failed is the ERROR's description.
type attempt struct {
	started, ended bool
	output         string
	failed         string
}

// recorded reads the child's journal for the execution whose EXECUTION_START config names callID.
func (x *Executor) recorded(ctx context.Context, uid, callID string) (attempt, error) {
	recs, err := x.sessions.Replay(ctx, uid, 1, 0)
	if err != nil {
		return attempt{}, fmt.Errorf("read child session %s's journal: %w", uid, err)
	}
	var a attempt
	var exec string
	for _, r := range recs {
		ev := r.GetEvent()
		if start := ev.GetExecutionStart(); start != nil && exec == "" {
			if p, ok := parseProvenance(start.GetConfig()); ok && p.ToolCallID == callID {
				exec, a.started = ev.GetExecutionId(), true
			}
			continue
		}
		if exec == "" || ev.GetExecutionId() != exec {
			continue
		}
		switch ev.GetKind() {
		case v1.EventKind_EVENT_OUTPUT:
			if m := wire.MessageFromProto(ev.GetMessage()); m != nil {
				a.output += m.Text()
			}
		case v1.EventKind_EVENT_END:
			a.ended = true
		case v1.EventKind_EVENT_ERROR:
			a.ended = true
			a.failed = ev.GetError().GetDescription()
			if a.failed == "" {
				a.failed = "the child's turn failed"
			}
		}
	}
	return a, nil
}

// parseProvenance reads the invocation provenance from an execution config, if it has one.
func parseProvenance(config []byte) (Provenance, bool) {
	var cfg map[string]json.RawMessage
	if len(config) == 0 || json.Unmarshal(config, &cfg) != nil || cfg[ConfigKey] == nil {
		return Provenance{}, false
	}
	var p Provenance
	if json.Unmarshal(cfg[ConfigKey], &p) != nil {
		return Provenance{}, false
	}
	return p, true
}

// provenance reads the provenance in a session's first EXECUTION_START.
func (x *Executor) provenance(ctx context.Context, uid string) (Provenance, bool, error) {
	recs, err := x.sessions.Replay(ctx, uid, 1, 0)
	if err != nil {
		return Provenance{}, false, fmt.Errorf("invoke: read session %s's journal: %w", uid, err)
	}
	for _, r := range recs {
		if start := r.GetEvent().GetExecutionStart(); start != nil {
			p, ok := parseProvenance(start.GetConfig())
			return p, ok, nil
		}
	}
	return Provenance{}, false, nil
}

// acquire takes one of edge.MaxConcurrent slots for the declaring agent's invocations of the
// target, across all of its sessions. The count is in memory and per daemon: a call in flight dies
// with the daemon, and two daemons sharing a journal each allow MaxConcurrent.
func (x *Executor) acquire(caller string, edge Edge) (func(), bool) {
	k := inflightKey{caller, edge.Target}
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
