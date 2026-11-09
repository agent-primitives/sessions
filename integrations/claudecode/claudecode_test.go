package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/agent-primitives/sessions/api"
)

func TestDescribe(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		model string
		want  []string
	}{
		{name: "no model"},
		{name: "configured model", model: "claude-test-model-1", want: []string{"claude-test-model-1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newTestHarness(t, Config{Model: test.model})
			got, err := h.Describe(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			// Version is the stable author-owned replay version: bump it only when execution
			// behavior changes.
			want := api.Descriptor{
				ID:      "claudecode",
				Version: "1",
				Models:  test.want,
				Capabilities: api.Capabilities{
					Resumability: api.ResumabilityRequiresMemorySnapshot,
					ForkSafe:     false,
					Streaming:    true,
				},
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Describe = %+v, want %+v", got, want)
			}
		})
	}
}

func TestNewValidatesConfig(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	valid := Config{Binary: exe, WorkDir: dir, PermissionMode: "dontAsk"}
	for _, test := range []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{name: "binary not found", mutate: func(c *Config) { c.Binary = "claudecode-test-no-such-binary" }, want: "find claude binary"},
		{name: "no workdir", mutate: func(c *Config) { c.WorkDir = "" }, want: "WorkDir is required"},
		{name: "missing workdir", mutate: func(c *Config) { c.WorkDir = filepath.Join(dir, "missing") }, want: "WorkDir"},
		{name: "workdir is a file", mutate: func(c *Config) { c.WorkDir = file }, want: "is not a directory"},
		{name: "no permission mode", mutate: func(c *Config) { c.PermissionMode = "" }, want: "PermissionMode is required"},
		{name: "permission mode is a flag", mutate: func(c *Config) { c.PermissionMode = "--bare" }, want: "invalid PermissionMode"},
		{name: "permission mode with space", mutate: func(c *Config) { c.PermissionMode = "a b" }, want: "invalid PermissionMode"},
		{name: "model is a flag", mutate: func(c *Config) { c.Model = "-m" }, want: "invalid Model"},
		{name: "model with newline", mutate: func(c *Config) { c.Model = "m\n" }, want: "invalid Model"},
		{name: "negative max turns", mutate: func(c *Config) { c.MaxTurns = -1 }, want: "MaxTurns"},
		{name: "negative budget", mutate: func(c *Config) { c.MaxBudgetUSD = -1 }, want: "MaxBudgetUSD"},
		{name: "NaN budget", mutate: func(c *Config) { c.MaxBudgetUSD = math.NaN() }, want: "MaxBudgetUSD"},
		{name: "infinite budget", mutate: func(c *Config) { c.MaxBudgetUSD = math.Inf(1) }, want: "MaxBudgetUSD"},
		{name: "negative interrupt grace", mutate: func(c *Config) { c.InterruptGrace = -time.Second }, want: "InterruptGrace"},
		{name: "negative start limit", mutate: func(c *Config) { c.StartLimit = -1 }, want: "StartLimit"},
		{name: "bypass without opt-in", mutate: func(c *Config) { c.PermissionMode = "bypassPermissions" }, want: "PermissionMode bypassPermissions also requires AllowBypassPermissions"},
		{name: "bypass case variant without opt-in", mutate: func(c *Config) { c.PermissionMode = "BYPASSPERMISSIONS" }, want: "requires AllowBypassPermissions"},
		{name: "opt-in without bypass", mutate: func(c *Config) { c.AllowBypassPermissions = true }, want: `AllowBypassPermissions is set but PermissionMode is "dontAsk"`},
		{name: "empty env name", mutate: func(c *Config) { c.EnvAllowlist = []string{"HTTPS_PROXY", ""} }, want: "invalid EnvAllowlist name"},
		{name: "env name with =", mutate: func(c *Config) { c.EnvAllowlist = []string{"A=B"} }, want: "invalid EnvAllowlist name"},
		{name: "env name with NUL", mutate: func(c *Config) { c.EnvAllowlist = []string{"A\x00"} }, want: "invalid EnvAllowlist name"},
		{name: "env name with space", mutate: func(c *Config) { c.EnvAllowlist = []string{" A"} }, want: "invalid EnvAllowlist name"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := valid
			test.mutate(&cfg)
			_, err := New(cfg)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("New error = %v, want one containing %q", err, test.want)
			}
		})
	}

	t.Run("bypass with opt-in", func(t *testing.T) {
		t.Parallel()
		cfg := valid
		cfg.PermissionMode, cfg.AllowBypassPermissions = "bypassPermissions", true
		if _, err := New(cfg); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("defaults", func(t *testing.T) {
		t.Parallel()
		h, err := New(valid)
		if err != nil {
			t.Fatal(err)
		}
		if h.cfg.InterruptGrace != defaultInterruptGrace || h.cfg.StartLimit != defaultStartLimit {
			t.Fatalf("defaults = %v, %d", h.cfg.InterruptGrace, h.cfg.StartLimit)
		}
		if h.postResultGrace != defaultPostResult || h.drainGrace != defaultDrainGrace {
			t.Fatalf("graces = %v, %v", h.postResultGrace, h.drainGrace)
		}
	})
}

// TestRelativeBinary: a relative Binary is found from the current directory, and a turn runs it
// from WorkDir, a different directory. Not parallel: it changes the current directory.
func TestRelativeBinary(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := filepath.Abs(filepath.Join("testdata", "text_turn.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Dir(exe))
	rel := "." + string(filepath.Separator) + filepath.Base(exe)
	h := newTestHarness(t, Config{Binary: rel})
	if h.cfg.Binary != exe {
		t.Fatalf("Binary = %q, want %q", h.cfg.Binary, exe)
	}
	setScenario(t, h, scenario{Fixture: fixture})
	sink := &recordingSink{t: t}
	if err := h.Run(context.Background(), startFor("uid", "e1", nil, "go"), sink); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sink.outputs, []string{"Hello, world."}) {
		t.Fatalf("outputs = %q", sink.outputs)
	}
}

func TestRunRequiresSessionUID(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	sink := &recordingSink{t: t}
	if err := h.Run(context.Background(), nil, sink); !errors.Is(err, errNoSession) {
		t.Fatalf("nil start: %v", err)
	}
	if err := h.Run(context.Background(), startFor("", "e1", nil, "hi"), sink); !errors.Is(err, errNoSession) {
		t.Fatalf("empty session UID: %v", err)
	}
	if got := starts(t, h); len(got) != 0 {
		t.Fatalf("spawned %d processes", len(got))
	}
}

var uuidV5 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestSessionID(t *testing.T) {
	t.Parallel()
	a0 := sessionID("uid-a", 0)
	if !uuidV5.MatchString(a0) {
		t.Fatalf("sessionID = %q, not a UUIDv5", a0)
	}
	if sessionID("uid-a", 0) != a0 {
		t.Fatal("sessionID is not deterministic")
	}
	for _, other := range []string{sessionID("uid-a", 1), sessionID("uid-b", 0), sessionID("uid-a/0", 0)} {
		if other == a0 {
			t.Fatalf("sessionID collision: %q", other)
		}
	}
}

func TestFirstTurnThenResume(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{Model: "claude-test-model-1", MaxTurns: 7, MaxBudgetUSD: 1.5})
	setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
	ctx := context.Background()
	id := sessionID("uid", 0)

	sink := &recordingSink{t: t}
	if err := h.Run(ctx, startFor("uid", "e1", nil, "first", "second"), sink); err != nil {
		t.Fatal(err)
	}
	got := starts(t, h)
	if len(got) != 1 {
		t.Fatalf("spawned %d processes, want 1", len(got))
	}
	wantArgs := []string{
		"-p", "--output-format", "stream-json", "--verbose", "--include-partial-messages",
		"--permission-mode", "dontAsk", "--setting-sources", "user", "--strict-mcp-config",
		"--session-id", id,
		"--model", "claude-test-model-1", "--max-turns", "7", "--max-budget-usd", "1.5",
	}
	if !slices.Equal(got[0].Args, wantArgs) {
		t.Fatalf("args = %q, want %q", got[0].Args, wantArgs)
	}
	if got[0].Stdin != "first\n\nsecond" {
		t.Fatalf("stdin = %q", got[0].Stdin)
	}
	// Partial text deltas are dropped; the result text is the one Output.
	if !slices.Equal(sink.outputs, []string{"Hello, world."}) {
		t.Fatalf("outputs = %q", sink.outputs)
	}
	if len(sink.reports) != 0 {
		t.Fatalf("reports = %+v", sink.reports)
	}
	wantUsage := []api.Usage{{Model: "claude-test-model-1", InputTokens: 12 + 100 + 200, OutputTokens: 5}}
	if !reflect.DeepEqual(sink.usages, wantUsage) {
		t.Fatalf("usages = %+v, want %+v", sink.usages, wantUsage)
	}

	for i, exec := range []string{"e2", "e3"} {
		sink := &recordingSink{t: t}
		if err := h.Run(ctx, startFor("uid", exec, priorTurn("e1"), "again"), sink); err != nil {
			t.Fatalf("turn %d: %v", i+2, err)
		}
		got := starts(t, h)
		args := got[len(got)-1].Args
		if i := slices.Index(args, "--resume"); i < 0 || args[i+1] != id {
			t.Fatalf("turn args = %q, want --resume %s", args, id)
		}
		if slices.Contains(args, "--session-id") {
			t.Fatalf("turn args = %q carry --session-id", args)
		}
		if !slices.Equal(args[:10], wantArgs[:10]) {
			t.Fatalf("turn args = %q, want the permission pins %q", args, wantArgs[:10])
		}
	}
}

// TestFailedFirstTurnMovesToNextAttempt: an id handed to a process that never confirmed it may
// have left a transcript behind, so the next first turn uses a fresh id.
func TestFailedFirstTurnMovesToNextAttempt(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		sc   scenario
		want string
	}{
		{name: "exit before init", sc: scenario{Exit: 1}, want: "claudecode: claude exited with status 1 before a result"},
		{name: "session mismatch", sc: scenario{Fixture: "session_mismatch.jsonl"}, want: errSessionMismatch.Error()},
		{name: "events before init", sc: scenario{Fixture: "before_init.jsonl"}, want: errNoInit.Error()},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newTestHarness(t, Config{})
			ctx := context.Background()
			setScenario(t, h, test.sc)
			err := h.Run(ctx, startFor("uid", "e1", nil, "hi"), &recordingSink{t: t})
			if err == nil || err.Error() != test.want {
				t.Fatalf("Run = %v, want %q", err, test.want)
			}

			setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
			if err := h.Run(ctx, startFor("uid", "e2", priorTurn("e1"), "hi"), &recordingSink{t: t}); err != nil {
				t.Fatal(err)
			}
			if err := h.Run(ctx, startFor("uid", "e3", priorTurn("e2"), "hi"), &recordingSink{t: t}); err != nil {
				t.Fatal(err)
			}
			got := starts(t, h)
			want := [][]string{
				{"--session-id", sessionID("uid", 0)},
				{"--session-id", sessionID("uid", 1)},
				{"--resume", sessionID("uid", 1)},
			}
			if len(got) != len(want) {
				t.Fatalf("spawned %d processes, want %d", len(got), len(want))
			}
			for i, w := range want {
				j := slices.Index(got[i].Args, w[0])
				if j < 0 || got[i].Args[j+1] != w[1] {
					t.Fatalf("turn %d args = %q, want %q", i+1, got[i].Args, w)
				}
			}
		})
	}
}

