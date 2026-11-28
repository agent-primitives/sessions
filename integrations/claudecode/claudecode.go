// Package claudecode is an unmediated, REQUIRES_MEMORY_SNAPSHOT api.Harness that runs Claude Code
// in headless mode. Each Run (one Exec) starts exactly one process:
//
//	claude -p --output-format stream-json --verbose --include-partial-messages \
//	  --permission-mode <mode> --setting-sources user --strict-mcp-config \
//	  (--session-id <id> | --resume <id> | --resume <from> --fork-session --session-id <id>) \
//	  [--model m] [--max-turns n] [--max-budget-usd x]
//
// with the turn's text inputs on stdin, and maps its stream-json events onto the sink. The recorded
// fixtures in testdata come from Claude Code 2.1.311; New does not check the installed version.
//
// Unmediated (spec §8.8, EXE-37): Claude Code owns its model loop, tools and transcript, so its
// model calls originate in the Claude Code process and leave through the runtime's egress, not
// through sink.Model. The log cannot rebuild its conversation, so the harness declares
// REQUIRES_MEMORY_SNAPSHOT (EXE-38): the session survives suspend and resume only through a memory
// snapshot of the compute that runs it. What the harness journals is what it can observe (EXE-39):
//
//   - the turn's final assistant text as Output, once, when the turn ends; a text over 1 MiB is
//     recorded as several consecutive Output events. Partial text deltas are parsed and dropped;
//     the host's EventSink has no ephemeral delta method.
//   - each tool result Claude Code executed as a reported TOOL_RESULT (sink.Report). The host
//     refuses sink.ToolCall for a tool the harness executes itself (IN_HARNESS_REPORTED), so tool
//     calls are never sent as intents. Receipts are bounded (see History size below).
//   - the turn's token counts as one Usage, taken from the result event's usage and attributed to
//     the model system/init reports. InputTokens counts uncached, cache-read and cache-write input
//     tokens. When no result reports nonzero usage, the Usage holds only the input tokens of the
//     turn's main-loop assistant messages (subagent messages excluded) and no output tokens. The
//     result's total_cost_usd is not recorded: api.Usage has no cost field. Usage is
//     observability, not billing.
//
// A turn the host cancels records nothing after the cancel: no Output, no Usage, and no
// TOOL_RESULT for a tool still running. Lines Claude Code writes after its result event have no
// effect, but a malformed one fails the turn. A process still running 5s after its result is sent
// SIGINT, and SIGKILL if it is still running InterruptGrace later. Its turn then succeeds only if
// it exits 0, as Claude Code 2.1.311 does, or dies of a signal the harness delivered to it. A death
// by any other signal fails the turn, a SIGKILL the harness never sent included. The exit status
// cannot tell the harness's SIGINT or SIGKILL from the same signal sent from outside, so a death by
// an outside one is excused when the harness also delivered that signal before it saw the process
// exit.
//
// For its calls, Claude Code's included, the harness gives up host-bounded spend, model-call audit
// and replay (EXE-41): the host neither sees nor limits them, the journal has no MODEL_CALL, and
// reconstruction replay is not supported. Describe declares the harness UNMEDIATED, so the host
// refuses reconstruction before Run and cannot start a billed Claude Code turn or its tools.
//
// Each turn's process runs in its own process group, which the harness kills before Run returns on
// every path, a successful turn included: a background shell or a server a tool started ends with
// the turn. A descendant that left the group (setsid) is not reached. Process groups make the
// harness Unix-only: New refuses any other platform (GOOS without the unix build tag), since
// killing only the direct child would report a clean teardown while tool processes keep running.
//
// Claude Code session identity is harness memory. The first turn passes --session-id with a UUID
// derived from the sessions session UID and an attempt number; once Claude Code's system/init event
// confirms that id, later turns pass --resume with the same id. A first turn that fails before
// init moves to the next attempt, so a transcript left by a dead attempt cannot block the session.
// Nothing extra is persisted: the memory snapshot carries the id, and the runtime's snapshot must
// also preserve the filesystem that holds Claude Code's transcripts. A harness instance that holds
// no state for a session whose History already has turns (memory lost, or a session that started
// on another harness) refuses the turn rather than start an empty conversation (EXE-43).
//
// A fork child on a runtime that snapshots memory runs on a clone of its parent's compute, so its
// harness memory holds the parent's entry, keyed by the parent's session UID, and none of its own.
// Its first turn adopts the parent's Claude Code session when all of these hold: History ends with
// a LIFECYCLE FORK marker and no EXECUTION_START or INPUT after it; the last execution before the
// marker, the fork point, is one this instance started; and exactly one session entry, not running
// a turn, has that execution as its last. The fork point is the execution of the last
// EXECUTION_START or INPUT before the marker, so an execution journaled with no EXECUTION_START
// (EXE-31) counts, and a legacy INPUT with no execution ID is not a known fork point. The child
// then passes --resume <parent id> --fork-session --session-id <child id>: Claude Code copies the
// parent's transcript under the child's own id and leaves the parent's untouched, and later child
// turns pass --resume <child id>. The parent's entry is read, never changed. Until Claude Code
// confirms the child's id, every child attempt, a retry of one cancelled before it ran or one that
// failed before init included, rechecks that the parent is still idle at the fork point, so it
// never copies a turn the parent began after the fork. While such an attempt runs, a turn of the
// parent, and of any fork not yet confirmed between the child and the session it copies, is
// refused with a fork-in-progress error, so the transcript cannot move on before Claude Code
// copies it: on an instance that serves both, continuing the parent right after a fork fails
// until the child's turn ends. A parent whose id Claude Code never confirmed has no conversation
// to continue, so the child starts its own, as the parent's next turn would. Any other fork child
// with no state refuses as above: a fork onto an instance that did not run the parent's last
// turn, a parent that ran on after the fork point on a shared instance (before the child's id is
// confirmed), or a child whose first turn was journaled but never reached this instance (a turn
// after the marker that this instance holds no record of). Such a child stays refused for good,
// and so does a fork of it: its unrecorded turn is the grandchild's fork point. A fork of a child
// whose id is not confirmed yet is refused whenever a retry of that child would be. The same holds
// for a fork whose fork point this instance refused before recording it as started (its ctx was
// already done, another turn was running, or a fork of it was in progress): the instance cannot
// tell that turn from one that ran elsewhere. A later parent turn that starts on the instance
// gives a new fork a known fork point. The transcript directory (CLAUDE_CONFIG_DIR, default
// HOME/.claude) must be on the filesystem the runtime clones with the memory.
//
// A re-run of an execution this instance already started fails without starting Claude
// Code; an interrupted turn is not retried (EXE-42). Every EXECUTION_START or INPUT is evidence that
// Claude Code may already have run. The journal cannot distinguish a process that started and died
// before its first event from an execution a wrapper failed before invoking this harness, so this
// harness does not infer non-delivery from event shape. A wrapper that positively knows it did not
// invoke this harness for an execution must omit all of that execution's records from the History it
// passes on later (EXE-44). If delivery is uncertain, it must retain the execution and a fresh
// harness fails closed with the state-loss error.
//
// Environment: the claude process gets only the allowlisted variables of the harness's environment,
// read once when New runs: PATH, HOME, USER, SHELL, TMPDIR, LANG, LC_ALL, LC_CTYPE,
// ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, ANTHROPIC_BASE_URL and CLAUDE_CONFIG_DIR (it locates the
// transcripts --resume reads), plus Config.EnvAllowlist. No other variable, another provider's key
// or cloud credentials included, is in the environment of the process or the tools it runs. Proxy
// and CA variables (HTTPS_PROXY, NO_PROXY, NODE_EXTRA_CA_CERTS) and Bedrock or Vertex settings are
// not defaults; a deployment that needs them adds them, knowing a proxy URL can carry credentials.
//
// Per-turn environment: when Config.TurnEnvAllowlist is set, a turn's Start.Config can set
// variables for that turn's claude process only. Start.Config is then a JSON object, of which the
// harness reads only the member "env", matched case-sensitively: an object that maps variable
// names to string values. Other members are ignored, so a producer can add its own; an empty
// Config, or one with no "env" member, sets nothing. With a duplicate member, or a duplicate name
// in "env", the last one is used, but a duplicate name whose earlier value is a number, boolean,
// array or object refuses the turn even when a later one is a string. Each name must match an
// allowlist entry and must not be reserved: PATH, HOME, USER, SHELL, TMPDIR, LANG, LC_ALL,
// LC_CTYPE, ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, CLAUDE_CONFIG_DIR, NODE_OPTIONS, BASH_ENV,
// ENV, PS4, PROMPT_COMMAND, and every name starting with LD_, DYLD_, BASH_FUNC_ or
// CLAUDE_CODE_OAUTH_. An allowlist entry that covers a reserved name is refused, ANTHROPIC_* and
// CLAUDE_CODE_* included, so Claude Code settings are allowlisted by exact name. A turn's value
// replaces the variable's base value for that turn; the next turn starts from the base environment
// again. A retry or Resume of the same execution reads the same journaled Config, so it gets the
// same environment. The turn is refused before claude starts when Config is not a JSON object,
// "env" is not valid UTF-8 (an escaped unpaired surrogate included) or not an object of strings (a
// null value included), a name is not allowed, a value holds a NUL, or "env" has more than 256
// names, a name over 256 bytes, or over 64 KiB of names and values. The harness never logs a
// value or puts one in an error; the first stderr line of a claude that exits before it starts a
// session is logged as for any turn. Start.Config is journaled and returned by Replay, so a value
// must not be a credential. With TurnEnvAllowlist empty, the default, Start.Config is not read at
// all.
//
// Credentials: the harness configures no model key. It passes ANTHROPIC_API_KEY or
// ANTHROPIC_AUTH_TOKEN through from its own environment, where it must be a placeholder that the
// runtime's egress replaces with the provider credential (EXE-40): a tool can read it, and the
// tool's result is journaled.
//
// Permissions: Claude Code's permission mode is the only gate on what a turn may do; the host does
// not mediate its tools, and approvals are not supported. To keep the work dir from widening it,
// every turn passes --setting-sources user and --strict-mcp-config with no --mcp-config: Claude
// Code loads neither the work dir's project or local settings (permission rules, hooks) nor its
// MCP servers. Settings in HOME (user settings) and managed settings still apply and are operator
// input: do not give a turn a mode in which its tools can write them. The bypassPermissions mode
// turns the gate off, so it needs Config.AllowBypassPermissions as well.
//
// Stderr can carry prompt or tool text, so the harness drops it, except for a process that fails
// before system/init (no tool has run): then it logs a warning with the exit status and the first
// stderr line, cut to 256 bytes. A --resume whose transcript is gone fails this way on every later
// turn. The turn's error keeps its fixed text.
//
// History size: the host sends the session's whole prior journal as Start.History, bounded by the
// harness server's receive limit (HARNESS_MAX_RECV_MSG_SIZE, see cmd/harnessnode). Tool receipts
// carry at most 2 KiB of input and 2 KiB of content, and shrink to sizes only once a turn's
// receipts pass 128 KiB or History passes half of Config.StartLimit. A long session still grows
// History; raise the receive limit and Config.StartLimit together.
package claudecode

