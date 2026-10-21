package agentsessionsd

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/client"
	"github.com/agent-primitives/sessions/harness/toolagent"
	"github.com/agent-primitives/sessions/wire"
)

// fakeChatModel is an OpenAI-compatible Chat Completions endpoint. A request whose last message is
// a tool result gets a text answer; any other request gets one publish call, numbered in order,
// whose text is the last user message. It serves the streaming and non-streaming forms.
type fakeChatModel struct {
	mu       sync.Mutex
	requests int
	calls    int
}

func (m *fakeChatModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Stream   bool `json:"stream"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Messages) == 0 || len(req.Tools) != 1 || req.Tools[0].Function.Name != "publish" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	m.requests++
	last := req.Messages[len(req.Messages)-1]
	var message map[string]any
	finish := "stop"
	if last.Role == "tool" {
		message = map[string]any{"role": "assistant", "content": "Published: " + last.Content}
	} else {
		m.calls++
		args, _ := json.Marshal(map[string]string{"text": last.Content})
		message = map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{
			"index": 0, "id": fmt.Sprintf("call_%d", m.calls), "type": "function",
			"function": map[string]any{"name": "publish", "arguments": string(args)},
		}}}
		finish = "tool_calls"
	}
	m.mu.Unlock()
	if !req.Stream {
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": finish}}})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, chunk := range []any{
		map[string]any{"choices": []any{map[string]any{"delta": message}}},
		map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": finish}}},
	} {
		data, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

func (m *fakeChatModel) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.requests
}

// publications reads the documented Python executor's effects: one row per executed publish.
func publications(t *testing.T, database string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT key || ' ' || arguments FROM publications ORDER BY rowid`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil
		}
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestApprovalGatedModelToolOnStockDaemon is the end-to-end case of docs/approval-gated-tool.md:
// the stock daemon (Run with only flags), the reference tool harness served over harnesswire as
// harnessnode serves it, an OpenAI-compatible model that calls the tool, and the documented Python
// executor. The call parks before any socket connection; the daemon and the harness restart while
// it is parked and again after the decision; and the approved call publishes exactly once. An
// approval for one call cannot run another, and arguments changed in the journal are refused.
func TestApprovalGatedModelToolOnStockDaemon(t *testing.T) {
	t.Setenv("MODEL_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	dir, err := os.MkdirTemp("", "sessions-gate-") // short: socket paths are limited to 103 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket, effects := filepath.Join(dir, "publish.sock"), filepath.Join(dir, "receipts.db")
	startPythonToolExample(t, socket, effects)
	model := &fakeChatModel{}
	endpoint := httptest.NewServer(model)
	t.Cleanup(endpoint.Close)
	config := writeToolConfig(t, `{"version":1,"executors":[{"name":"publishd","type":"unix","socket":"`+socket+`"}],`+
		`"tools":[{"harness":"publisher","tool":"publish","executor":"publishd","durable_dedup":true}]}`)
	journal := filepath.Join(t.TempDir(), "sessions.db")
	harness := toolagent.Harness{Model: "gpt-test", Tools: []api.ToolDefinition{toolagent.PublishTool()}}

	// start runs a fresh harness process and daemon on the same journal, as an operator restart.
	var stopRunning func()
	start := func() *client.Client {
		t.Helper()
		if stopRunning != nil {
			stopRunning()
		}
		harnessAddr, stopHarness := serveStoppableHarness(t, harness)
		c, stopDaemon := runDaemon(t, []string{
			"-journal", journal, "-addr", "127.0.0.1:0",
			"-model", "gpt-test", "-model-base-url", endpoint.URL,
			"-harness", "publisher=" + harnessAddr, "-tool-executors", config,
		})
		stopRunning = func() { stopDaemon(); stopHarness() }
		return c
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	pending := func(c *client.Client, uid string) *v1.ApprovalRef {
		t.Helper()
		s, err := c.GetSession(ctx, uid)
		if err != nil {
			t.Fatal(err)
		}
		return s.GetPendingApproval()
	}
	decision := func(ref *v1.ApprovalRef) api.ApprovalDecision {
		return api.ApprovalDecision{ExecutionID: ref.GetExecutionId(), ToolCallID: ref.GetToolCallId(), RequestSeq: ref.GetRequestSeq(), Approved: true}
	}

	// The model calls publish; the call parks before the executor is dialed.
	c := start()
	turn, err := c.Exec(ctx, client.ExecOptions{Harness: "publisher", Inputs: []string{"hello"}})
	if err != nil {
		t.Fatal(err)
	}
	uid := turn.Session.GetMetadata().GetUid()
	a := turn.Session.GetPendingApproval()
	if a == nil || a.GetToolCallId() != "call_1" || model.count() != 1 || len(publications(t, effects)) != 0 {
		t.Fatalf("Exec did not park before the executor: ref=%v model requests=%d", a, model.count())
	}
	if _, err := c.Resume(ctx, uid, false); err != nil || len(publications(t, effects)) != 0 {
		t.Fatalf("Resume without a decision = %v, publications %v", err, publications(t, effects))
	}

	// Restart while parked: the same request is still pending, and nothing ran.
	c = start()
	if got := pending(c, uid); !proto.Equal(got, a) {
		t.Fatalf("pending approval after restart = %v, want %v", got, a)
	}
	if _, err := c.Approve(ctx, uid, decision(a)); err != nil {
		t.Fatal(err)
	}
	if got := publications(t, effects); len(got) != 0 {
		t.Fatalf("Approve executed the call: %v", got)
	}

	// Restart after the decision, then Resume runs the approved call once.
	c = start()
	if _, err := c.Resume(ctx, uid, false); err != nil {
		t.Fatal(err)
	}
	wantRow := a.GetExecutionId() + `/call_1 {"text": "hello"}`
	if got := publications(t, effects); len(got) != 1 || got[0] != wantRow {
		t.Fatalf("publications = %q, want [%q]", got, wantRow)
	}
	// Resume served the first completion from the journal and asked the model once more.
	if model.count() != 2 {
		t.Fatalf("model requests = %d, want 2", model.count())
	}
	records, err := c.Replay(ctx, uid, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var receipts []*v1.ToolResult
	var output string
	for _, r := range records {
		switch ev := r.GetEvent(); ev.GetKind() {
		case v1.EventKind_EVENT_TOOL_RESULT:
			receipts = append(receipts, ev.GetResult())
		case v1.EventKind_EVENT_OUTPUT:
			output = wire.MessageFromProto(ev.GetMessage()).Text()
		}
	}
	if len(receipts) != 1 || receipts[0].GetIsError() || receipts[0].GetApprovalRequestSeq() != a.GetRequestSeq() {
		t.Fatalf("receipts = %v", receipts)
	}
	if output != `Published: {"text":"published"}` {
		t.Fatalf("final output = %q", output)
	}

	// Completed: more Resumes, across another restart, run nothing again.
	if _, err := c.Resume(ctx, uid, false); err != nil {
		t.Fatal(err)
	}
	c = start()
	if _, err := c.Resume(ctx, uid, false); err != nil {
		t.Fatal(err)
	}
	if got := publications(t, effects); len(got) != 1 || model.count() != 2 {
		t.Fatalf("completed call ran again: publications %q, model requests %d", got, model.count())
	}

	// A second turn parks call B. A's approval, or any mix of A's and B's identities, cannot run it.
	turn, err = c.Exec(ctx, client.ExecOptions{Session: uid, Inputs: []string{"second"}})
	if err != nil {
		t.Fatal(err)
	}
	b := turn.Session.GetPendingApproval()
	if b == nil || b.GetToolCallId() != "call_2" || b.GetExecutionId() == a.GetExecutionId() {
		t.Fatalf("second turn park = %v", b)
	}
	if _, err := c.Approve(ctx, uid, decision(a)); err != nil {
		t.Fatalf("exact retry of A's decision = %v, want its existing record", err)
	}
	for name, mixed := range map[string]*v1.ApprovalRef{
		"A's call in B's execution":     {ExecutionId: b.GetExecutionId(), ToolCallId: a.GetToolCallId(), RequestSeq: a.GetRequestSeq()},
		"B's call with A's request":     {ExecutionId: b.GetExecutionId(), ToolCallId: b.GetToolCallId(), RequestSeq: a.GetRequestSeq()},
		"B's call in A's execution":     {ExecutionId: a.GetExecutionId(), ToolCallId: b.GetToolCallId(), RequestSeq: b.GetRequestSeq()},
		"B's request with A's call":     {ExecutionId: b.GetExecutionId(), ToolCallId: a.GetToolCallId(), RequestSeq: b.GetRequestSeq()},
		"B's request in A's execution":  {ExecutionId: a.GetExecutionId(), ToolCallId: a.GetToolCallId(), RequestSeq: b.GetRequestSeq()},
		"B's call in A's decision":      {ExecutionId: a.GetExecutionId(), ToolCallId: b.GetToolCallId(), RequestSeq: a.GetRequestSeq()},
		"a call the journal never made": {ExecutionId: b.GetExecutionId(), ToolCallId: "call_9", RequestSeq: b.GetRequestSeq()},
	} {
		if _, err := c.Approve(ctx, uid, decision(mixed)); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("%s: Approve = %v, want FailedPrecondition", name, err)
		}
	}
	if _, err := c.Resume(ctx, uid, false); err != nil {
		t.Fatal(err)
	}
	if got := pending(c, uid); !proto.Equal(got, b) || len(publications(t, effects)) != 1 {
		t.Fatalf("B ran without its own decision: pending %v, publications %q", got, publications(t, effects))
	}

	// B is approved, then its journaled arguments are changed while the daemon is down. The
	// approval binds the TOOL_CALL record's content_hash, so Resume refuses before dialing.
	if _, err := c.Approve(ctx, uid, decision(b)); err != nil {
		t.Fatal(err)
	}
	stopRunning()
	tamperToolCallArgs(t, journal, uid, b.GetRequestSeq()-1, b.GetToolCallId())
	c = start()
	if _, err := c.Resume(ctx, uid, false); status.Code(err) != codes.DataLoss {
		t.Fatalf("Resume with changed arguments = %v, want DataLoss", err)
	}
	if got := publications(t, effects); len(got) != 1 || got[0] != wantRow {
		t.Fatalf("changed arguments reached the executor: %q", got)
	}
}

// tamperToolCallArgs rewrites the arguments of the TOOL_CALL stored at seq, keeping its hashes.
func tamperToolCallArgs(t *testing.T, journal, uid string, seq int64, callID string) {
	t.Helper()
	db, err := sql.Open("sqlite", journal)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var blob []byte
	if err := db.QueryRow(`SELECT event FROM events WHERE session = ? AND seq = ?`, uid, seq).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	pb := &v1.Event{}
	if err := proto.Unmarshal(blob, pb); err != nil {
		t.Fatal(err)
	}
	ev := wire.EventFromProto(pb)
	if ev.Kind != api.EventToolCall || ev.ToolCall.ID != callID {
		t.Fatalf("seq %d is %s, not call %s", seq, ev.Kind, callID)
	}
	ev.ToolCall.Args = map[string]any{"text": "changed"}
	if blob, err = proto.Marshal(wire.EventToProto(ev)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE events SET event = ? WHERE session = ? AND seq = ?`, blob, uid, seq); err != nil {
		t.Fatal(err)
	}
}