func TestToolResultsReported(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	setScenario(t, h, scenario{Fixture: "tool_turn.jsonl"})
	sink := &recordingSink{t: t}
	if err := h.Run(context.Background(), startFor("uid", "e1", nil, "go"), sink); err != nil {
		t.Fatal(err)
	}
	text := func(s string) []api.Part { return []api.Part{{Text: &api.TextPart{Text: s}}} }
	bashIn := `{"command":"ls","description":"List files"}`
	grepIn := `{"pattern":"TODO"}`
	taskIn := `{"description":"search","prompt":"find TODOs","subagent_type":"general-purpose"}`
	readIn := `{"file_path":"a.txt"}`
	want := []api.ToolResult{
		{ID: "toolu_01", Output: map[string]any{"tool": "Bash", "input": bashIn, "input_bytes": float64(len(bashIn)), "truncated": false}, Content: text("a.txt\nb.txt")},
		{ID: "toolu_03", Output: map[string]any{"tool": "Grep", "parent_tool_use_id": "toolu_02", "input": grepIn, "input_bytes": float64(len(grepIn)), "truncated": false}, Content: text("a.txt:1: TODO"), IsError: true},
		{ID: "toolu_02", Output: map[string]any{"tool": "Task", "input": taskIn, "input_bytes": float64(len(taskIn)), "truncated": false}, Content: text("Found one TODO.")},
		// A result whose tool_use the stream never showed is reported without a tool name.
		{ID: "toolu_99", Output: map[string]any{"input": "", "input_bytes": float64(0), "truncated": false}, Content: text("orphan")},
		// A tool_use that never got a result is closed out at the end of the turn.
		{ID: "toolu_04", Output: map[string]any{"tool": "Read", "input": readIn, "input_bytes": float64(len(readIn)), "truncated": false}, IsError: true, Error: "no result observed"},
	}
	if !reflect.DeepEqual(sink.reports, want) {
		t.Fatalf("reports =\n%+v\nwant\n%+v", sink.reports, want)
	}
	if !slices.Equal(sink.outputs, []string{"Done."}) {
		t.Fatalf("outputs = %q", sink.outputs)
	}
	wantUsage := []api.Usage{{Model: "claude-test-model-1", InputTokens: 60 + 5 + 180, OutputTokens: 40}}
	if !reflect.DeepEqual(sink.usages, wantUsage) {
		t.Fatalf("usages = %+v, want %+v", sink.usages, wantUsage)
	}
}