import (
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agent-primitives/sessions/api"
)

const (
	defaultBinary         = "claude"
	defaultInterruptGrace = 10 * time.Second
	defaultStartLimit     = 4 << 20 // gRPC's default receive limit
	defaultPostResult     = 5 * time.Second
	defaultDrainGrace     = time.Second

	// maxPromptBytes is Claude Code's documented stdin limit for -p.
	maxPromptBytes = 10_000_000

	bypassPermissions = "bypassPermissions"
)

// Config configures the harness. Zero values select the defaults documented per field.
type Config struct {
	// Binary is the claude executable, a path or a name looked up in PATH. Default "claude". A
	// relative path is resolved against the current directory when New runs, not WorkDir.
	Binary string
	// WorkDir is the directory Claude Code runs in. Required; it must exist.
	WorkDir string
	// Model, when set, is passed as --model and declared in Descriptor.Models. A Start.Model that
	// differs fails the turn. When empty, a nonempty Start.Model is passed instead, else Claude
	// Code's own default applies.
	Model string
	// PermissionMode is passed as --permission-mode. Required: the harness cannot answer
	// permission prompts, so the operator chooses the mode explicitly. bypassPermissions also
	// needs AllowBypassPermissions.
	PermissionMode string
	// AllowBypassPermissions is the second opt-in PermissionMode bypassPermissions requires: that
	// mode lets every tool run unchecked, and Claude Code's permission system is the only gate on
	// a turn. New refuses either setting without the other.
	AllowBypassPermissions bool
	// EnvAllowlist names environment variables the claude process receives from the harness's
	// environment, in addition to the defaults listed in the package doc. Values are read once,
	// when New runs; an unset variable is omitted.
	EnvAllowlist []string
	// TurnEnvAllowlist lets a turn's Start.Config set environment variables for that turn's claude
	// process (see the package doc). Each entry is a variable name, or a nonempty name prefix
	// followed by '*' that allows every name with that prefix. New refuses an entry that names or
	// could match a reserved variable (see the package doc), such as ANTHROPIC_* or L*. Empty, the
	// default, disables the feature: Start.Config is not read.
	//
	// A variable such as GIT_CONFIG_* or ANTHROPIC_BASE_URL can redirect where the process sends
	// data or make git run commands (core.sshCommand, core.fsmonitor), whatever the permission
	// mode: whoever can start a turn on the session gets that power. Allow only what the producer
	// that builds Start.Config needs.
	TurnEnvAllowlist []string
	// MaxTurns, when positive, is passed as --max-turns.
	MaxTurns int
	// MaxBudgetUSD, when positive, is passed as --max-budget-usd. It caps one turn's spend.
	MaxBudgetUSD float64
	// InterruptGrace is how long the harness waits after SIGINT before it kills the process
	// group. Default 10s.
	InterruptGrace time.Duration
	// StartLimit is the largest Start message, in bytes, the harness server accepts. It sizes tool
	// receipts against History. Default 4 MiB (gRPC's default).
	StartLimit int
}

