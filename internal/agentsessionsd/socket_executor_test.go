package agentsessionsd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/controller"
)

// A real Unix peer catches wrong transport, framing, routing or repeat connections.
func unixToolPeer(t *testing.T, handle func(net.Conn)) (string, *atomic.Int32) {
	t.Helper()
	dir, err := os.MkdirTemp("", "sessions-tool-")
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "executor.sock")
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			wg.Go(func() { defer conn.Close(); _ = conn.SetDeadline(time.Now().Add(5 * time.Second)); handle(conn) })
		}
	}()
	t.Cleanup(func() { _ = lis.Close(); <-done; wg.Wait(); _ = os.RemoveAll(dir) })
	return socket, &accepted
}

func socketCall() api.ToolCall {
	return api.ToolCall{ID: "call-1", Tool: "publish", Args: map[string]any{"text": "hello\n世界", "nested": map[string]any{"n": float64(42), "list": []any{true, nil, "x"}}}, Mediation: api.MediationControllerMediated, IdempotencyKey: "same-key"}
}

func configExecutor(t *testing.T, socket string, declaration string, harness string, logs io.Writer) controller.ToolFunc {
	t.Helper()
	data := strings.Replace(validToolConfig, "/run/tool.sock", socket, 1)
	data = strings.Replace(data, `"durable_dedup":true`, `"durable_dedup":`+declaration, 1)
	c, err := loadToolExecutorConfig(writeToolConfig(t, data), remoteHarnesses{"h": "127.0.0.1:9", "other": "127.0.0.1:10"})
	if err != nil {
		t.Fatal(err)
	}
	factory := c.factory(slog.New(slog.NewJSONHandler(logs, nil)))
	fn, err := factory(harness, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fn
}

func TestSocketExecutorForwardsIdentityAndResult(t *testing.T) {
	captured := make(chan map[string]any, 1)
	socket, calls := unixToolPeer(t, func(conn net.Conn) {
		data, err := bufio.NewReader(conn).ReadBytes('\n')
		if err != nil {
			return
		}
		var request map[string]any
		if err := json.Unmarshal(data, &request); err != nil {
			return
		}
		captured <- request
		_, _ = io.WriteString(conn, `{"version":1,"call_id":"call-1","is_error":false,"output":{"text":"published","n":42}}`+"\n")
	})
	var logs bytes.Buffer
	fn := configExecutor(t, socket, "true", "h", &logs)
	call := socketCall()
	result, err := fn(t.Context(), controller.ToolCallContext{SessionUID: "session-1"}, call)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"version": float64(1), "harness": "h", "session_uid": "session-1", "call_id": "call-1", "tool": "publish", "arguments": call.Args, "idempotency_key": "same-key"}
	if got := <-captured; !reflect.DeepEqual(got, want) {
		t.Fatalf("request = %#v, want %#v", got, want)
	}
	if result.IsError || result.Output["text"] != "published" || result.Output["n"] != float64(42) {
		t.Fatalf("result = %#v", result)
	}
	if calls.Load() != 1 {
		t.Fatalf("connections = %d", calls.Load())
	}
	if !strings.Contains(logs.String(), `"durable_dedup":"true"`) || !strings.Contains(logs.String(), `"level":"INFO"`) || strings.Contains(logs.String(), `"level":"WARN"`) || strings.Contains(logs.String(), "same-key") {
		t.Fatalf("operator declarations = %s", logs.String())
	}
}

func TestConfiguredRoutesRefuseWithoutDialing(t *testing.T) {
	socket, calls := unixToolPeer(t, func(conn net.Conn) { _, _ = io.Copy(io.Discard, conn) })
	for _, declaration := range []string{"false", "null", "true"} {
		var logs bytes.Buffer
		fn := configExecutor(t, socket, declaration, "h", &logs)
		call := socketCall()
		if declaration == "true" {
			call.Tool = "Publish"
		}
		_, err := fn(t.Context(), controller.ToolCallContext{SessionUID: "session"}, call)
		if err == nil {
			t.Fatalf("%s route must refuse", declaration)
		}
		want := declaration
		if declaration == "null" {
			want = "unknown"
		}
		if !strings.Contains(logs.String(), `"durable_dedup":"`+want+`"`) {
			t.Fatalf("missing declaration %s: %s", want, logs.String())
		}
		if declaration != "true" {
			if !strings.Contains(logs.String(), `"level":"WARN"`) {
				t.Errorf("non-dispatchable route must warn: %s", logs.String())
			}
			if !strings.Contains(err.Error(), "requires durable_dedup true") || strings.Contains(err.Error(), "#139") {
				t.Errorf("refusal must state requirement without issue reference: %v", err)
			}
		}
	}
	if fn := configExecutor(t, socket, "true", "other", io.Discard); fn != nil {
		t.Fatal("unconfigured harness borrowed executor")
	}
	if calls.Load() != 0 {
		t.Fatalf("refusal dialed %d times", calls.Load())
	}
}