// toolPair returns an assistant tool_use line and its user tool_result line.
func toolPair(id, input, content string) []string {
	return []string{
		fmt.Sprintf(`{"type":"assistant","message":{"id":"msg_%s","content":[{"type":"tool_use","id":"%s","name":"Bash","input":{"command":%q}}]},"parent_tool_use_id":null,"session_id":"{{SESSION_ID}}"}`, id, id, input),
		fmt.Sprintf(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"%s","content":%q}]},"parent_tool_use_id":null,"session_id":"{{SESSION_ID}}"}`, id, content),
	}
}

const (
	initLine    = `{"type":"system","subtype":"init","session_id":"{{SESSION_ID}}","model":"claude-test-model-1"}`
	successLine = `{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"{{SESSION_ID}}","usage":{"input_tokens":1,"output_tokens":1}}`
)

func TestReceiptIsBounded(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	big := strings.Repeat("é", 5000) // 10000 bytes; 2048 is not a rune boundary
	lines := append([]string{initLine}, toolPair("toolu_big", big, big)...)
	setScenario(t, h, scenario{Lines: append(lines, successLine)})
	sink := &recordingSink{t: t}
	if err := h.Run(context.Background(), startFor("uid", "e1", nil, "go"), sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.reports) != 1 {
		t.Fatalf("reports = %d", len(sink.reports))
	}
	tr := sink.reports[0]
	in, _ := tr.Output["input"].(string)
	content := tr.Content[0].Text.Text
	for name, s := range map[string]string{"input": in, "content": content} {
		if len(s) > receiptFieldBytes || len(s) < receiptFieldBytes-4 || !utf8.ValidString(s) {
			t.Fatalf("%s receipt is %d bytes (valid UTF-8 %v), want at most %d", name, len(s), utf8.ValidString(s), receiptFieldBytes)
		}
	}
	if tr.Output["truncated"] != true || tr.Output["input_bytes"] != float64(len(`{"command":"`+big+`"}`)) {
		t.Fatalf("receipt output = %v", tr.Output)
	}
}

// TestReceiptsShrinkPastTurnBudget: once one turn's full receipts pass the budget, the rest of the
// turn reports sizes only.
func TestReceiptsShrinkPastTurnBudget(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	payload := strings.Repeat("x", 3000)
	lines := []string{initLine}
	const n = 60 // about 4 KiB each, past the 128 KiB budget
	for i := range n {
		lines = append(lines, toolPair(fmt.Sprintf("toolu_%02d", i), payload, payload)...)
	}
	setScenario(t, h, scenario{Lines: append(lines, successLine)})
	sink := &recordingSink{t: t}
	if err := h.Run(context.Background(), startFor("uid", "e1", nil, "go"), sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.reports) != n {
		t.Fatalf("reports = %d, want %d", len(sink.reports), n)
	}
	full := 0
	for i, tr := range sink.reports {
		if tr.Output["omitted"] == true {
			if tr.Content != nil || tr.Output["input"] != nil {
				t.Fatalf("minimal receipt %d carries content: %+v", i, tr)
			}
			if tr.Output["content_bytes"] != float64(len(payload)+2) || tr.Output["tool"] != "Bash" {
				t.Fatalf("minimal receipt %d = %v", i, tr.Output)
			}
			continue
		}
		if full != i {
			t.Fatalf("full receipt %d follows a minimal one", i)
		}
		full++
	}
	if full == 0 || full == n {
		t.Fatalf("full receipts = %d of %d, want the budget to cut in", full, n)
	}
}

// TestReceiptsShrinkWithLargeHistory: History past half the Start limit leaves no room for full
// receipts, and the harness warns once History nears the limit.
func TestReceiptsShrinkWithLargeHistory(t *testing.T) {
	t.Parallel()
	history := []api.Event{{Kind: api.EventOutput, Message: api.TextMessage("assistant", strings.Repeat("h", 3500))}}
	for _, test := range []struct {
		name        string
		limit       int
		wantOmitted bool
		wantWarn    bool
	}{
		{name: "small history", limit: 0},
		{name: "history past half the limit", limit: 6000, wantOmitted: true},
		{name: "history near the limit", limit: 4096, wantOmitted: true, wantWarn: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newTestHarness(t, Config{StartLimit: test.limit})
			var logs bytes.Buffer
			h.logger = slog.New(slog.NewTextHandler(&logs, nil))
			lines := append([]string{initLine}, toolPair("toolu_1", "ls", "a.txt")...)
			setScenario(t, h, scenario{Lines: append(lines, successLine)})
			sink := &recordingSink{t: t}
			if err := h.Run(context.Background(), startFor("uid", "e1", history, "go"), sink); err != nil {
				t.Fatal(err)
			}
			if got := sink.reports[0].Output["omitted"] == true; got != test.wantOmitted {
				t.Fatalf("omitted = %v, want %v: %+v", got, test.wantOmitted, sink.reports[0])
			}
			if got := strings.Contains(logs.String(), "level=WARN"); got != test.wantWarn {
				t.Fatalf("warned = %v, want %v: %s", got, test.wantWarn, logs.String())
			}
		})
	}
}

func TestUsage(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		sc      scenario
		wantErr string
		want    []api.Usage
	}{
		{
			name: "result usage: token counts only, total_cost_usd dropped",
			sc:   scenario{Fixture: "text_turn.jsonl"},
			want: []api.Usage{{Model: "claude-test-model-1", InputTokens: 12 + 100 + 200, OutputTokens: 5}},
		},
		{
			name:    "zeroed result usage falls back to main-loop input",
			sc:      scenario{Fixture: "zero_usage_result.jsonl"},
			wantErr: "claudecode: claude reported an error result (subtype error_during_execution)",
			want:    []api.Usage{{Model: "claude-test-model-1", InputTokens: (4 + 2 + 1) + (8 + 0 + 3)}},
		},
		{
			name: "no result: once per message id, subagents excluded",
			sc: scenario{Lines: []string{
				initLine,
				`{"type":"assistant","message":{"id":"msg_a","content":[{"type":"text","text":"a"}],"usage":{"input_tokens":5,"cache_read_input_tokens":2,"output_tokens":1}},"parent_tool_use_id":null}`,
				`{"type":"assistant","message":{"id":"msg_a","content":[{"type":"text","text":"b"}],"usage":{"input_tokens":5,"cache_read_input_tokens":2,"output_tokens":1}},"parent_tool_use_id":null}`,
				`{"type":"assistant","message":{"id":"msg_s","content":[],"usage":{"input_tokens":999,"output_tokens":1}},"parent_tool_use_id":"toolu_x"}`,
			}},
			wantErr: errNoResult.Error(),
			want:    []api.Usage{{Model: "claude-test-model-1", InputTokens: 7}},
		},
		{
			name:    "nothing to report",
			sc:      scenario{Fixture: "init_only.jsonl"},
			wantErr: errNoResult.Error(),
		},
		{
			name: "init model missing: the turn's model",
			sc: scenario{Lines: []string{
				`{"type":"system","subtype":"init","session_id":"{{SESSION_ID}}"}`,
				successLine,
			}},
			want: []api.Usage{{Model: "configured", InputTokens: 1, OutputTokens: 1}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newTestHarness(t, Config{Model: "configured"})
			setScenario(t, h, test.sc)
			sink := &recordingSink{t: t}
			err := h.Run(context.Background(), startFor("uid", "e1", nil, "go"), sink)
			if got := fmt.Sprint(err); (err != nil || test.wantErr != "") && got != test.wantErr {
				t.Fatalf("Run = %v, want %q", err, test.wantErr)
			}
			if !reflect.DeepEqual(sink.usages, test.want) {
				t.Fatalf("usages = %+v, want %+v", sink.usages, test.want)
			}
		})
	}
}

func TestFailedTurns(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		sc          scenario
		want        string
		wantUsage   []api.Usage
		wantReports int
	}{
		{
			name:      "error result",
			sc:        scenario{Fixture: "error_result.jsonl"},
			want:      "claudecode: claude reported an error result (subtype error_max_turns)",
			wantUsage: []api.Usage{{Model: "claude-test-model-1", InputTokens: 10, OutputTokens: 2}},
		},
		{
			name: "error flag with success subtype",
			sc: scenario{Lines: []string{initLine,
				`{"type":"result","subtype":"success","is_error":true,"result":"API Error: 500","session_id":"{{SESSION_ID}}"}`}},
			want: "claudecode: claude reported an error result (subtype success)",
		},
		{
			name: "unsafe subtype",
			sc: scenario{Lines: []string{initLine,
				`{"type":"result","subtype":"error: sk-secret","is_error":true,"session_id":"{{SESSION_ID}}"}`}},
			want: "claudecode: claude reported an error result (subtype unknown)",
		},
		{
			name: "non-zero exit before a result",
			sc:   scenario{Fixture: "init_only.jsonl", Exit: 3},
			want: "claudecode: claude exited with status 3 before a result",
		},
		{
			name:      "non-zero exit after success",
			sc:        scenario{Fixture: "text_turn.jsonl", Exit: 2},
			want:      "claudecode: claude exited with status 2 after a success result",
			wantUsage: []api.Usage{{Model: "claude-test-model-1", InputTokens: 312, OutputTokens: 5}},
		},
		{
			name:        "stream ends without a result",
			sc:          scenario{Fixture: "truncated.jsonl"},
			want:        errNoResult.Error(),
			wantUsage:   []api.Usage{{Model: "claude-test-model-1", InputTokens: 7}},
			wantReports: 1,
		},
		{
			name: "stream cut mid-line",
			sc:   scenario{Fixture: "init_only.jsonl", Partial: `{"type":"result","subtype":"succ`},
			want: errMalformed.Error(),
		},
		{
			name: "malformed line",
			sc:   scenario{Fixture: "malformed.jsonl"},
			want: errMalformed.Error(),
		},
		{
			name:      "malformed line after a success result",
			sc:        scenario{Fixture: "text_turn.jsonl", Lines: []string{`{"type":"assistant","message":`}},
			want:      errMalformed.Error(),
			wantUsage: []api.Usage{{Model: "claude-test-model-1", InputTokens: 312, OutputTokens: 5}},
		},
		{
			name:      "stream cut mid-line after a success result",
			sc:        scenario{Fixture: "text_turn.jsonl", Partial: `{"type":"stream_ev`},
			want:      errMalformed.Error(),
			wantUsage: []api.Usage{{Model: "claude-test-model-1", InputTokens: 312, OutputTokens: 5}},
		},
		{
			name: "negative usage",
			sc: scenario{Lines: []string{initLine,
				`{"type":"result","subtype":"success","result":"x","session_id":"{{SESSION_ID}}","usage":{"input_tokens":-1}}`}},
			want: errMalformed.Error(),
		},
		{
			name: "tool_use without id",
			sc: scenario{Lines: []string{initLine,
				`{"type":"assistant","message":{"id":"m","content":[{"type":"tool_use","name":"Bash","input":{}}]}}`}},
			want: errMalformed.Error(),
		},
		{
			name: "tool_result without tool_use_id",
			sc: scenario{Lines: []string{initLine,
				`{"type":"user","message":{"content":[{"type":"tool_result","content":"x"}]}}`}},
			want: errMalformed.Error(),
		},
		{
			name: "content is not blocks",
			sc:   scenario{Lines: []string{initLine, `{"type":"assistant","message":{"id":"m","content":[1]}}`}},
			want: errMalformed.Error(),
		},
		{
			name: "assistant content is an object",
			sc:   scenario{Lines: []string{initLine, `{"type":"assistant","message":{"id":"m","content":{}}}`, successLine}},
			want: errMalformed.Error(),
		},
		{
			name: "assistant content is a number",
			sc:   scenario{Lines: []string{initLine, `{"type":"assistant","message":{"id":"m","content":1}}`, successLine}},
			want: errMalformed.Error(),
		},
		{
			name: "assistant content is a bool",
			sc:   scenario{Lines: []string{initLine, `{"type":"assistant","message":{"id":"m","content":true}}`, successLine}},
			want: errMalformed.Error(),
		},
		{
			name: "assistant content is null",
			sc:   scenario{Lines: []string{initLine, `{"type":"assistant","message":{"id":"m","content":null}}`, successLine}},
			want: errMalformed.Error(),
		},
		{
			name: "assistant content is missing",
			sc:   scenario{Lines: []string{initLine, `{"type":"assistant","message":{"id":"m"}}`, successLine}},
			want: errMalformed.Error(),
		},
		{
			name: "assistant message is null",
			sc:   scenario{Lines: []string{initLine, `{"type":"assistant","message":null}`, successLine}},
			want: errMalformed.Error(),
		},
		{
			name: "user content is an object",
			sc:   scenario{Lines: []string{initLine, `{"type":"user","message":{"content":{"type":"tool_result","tool_use_id":"t","content":"x"}}}`, successLine}},
			want: errMalformed.Error(),
		},
		{
			name: "user content is null",
			sc:   scenario{Lines: []string{initLine, `{"type":"user","message":{"content":null}}`, successLine}},
			want: errMalformed.Error(),
		},
		{
			name: "tool_result content is an object",
			sc:   scenario{Lines: []string{initLine, `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":{}}]}}`, successLine}},
			want: errMalformed.Error(),
		},
		{
			name: "tool_result content is a number",
			sc:   scenario{Lines: []string{initLine, `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":1}]}}`, successLine}},
			want: errMalformed.Error(),
		},
		{
			name: "tool_result content is null",
			sc:   scenario{Lines: []string{initLine, `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":null}]}}`, successLine}},
			want: errMalformed.Error(),
		},
		{
			name: "tool_result content is not text blocks",
			sc:   scenario{Lines: []string{initLine, `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":[1]}]}}`, successLine}},
			want: errMalformed.Error(),
		},
		{
			name: "line over the limit",
			sc:   scenario{Lines: []string{initLine, `{"type":"assistant","x":"` + strings.Repeat("x", maxLineBytes) + `"}`, successLine}},
			want: errMalformed.Error(),
		},
		{
			name: "session id changes mid-stream",
			sc: scenario{Lines: []string{initLine,
				`{"type":"result","subtype":"success","result":"x","session_id":"11111111-1111-4111-8111-111111111111"}`}},
			want: errSessionMismatch.Error(),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newTestHarness(t, Config{})
			setScenario(t, h, test.sc)
			sink := &recordingSink{t: t}
			err := h.Run(context.Background(), startFor("uid", "e1", nil, "go"), sink)
			if err == nil || err.Error() != test.want {
				t.Fatalf("Run = %v, want %q", err, test.want)
			}
			if len(sink.outputs) != 0 {
				t.Fatalf("failed turn recorded output %q", sink.outputs)
			}
			if !reflect.DeepEqual(sink.usages, test.wantUsage) {
				t.Fatalf("usages = %+v, want %+v", sink.usages, test.wantUsage)
			}
			if len(sink.reports) != test.wantReports {
				t.Fatalf("reports = %+v, want %d", sink.reports, test.wantReports)
			}
		})
	}
}