// Harness is the Claude Code harness. Use New; the zero value is not usable. Its state lives only
// in process memory, so it must be a single long-lived instance per process.
type Harness struct {
	cfg     Config
	env     []string      // the claude process's base environment; never nil, read-only after New
	turnEnv *envAllowlist // Config.TurnEnvAllowlist, validated; nil when the feature is off
	logger  *slog.Logger

	// postResultGrace bounds how long the process may run after its result event before the
	// harness interrupts it. drainGrace bounds how long the harness waits for stdout to close
	// once the process exited or was killed; time the sink takes to record a line does not
	// count. Fields, not constants, so tests can shorten them.
	postResultGrace time.Duration
	drainGrace      time.Duration
	// Test seams, nil outside tests. after replaces time.After for a turn's phase deadlines;
	// waitExit replaces the platform's waitExit; exitObserved runs once the leader's exit was
	// observed, before the turn sees it; beforeReap runs before the turn reaps the leader, after
	// the last kill of its group.
	after        func(time.Duration) <-chan time.Time
	waitExit     func(*os.Process) error
	exitObserved func()
	beforeReap   func(pid int)
	// waitExitWarn logs a failed waitExit once per harness rather than once per turn.
	waitExitWarn sync.Once

	mu       sync.Mutex
	sessions map[string]*sessionState
	started  map[string]struct{}
}

