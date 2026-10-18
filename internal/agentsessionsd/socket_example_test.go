package agentsessionsd

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/client"
)

func startPythonToolExample(t *testing.T, socket, database string) func() {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python 3 is needed to smoke-test the documented example")
	}
	ready := filepath.Join(filepath.Dir(socket), "ready")
	cmd := exec.CommandContext(t.Context(), python, "-u", "../../docs/tool-executor-example.py", socket, database)
	// A file avoids racing process output with readiness polling.
	log, err := os.Create(ready)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			_ = log.Close()
			_ = os.Remove(socket)
		}
	}
	t.Cleanup(stop)
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		data, _ := os.ReadFile(ready)
		if strings.Contains(string(data), "listening") {
			return stop
		}
		time.Sleep(10 * time.Millisecond)
	}
	data, _ := os.ReadFile(ready)
	t.Fatalf("example did not start: %s", data)
	return stop
}

func TestDocumentedPythonExecutorThroughDaemon(t *testing.T) {
	dir, err := os.MkdirTemp("", "sessions-example-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket, database := filepath.Join(dir, "tool.sock"), filepath.Join(dir, "effects.db")
	startPythonToolExample(t, socket, database)
	harness := serveHarness(t, gatedHarness{})
	c, _ := runSocketDaemon(t, harness, filepath.Join(t.TempDir(), "journal.db"), configuredSocketJSON(socket, "true", "publish"))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	turn, err := c.Exec(ctx, client.ExecOptions{Harness: "h", Inputs: []string{"hi"}})
	if err != nil {
		t.Fatal(err)
	}
	ref := turn.Session.GetPendingApproval()
	if ref == nil {
		t.Fatal("missing approval")
	}
	// Use the same real daemon gate as the operator guide, with an independently running Python peer.
	uid := turn.Session.GetMetadata().GetUid()
	if _, err := c.Approve(ctx, uid, api.ApprovalDecision{ExecutionID: ref.GetExecutionId(), ToolCallID: ref.GetToolCallId(), RequestSeq: ref.GetRequestSeq(), Approved: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Resume(ctx, uid, false); err != nil {
		t.Fatal(err)
	}
	records, err := c.Replay(ctx, uid, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range records {
		if r.GetEvent().GetKind() == v1.EventKind_EVENT_TOOL_RESULT {
			result := r.GetEvent().GetResult()
			if result.GetIsError() {
				t.Fatalf("Python executor: %v", result)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("no Python executor receipt")
	}
}

func TestDocumentedPythonExecutorLogsStorageFailureWithoutPayload(t *testing.T) {
	dir, err := os.MkdirTemp("", "sessions-example-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket, database := filepath.Join(dir, "tool.sock"), filepath.Join(dir, "effects.db")
	startPythonToolExample(t, socket, database)
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Readiness means schema initialization has finished. Remove the receipt table
	// to force a storage error without coupling the response deadline to SQLite's
	// platform-dependent busy/lock waits.
	if _, err := db.Exec("DROP TABLE receipts"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
	_, err = io.WriteString(conn, `{"version":1,"harness":"h","session_uid":"s","call_id":"c","tool":"publish","arguments":{"text":"TOP_SECRET_DO_NOT_LOG"},"idempotency_key":"k"}`+"\n")
	if err != nil {
		t.Fatal(err)
	}
	var reply map[string]any
	if err := json.NewDecoder(conn).Decode(&reply); err != nil {
		t.Fatal(err)
	}
	if reply["is_error"] != true || reply["error"] != "request refused" {
		t.Fatalf("wire error was not sanitized: %v", reply)
	}
	diagnostics, err := os.ReadFile(filepath.Join(dir, "ready"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(diagnostics), "executor storage failure: OperationalError SQLITE_ERROR") || strings.Contains(string(diagnostics), "TOP_SECRET_DO_NOT_LOG") {
		t.Fatalf("missing safe storage diagnostic: %s", diagnostics)
	}
}

func TestDocumentedPythonExecutorDurability(t *testing.T) {
	dir, err := os.MkdirTemp("", "sessions-example-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket, database := filepath.Join(dir, "tool.sock"), filepath.Join(dir, "effects.db")
	stop := startPythonToolExample(t, socket, database)
	call := func(uid, id string) map[string]any {
		conn, err := net.DialTimeout("unix", socket, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		request := map[string]any{"version": 1, "harness": "h", "session_uid": uid, "call_id": id, "tool": "publish", "arguments": map[string]any{"text": "demo"}, "idempotency_key": "key"}
		if err := json.NewEncoder(conn).Encode(request); err != nil {
			t.Fatal(err)
		}
		var reply map[string]any
		if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&reply); err != nil {
			t.Fatal(err)
		}
		if reply["is_error"] != false || reply["call_id"] != id {
			t.Fatalf("reply = %v", reply)
		}
		return reply
	}
	first := call("a", "c1")
	stop()
	startPythonToolExample(t, socket, database)
	second := call("a", "c2")
	call("b", "c3")
	a, _ := json.Marshal(first["output"])
	b, _ := json.Marshal(second["output"])
	if string(a) != string(b) {
		t.Fatalf("lost original receipt: %s != %s", a, b)
	}
	python, _ := exec.LookPath("python3")
	out, err := exec.Command(python, "-c", "import sqlite3,sys; print(sqlite3.connect(sys.argv[1]).execute('SELECT count(*) FROM publications').fetchone()[0])", database).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "2" {
		t.Fatalf("effects = %s, %v; want 2", out, err)
	}
}