// TestStringContent: message content may be a string, which carries no blocks.
func TestStringContent(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	setScenario(t, h, scenario{Lines: []string{
		initLine,
		`{"type":"assistant","message":{"id":"m","content":"thinking"},"session_id":"{{SESSION_ID}}"}`,
		`{"type":"user","message":{"role":"user","content":"go on"},"session_id":"{{SESSION_ID}}"}`,
		successLine,
	}})
	sink := &recordingSink{t: t}
	if err := h.Run(context.Background(), startFor("uid", "e1", nil, "go"), sink); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sink.outputs, []string{"ok"}) || len(sink.reports) != 0 {
		t.Fatalf("outputs = %q, reports = %+v", sink.outputs, sink.reports)
	}
}

// TestToolResultWithoutContent: a tool_result may omit its content; it is reported with no text.
func TestToolResultWithoutContent(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	setScenario(t, h, scenario{Lines: []string{
		initLine,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t"}]},"session_id":"{{SESSION_ID}}"}`,
		successLine,
	}})
	sink := &recordingSink{t: t}
	if err := h.Run(context.Background(), startFor("uid", "e1", nil, "go"), sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.reports) != 1 || sink.reports[0].ID != "t" || sink.reports[0].Content != nil {
		t.Fatalf("reports = %+v", sink.reports)
	}
}

