package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/wire"
)

const (
	// maxLineBytes bounds one stream-json line. A longer line fails the turn as malformed.
	maxLineBytes = 16 << 20
	// receiptFieldBytes bounds the tool input and the tool content a full receipt carries.
	receiptFieldBytes = 2 << 10
	// turnReceiptBudget bounds one turn's full receipts; later receipts in the turn are minimal.
	turnReceiptBudget = 128 << 10
	// outputChunkBytes bounds one Output call, well under gRPC's default 4 MiB frame limit.
	outputChunkBytes = 1 << 20
	// stderrLineBytes bounds the stderr line logged for a process that failed before system/init.
	stderrLineBytes = 256
)

var (
	errMalformed       = errors.New("claudecode: malformed stream-json from claude")
	errNoInit          = errors.New("claudecode: claude stream has events before system/init")
	errSessionMismatch = errors.New("claudecode: claude reported a different session id")
	errNoResult        = errors.New("claudecode: claude stream ended without a result")
)

// streamLine is one stream-json event. Only the fields the harness maps are decoded; unknown
// event types and fields are ignored.
type streamLine struct {
	Type            string          `json:"type"`
	Subtype         string          `json:"subtype"`
	SessionID       string          `json:"session_id"`
	Model           string          `json:"model"`
	ParentToolUseID string          `json:"parent_tool_use_id"`
	Message         json.RawMessage `json:"message"`
	IsError         bool            `json:"is_error"`
	Result          string          `json:"result"`
	Usage           *tokenUsage     `json:"usage"`
}