func TestSocketExecutorValidatesReplies(t *testing.T) {
	good := `{"version":1,"call_id":"call-1","is_error":false}`
	cases := map[string]string{
		"success": good + "\n", "null output": strings.TrimSuffix(good, "}") + `,"output":null}` + "\n",
		"domain error": `{"version":1,"call_id":"call-1","is_error":true,"error":"declined"}` + "\n",
		"malformed":    "{\n", "truncated": good, "missing version": `{"call_id":"call-1","is_error":false}` + "\n",
		"version":          strings.Replace(good, `"version":1`, `"version":2`, 1) + "\n",
		"null version":     strings.Replace(good, `"version":1`, `"version":null`, 1) + "\n",
		"correlation":      strings.Replace(good, "call-1", "other", 1) + "\n",
		"missing status":   `{"version":1,"call_id":"call-1"}` + "\n",
		"null status":      strings.Replace(good, "false", "null", 1) + "\n",
		"string status":    strings.Replace(good, "false", `"false"`, 1) + "\n",
		"duplicate status": strings.Replace(good, `"is_error":false`, `"is_error":true,"is_error":false`, 1) + "\n",
		"case status":      strings.Replace(good, "is_error", "Is_Error", 1) + "\n",
		"unknown":          strings.TrimSuffix(good, "}") + `,"Approval":{}}` + "\n",
		"output scalar":    strings.TrimSuffix(good, "}") + `,"output":"bad"}` + "\n",
		"output duplicate": strings.TrimSuffix(good, "}") + `,"output":{"a":1,"a":2}}` + "\n",
		"utf8":             strings.TrimSuffix(good, "}") + `,"error":"` + string([]byte{0xff}) + `"}` + "\n",
		"oversize":         strings.Repeat(" ", 1<<20) + good + "\n",
		"trailing value":   good + ` {}` + "\n",
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			socket, calls := unixToolPeer(t, func(conn net.Conn) { _, _ = bufio.NewReader(conn).ReadBytes('\n'); _, _ = io.WriteString(conn, reply) })
			fn := configExecutor(t, socket, "true", "h", io.Discard)
			result, err := fn(t.Context(), controller.ToolCallContext{SessionUID: "s"}, socketCall())
			valid := name == "success" || name == "null output" || name == "domain error"
			if valid && err != nil || !valid && err == nil {
				t.Fatalf("result=%#v err=%v", result, err)
			}
			if name == "oversize" && !strings.Contains(err.Error(), "read: frame exceeds 1 MiB") {
				t.Fatalf("oversized reply: %v", err)
			}
			if name == "domain error" && (!result.IsError || result.Error != "declined") {
				t.Fatalf("domain error = %#v", result)
			}
			if calls.Load() != 1 {
				t.Fatalf("connections = %d, want 1 (no retries)", calls.Load())
			}
		})
	}
}

func TestSocketExecutorDeadlinesAndCancellation(t *testing.T) {
	for _, stage := range []string{"read", "write"} {
		for _, mode := range []string{"timeout", "deadline", "cancel"} {
			t.Run(stage+"/"+mode, func(t *testing.T) {
				arrived := make(chan struct{}, 1)
				socket, _ := unixToolPeer(t, func(conn net.Conn) {
					if stage == "read" {
						_, _ = bufio.NewReader(conn).ReadBytes('\n')
					}
					arrived <- struct{}{}
					if stage == "read" {
						_, _ = io.Copy(io.Discard, conn)
					} else {
						<-t.Context().Done()
					}
				})
				// Keep the write larger than the Unix send buffer so a non-reading peer really stalls.
				e := socketExecutorConfig{Socket: socket}
				timeout := 100
				if mode == "cancel" {
					// Cancellation must win even when race-instrumented marshaling is slow.
					timeout = 10000
				}
				e.TimeoutMS = &timeout
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if mode == "deadline" {
					var stop context.CancelFunc
					ctx, stop = context.WithTimeout(ctx, 30*time.Millisecond)
					defer stop()
				}
				if mode == "cancel" {
					go func() { <-arrived; cancel() }()
				}
				call := socketCall()
				if stage == "write" {
					call.Args = map[string]any{"text": strings.Repeat("x", 900000)}
				}
				start := time.Now()
				_, err := e.execute(ctx, "h", controller.ToolCallContext{SessionUID: "s"}, call)
				want := context.DeadlineExceeded
				if mode == "cancel" {
					want = context.Canceled
				}
				if !errors.Is(err, want) {
					t.Fatalf("want %v, got %v", want, err)
				}
				if time.Since(start) > time.Second {
					t.Fatalf("cancellation/timeout took %v", time.Since(start))
				}
			})
		}
	}
}