// TestLinesAfterResult: well-formed lines after the result are decoded but have no sink effect: a
// late tool result is not reported and late usage is not counted.
func TestLinesAfterResult(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	setScenario(t, h, scenario{Fixture: "text_turn.jsonl", Lines: []string{
		"",
		`{"type":"assistant","message":{"id":"late","content":[{"type":"tool_use","id":"toolu_late","name":"Bash","input":{}}],"usage":{"input_tokens":999}},"session_id":"{{SESSION_ID}}"}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_late","content":"x"}]},"session_id":"{{SESSION_ID}}"}`,
		`{"type":"some_future_event"}`,
	}})
	sink := &recordingSink{t: t}
	if err := h.Run(context.Background(), startFor("uid", "e1", nil, "go"), sink); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sink.outputs, []string{"Hello, world."}) || len(sink.reports) != 0 {
		t.Fatalf("outputs = %q, reports = %+v", sink.outputs, sink.reports)
	}
	if want := []api.Usage{{Model: "claude-test-model-1", InputTokens: 312, OutputTokens: 5}}; !reflect.DeepEqual(sink.usages, want) {
		t.Fatalf("usages = %+v, want %+v", sink.usages, want)
	}
}

func TestTruncatedStreamClosesDanglingTool(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	setScenario(t, h, scenario{Fixture: "truncated.jsonl"})
	sink := &recordingSink{t: t}
	if err := h.Run(context.Background(), startFor("uid", "e1", nil, "go"), sink); !errors.Is(err, errNoResult) {
		t.Fatalf("Run = %v", err)
	}
	tr := sink.reports[0]
	if tr.ID != "toolu_30" || !tr.IsError || tr.Error != "no result observed" || tr.Output["tool"] != "Bash" {
		t.Fatalf("dangling report = %+v", tr)
	}
}

