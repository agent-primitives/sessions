// Package claudecode is an unmediated, REQUIRES_MEMORY_SNAPSHOT api.Harness that runs Claude Code
// in headless mode. Each Run (one Exec) starts exactly one process:
//
//	claude -p --output-format stream-json --verbose --include-partial-messages \
//	  --permission-mode <mode> --setting-sources user --strict-mcp-config \
//	  (--session-id|--resume) <id> [--model m] [--max-turns n] [--max-budget-usd x]
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
// reconstruction replay is not supported. Do not point controller.Replay at this harness: the host
// does not refuse it, and on a fresh instance the first recorded execution looks like a genuine
// first turn and runs a real, billed Claude Code turn with real tools.
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
// no state for a session whose History already has turns (memory lost, a fork child, or a session
// that started on another harness) refuses the turn rather than start an empty conversation
// (EXE-43). A re-run of an execution this instance already started fails without starting Claude
// Code; an interrupted turn is not retried (EXE-42).
//
// Environment: the claude process gets only the allowlisted variables of the harness's environment,
// read once when New runs: PATH, HOME, USER, SHELL, TMPDIR, LANG, LC_ALL, LC_CTYPE,
// ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, ANTHROPIC_BASE_URL and CLAUDE_CONFIG_DIR (it locates the
// transcripts --resume reads), plus Config.EnvAllowlist. No other variable, another provider's key
// or cloud credentials included, is in the environment of the process or the tools it runs. Proxy
// and CA variables (HTTPS_PROXY, NO_PROXY, NODE_EXTRA_CA_CERTS) and Bedrock or Vertex settings are
// not defaults; a deployment that needs them adds them, knowing a proxy URL can carry credentials.
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
	cfg    Config
	env    []string // the claude process's environment; never nil, read-only after New
	logger *slog.Logger

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
// and the transcript on the compute's filesystem), not ForkSafe (a fork child holds no session of
// its own), and Streaming.
func (h *Harness) Describe(context.Context) (api.Descriptor, error) {
	var models []string
	if h.cfg.Model != "" {
		models = []string{h.cfg.Model}
	}
	return api.Descriptor{
		ID:      "claudecode",
		Version: "1", // bump when execution behavior changes, not for host releases
		Models:  models,
		Capabilities: api.Capabilities{
			Resumability: api.ResumabilityRequiresMemorySnapshot,
			ForkSafe:     false,
			Streaming:    true,
		},
	}, nil
}

// Errors returned by Run. Their text is fixed: the host journals a failed turn's error text.
var (
	errNoSession     = errors.New("claudecode: start has no session UID")
	errRerun         = errors.New("claudecode: execution already started on this harness; an interrupted turn is not retried")
	errStateLost     = errors.New("claudecode: session has earlier turns but this harness holds no state for it (memory lost, fork, or harness switch)")
	errBusy          = errors.New("claudecode: another turn is running for this session")
	errNoPrompt      = errors.New("claudecode: inputs carry no text")
	errNonText       = errors.New("claudecode: inputs must be text only")
	errPromptSize    = errors.New("claudecode: inputs exceed the 10 MB prompt limit")
	errModelMismatch = errors.New("claudecode: requested model is not the configured model")
	errModelArg      = errors.New("claudecode: requested model is not a valid model argument")
)

// Run drives one turn: one claude -p process. It returns nil when Claude Code reports a successful
// result, and an error when the result is an error, the process fails or exits without a result,
// the stream is malformed, or ctx is cancelled (SIGINT, then a group kill after InterruptGrace).
// Start.Config is ignored.
func (h *Harness) Run(ctx context.Context, s *api.Start, sink api.EventSink) error {
	if s == nil || s.SessionUID == "" {
		return errNoSession
	}
	if err := h.begin(ctx, s); err != nil {
		return err
	}
	established := false
	defer func() { h.end(s.SessionUID, established) }()

	prompt, err := promptText(s.Inputs)
	if err != nil {
		return err
	}
	model, err := h.turnModel(s.Model)
	if err != nil {
		return err
	}
	flag, id := h.sessionArg(s.SessionUID)
	t := &turn{
		h:          h,
		sink:       sink,
		sessionUID: s.SessionUID,
		flag:       flag,
		sessionID:  id,
		model:      model,
		receipts:   h.receiptMode(s),
	}
	err = t.run(ctx, h.args(flag, id, model), prompt)
	established = t.initSeen
	return err
}

// begin applies the checks that need harness memory and marks the turn running. It creates the
// session's state on the first turn, before input validation, so a first turn that fails
// validation does not make every later turn look like lost memory. A turn whose ctx is already
// done stops there, before the execution is recorded as started, so nothing runs and a retry of
// it is not refused.
func (h *Harness) begin(ctx context.Context, s *api.Start) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.started[s.ExecutionID]; ok {
		return errRerun
	}
	st := h.sessions[s.SessionUID]
	if st == nil {
		if hasPriorTurn(s.History) {
			return errStateLost
		}
		st = &sessionState{}
		h.sessions[s.SessionUID] = st
	}
	if err := ctx.Err(); err != nil {
		return interruptedErr(err)
	}
	if st.running {
		return errBusy
	}
	h.started[s.ExecutionID] = struct{}{}
	st.running = true
	return nil
}

// end clears the running mark and records whether Claude Code confirmed the session id.
func (h *Harness) end(sessionUID string, established bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.sessions[sessionUID]
	st.running = false
	if established {
		st.established = true
		st.pending = false
	}
}

// sessionArg picks --resume for a confirmed session id, else --session-id with the current attempt,
// moving to the next attempt when the previous one was handed to a process that never confirmed it.
func (h *Harness) sessionArg(sessionUID string) (flag, id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.sessions[sessionUID]
	if st.established {
		return "--resume", sessionID(sessionUID, st.attempt)
	}
	if st.pending {
		st.attempt++
	}
	st.pending = true
	return "--session-id", sessionID(sessionUID, st.attempt)
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

// args builds the claude argv after the binary. The prompt goes on stdin, out of argv and ps.
func (h *Harness) args(flag, id, model string) []string {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--permission-mode", h.cfg.PermissionMode,
		// Only user settings, and no MCP server: the work dir cannot widen the permission mode.
		"--setting-sources", "user",
		"--strict-mcp-config",
		flag, id,
	}
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
