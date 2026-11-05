package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-primitives/sessions/api"
)

// The test binary doubles as a fake claude: the harness runs it with "-p" first, which no go test
// invocation passes. The fake reads its scenario from fake-claude.json in its working directory,
// records what it saw in fake-claude-record.jsonl there, and replays stream-json lines with
// {{SESSION_ID}} replaced by the id from --session-id or --resume. It never touches the network.

const (
	scenarioFile = "fake-claude.json"
	recordFile   = "fake-claude-record.jsonl"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "-p" {
		os.Exit(fakeClaude(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// scenario is one fake claude run.
type scenario struct {
	Fixture     string   `json:"fixture,omitempty"`      // absolute path of a testdata .jsonl file
	Lines       []string `json:"lines,omitempty"`        // lines emitted after Fixture
	Partial     string   `json:"partial,omitempty"`      // written last, without a newline
	Stderr      string   `json:"stderr,omitempty"`       // written to stderr first
	Exit        int      `json:"exit,omitempty"`         // exit status when not blocking
	Block       bool     `json:"block,omitempty"`        // after emitting, wait for SIGINT
	OnInterrupt string   `json:"on_interrupt,omitempty"` // with Block: "ignore" keeps running; else exit 130
	Grandchild  bool     `json:"grandchild,omitempty"`   // leave a sleeping child holding stdout
	Detached    bool     `json:"detached,omitempty"`     // leave a sleeping child on /dev/null stdio
}

// record is one fake claude observation.
type record struct {
	Event string   `json:"event"` // start | blocking | interrupt | grandchild | detached
	Args  []string `json:"args,omitempty"`
	Stdin string   `json:"stdin,omitempty"`
	PID   int      `json:"pid,omitempty"`
}

func fakeClaude(args []string) int {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)

	var sc scenario
	b, err := os.ReadFile(scenarioFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake claude: no scenario:", err)
		return 97
	}
	if err := json.Unmarshal(b, &sc); err != nil {
		fmt.Fprintln(os.Stderr, "fake claude: bad scenario:", err)
		return 97
	}
	stdin, _ := io.ReadAll(os.Stdin)
	writeRecord(record{Event: "start", Args: args, Stdin: string(stdin)})
	if sc.Stderr != "" {
		fmt.Fprintln(os.Stderr, sc.Stderr)
	}

	var id string
	for i, a := range args {
		if (a == "--session-id" || a == "--resume") && i+1 < len(args) {
			id = args[i+1]
		}
	}
	out := bufio.NewWriter(os.Stdout)
	emit := func(line string) {
		out.WriteString(strings.ReplaceAll(line, "{{SESSION_ID}}", id))
		out.WriteByte('\n')
	}
	if sc.Fixture != "" {
		f, err := os.ReadFile(sc.Fixture)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake claude: fixture:", err)
			return 97
		}
		for _, line := range strings.Split(strings.TrimRight(string(f), "\n"), "\n") {
			emit(line)
		}
	}
	for _, line := range sc.Lines {
		emit(line)
	}
	if sc.Partial != "" {
		out.WriteString(sc.Partial)
	}
	out.Flush()

	if sc.Grandchild {
		gc := exec.Command("sleep", "30")
		gc.Stdout = os.Stdout
		if err := gc.Start(); err != nil {
			return 97
		}
		writeRecord(record{Event: "grandchild", PID: gc.Process.Pid})
	}
	if sc.Detached {
		// exec.Command leaves Stdin, Stdout and Stderr nil: the child gets /dev/null for each and
		// stays in this process's group.
		gc := exec.Command("sleep", "60")
		if err := gc.Start(); err != nil {
			return 97
		}
		writeRecord(record{Event: "detached", PID: gc.Process.Pid})
	}
	if !sc.Block {
		return sc.Exit
	}
	writeRecord(record{Event: "blocking"})
	for range sigs {
		writeRecord(record{Event: "interrupt"})
		if sc.OnInterrupt != "ignore" {
			return 130
		}
	}
	return 0
}

func writeRecord(r record) {
	f, err := os.OpenFile(recordFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(r)
	f.Write(append(b, '\n'))
}

// newTestHarness returns a harness that runs the fake claude in a fresh WorkDir.
func newTestHarness(t *testing.T, cfg Config) *Harness {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Binary == "" {
		cfg.Binary = exe
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = t.TempDir()
	}
	if cfg.PermissionMode == "" {
		cfg.PermissionMode = "dontAsk"
	}
	if cfg.InterruptGrace == 0 {
		cfg.InterruptGrace = 5 * time.Second
	}
	h, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Generous, so a loaded machine does not turn an exit into an interrupt; tests of the graces
	// shorten them.
	h.postResultGrace = 10 * time.Second
	h.drainGrace = 5 * time.Second
	return h
}

// setScenario writes the fake's scenario for the harness's next turn.
func setScenario(t *testing.T, h *Harness, sc scenario) {
	t.Helper()
	if sc.Fixture != "" && !filepath.IsAbs(sc.Fixture) {
		abs, err := filepath.Abs(filepath.Join("testdata", sc.Fixture))
		if err != nil {
			t.Fatal(err)
		}
		sc.Fixture = abs
	}
	b, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.cfg.WorkDir, scenarioFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// records returns what the fake recorded so far, across turns.
func records(t *testing.T, h *Harness) []record {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(h.cfg.WorkDir, recordFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []record
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var r record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("record %q: %v", line, err)
		}
		out = append(out, r)
	}
	return out
}

// starts returns the fake's start records: one per spawned process.
func starts(t *testing.T, h *Harness) []record {
	t.Helper()
	var out []record
	for _, r := range records(t, h) {
		if r.Event == "start" {
			out = append(out, r)
		}
	}
	return out
}

// waitRecord waits until the fake recorded event.
func waitRecord(t *testing.T, h *Harness, event string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range records(t, h) {
			if r.Event == event {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fake claude never recorded %q", event)
}

// startFor builds a Start for one turn of session uid.
func startFor(uid, exec string, history []api.Event, texts ...string) *api.Start {
	var inputs []api.Message
	for _, s := range texts {
		inputs = append(inputs, *api.TextMessage("user", s))
	}
	return &api.Start{ExecutionID: exec, SessionUID: uid, History: history, Inputs: inputs}
}

// priorTurn is the History a later turn sees: the host journals each execution start.
func priorTurn(exec string) []api.Event {
	return []api.Event{{ExecutionID: exec, Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{}}}
}

// recordingSink records what the harness emits. It fails the test on Model or ToolCall: the
// harness is unmediated and reports its tools.
type recordingSink struct {
	t *testing.T

	mu        sync.Mutex
	outputs   []string
	reports   []api.ToolResult
	usages    []api.Usage
	reportErr error
	outputErr error
}

func (s *recordingSink) Model(context.Context, api.ModelRequest) (api.ModelResponse, error) {
	s.t.Error("unmediated harness called sink.Model")
	return api.ModelResponse{}, errors.New("unexpected")
}

func (s *recordingSink) ToolCall(context.Context, api.ToolCall) (api.ToolResult, error) {
	s.t.Error("harness called sink.ToolCall; in-harness tools are reported")
	return api.ToolResult{}, errors.New("unexpected")
}

func (s *recordingSink) Output(_ context.Context, delta string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.outputErr != nil {
		return s.outputErr
	}
	s.outputs = append(s.outputs, delta)
	return nil
}

func (s *recordingSink) Report(_ context.Context, tr api.ToolResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reportErr != nil {
		return s.reportErr
	}
	s.reports = append(s.reports, tr)
	return nil
}

func (s *recordingSink) Usage(_ context.Context, u api.Usage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usages = append(s.usages, u)
	return nil
}