// TestMalformedToolResultLeavesToolPending: a tool_result whose content is malformed fails the turn,
// and the tool it answers is still closed out as having no result, as if the line never came.
func TestMalformedToolResultLeavesToolPending(t *testing.T) {
	t.Parallel()
	const (
		useA = `{"type":"assistant","message":{"id":"m","content":[{"type":"tool_use","id":"toolu_a","name":"Bash","input":{"command":"ls"}}]}}`
		useB = `{"type":"assistant","message":{"id":"m2","content":[{"type":"tool_use","id":"toolu_b","name":"Read","input":{"file_path":"a.txt"}}]}}`
	)
	bashIn, readIn := `{"command":"ls"}`, `{"file_path":"a.txt"}`
	pending := func(id, tool, input string) api.ToolResult {
		return api.ToolResult{ID: id, Output: map[string]any{"tool": tool, "input": input, "input_bytes": float64(len(input)), "truncated": false}, IsError: true, Error: "no result observed"}
	}
	for _, test := range []struct {
		name  string
		lines []string
		want  []api.ToolResult
	}{
		{
			name:  "null content",
			lines: []string{useA, `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_a","content":null}]}}`},
			want:  []api.ToolResult{pending("toolu_a", "Bash", bashIn)},
		},
		{
			name:  "object content",
			lines: []string{useA, `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_a","content":{}}]}}`},
			want:  []api.ToolResult{pending("toolu_a", "Bash", bashIn)},
		},
		{
			name: "valid result before a malformed one in the same message",
			lines: []string{useA, useB,
				`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_a","content":"x"},{"type":"tool_result","tool_use_id":"toolu_b","content":null}]}}`},
			want: []api.ToolResult{
				{ID: "toolu_a", Output: map[string]any{"tool": "Bash", "input": bashIn, "input_bytes": float64(len(bashIn)), "truncated": false}, Content: []api.Part{{Text: &api.TextPart{Text: "x"}}}},
				pending("toolu_b", "Read", readIn),
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newTestHarness(t, Config{})
			setScenario(t, h, scenario{Lines: append(append([]string{initLine}, test.lines...), successLine)})
			sink := &recordingSink{t: t}
			if err := h.Run(context.Background(), startFor("uid", "e1", nil, "go"), sink); !errors.Is(err, errMalformed) {
				t.Fatalf("Run = %v, want %v", err, errMalformed)
			}
			if len(sink.outputs) != 0 || len(sink.usages) != 0 {
				t.Fatalf("failed turn recorded outputs %q, usages %+v", sink.outputs, sink.usages)
			}
			if !reflect.DeepEqual(sink.reports, test.want) {
				t.Fatalf("reports =\n%+v\nwant\n%+v", sink.reports, test.want)
			}
		})
	}
}

func TestOutputChunked(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	text := strings.Repeat("é", 1_300_000) // 2.6 MB; 1 MiB is not a rune boundary
	setScenario(t, h, scenario{Lines: []string{
		initLine,
		fmt.Sprintf(`{"type":"result","subtype":"success","result":%q,"session_id":"{{SESSION_ID}}"}`, text),
	}})
	sink := &recordingSink{t: t}
	if err := h.Run(context.Background(), startFor("uid", "e1", nil, "go"), sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.outputs) != 3 {
		t.Fatalf("outputs = %d chunks, want 3", len(sink.outputs))
	}
	for i, c := range sink.outputs {
		if len(c) > outputChunkBytes || !utf8.ValidString(c) {
			t.Fatalf("chunk %d: %d bytes, valid UTF-8 %v", i, len(c), utf8.ValidString(c))
		}
	}
	if strings.Join(sink.outputs, "") != text {
		t.Fatal("chunks do not rejoin to the result")
	}
}

func TestChunksAndTruncate(t *testing.T) {
	t.Parallel()
	if got := chunks("", 4); got != nil {
		t.Fatalf("chunks of empty = %q", got)
	}
	if got := chunks("aébc", 2); !slices.Equal(got, []string{"a", "é", "bc"}) {
		t.Fatalf("chunks = %q", got)
	}
	if got := chunks("é", 1); strings.Join(got, "") != "é" {
		t.Fatalf("chunks below one rune = %q", got)
	}
	if s, cut := truncate("abc", 3); s != "abc" || cut {
		t.Fatalf("truncate = %q, %v", s, cut)
	}
	if s, cut := truncate("aé", 2); s != "a" || !cut {
		t.Fatalf("truncate = %q, %v", s, cut)
	}
}

func TestRerunRefused(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
	ctx := context.Background()
	if err := h.Run(ctx, startFor("uid", "e1", nil, "go"), &recordingSink{t: t}); err != nil {
		t.Fatal(err)
	}
	// A recovery re-run of the same execution must not start a second billed turn.
	if err := h.Run(ctx, startFor("uid", "e1", nil, "go"), &recordingSink{t: t}); !errors.Is(err, errRerun) {
		t.Fatalf("re-run = %v, want %v", err, errRerun)
	}
	if got := starts(t, h); len(got) != 1 {
		t.Fatalf("spawned %d processes, want 1", len(got))
	}
}

// TestCancelledBeforeStart: a turn whose ctx is done when Run is called launches nothing and is
// not recorded as started, so its retry runs. The session's state is still created, so a later
// turn whose History shows the cancelled one does not look like lost memory.
func TestCancelledBeforeStart(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := h.Run(ctx, startFor("uid", "e1", nil, "go"), &recordingSink{t: t})
	if !errors.Is(err, context.Canceled) || err.Error() != "claudecode: turn interrupted: context canceled" {
		t.Fatalf("Run = %v", err)
	}
	if got := starts(t, h); len(got) != 0 {
		t.Fatalf("spawned %d processes", len(got))
	}
	if err := h.Run(context.Background(), startFor("uid", "e1", nil, "go"), &recordingSink{t: t}); err != nil {
		t.Fatalf("retry = %v", err)
	}
	// A ctx cancelled after begin, while the inputs are checked, still launches nothing.
	tr := &turn{h: h, sink: &recordingSink{t: t}, sessionUID: "uid", flag: "--resume", sessionID: sessionID("uid", 0)}
	if err := tr.run(ctx, h.args(tr.flag, tr.sessionID, ""), "go"); !errors.Is(err, context.Canceled) {
		t.Fatalf("run = %v", err)
	}
	if got := starts(t, h); len(got) != 1 {
		t.Fatalf("spawned %d processes, want 1", len(got))
	}
	h2 := newTestHarness(t, Config{})
	setScenario(t, h2, scenario{Fixture: "text_turn.jsonl"})
	if err := h2.Run(ctx, startFor("uid", "e1", nil, "go"), &recordingSink{t: t}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
	if err := h2.Run(context.Background(), startFor("uid", "e2", priorTurn("e1"), "go"), &recordingSink{t: t}); err != nil {
		t.Fatalf("next turn = %v", err)
	}
}

func TestLostStateFailsClosed(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		history []api.Event
	}{
		{name: "earlier execution", history: priorTurn("e0")},
		{name: "legacy input", history: []api.Event{{Kind: api.EventInput, Message: api.TextMessage("user", "hi")}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newTestHarness(t, Config{})
			setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
			err := h.Run(context.Background(), startFor("uid", "e1", test.history, "go"), &recordingSink{t: t})
			if !errors.Is(err, errStateLost) {
				t.Fatalf("Run = %v, want %v", err, errStateLost)
			}
			if got := starts(t, h); len(got) != 0 {
				t.Fatalf("spawned %d processes", len(got))
			}
		})
	}
}