func TestSocketExecutorRequestAndReplySizeBoundaries(t *testing.T) {
	// Valid JSON padded with whitespace to the exact frame limit is allowed.
	good := `{"version":1,"call_id":"call-1","is_error":false}`
	reply := good + strings.Repeat(" ", (1<<20)-len(good)-1) + "\n"
	socket, calls := unixToolPeer(t, func(conn net.Conn) { _, _ = bufio.NewReader(conn).ReadBytes('\n'); _, _ = io.WriteString(conn, reply) })
	fn := configExecutor(t, socket, "true", "h", io.Discard)
	call := socketCall()
	if _, err := fn(t.Context(), controller.ToolCallContext{SessionUID: "s"}, call); err != nil {
		t.Fatalf("exact reply limit: %v", err)
	}
	call.Args = map[string]any{"text": strings.Repeat("x", 1<<20)}
	if _, err := fn(t.Context(), controller.ToolCallContext{SessionUID: "s"}, call); err == nil {
		t.Fatal("oversized request accepted")
	}
	if calls.Load() != 1 {
		t.Fatalf("oversized request dialed: %d", calls.Load())
	}
}

// Protocol failures must not smuggle peer-controlled values into journaled Go errors.
func TestSocketExecutorProtocolErrorsDoNotExposePayload(t *testing.T) {
	socket, _ := unixToolPeer(t, func(conn net.Conn) {
		_, _ = bufio.NewReader(conn).ReadBytes('\n')
		_, _ = io.WriteString(conn, `{"version":1,"call_id":"call-1","is_error":false,"output":{"secret":1e999999}}`+"\n")
	})
	fn := configExecutor(t, socket, "true", "h", io.Discard)
	_, err := fn(t.Context(), controller.ToolCallContext{SessionUID: "s"}, socketCall())
	if err == nil || strings.Contains(err.Error(), "1e999999") {
		t.Fatalf("protocol error exposed peer payload: %v", err)
	}
}

func TestSocketExecutorDownAndAlreadyCanceled(t *testing.T) {
	// t.TempDir includes the test name and can exceed macOS's Unix socket path limit.
	dir, err := os.MkdirTemp("", "sessions-tool-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	e := socketExecutorConfig{Socket: filepath.Join(dir, "absent.sock")}
	if _, err := e.execute(t.Context(), "h", controller.ToolCallContext{SessionUID: "s"}, socketCall()); !errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), e.Socket) {
		t.Fatalf("down socket must preserve cause without path: %v", err)
	}
	for _, stage := range []string{"write", "read"} {
		t.Run(stage, func(t *testing.T) {
			socket, _ := unixToolPeer(t, func(conn net.Conn) {
				if stage == "read" {
					// Leave request bytes unread: Linux resets the peer's read; Darwin returns EOF.
					_, _ = io.ReadFull(conn, make([]byte, 1))
				}
			})
			call := socketCall()
			if stage == "write" {
				// Exceed the socket buffer so a peer closing interrupts the write.
				call.Args = map[string]any{"text": strings.Repeat("x", toolMessageLimit-1024)}
			}
			e := socketExecutorConfig{Socket: socket}
			_, err := e.execute(t.Context(), "h", controller.ToolCallContext{SessionUID: "s"}, call)
			if err == nil || !strings.Contains(err.Error(), " "+stage+": ") || strings.Contains(err.Error(), socket) {
				t.Fatalf("%s error must preserve stage without path: %v", stage, err)
			}
			// Write failures and Linux read resets have syscall causes. Other systems
			// may return plain EOF on read; the path/address checks still apply there.
			var syscallErr *os.SyscallError
			if !errors.As(err, &syscallErr) && (stage != "read" || runtime.GOOS == "linux" || !errors.Is(err, io.EOF)) {
				t.Fatalf("%s error must preserve its syscall cause (or EOF on non-Linux reads): %v", stage, err)
			}
			var op *net.OpError
			if errors.As(err, &op) {
				t.Fatalf("%s error still wraps socket addresses: %v", stage, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := e.execute(ctx, "h", controller.ToolCallContext{SessionUID: "s"}, socketCall()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled = %v", err)
	}
}
