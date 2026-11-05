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
	"time"
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
	h         *Harness
	sink      api.EventSink
	sessionID string
	model     string
	receipts  bool // full receipts; false once the session or turn budget is spent

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
	phaseKilled      // the group was killed, or the leader exited: stdout gets drainGrace to close
)

// run starts the process, maps its stream, and returns the turn's outcome.
func (t *turn) run(ctx context.Context, args []string, prompt string) error {
	t.tools = make(map[string]toolUse)
	t.seenMsgs = make(map[string]struct{})

	r, w, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("claudecode: start claude: %w", err)
	}
	defer func() { _ = r.Close() }()
	cmd := exec.Command(t.h.cfg.Binary, args...)
	cmd.Dir = t.h.cfg.WorkDir
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Stdout = w
	// stderr is discarded: it can carry prompt or tool text, which must not reach errors or logs.
	cmd.SysProcAttr = sysProcAttr()
	cmd.WaitDelay = t.h.drainGrace
	err = cmd.Start()
	_ = w.Close() // the child holds its own copy
	if err != nil {
		return fmt.Errorf("claudecode: start claude: %w", err)
	}

	lines := make(chan readResult)
	go readLines(r, lines)
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait() // the outcome is read from cmd.ProcessState
		close(exited)
	}()

	var (
		phase    = phaseRunning
		deadline <-chan time.Time
		done     = ctx.Done()
		eof      bool
		exitSeen bool
		forced   bool // the harness closed stdout itself
		signaled bool // the harness signalled the process, so its exit status is not its own verdict
		failErr  error
	)
	enter := func(p int, d time.Duration) {
		phase = p
		deadline = time.After(d)
	}
	interrupt := func() {
		if phase >= phaseInterrupted {
			return
		}
		interruptProcess(cmd.Process)
		signaled = true
		enter(phaseInterrupted, t.h.cfg.InterruptGrace)
	}
	for !eof || !exitSeen {
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
				if err := t.handle(ctx, m.line); err != nil {
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
				killGroup(cmd.Process)
				enter(phaseKilled, t.h.drainGrace)
			default:
				killGroup(cmd.Process)
				forced = true
				_ = r.Close()
				deadline = nil
			}
		}
	}

	// The loop ends once the leader exited and stdout closed, but the group can outlive its leader:
	// a tool Claude Code started that redirected its stdio holds neither. Kill it on every path,
	// success included, so nothing the turn started runs once Run returns, or while it records.
	if !stopGroup(cmd.Process, t.h.drainGrace) {
		t.h.logger.Warn("claudecode: processes of the turn's group remain after the kill",
			"pid", cmd.Process.Pid, "wait", t.h.drainGrace)
	}

	code := cmd.ProcessState.ExitCode()
	if ctx.Err() != nil {
		return fmt.Errorf("claudecode: turn interrupted: %w", ctx.Err())
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
		if !signaled && code != 0 {
			return fmt.Errorf("claudecode: claude exited with status %d before a result", code)
		}
		return errNoResult
	}
	if t.result.IsError || t.result.Subtype != "success" {
		return fmt.Errorf("claudecode: claude reported an error result (subtype %s)", subtype(t.result.Subtype))
	}
	if !signaled && code != 0 {
		return fmt.Errorf("claudecode: claude exited with status %d after a success result", code)
	}
	for _, chunk := range chunks(t.result.Result, outputChunkBytes) {
		if err := t.sink.Output(ctx, chunk); err != nil {
			return fmt.Errorf("claudecode: record output: %w", err)
		}
	}
	return nil
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
		use, ok := t.tools[b.ToolUseID]
		if ok {
			delete(t.tools, b.ToolUseID)
		} else {
			use = toolUse{parent: ev.ParentToolUseID} // orphan: reported with no tool name
		}
		text, n := toolContent(b.Content)
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

// contentBlocks decodes message content: an array of blocks, or a string, which has none.
func contentBlocks(raw json.RawMessage) ([]contentBlock, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '[' {
		return nil, nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, errMalformed
	}
	return blocks, nil
}

// toolContent returns a tool result's text (a string, or the text blocks of an array) and the size
// of the raw content.
func toolContent(raw json.RawMessage) (string, int) {
	n := len(raw)
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "", n
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s, n
		}
		return "", n
	}
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return "", n
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return b.String(), n
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