// TestInvalidFirstTurnDoesNotWedge: a first turn refused before it spawns keeps the session's
// state, so the next turn, whose History now shows a turn, still starts the session.
func TestInvalidFirstTurnDoesNotWedge(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
	ctx := context.Background()
	if err := h.Run(ctx, startFor("uid", "e1", nil), &recordingSink{t: t}); !errors.Is(err, errNoPrompt) {
		t.Fatalf("Run = %v, want %v", err, errNoPrompt)
	}
	if err := h.Run(ctx, startFor("uid", "e2", priorTurn("e1"), "go"), &recordingSink{t: t}); err != nil {
		t.Fatal(err)
	}
	got := starts(t, h)
	if len(got) != 1 || !slices.Contains(got[0].Args, "--session-id") || !slices.Contains(got[0].Args, sessionID("uid", 0)) {
		t.Fatalf("starts = %+v, want one --session-id %s", got, sessionID("uid", 0))
	}
}

func TestInputValidation(t *testing.T) {
	t.Parallel()
	data := api.Message{Role: "user", Parts: []api.Part{{Data: map[string]any{"k": "v"}}}}
	for _, test := range []struct {
		name   string
		inputs []api.Message
		want   error
	}{
		{name: "no inputs", want: errNoPrompt},
		{name: "empty text", inputs: []api.Message{*api.TextMessage("user", "")}, want: errNoPrompt},
		{name: "non-text part", inputs: []api.Message{*api.TextMessage("user", "hi"), data}, want: errNonText},
		{name: "too large", inputs: []api.Message{*api.TextMessage("user", strings.Repeat("x", maxPromptBytes+1))}, want: errPromptSize},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newTestHarness(t, Config{})
			s := &api.Start{ExecutionID: "e1", SessionUID: "uid", Inputs: test.inputs}
			if err := h.Run(context.Background(), s, &recordingSink{t: t}); !errors.Is(err, test.want) {
				t.Fatalf("Run = %v, want %v", err, test.want)
			}
			if got := starts(t, h); len(got) != 0 {
				t.Fatalf("spawned %d processes", len(got))
			}
		})
	}
}

func TestModelSelection(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		cfg       string
		requested string
		wantModel string // "" means no --model
		wantErr   error
	}{
		{name: "configured", cfg: "claude-test-model-1", wantModel: "claude-test-model-1"},
		{name: "configured and requested", cfg: "claude-test-model-1", requested: "claude-test-model-1", wantModel: "claude-test-model-1"},
		{name: "requested differs from configured", cfg: "claude-test-model-1", requested: "other", wantErr: errModelMismatch},
		{name: "requested only", requested: "claude-test-model-2", wantModel: "claude-test-model-2"},
		{name: "requested flag", requested: "--bare", wantErr: errModelArg},
		{name: "neither"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newTestHarness(t, Config{Model: test.cfg})
			setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
			s := startFor("uid", "e1", nil, "go")
			s.Model = test.requested
			err := h.Run(context.Background(), s, &recordingSink{t: t})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Run = %v, want %v", err, test.wantErr)
			}
			got := starts(t, h)
			if test.wantErr != nil {
				if len(got) != 0 {
					t.Fatalf("spawned %d processes", len(got))
				}
				return
			}
			i := slices.Index(got[0].Args, "--model")
			switch {
			case test.wantModel == "" && i >= 0:
				t.Fatalf("args = %q, want no --model", got[0].Args)
			case test.wantModel != "" && (i < 0 || got[0].Args[i+1] != test.wantModel):
				t.Fatalf("args = %q, want --model %s", got[0].Args, test.wantModel)
			}
		})
	}
}

// TestStderrNeverSurfaces: stderr can carry prompt or tool text, so for a process that reached
// system/init it reaches no error, sink call or log.
func TestStderrNeverSurfaces(t *testing.T) {
	t.Parallel()
	const sentinel = "STDERR-SENTINEL-must-not-surface"
	for _, sc := range []scenario{
		{Fixture: "init_only.jsonl", Exit: 3, Stderr: sentinel},
		{Fixture: "error_result.jsonl", Stderr: sentinel},
		{Fixture: "tool_turn.jsonl", Stderr: sentinel},
	} {
		h := newTestHarness(t, Config{})
		var logs bytes.Buffer
		h.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		setScenario(t, h, sc)
		sink := &recordingSink{t: t}
		err := h.Run(context.Background(), startFor("uid", "e1", nil, "go"), sink)
		seen := fmt.Sprint(err, sink.outputs, sink.reports, sink.usages, logs.String())
		if strings.Contains(seen, sentinel) {
			t.Fatalf("stderr surfaced: %s", seen)
		}
	}
}

func TestSinkErrorsFailTheTurn(t *testing.T) {
	t.Parallel()
	refused := errors.New("refused")
	for _, test := range []struct {
		name string
		sink *recordingSink
		want string
	}{
		{name: "report", sink: &recordingSink{reportErr: refused}, want: "claudecode: record tool result: refused"},
		{name: "output", sink: &recordingSink{outputErr: refused}, want: "claudecode: record output: refused"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newTestHarness(t, Config{})
			setScenario(t, h, scenario{Fixture: "tool_turn.jsonl"})
			test.sink.t = t
			err := h.Run(context.Background(), startFor("uid", "e1", nil, "go"), test.sink)
			if err == nil || err.Error() != test.want {
				t.Fatalf("Run = %v, want %q", err, test.want)
			}
		})
	}
}