// sessionState is the per-session harness memory.
type sessionState struct {
	attempt     int  // index into the derived Claude Code session ids
	established bool // system/init confirmed the id for attempt; later turns resume it
	pending     bool // the id for attempt was handed to a process that did not confirm it
	running     bool // a turn is in flight
	// lastExec is the execution ID of the last turn begin accepted. A fork child matches the
	// session it forks from by it.
	lastExec string
	// forkParent and forkPoint name the session a fork child adopted and the execution it forked
	// at, until Claude Code confirms the child's own id; empty when the session starts its own.
	// Every attempt until then rechecks them (forkSource).
	forkParent, forkPoint string
	// forkReaders counts the fork-child attempts in flight that reserved this session (begin);
	// while it is nonzero, a turn of this session is refused (errForkInFlight).
	forkReaders int
}

// New validates cfg and returns a harness.
func New(cfg Config) (*Harness, error) {
	if err := checkPlatform(); err != nil {
		return nil, err
	}
	if cfg.Binary == "" {
		cfg.Binary = defaultBinary
	}
	bin, err := exec.LookPath(cfg.Binary)
	if err != nil {
		return nil, fmt.Errorf("claudecode: find claude binary: %w", err)
	}
	// LookPath returns a relative Binary (./bin/claude) as is, and exec would resolve it against
	// WorkDir, not this process's directory, where it was found.
	if cfg.Binary, err = filepath.Abs(bin); err != nil {
		return nil, fmt.Errorf("claudecode: find claude binary: %w", err)
	}
	if cfg.WorkDir == "" {
		return nil, errors.New("claudecode: WorkDir is required")
	}
	dir, err := filepath.Abs(cfg.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("claudecode: WorkDir: %w", err)
	}
	if fi, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("claudecode: WorkDir: %w", err)
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("claudecode: WorkDir %q is not a directory", dir)
	}
	cfg.WorkDir = dir
	if cfg.PermissionMode == "" {
		return nil, errors.New("claudecode: PermissionMode is required")
	}
	if !argValue(cfg.PermissionMode) {
		return nil, fmt.Errorf("claudecode: invalid PermissionMode %q", cfg.PermissionMode)
	}
	switch bypass := strings.EqualFold(cfg.PermissionMode, bypassPermissions); {
	case bypass && !cfg.AllowBypassPermissions:
		return nil, errors.New("claudecode: PermissionMode bypassPermissions also requires AllowBypassPermissions")
	case !bypass && cfg.AllowBypassPermissions:
		return nil, fmt.Errorf("claudecode: AllowBypassPermissions is set but PermissionMode is %q", cfg.PermissionMode)
	}
	env, err := childEnv(cfg.EnvAllowlist)
	if err != nil {
		return nil, err
	}
	turnEnv, err := newEnvAllowlist(cfg.TurnEnvAllowlist)
	if err != nil {
		return nil, err
	}
	if cfg.Model != "" && !argValue(cfg.Model) {
		return nil, fmt.Errorf("claudecode: invalid Model %q", cfg.Model)
	}
	if cfg.MaxTurns < 0 {
		return nil, errors.New("claudecode: MaxTurns must not be negative")
	}
	if cfg.MaxBudgetUSD < 0 || math.IsNaN(cfg.MaxBudgetUSD) || math.IsInf(cfg.MaxBudgetUSD, 0) {
		return nil, errors.New("claudecode: MaxBudgetUSD must be a finite, non-negative number")
	}
	switch {
	case cfg.InterruptGrace < 0:
		return nil, errors.New("claudecode: InterruptGrace must not be negative")
	case cfg.InterruptGrace == 0:
		cfg.InterruptGrace = defaultInterruptGrace
	}
	switch {
	case cfg.StartLimit < 0:
		return nil, errors.New("claudecode: StartLimit must not be negative")
	case cfg.StartLimit == 0:
		cfg.StartLimit = defaultStartLimit
	}
	return &Harness{
		cfg:             cfg,
		env:             env,
		turnEnv:         turnEnv,
		logger:          slog.Default(),
		postResultGrace: defaultPostResult,
		drainGrace:      defaultDrainGrace,
		sessions:        make(map[string]*sessionState),
		started:         make(map[string]struct{}),
	}, nil
}

