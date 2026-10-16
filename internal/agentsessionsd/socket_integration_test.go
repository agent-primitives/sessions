package agentsessionsd

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/client"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/sqlitelog"
)

func runSocketDaemon(t *testing.T, harnessAddr, journal, config string) (*client.Client, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	addrs := make(chan string, 1)
	done := make(chan error, 1)
	args := []string{"-journal", journal, "-addr", "127.0.0.1:0", "-model", "m", "-harness", "h=" + harnessAddr}
	if config != "" {
		args = append(args, "-tool-executors", writeToolConfig(t, config))
	}
	go func() { done <- Run(args, withContext(ctx), withListening(func(addr string) { addrs <- addr })) }()
	var addr string
	select {
	case addr = <-addrs:
	case err := <-done:
		cancel()
		t.Fatalf("Run before listening: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("Run startup timeout")
	}
	c, err := client.Dial(addr)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = c.Close()
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Run: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("Run did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return c, stop
}

func configuredSocketJSON(socket, declaration, tool string) string {
	data := strings.Replace(validToolConfig, "/run/tool.sock", socket, 1)
	data = strings.Replace(data, `"durable_dedup":true`, `"durable_dedup":`+declaration, 1)
	return strings.Replace(data, `"tool":"publish"`, `"tool":"`+tool+`"`, 1)
}

func TestRunSocketApprovalAndReceiptRecovery(t *testing.T) {
	harness := serveHarness(t, gatedHarness{})
	for _, scenario := range []string{"approve", "deny", "false", "unknown", "response lost", "socket down", "unconfigured tool", "no config"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var effects int
			var mu sync.Mutex
			socket, calls := unixToolPeer(t, func(conn net.Conn) {
				data, err := bufio.NewReader(conn).ReadBytes('\n')
				if err != nil {
					return
				}
				var req map[string]any
				if json.Unmarshal(data, &req) != nil {
					return
				}
				mu.Lock()
				effects++
				mu.Unlock() // A committed effect can precede a lost response.
				if scenario == "response lost" {
					return
				}
				reply := map[string]any{"version": 1, "call_id": req["call_id"], "is_error": false, "output": map[string]any{"text": "published"}}
				_ = json.NewEncoder(conn).Encode(reply)
			})
			declaration := "true"
			if scenario == "false" {
				declaration = "false"
			}
			if scenario == "unknown" {
				declaration = "null"
			}
			tool := "publish"
			if scenario == "unconfigured tool" {
				tool = "Publish"
			}
			if scenario == "socket down" {
				socket = filepath.Join(filepath.Dir(socket), "absent.sock")
			}
			config := configuredSocketJSON(socket, declaration, tool)
			if scenario == "no config" {
				config = ""
			}
			journal := filepath.Join(t.TempDir(), "journal.db")
			c, stop := runSocketDaemon(t, harness, journal, config)
			turn, err := c.Exec(ctx, client.ExecOptions{Harness: "h", Inputs: []string{"hi"}})
			if err != nil {
				t.Fatal(err)
			}
			uid := turn.Session.GetMetadata().GetUid()
			ref := turn.Session.GetPendingApproval()
			if ref == nil || calls.Load() != 0 {
				t.Fatalf("call did not park before dialing: %v, %d", ref, calls.Load())
			}
			if _, err := c.Approve(ctx, uid, api.ApprovalDecision{ExecutionID: ref.GetExecutionId(), ToolCallID: ref.GetToolCallId(), RequestSeq: ref.GetRequestSeq(), Approved: scenario != "deny"}); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 0 {
				t.Fatal("Approve dialed; only Resume may execute")
			}
			_, err = c.Resume(ctx, uid, false)
			if scenario == "no config" {
				if err == nil {
					t.Fatal("no config must retain missing-executor behavior")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			records, err := c.Replay(ctx, uid, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			var receipt *v1.ToolResult
			for _, r := range records {
				if r.GetEvent().GetKind() == v1.EventKind_EVENT_TOOL_RESULT {
					receipt = r.GetEvent().GetResult()
				}
			}
			if receipt == nil {
				t.Fatal("missing durable tool receipt")
			}
			wantCalls := int32(0)
			wantCode := v1.ToolResult_EXECUTOR_ERROR
			switch scenario {
			case "approve":
				wantCalls = 1
				wantCode = v1.ToolResult_CODE_UNSPECIFIED
			case "deny":
				wantCode = v1.ToolResult_APPROVAL_DENIED
			case "response lost":
				wantCalls = 1
			}
			if calls.Load() != wantCalls || receipt.GetCode() != wantCode {
				t.Fatalf("calls=%d receipt=%v; want calls=%d code=%v", calls.Load(), receipt, wantCalls, wantCode)
			}
			if scenario == "socket down" && (!receipt.GetIsError() || !strings.Contains(receipt.GetError(), "dial:") || strings.Contains(receipt.GetError(), socket) || strings.Contains(receipt.GetError(), filepath.Dir(socket))) {
				t.Fatalf("dial failure receipt must omit local paths: %v", receipt)
			}
			mu.Lock()
			committed := effects
			mu.Unlock()
			if scenario == "response lost" && (!receipt.GetIsError() || committed != 1) {
				t.Fatalf("EXECUTOR_ERROR must coexist with committed effect: effects=%d receipt=%v", committed, receipt)
			}
			stop()
			c, _ = runSocketDaemon(t, harness, journal, config)
			if _, err := c.Resume(ctx, uid, false); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Resume(ctx, uid, false); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != wantCalls {
				t.Fatalf("completed receipt redialed after restart: %d", calls.Load())
			}
		})
	}
}

func TestRunSocketExactRoutingAndNoConfigParity(t *testing.T) {
	harness := serveHarness(t, toolHarness{})
	for _, scenario := range []string{"configured", "wrong tool", "no config", "false", "unknown"} {
		t.Run(scenario, func(t *testing.T) {
			socket, calls := unixToolPeer(t, func(conn net.Conn) {
				var req map[string]any
				if json.NewDecoder(conn).Decode(&req) != nil {
					return
				}
				_ = json.NewEncoder(conn).Encode(map[string]any{"version": 1, "call_id": req["call_id"], "is_error": false, "output": map[string]any{"text": "ok"}})
			})
			declaration := "true"
			if scenario == "false" {
				declaration = "false"
			}
			if scenario == "unknown" {
				declaration = "null"
			}
			tool := "probe"
			if scenario == "wrong tool" {
				tool = "Probe"
			}
			config := configuredSocketJSON(socket, declaration, tool)
			if scenario == "no config" {
				config = ""
			}
			c, _ := runSocketDaemon(t, harness, filepath.Join(t.TempDir(), "j.db"), config)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			turn, err := c.Exec(ctx, client.ExecOptions{Harness: "h", Inputs: []string{"hi"}})
			if scenario == "configured" {
				if err != nil || turn.Output != "tool:ok" || calls.Load() != 1 {
					t.Fatalf("turn=%v err=%v calls=%d", turn, err, calls.Load())
				}
			} else if err == nil || calls.Load() != 0 {
				t.Fatalf("refusal err=%v calls=%d", err, calls.Load())
			}
		})
	}
}

// Fail the journal's first receipt after the executor commits. A fresh controller must resend
// the same identity, allowing a restarted durable executor to return its original receipt.
type socketReceiptFault struct {
	eventlog.Store
	fail bool
}

func (f *socketReceiptFault) Append(head, fence int64, event api.Event) (eventlog.Record, error) {
	if f.fail && event.Kind == api.EventToolResult {
		return eventlog.Record{}, errors.New("receipt write lost")
	}
	return f.Store.Append(head, fence, event)
}

type fixedSocketHarness struct{}

func (fixedSocketHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "fixed", Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}}, nil
}
func (fixedSocketHarness) Run(ctx context.Context, _ *api.Start, sink api.EventSink) error {
	_, err := sink.ToolCall(ctx, socketCall())
	return err
}

func TestSocketExecutorDurableDedupAfterJournalReceiptLoss(t *testing.T) {
	path := filepath.Join(t.TempDir(), "effects.db")
	open := func() *sql.DB {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		return db
	}
	db := open()
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TABLE effects(uid TEXT,key TEXT); CREATE TABLE receipts(uid TEXT,key TEXT,reply TEXT,PRIMARY KEY(uid,key));`); err != nil {
		t.Fatal(err)
	}
	requests := make(chan map[string]any, 4)
	socket, calls := unixToolPeer(t, func(conn net.Conn) {
		var req map[string]any
		if json.NewDecoder(conn).Decode(&req) != nil {
			return
		}
		requests <- req
		tx, err := db.Begin()
		if err != nil {
			t.Error(err)
			return
		}
		defer tx.Rollback()
		uid, key := req["session_uid"], req["idempotency_key"]
		var reply string
		err = tx.QueryRow("SELECT reply FROM receipts WHERE uid=? AND key=?", uid, key).Scan(&reply)
		if errors.Is(err, sql.ErrNoRows) {
			reply = fmt.Sprintf(`{"version":1,"call_id":%q,"is_error":false,"output":{"text":"original receipt"}}`, req["call_id"])
			if _, err = tx.Exec("INSERT INTO effects VALUES(?,?);", uid, key); err != nil {
				t.Error(err)
				return
			}
			if _, err = tx.Exec("INSERT INTO receipts VALUES(?,?,?)", uid, key, reply); err != nil {
				t.Error(err)
				return
			}
		} else if err != nil {
			t.Error(err)
			return
		}
		if err := tx.Commit(); err != nil {
			t.Error(err)
			return
		}
		_, _ = io.WriteString(conn, reply+"\n")
	})
	for _, uid := range []string{"session-a", "session-b"} {
		store, err := sqlitelog.Open(filepath.Join(t.TempDir(), "j.db"))
		if err != nil {
			t.Fatal(err)
		}
		log := store.Session(uid)
		fault := &socketReceiptFault{Store: log, fail: true}
		executor := configExecutor(t, socket, "true", "h", io.Discard)
		c, err := controller.New(fault, nil, controller.WithSessionUID(uid), controller.WithToolExecutor(executor))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Exec(t.Context(), fixedSocketHarness{}, nil, 0); err == nil {
			t.Fatal("receipt fault missed")
		}
		first := <-requests
		// Closing/reopening the independent executor database discards all in-memory state.
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		db = open()
		fault.fail = false
		c, err = controller.New(fault, nil, controller.WithSessionUID(uid), controller.WithToolExecutor(executor))
		if err != nil {
			t.Fatal(err)
		}
		if resumed, err := c.Resume(t.Context(), fixedSocketHarness{}); !resumed || err != nil {
			t.Fatalf("Resume=%v,%v", resumed, err)
		}
		second := <-requests
		a, _ := json.Marshal(first)
		b, _ := json.Marshal(second)
		if string(a) != string(b) {
			t.Fatalf("redrive changed identity: %s != %s", a, b)
		}
		if resumed, err := c.Resume(t.Context(), fixedSocketHarness{}); resumed || err != nil {
			t.Fatalf("completed Resume=%v,%v", resumed, err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM effects").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 || calls.Load() != 4 {
		t.Fatalf("effects=%d calls=%d; want 2 effects for 4 attempts across 2 UIDs", count, calls.Load())
	}
}