// TestChildEnvironment: the claude process gets the default and configured variables of the
// harness's environment and nothing else. Not parallel: it sets environment variables.
func TestChildEnvironment(t *testing.T) {
	t.Setenv("CLAUDECODE_TEST_SECRET", "must-not-reach-claude")
	t.Setenv("CLAUDECODE_TEST_EXTRA", "x")
	t.Setenv("CLAUDECODE_TEST_EMPTY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "http://egress.invalid")
	t.Setenv("CLAUDECODE_TEST_UNSET", "")
	os.Unsetenv("CLAUDECODE_TEST_UNSET")
	extra := []string{"CLAUDECODE_TEST_EXTRA", "CLAUDECODE_TEST_EMPTY", "CLAUDECODE_TEST_UNSET", "PATH"}
	h := newTestHarness(t, Config{EnvAllowlist: extra})
	// Read once at New: a later change does not reach the process.
	t.Setenv("CLAUDECODE_TEST_EXTRA", "changed")
	setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
	if err := h.Run(context.Background(), startFor("uid", "e1", nil, "go"), &recordingSink{t: t}); err != nil {
		t.Fatal(err)
	}
	got := starts(t, h)[0].Env
	for _, name := range []string{"CLAUDECODE_TEST_EXTRA", "CLAUDECODE_TEST_EMPTY", "ANTHROPIC_BASE_URL", "PATH"} {
		if !slices.Contains(got, name) {
			t.Errorf("claude env %q lacks %s", got, name)
		}
	}
	allowed := slices.Concat(defaultEnv, extra)
	for _, name := range got {
		if !slices.Contains(allowed, name) {
			t.Errorf("claude env has %s, which is not allowlisted", name)
		}
	}
	if slices.Contains(got, "CLAUDECODE_TEST_SECRET") || slices.Contains(got, "CLAUDECODE_TEST_UNSET") {
		t.Fatalf("claude env = %q", got)
	}
	// Failures name the variable only: the environment can hold a real ANTHROPIC_API_KEY.
	for _, kv := range []string{"CLAUDECODE_TEST_EXTRA=x", "CLAUDECODE_TEST_EMPTY="} {
		if !slices.Contains(h.env, kv) {
			name, _, _ := strings.Cut(kv, "=")
			t.Errorf("harness env lacks %s with its value read at New", name)
		}
	}
}

// TestChildEnvironmentEmpty: with no allowlisted variable set, the process gets an empty
// environment, not the harness's. Not parallel: it unsets environment variables.
func TestChildEnvironmentEmpty(t *testing.T) {
	for _, name := range defaultEnv {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	env, err := childEnv(nil)
	if err != nil {
		t.Fatal(err)
	}
	if env == nil || len(env) != 0 {
		// Names only: the environment can hold a real ANTHROPIC_API_KEY.
		var names []string
		for _, kv := range env {
			name, _, _ := strings.Cut(kv, "=")
			names = append(names, name)
		}
		t.Fatalf("childEnv has %d variables %q (nil: %t), want an empty, non-nil environment",
			len(env), names, env == nil)
	}
}

// TestFailureBeforeInitLogged: a process that fails before system/init, as a --resume of a missing
// transcript does on every later turn, logs its exit status and first stderr line. The turn's
// error keeps its fixed text.
func TestFailureBeforeInitLogged(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	var logs bytes.Buffer
	h.logger = slog.New(slog.NewTextHandler(&logs, nil))
	ctx := context.Background()
	setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
	if err := h.Run(ctx, startFor("uid", "e1", nil, "go"), &recordingSink{t: t}); err != nil {
		t.Fatal(err)
	}
	if logs.Len() != 0 {
		t.Fatalf("successful turn logged: %s", logs.String())
	}

	id := sessionID("uid", 0)
	reason := "No conversation found with session ID: " + id
	const later = "STDERR-LATER-LINE-must-not-surface"
	// The CLI's own output for a missing transcript (testdata/README.md): a result, no init.
	setScenario(t, h, scenario{Fixture: "recorded_resume_missing.jsonl", Stderr: "\n  \n" + reason + "\n" + later, Exit: 1})
	for i, exec := range []string{"e2", "e3"} {
		logs.Reset()
		sink := &recordingSink{t: t}
		err := h.Run(ctx, startFor("uid", exec, priorTurn("e1"), "again"), sink)
		if !errors.Is(err, errNoInit) {
			t.Fatalf("turn %d: Run = %v, want %v", i+2, err, errNoInit)
		}
		got := logs.String()
		for _, want := range []string{
			`level=WARN msg="claudecode: claude failed before system/init"`,
			"session_uid=uid", "flag=--resume", "claude_session_id=" + id,
			`exit_status="exit status 1"`, `stderr="` + reason + `"`,
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("turn %d: log %q lacks %q", i+2, got, want)
			}
		}
		if seen := fmt.Sprint(err, sink.outputs, sink.reports, sink.usages, got); strings.Contains(seen, later) {
			t.Fatalf("a later stderr line surfaced: %s", seen)
		}
		if strings.Contains(err.Error(), "No conversation") {
			t.Fatalf("stderr reached the error: %v", err)
		}
	}
	args := starts(t, h)[2].Args
	if i := slices.Index(args, "--resume"); i < 0 || args[i+1] != id {
		t.Fatalf("args = %q, want --resume %s", args, id)
	}
}

// TestFailureBeforeInitWithoutStderr: a process that exits before init with nothing on stderr
// still logs its exit status, and a cancelled turn logs nothing.
func TestFailureBeforeInitWithoutStderr(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	var logs bytes.Buffer
	h.logger = slog.New(slog.NewTextHandler(&logs, nil))
	setScenario(t, h, scenario{Exit: 2})
	err := h.Run(context.Background(), startFor("uid", "e1", nil, "go"), &recordingSink{t: t})
	if err == nil || err.Error() != "claudecode: claude exited with status 2 before a result" {
		t.Fatalf("Run = %v", err)
	}
	if got := logs.String(); !strings.Contains(got, `exit_status="exit status 2"`) || !strings.Contains(got, `stderr=""`) ||
		!strings.Contains(got, "flag=--session-id") {
		t.Fatalf("log = %q", got)
	}

	logs.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	setScenario(t, h, scenario{Stderr: "STDERR-SENTINEL", Exit: 1})
	if err := h.Run(ctx, startFor("uid", "e2", nil, "go"), &recordingSink{t: t}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if logs.Len() != 0 {
		t.Fatalf("cancelled turn logged: %s", logs.String())
	}
}

func TestFirstLine(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 5000)
	for _, test := range []struct {
		name, in, want string
	}{
		{name: "empty"},
		{name: "blank lines only", in: "\n \t\n\r\n"},
		{name: "first non-blank line", in: "\n\n  Error: no conversation  \nsecond\n", want: "Error: no conversation"},
		{name: "no trailing newline", in: "only", want: "only"},
		{name: "long line cut", in: long + "\nsecond", want: long[:stderrLineBytes]},
		{name: "control characters", in: "a\x1b[31mb\tc\r\n", want: "a?[31mb?c"},
		{name: "invalid UTF-8", in: "a\xffb", want: "a\uFFFDb"},
		{name: "format characters", in: "safe\u202Etxt.exe\u200B\uFEFF", want: "safe?txt.exe??"},
		{name: "leading space past one read", in: strings.Repeat(" ", 5000) + "Error: late\nsecond", want: "Error: late"},
		{name: "space-only line past one read", in: strings.Repeat(" ", 5000) + "\nsecond\n", want: "second"},
		{name: "cut on a rune boundary", in: strings.Repeat("x", stderrLineBytes-1) + "é", want: strings.Repeat("x", stderrLineBytes-1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			r := bufio.NewReader(strings.NewReader(test.in))
			if got := firstLine(r); got != test.want {
				t.Fatalf("firstLine = %q, want %q", got, test.want)
			}
		})
	}
	// firstLine stops at the end of the first line, so the caller drains the rest.
	r := bufio.NewReader(strings.NewReader("first\nsecond\n"))
	firstLine(r)
	if rest, _ := io.ReadAll(r); string(rest) != "second\n" {
		t.Fatalf("rest = %q", rest)
	}
}