// message is the Anthropic-shaped message inside assistant and user events. Content is a string or
// an array of blocks.
type message struct {
	ID      string          `json:"id"`
	Content json.RawMessage `json:"content"`
	Usage   *tokenUsage     `json:"usage"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type tokenUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

func (u *tokenUsage) valid() bool {
	return u.InputTokens >= 0 && u.OutputTokens >= 0 && u.CacheCreationInputTokens >= 0 && u.CacheReadInputTokens >= 0
}

// input is every input token the model processed: uncached, cache reads and cache writes.
func (u *tokenUsage) input() int64 {
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}

type toolUse struct {
	name   string
	input  json.RawMessage
	parent string
}

// turn is one Run's process and stream state. Only the goroutine running run touches it.
type turn struct {
	h          *Harness
	sink       api.EventSink
	sessionUID string
	flag       string // --session-id or --resume
	sessionID  string
	model      string
	receipts   bool // full receipts; false once the session or turn budget is spent

	receiptBytes int
	initSeen     bool
	initModel    string
	tools        map[string]toolUse
	toolOrder    []string
	seenMsgs     map[string]struct{}
	recovered    tokenUsage // main-loop assistant usage, deduplicated by message id
	result       *streamLine
	sinkErr      error
}

// receiptMode reports whether full receipts are allowed this turn: History, sized as the wire sends
// it, is under half of StartLimit. It warns once History reaches three quarters of it.
func (h *Harness) receiptMode(s *api.Start) bool {
	n := 0
	for _, e := range s.History {
		n += proto.Size(wire.EventToProto(e))
	}
	if n >= h.cfg.StartLimit/4*3 {
		h.logger.Warn("claudecode: session history nears the harness receive limit; raise the limit and Config.StartLimit",
			"session_uid", s.SessionUID, "history_bytes", n, "start_limit", h.cfg.StartLimit)
	}
	return n < h.cfg.StartLimit/2
}

type readResult struct {
	line []byte
	err  error
}

// Process phases. Each later phase is entered at most once, in order.
const (
	phaseRunning     = iota
	phaseSettling    // result seen or stdout closed: the process gets postResultGrace to exit
	phaseInterrupted // SIGINT sent: the process gets InterruptGrace to exit
	phaseKilled      // group killed or leader exited: stdout gets drainGrace of waiting to close
)

// run starts the process, maps its stream, and returns the turn's outcome.
func (t *turn) run(ctx context.Context, args []string, prompt string) error {
	t.tools = make(map[string]toolUse)
	t.seenMsgs = make(map[string]struct{})
	if err := ctx.Err(); err != nil {
		return interruptedErr(err) // cancelled while the inputs were validated: launch nothing
	}

	r, w, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("claudecode: start claude: %w", err)
	}
	defer func() { _ = r.Close() }()
	// stderr can carry prompt or tool text. Only its first line is kept, and logged only for a
	// process that failed before system/init. A pipe, not an io.Writer, so cmd.Wait does not wait
	// for a descendant that holds stderr.
	er, ew, err := os.Pipe()
	if err != nil {
		_ = w.Close()
		return fmt.Errorf("claudecode: start claude: %w", err)
	}
	defer func() { _ = er.Close() }()
	cmd := exec.Command(t.h.cfg.Binary, args...)
	cmd.Dir = t.h.cfg.WorkDir
	cmd.Env = t.h.env
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Stdout = w
	cmd.Stderr = ew
	cmd.SysProcAttr = sysProcAttr()
	cmd.WaitDelay = t.h.drainGrace
	err = cmd.Start()
	_ = w.Close() // the child holds its own copies
	_ = ew.Close()
	if err != nil {
		return fmt.Errorf("claudecode: start claude: %w", err)
	}

	stderrLine := make(chan string, 1)
	go func() {
		br := bufio.NewReader(er)
		stderrLine <- firstLine(br)
		_, _ = io.Copy(io.Discard, br) // keep draining so the process never blocks on stderr
	}()
	lines := make(chan readResult)
	go readLines(r, lines)
	// The leader is reaped only once its group is killed for the last time (stopGroup, below):
	// until then the zombie reserves the id every kill of the group signals. The outcome is read
	// from cmd.ProcessState after the reap.
	var (
		exited       = make(chan struct{})
		leaderExited atomic.Bool // the observer saw the leader exit
		reaped       bool        // the observer had to reap it; read only after exited closed
	)
	go func() {
		wait := waitExit
		if t.h.waitExit != nil {
			wait = t.h.waitExit
		}
		if err := wait(cmd.Process); err != nil {
			if !errors.Is(err, errors.ErrUnsupported) {
				t.h.waitExitWarn.Do(func() {
					t.h.logger.Warn("claudecode: cannot wait for claude's exit without reaping it; "+
						"its process group id is not reserved after it exits", "err", err)
				})
			}
			_ = cmd.Wait()
			reaped = true
		}
		leaderExited.Store(true)
		if t.h.exitObserved != nil {
			t.h.exitObserved()
		}
		close(exited)
	}()

	var (
		phase    = phaseRunning
		deadline <-chan time.Time
		done     = ctx.Done()
		eof      bool
		exitSeen bool
		forced   bool // the harness closed stdout itself
		signaled bool // SIGINT reached the process before the turn saw it exit
		killed   bool // SIGKILL reached the process before the turn saw it exit
		failErr  error
		drainBy  time.Time // phaseKilled: when stdout is closed
	)
	enter := func(p int, d time.Duration) {
		phase = p
		if p == phaseKilled {
			drainBy, deadline = time.Now().Add(d), nil // armed per wait, below
			return
		}
		if t.h.after != nil {
			deadline = t.h.after(d)
			return
		}
		deadline = time.After(d)
	}
	interrupt := func() {
		if phase >= phaseInterrupted {
			return
		}
		// An unreaped zombie takes the signal, so delivery counts only before the turn saw the
		// process exit. Delivery alone does not excuse the exit status either: outcome also needs
		// the status to show it died of SIGINT.
		running := !leaderExited.Load()
		signaled = interruptProcess(cmd.Process) == nil && running
		enter(phaseInterrupted, t.h.cfg.InterruptGrace)
	}
	// kill kills the group and reports whether SIGKILL reached the process before the turn saw it
	// exit, as interrupt does for SIGINT.
	kill := func() bool {
		running := !leaderExited.Load()
		return killGroup(cmd.Process) && running
	}
	for !eof || !exitSeen {
		var drain *time.Timer
		if phase == phaseKilled && !forced {
			drain = time.NewTimer(time.Until(drainBy))
			deadline = drain.C
		}
		select {
		case m, ok := <-lines:
			switch {
			case !ok:
				lines, eof = nil, true
				if phase == phaseRunning {
					enter(phaseSettling, t.h.postResultGrace)
				}
			case m.err != nil:
				if !forced && failErr == nil {
					failErr = errMalformed
					interrupt()
				}
			case failErr != nil || ctx.Err() != nil:
				// Stopping: drain.
			case t.result != nil:
				// Past the result: no further sink effects, but a malformed line still fails the
				// turn, as it would before the result.
				if !validLine(m.line) {
					failErr = errMalformed
					interrupt()
				}
			default:
				began := time.Now()
				err := t.handle(ctx, m.line)
				if phase == phaseKilled {
					// drainGrace bounds how long stdout stays open while the harness waits on
					// it, not how long the sink takes: a slow Report must not cut off lines the
					// process wrote before it exited.
					drainBy = drainBy.Add(time.Since(began))
				}
				if err != nil {
					failErr = err
					interrupt()
				} else if t.result != nil && phase == phaseRunning {
					enter(phaseSettling, t.h.postResultGrace)
				}
			}
		case <-exited:
			exited, exitSeen = nil, true
			if !eof && phase < phaseKilled {
				// Something the process started still holds stdout.
				enter(phaseKilled, t.h.drainGrace)
			}
		case <-done:
			done = nil
			interrupt()
		case <-deadline:
			switch phase {
			case phaseSettling:
				interrupt()
			case phaseInterrupted:
				killed = kill() || killed
				enter(phaseKilled, t.h.drainGrace)
			default:
				killed = kill() || killed
				forced = true
				_ = r.Close()
				deadline = nil
			}
		}
		if drain != nil {
			drain.Stop()
		}
	}

	// The loop ends once the leader exited and stdout closed, but the group can outlive its leader:
	// a tool Claude Code started that redirected its stdio holds neither. Kill it on every path,
	// success included, so nothing the turn started runs once Run returns, or while it records.
	reap := func() {
		if t.h.beforeReap != nil {
			t.h.beforeReap(cmd.Process.Pid)
		}
		if !reaped {
			_ = cmd.Wait()
		}
	}
	if !stopGroup(cmd.Process, reap, t.h.drainGrace) {
		t.h.logger.Warn("claudecode: processes of the turn's group remain after the kill",
			"pid", cmd.Process.Pid, "wait", t.h.drainGrace)
	}

	err = t.outcome(ctx, cmd.ProcessState.ExitCode(), stoppedByHarness(cmd.ProcessState, signaled, killed), failErr)
	if err != nil && !t.initSeen && ctx.Err() == nil {
		// Claude Code failed before it confirmed the session, so no tool ran and the model said
		// nothing; a failing --resume ends here on every later turn. Its reason is on stderr.
		var line string
		select {
		case line = <-stderrLine:
		case <-time.After(t.h.drainGrace): // a descendant outside the group holds stderr
		}
		t.h.logger.Warn("claudecode: claude failed before system/init",
			"session_uid", t.sessionUID, "flag", t.flag, "claude_session_id", t.sessionID,
			"exit_status", cmd.ProcessState.String(), "stderr", line)
	}
	return err
}

// outcome records the turn's remaining effects and returns its result, once the process is gone.
// stopped reports that a signal the harness delivered ended the process, so its exit status is not
// its own verdict.
func (t *turn) outcome(ctx context.Context, code int, stopped bool, failErr error) error {
	if err := ctx.Err(); err != nil {
		return interruptedErr(err)
	}
	if t.sinkErr != nil {
		return t.sinkErr
	}
	if err := t.finish(ctx); err != nil {
		return err
	}
	if failErr != nil {
		return failErr
	}
	if t.result == nil {
		if !stopped && code != 0 {
			return fmt.Errorf("claudecode: claude exited with status %d before a result", code)
		}
		return errNoResult
	}
	if t.result.IsError || t.result.Subtype != "success" {
		return fmt.Errorf("claudecode: claude reported an error result (subtype %s)", subtype(t.result.Subtype))
	}
	if !stopped && code != 0 {
		return fmt.Errorf("claudecode: claude exited with status %d after a success result", code)
	}
	for _, chunk := range chunks(t.result.Result, outputChunkBytes) {
		if err := t.sink.Output(ctx, chunk); err != nil {
			return fmt.Errorf("claudecode: record output: %w", err)
		}
	}
	return nil
}

// interruptedErr is the error of a turn whose ctx is done.
func interruptedErr(err error) error {
	return fmt.Errorf("claudecode: turn interrupted: %w", err)
}

// readLines sends each stdout line, then a read error if any, then closes out. After an error it
// keeps draining so the process never blocks on a full pipe.
func readLines(r io.Reader, out chan<- readResult) {
	defer close(out)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
	for sc.Scan() {
		out <- readResult{line: bytes.Clone(sc.Bytes())}
	}
	if err := sc.Err(); err != nil {
		out <- readResult{err: err}
		_, _ = io.Copy(io.Discard, r)
	}
}

// firstLine returns the first non-blank line of r, cut to stderrLineBytes, with invalid UTF-8,
// control and format characters (bidi overrides included) replaced. Leading space is skipped
// across ReadLine's chunks, so a long line that starts with spaces is still that line. It reads
// to the end of that line and no further; "" at EOF.
func firstLine(r *bufio.Reader) string {
	var b []byte
	for {
		chunk, more, err := r.ReadLine()
		if len(b) == 0 {
			chunk = bytes.TrimLeftFunc(chunk, unicode.IsSpace)
		}
		b = append(b, chunk[:min(len(chunk), stderrLineBytes-len(b))]...)
		if more && err == nil {
			continue
		}
		if len(b) > 0 {
			break
		}
		if err != nil {
			return ""
		}
	}
	s := strings.Map(func(c rune) rune {
		if unicode.IsControl(c) || unicode.Is(unicode.Cf, c) {
			return '?'
		}
		return c
	}, strings.ToValidUTF8(string(bytes.TrimRightFunc(b, unicode.IsSpace)), "\uFFFD"))
	s, _ = truncate(s, stderrLineBytes)
	return s
}

// validLine reports whether line is blank or a stream-json event, by the same decoding handle
// applies.
func validLine(line []byte) bool {
	if len(bytes.TrimSpace(line)) == 0 {
		return true
	}
	var ev streamLine
	return json.Unmarshal(line, &ev) == nil
}

// handle maps one stream-json line.
func (t *turn) handle(ctx context.Context, line []byte) error {
	if len(bytes.TrimSpace(line)) == 0 {
		return nil
	}
	var ev streamLine
	if err := json.Unmarshal(line, &ev); err != nil {
		return errMalformed
	}
	switch ev.Type {
	case "system":
		if ev.Subtype != "init" {
			return nil
		}
		if ev.SessionID != t.sessionID {
			return errSessionMismatch
		}
		t.initSeen, t.initModel = true, ev.Model
		return nil
	case "stream_event":
		// Partial message deltas: there is no sink method for an ephemeral delta, so they are
		// dropped. The turn's text is recorded once from the result.
		return nil
	case "assistant", "user", "result":
	default:
		return nil
	}
	if !t.initSeen {
		return errNoInit
	}
	if ev.SessionID != "" && ev.SessionID != t.sessionID {
		return errSessionMismatch
	}
	switch ev.Type {
	case "assistant":
		return t.assistant(&ev)
	case "user":
		return t.user(ctx, &ev)
	default:
		if ev.Usage != nil && !ev.Usage.valid() {
			return errMalformed
		}
		t.result = &ev
		return nil
	}
}

// assistant holds tool_use blocks until their result and records main-loop usage once per message
// id, the fallback when no result reports usage.
func (t *turn) assistant(ev *streamLine) error {
	var msg message
	if err := json.Unmarshal(ev.Message, &msg); err != nil {
		return errMalformed
	}
	blocks, err := contentBlocks(msg.Content)
	if err != nil {
		return err
	}
	for _, b := range blocks {
		if b.Type != "tool_use" {
			continue
		}
		if b.ID == "" {
			return errMalformed
		}
		if _, ok := t.tools[b.ID]; !ok {
			t.toolOrder = append(t.toolOrder, b.ID)
		}
		t.tools[b.ID] = toolUse{name: b.Name, input: b.Input, parent: ev.ParentToolUseID}
	}
	if msg.Usage != nil {
		if !msg.Usage.valid() {
			return errMalformed
		}
		if ev.ParentToolUseID == "" && msg.ID != "" {
			if _, ok := t.seenMsgs[msg.ID]; !ok {
				t.seenMsgs[msg.ID] = struct{}{}
				t.recovered.InputTokens += msg.Usage.InputTokens
				t.recovered.CacheReadInputTokens += msg.Usage.CacheReadInputTokens
				t.recovered.CacheCreationInputTokens += msg.Usage.CacheCreationInputTokens
			}
		}
	}
	return nil
}

// user reports each tool_result block.
func (t *turn) user(ctx context.Context, ev *streamLine) error {
	var msg message
	if err := json.Unmarshal(ev.Message, &msg); err != nil {
		return errMalformed
	}
	blocks, err := contentBlocks(msg.Content)
	if err != nil {
		return err
	}
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		if b.ToolUseID == "" {
			return errMalformed
		}
		// Validate before taking the tool off the pending list, so a rejected result leaves it for
		// finish to close out.
		text, n, err := toolContent(b.Content)
		if err != nil {
			return err
		}
		use, ok := t.tools[b.ToolUseID]
		if ok {
			delete(t.tools, b.ToolUseID)
		} else {
			use = toolUse{parent: ev.ParentToolUseID} // orphan: reported with no tool name
		}
		tr := t.receipt(b.ToolUseID, use, text, n)
		tr.IsError = b.IsError
		if err := t.report(ctx, tr); err != nil {
			return err
		}
	}
	return nil
}

// receipt builds a tool result receipt: full while the budgets allow, else sizes only.
func (t *turn) receipt(id string, use toolUse, content string, contentBytes int) api.ToolResult {
	if t.receipts {
		input, inTrunc := truncate(string(use.input), receiptFieldBytes)
		text, outTrunc := truncate(content, receiptFieldBytes)
		out := map[string]any{
			"input":       input,
			"input_bytes": float64(len(use.input)),
			"truncated":   inTrunc || outTrunc,
		}
		tagTool(out, use)
		tr := api.ToolResult{ID: id, Output: out}
		if text != "" {
			tr.Content = []api.Part{{Text: &api.TextPart{Text: text}}}
		}
		size := proto.Size(wire.ToolResultToProto(&tr))
		if t.receiptBytes+size <= turnReceiptBudget {
			t.receiptBytes += size
			return tr
		}
		t.receipts = false
	}
	out := map[string]any{
		"input_bytes":   float64(len(use.input)),
		"content_bytes": float64(contentBytes),
		"omitted":       true,
	}
	tagTool(out, use)
	return api.ToolResult{ID: id, Output: out}
}

func tagTool(out map[string]any, use toolUse) {
	if use.name != "" {
		out["tool"] = use.name
	}
	if use.parent != "" {
		out["parent_tool_use_id"] = use.parent
	}
}

func (t *turn) report(ctx context.Context, tr api.ToolResult) error {
	if err := t.sink.Report(ctx, tr); err != nil {
		t.sinkErr = fmt.Errorf("claudecode: record tool result: %w", err)
		return t.sinkErr
	}
	return nil
}

// finish reports tool calls that never got a result and the turn's usage.
func (t *turn) finish(ctx context.Context) error {
	for _, id := range t.toolOrder {
		use, ok := t.tools[id]
		if !ok {
			continue
		}
		delete(t.tools, id)
		tr := t.receipt(id, use, "", 0)
		tr.IsError, tr.Error = true, "no result observed"
		if err := t.report(ctx, tr); err != nil {
			return err
		}
	}
	var u tokenUsage
	if t.result != nil && t.result.Usage != nil {
		u = *t.result.Usage
	}
	if u.input() == 0 && u.OutputTokens == 0 {
		u = t.recovered // crashed or zeroed result: input and cache tokens only
	}
	if u.input() == 0 && u.OutputTokens == 0 {
		return nil
	}
	model := t.initModel
	if model == "" {
		model = t.model
	}
	if err := t.sink.Usage(ctx, api.Usage{Model: model, InputTokens: u.input(), OutputTokens: u.OutputTokens}); err != nil {
		return fmt.Errorf("claudecode: record usage: %w", err)
	}
	return nil
}

// contentBlocks decodes message content: an array of blocks, or a string, which has none. Any other
// shape, null or missing content included, is malformed.
func contentBlocks(raw json.RawMessage) ([]contentBlock, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, errMalformed
	}
	switch raw[0] {
	case '"':
		return nil, nil
	case '[':
	default:
		return nil, errMalformed
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, errMalformed
	}
	return blocks, nil
}

// toolContent returns a tool result's text (a string, or the text blocks of an array) and the size
// of the raw content. Missing content has no text. Any other shape, null included, is malformed.
func toolContent(raw json.RawMessage) (string, int, error) {
	n := len(raw)
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "", n, nil
	}
	switch raw[0] {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", n, errMalformed
		}
		return s, n, nil
	case '[':
	default:
		return "", n, errMalformed
	}
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return "", n, errMalformed
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return b.String(), n, nil
}

// truncate cuts s to at most n bytes on a UTF-8 boundary.
func truncate(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// chunks splits s into pieces of at most n bytes on UTF-8 boundaries. Empty s has none.
func chunks(s string, n int) []string {
	var out []string
	for len(s) > 0 {
		c, _ := truncate(s, n)
		if c == "" { // n is smaller than one rune; never true for outputChunkBytes
			c = s[:1]
		}
		out = append(out, c)
		s = s[len(c):]
	}
	return out
}

// subtype returns a result subtype safe for fixed error text.
func subtype(s string) string {
	if s == "" || len(s) > 64 {
		return "unknown"
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && r != '_' {
			return "unknown"
		}
	}
	return s
}
