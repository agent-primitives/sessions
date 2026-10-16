package agentsessionsd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/controller"
)

type socketToolRequest struct {
	Version        int            `json:"version"`
	Harness        string         `json:"harness"`
	SessionUID     string         `json:"session_uid"`
	CallID         string         `json:"call_id"`
	Tool           string         `json:"tool"`
	Arguments      map[string]any `json:"arguments"`
	IdempotencyKey string         `json:"idempotency_key"`
}

type socketToolResponse struct {
	Version int            `json:"version"`
	CallID  string         `json:"call_id"`
	Output  map[string]any `json:"output"`
	IsError *bool          `json:"is_error"`
	Error   string         `json:"error"`
}

func (e socketExecutorConfig) execute(parent context.Context, harness string, scope controller.ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
	timeout := 30 * time.Second
	if e.TimeoutMS != nil {
		timeout = time.Duration(*e.TimeoutMS) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	fail := func(stage string, err error) (api.ToolResult, error) {
		// A socket deadline can fire just before the context's timer is observed.
		if ctx.Err() != nil {
			err = ctx.Err()
		} else if ne := net.Error(nil); errors.As(err, &ne) && ne.Timeout() {
			err = context.DeadlineExceeded
		}
		return api.ToolResult{}, fmt.Errorf("tool executor harness %q tool %q %s: %w", harness, call.Tool, stage, err)
	}
	if err := ctx.Err(); err != nil {
		return fail("request", err)
	}
	frame, err := json.Marshal(socketToolRequest{Version: 1, Harness: harness, SessionUID: scope.SessionUID, CallID: call.ID, Tool: call.Tool, Arguments: call.Args, IdempotencyKey: call.IdempotencyKey})
	if err != nil {
		return fail("request", errors.New("arguments are not JSON-shaped"))
	}
	frame = append(frame, '\n')
	if len(frame) > toolMessageLimit {
		return fail("request", errors.New("frame exceeds 1 MiB"))
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", e.Socket)
	if err != nil {
		return fail("dial", err)
	}
	defer conn.Close()
	// Closing on cancellation also interrupts a blocked write, not just the next read.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return fail("deadline", err)
	}
	if _, err := io.Copy(conn, bytes.NewReader(frame)); err != nil {
		return fail("write", err)
	}
	reply, err := bufio.NewReader(io.LimitReader(conn, toolMessageLimit+1)).ReadBytes('\n')
	if err != nil {
		return fail("read", err)
	}
	if len(reply) > toolMessageLimit {
		return fail("read", errors.New("frame exceeds 1 MiB"))
	}
	var response socketToolResponse
	if err := strictJSON(reply, &response); err != nil {
		return fail("response", err)
	}
	if response.Version != 1 || response.CallID != call.ID || response.IsError == nil {
		return fail("response", errors.New("requires version 1, matching call_id and boolean is_error"))
	}
	return api.ToolResult{Output: response.Output, IsError: *response.IsError, Error: response.Error}, nil
}