// defaultEnv is the environment the claude process always receives, when set. Keep the package doc
// in sync.
var defaultEnv = []string{
	"PATH", "HOME", "USER", "SHELL", "TMPDIR", "LANG", "LC_ALL", "LC_CTYPE",
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDE_CONFIG_DIR",
}

// childEnv returns the claude process's environment: the defaults and the extra names, with their
// values in this process's environment. It is never nil: exec.Cmd passes a nil Env as the whole
// environment.
func childEnv(extra []string) ([]string, error) {
	env := []string{}
	seen := make(map[string]bool, len(defaultEnv)+len(extra))
	for i, name := range slices.Concat(defaultEnv, extra) {
		if i >= len(defaultEnv) && !envName(name) {
			return nil, fmt.Errorf("claudecode: invalid EnvAllowlist name %q", name)
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env, nil
}

// envName reports whether name can be an environment variable name: nonempty, with no '=', space
// or control character.
func envName(name string) bool {
	return name != "" && !strings.ContainsFunc(name, func(r rune) bool { return r == '=' || r <= ' ' || r == 0x7f })
}

// argValue reports whether v can follow a flag as its own argument: nonempty, no leading '-' (it
// would parse as a flag), and no whitespace or control characters.
func argValue(v string) bool {
	if v == "" || v[0] == '-' {
		return false
	}
	for _, r := range v {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

// Describe declares REQUIRES_MEMORY_SNAPSHOT (the Claude Code session id lives in harness memory
// and the transcript on the compute's filesystem), not ForkSafe (a fork by replay cannot rebuild
// the conversation; a fork that clones memory adopts the parent's session, see the package doc),
// and Streaming.
func (h *Harness) Describe(context.Context) (api.Descriptor, error) {
	var models []string
	if h.cfg.Model != "" {
		models = []string{h.cfg.Model}
	}
	return api.Descriptor{
		ID:           "claudecode",
		Version:      "2", // bump when execution behavior changes, not for host releases
		Models:       models,
		HarnessClass: api.HarnessClassUnmediated,
		Capabilities: api.Capabilities{
			Resumability: api.ResumabilityRequiresMemorySnapshot,
			ForkSafe:     false,
			Streaming:    true,
		},
	}, nil
}

// Errors returned by Run. Their text is fixed: the host journals a failed turn's error text. A
// per-turn environment refusal (turnEnvFor) wraps errTurnEnv and may add the variable's name,
// quoted and at most maxTurnEnvName bytes, never its value.
var (
	errNoSession     = errors.New("claudecode: start has no session UID")
	errRerun         = errors.New("claudecode: execution already started on this harness; an interrupted turn is not retried")
	errStateLost     = errors.New("claudecode: session has earlier turns but this harness holds no state for it (memory lost, fork, or harness switch)")
	errBusy          = errors.New("claudecode: another turn is running for this session")
	errForkInFlight  = errors.New("claudecode: a fork of this session is running a turn that may still read this session's conversation; retry when it ends")
	errNoPrompt      = errors.New("claudecode: inputs carry no text")
	errNonText       = errors.New("claudecode: inputs must be text only")
	errPromptSize    = errors.New("claudecode: inputs exceed the 10 MB prompt limit")
	errModelMismatch = errors.New("claudecode: requested model is not the configured model")
	errModelArg      = errors.New("claudecode: requested model is not a valid model argument")
	errTurnEnv       = errors.New("claudecode: invalid per-turn environment in Start.Config")
)

// Run drives one turn: one claude -p process. It returns nil when Claude Code reports a successful
// result, and an error when the result is an error, the process fails or exits without a result,
// the stream is malformed, or ctx is cancelled (SIGINT, then a group kill after InterruptGrace).
// Start.Config is read only when Config.TurnEnvAllowlist is set (see the package doc).
func (h *Harness) Run(ctx context.Context, s *api.Start, sink api.EventSink) error {
	if s == nil || s.SessionUID == "" {
		return errNoSession
	}
	forkFrom, reserved, err := h.begin(ctx, s)
	if err != nil {
		return err
	}
	established := false
	defer func() { h.end(s.SessionUID, established, reserved) }()

	prompt, err := promptText(s.Inputs)
	if err != nil {
		return err
	}
	model, err := h.turnModel(s.Model)
	if err != nil {
		return err
	}
	// Validated with the other inputs: after begin, so a refused first turn keeps the session's
	// state, and before sessionArg uses up an attempt.
	env, err := h.turnEnvFor(s.Config)
	if err != nil {
		return err
	}
	flag, id, forkFrom := h.sessionArg(s.SessionUID, forkFrom)
	t := &turn{
		h:          h,
		sink:       sink,
		sessionUID: s.SessionUID,
		flag:       flag,
		sessionID:  id,
		forkFrom:   forkFrom,
		model:      model,
		env:        env,
		receipts:   h.receiptMode(s),
	}
	err = t.run(ctx, h.args(flag, id, forkFrom, model), prompt)
	established = t.initSeen
	return err
}

// begin applies the checks that need harness memory and marks the turn running. It creates the
// session's state on the first turn, before input validation, so a first turn that fails
// validation does not make every later turn look like lost memory; a fork child's first turn
// adopts the session it forks from (adoptFork) or is refused. Until Claude Code confirms a fork
// child's own id, every attempt, the first included, rechecks that the session it forks from is
// still idle at the fork point and returns the id the attempt forks (forkFrom), or is refused: an
// attempt cancelled or failed before init must not copy a transcript that has moved on since. A
// turn whose ctx is already done stops there, before the execution is recorded as started, so
// nothing runs and a retry of it is not refused; a fork taken right after it, with no later turn,
// is refused.
//
// Such an attempt also reserves every session forkSource checked (sources) until end releases
// them: Claude Code reads the source transcript some time after begin returns, so a turn of the
// session it forks from, which appends to that transcript, is refused meanwhile with
// errForkInFlight. Only the session whose confirmed id the attempt forks can move under it; the
// unconfirmed forks in between and a source with no conversation are reserved too, deliberately
// conservative, so one rule covers the chain. Every refusal comes before the reservation, so begin
// never returns an error holding one. A turn refused this way, like one refused as busy, is not
// recorded as started.
func (h *Harness) begin(ctx context.Context, s *api.Start) (string, []string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var (
		forkFrom string
		sources  []string
	)
	if _, ok := h.started[s.ExecutionID]; ok {
		return "", nil, errRerun
	}
	st := h.sessions[s.SessionUID]
	fresh := st == nil
	if fresh {
		st = &sessionState{}
		if hasPriorTurn(s.History) {
			parent, point, ok := h.adoptFork(s.History)
			if !ok {
				return "", nil, errStateLost
			}
			st.forkParent, st.forkPoint = parent, point
		}
	}
	if !st.established && st.forkParent != "" {
		var ok bool
		if forkFrom, sources, ok = h.forkSource(st.forkParent, st.forkPoint); !ok {
			return "", nil, errStateLost
		}
	}
	if fresh {
		h.sessions[s.SessionUID] = st
	}
	if err := ctx.Err(); err != nil {
		return "", nil, interruptedErr(err)
	}
	if st.running {
		return "", nil, errBusy
	}
	if st.forkReaders > 0 {
		return "", nil, errForkInFlight
	}
	h.started[s.ExecutionID] = struct{}{}
	st.lastExec = s.ExecutionID
	st.running = true
	for _, uid := range sources {
		h.sessions[uid].forkReaders++
	}
	return forkFrom, sources, nil
}

// adoptFork decides whether a session with no state of its own is a fork child of a session this
// instance holds, and returns that session's UID and the fork point; begin then checks the parent's
// conversation with forkSource. ok is false, and the turn fails closed, unless history ends with a
// FORK marker with no turn after it, the last execution before the marker (the fork point) is one
// this instance started, and exactly one idle session's last begun execution is the fork point. The
// last execution is the one of the last EXECUTION_START or INPUT before the marker, as hasPriorTurn
// reads a turn: an execution with no EXECUTION_START (EXE-31) still counts, and a legacy INPUT with
// no execution ID is not a known fork point. An execution this instance never started is not
// skipped: it may have run elsewhere or on a process that died, and adopting an older execution's
// session would continue an older conversation than the parent's last turn. That includes one begin
// refused before recording it (ctx done, busy, a fork in progress). Executions a wrapper omitted
// under EXE-44 are absent from history. The caller holds h.mu.
func (h *Harness) adoptFork(history []api.Event) (parentUID, point string, ok bool) {
	marker := -1
	for i := len(history) - 1; i >= 0; i-- {
		e := history[i]
		if e.Kind == api.EventExecutionStart || e.Kind == api.EventInput {
			return "", "", false // a turn after the fork that this instance holds no record of
		}
		if e.Kind == api.EventLifecycle && e.Lifecycle != nil && e.Lifecycle.Kind == api.LifecycleFork {
			marker = i
			break
		}
	}
	if marker < 0 {
		return "", "", false
	}
	for _, e := range slices.Backward(history[:marker]) {
		if e.Kind == api.EventExecutionStart || e.Kind == api.EventInput {
			point = e.ExecutionID
			break
		}
	}
	if _, ok := h.started[point]; point == "" || !ok {
		return "", "", false // an execution this instance never started may have run: unknown state
	}
	found := false
	for uid, st := range h.sessions {
		if st.lastExec != point || st.running {
			continue
		}
		if found {
			return "", "", false // ambiguous: no execution begins under two sessions, so state is corrupt
		}
		parentUID, found = uid, true
	}
	return parentUID, point, found
}

// forkSource returns the Claude Code session id a fork of parentUID taken at execution point
// continues: the parent's confirmed id; the id the parent, itself a fork child not yet confirmed,
// still forks from; or "" when the parent never had a conversation. ok is false while the parent
// is gone, running a turn, or has begun a turn after point: its conversation then holds more than
// the fork point's, or may. The same holds up the chain of fork children not yet confirmed. chain
// holds the UIDs of the sessions it checked, parentUID and every one up that chain, for begin to
// reserve. The caller holds h.mu.
func (h *Harness) forkSource(parentUID, point string) (from string, chain []string, ok bool) {
	p := h.sessions[parentUID]
	switch {
	case p == nil || p.running || p.lastExec != point:
		return "", nil, false
	case p.established:
		return sessionID(parentUID, p.attempt), []string{parentUID}, true
	case p.forkParent == "":
		return "", []string{parentUID}, true
	}
	if from, chain, ok = h.forkSource(p.forkParent, p.forkPoint); !ok {
		return "", nil, false
	}
	return from, append(chain, parentUID), true
}

// end clears the running mark, releases the sessions begin reserved, and records whether Claude
// Code confirmed the session id. Run defers it as soon as begin succeeds, so every path out of an
// attempt releases its reservation.
func (h *Harness) end(sessionUID string, established bool, reserved []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, uid := range reserved {
		if r := h.sessions[uid]; r != nil { // never deleted; checked so a release cannot panic
			r.forkReaders--
		}
	}
	st := h.sessions[sessionUID]
	st.running = false
	if established {
		st.established = true
		st.pending = false
		st.forkParent, st.forkPoint = "", ""
	}
}

// sessionArg picks --resume for a confirmed session id, else --session-id with the current attempt,
// moving to the next attempt when the previous one was handed to a process that never confirmed it.
// forkFrom, from begin, is set for a fork child until its own id is confirmed: the turn then forks
// that session into the --session-id one.
func (h *Harness) sessionArg(sessionUID, forkFrom string) (flag, id, from string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.sessions[sessionUID]
	if st.established {
		return "--resume", sessionID(sessionUID, st.attempt), ""
	}
	if st.pending {
		st.attempt++
	}
	st.pending = true
	return "--session-id", sessionID(sessionUID, st.attempt), forkFrom
}

// hasPriorTurn reports whether History records an earlier turn: an execution start, or an input
// from a legacy log that predates execution starts. Any harness name counts, since the journaled
// name may be a registry alias of this harness.
func hasPriorTurn(history []api.Event) bool {
	for _, e := range history {
		if e.Kind == api.EventExecutionStart || e.Kind == api.EventInput {
			return true
		}
	}
	return false
}

// promptText joins the text of the turn's inputs. Non-text parts are refused rather than dropped.
func promptText(inputs []api.Message) (string, error) {
	var b strings.Builder
	for _, m := range inputs {
		for _, p := range m.Parts {
			if p.Text == nil {
				return "", errNonText
			}
			if p.Text.Text == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(p.Text.Text)
			if b.Len() > maxPromptBytes {
				return "", errPromptSize
			}
		}
	}
	if b.Len() == 0 {
		return "", errNoPrompt
	}
	return b.String(), nil
}

// turnModel resolves the model passed to Claude Code: the configured one, which the requested
// model must match, else the requested one, else none.
func (h *Harness) turnModel(requested string) (string, error) {
	if h.cfg.Model != "" {
		if requested != "" && requested != h.cfg.Model {
			return "", errModelMismatch
		}
		return h.cfg.Model, nil
	}
	if requested != "" && !argValue(requested) {
		return "", errModelArg
	}
	return requested, nil
}

// args builds the claude argv after the binary. The prompt goes on stdin, out of argv and ps. A
// nonempty forkFrom resumes that session and forks it into id, which Claude Code accepts only with
// --session-id.
func (h *Harness) args(flag, id, forkFrom, model string) []string {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--permission-mode", h.cfg.PermissionMode,
		// Only user settings, and no MCP server: the work dir cannot widen the permission mode.
		"--setting-sources", "user",
		"--strict-mcp-config",
	}
	if forkFrom != "" {
		args = append(args, "--resume", forkFrom, "--fork-session")
	}
	args = append(args, flag, id)
	if model != "" {
		args = append(args, "--model", model)
	}
	if h.cfg.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(h.cfg.MaxTurns))
	}
	if h.cfg.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(h.cfg.MaxBudgetUSD, 'f', -1, 64))
	}
	return args
}

// sessionNamespace is the UUIDv5 namespace for Claude Code session ids derived from sessions
// session UIDs. It is a fixed random UUID; changing it changes every derived id.
var sessionNamespace = [16]byte{
	0x5b, 0x0e, 0x2f, 0x6c, 0x8d, 0x4a, 0x4e, 0x1b, 0x9c, 0x37, 0x61, 0xd2, 0x0a, 0xf4, 0x83, 0x5e,
}

// sessionID derives the Claude Code session id for a session's attempt: UUIDv5 (RFC 9562) of
// "<sessionUID>/<attempt>" in sessionNamespace. Deterministic, so an operator can map one to the
// other.
func sessionID(sessionUID string, attempt int) string {
	sum := sha1.New()
	sum.Write(sessionNamespace[:])
	sum.Write([]byte(sessionUID + "/" + strconv.Itoa(attempt)))
	var u [16]byte
	copy(u[:], sum.Sum(nil))
	u[6] = u[6]&0x0f | 0x50 // version 5
	u[8] = u[8]&0x3f | 0x80 // RFC 9562 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}
