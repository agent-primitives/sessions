package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agent-primitives/sessions/api"
)

func responseEvent(kind, fields string) string {
	return "event: " + kind + "\r\ndata: {\"type\":\"" + kind + "\"" + fields + "}\r\n\r\n"
}

const streamReasoning = `{"id":"rs","type":"reasoning","summary":[{"type":"summary_text","text":"brief"}],"encrypted_content":"cipher==","status":"completed"}`
const streamCallOne = `{"id":"fc1","type":"function_call","call_id":"call1","name":"first","arguments":"{\"x\":1}","status":"completed"}`
const streamCallTwo = `{"id":"fc2","type":"function_call","call_id":"call2","name":"second","arguments":"{\"y\":2}","status":"completed"}`
const streamMessage = `{"id":"msg","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello","annotations":[]},{"type":"output_text","text":"!","annotations":[]}]}`
const streamFinal = `{"id":"resp","object":"response","status":"completed","output":[` + streamReasoning + `,` + streamCallOne + `,` + streamCallTwo + `,` + streamMessage + `],"usage":{"input_tokens":19,"output_tokens":23,"output_tokens_details":{"reasoning_tokens":7}}}`

func responsesStreamFixture() string {
	return ": comment\r\n\r\n" +
		responseEvent("response.created", `,"response":{"id":"resp","status":"in_progress","output":[]}`) +
		responseEvent("response.in_progress", `,"response":{"id":"resp","status":"in_progress"}`) +
		responseEvent("response.output_item.added", `,"output_index":0,"item":{"id":"rs","type":"reasoning","summary":[],"encrypted_content":null}`) +
		responseEvent("response.reasoning_summary_part.added", `,"output_index":0,"item_id":"rs","summary_index":0,"part":{"type":"summary_text","text":""}`) +
		responseEvent("response.reasoning_summary_text.delta", `,"output_index":0,"item_id":"rs","summary_index":0,"delta":"brief"`) +
		responseEvent("response.reasoning_summary_text.done", `,"output_index":0,"item_id":"rs","summary_index":0,"text":"brief"`) +
		responseEvent("response.reasoning_summary_part.done", `,"output_index":0,"item_id":"rs","summary_index":0,"part":{"type":"summary_text","text":"brief"}`) +
		responseEvent("response.output_item.done", `,"output_index":0,"item":`+streamReasoning) +
		responseEvent("response.output_item.added", `,"output_index":1,"item":{"id":"fc1","type":"function_call","call_id":"call1","name":"first","arguments":"","status":"in_progress"}`) +
		responseEvent("response.output_item.added", `,"output_index":2,"item":{"id":"fc2","type":"function_call","call_id":"call2","name":"second","arguments":"","status":"in_progress"}`) +
		responseEvent("response.function_call_arguments.delta", `,"output_index":1,"item_id":"fc1","delta":"{\"x\":"`) +
		responseEvent("response.function_call_arguments.delta", `,"output_index":2,"item_id":"fc2","delta":"{\"y\":"`) +
		responseEvent("response.function_call_arguments.delta", `,"output_index":1,"item_id":"fc1","delta":"1}"`) +
		responseEvent("response.function_call_arguments.delta", `,"output_index":2,"item_id":"fc2","delta":"2}"`) +
		responseEvent("response.function_call_arguments.done", `,"output_index":2,"item_id":"fc2","arguments":"{\"y\":2}"`) +
		responseEvent("response.function_call_arguments.done", `,"output_index":1,"item_id":"fc1","arguments":"{\"x\":1}"`) +
		responseEvent("response.output_item.done", `,"output_index":2,"item":`+streamCallTwo) +
		responseEvent("response.output_item.done", `,"output_index":1,"item":`+streamCallOne) +
		responseEvent("response.output_item.added", `,"output_index":3,"item":{"id":"msg","type":"message","role":"assistant","status":"in_progress","content":[]}`) +
		responseEvent("response.content_part.added", `,"output_index":3,"item_id":"msg","content_index":0,"part":{"type":"output_text","text":"","annotations":[]}`) +
		responseEvent("response.output_text.delta", `,"output_index":3,"item_id":"msg","content_index":0,"delta":"he"`) +
		responseEvent("response.output_text.delta", `,"output_index":3,"item_id":"msg","content_index":0,"delta":"llo"`) +
		responseEvent("response.output_text.done", `,"output_index":3,"item_id":"msg","content_index":0,"text":"hello"`) +
		responseEvent("response.content_part.done", `,"output_index":3,"item_id":"msg","content_index":0,"part":{"type":"output_text","text":"hello","annotations":[]}`) +
		responseEvent("response.output_text.delta", `,"output_index":3,"item_id":"msg","content_index":1,"delta":"!"`) +
		responseEvent("response.output_text.done", `,"output_index":3,"item_id":"msg","content_index":1,"text":"!"`) +
		responseEvent("response.output_item.done", `,"output_index":3,"item":`+streamMessage) +
		responseEvent("response.completed", `,"response":`+streamFinal)
}

// Catches using deltas as authoritative output, conflating tool streams, exposing
// encrypted/argument fragments, and parsing lines instead of SSE records.
func TestResponsesStreamEqualsNonstream(t *testing.T) {
	for _, fragment := range []int{1, 7, 4096} {
		t.Run(fmt.Sprint(fragment), func(t *testing.T) {
			c := newResponses(t, func(w http.ResponseWriter, r *http.Request) {
				var req map[string]any
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if req["stream"] != true {
					_, _ = io.WriteString(w, streamFinal)
					return
				}
				if _, ok := req["stream_options"]; ok {
					t.Error("Chat stream_options sent")
				}
				data := responsesStreamFixture()
				// A JSON record can span multiple SSE data lines; comments don't dispatch it.
				data = strings.Replace(data, `data: {"type":"response.completed","response":`, "data: {\"type\":\"response.completed\",\n: mid-record comment\ndata: \"response\":", 1)
				w.Header().Set("Content-Type", "text/event-stream")
				for len(data) > 0 {
					n := min(fragment, len(data))
					_, _ = io.WriteString(w, data[:n])
					w.(http.Flusher).Flush()
					data = data[n:]
				}
			})
			req := api.ModelRequest{Tools: []api.ToolDefinition{{Name: "first"}, {Name: "second"}}}
			want, err := c.Model(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			var chunks []string
			got, err := c.StreamModel(t.Context(), req, func(s string) { chunks = append(chunks, s) })
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("stream=%+v\nnonstream=%+v", got, want)
			}
			if !reflect.DeepEqual(chunks, []string{"he", "llo", "!"}) {
				t.Fatalf("chunks=%q", chunks)
			}
			if string(got.Message.Parts[0].Reasoning.Opaque) != streamReasoning {
				t.Fatal("lost completed reasoning bytes")
			}
		})
	}
}

func TestResponsesStreamFailures(t *testing.T) {
	prefix := responseEvent("response.output_item.added", `,"output_index":0,"item":{"id":"m","type":"message","role":"assistant","status":"in_progress","content":[]}`)
	delta := responseEvent("response.output_text.delta", `,"output_index":0,"item_id":"m","content_index":0,"delta":"hello"`)
	final := responseEvent("response.completed", `,"response":{"status":"completed","output":[{"id":"m","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello","annotations":[]}]}]}`)
	cases := map[string]string{
		"EOF": "", "DONE only": "data: [DONE]\n\n", "truncated": prefix + delta,
		"provider error":     responseEvent("error", `,"code":"bad","message":"provider failed"`),
		"failed":             responseEvent("response.failed", `,"response":{"status":"failed","error":{"message":"failed"}}`),
		"incomplete":         responseEvent("response.incomplete", `,"response":{"status":"incomplete"}`),
		"wrong final status": responseEvent("response.completed", `,"response":{"status":"incomplete","output":[]}`),
		"missing final":      responseEvent("response.completed", ``),
		"malformed JSON":     "data: {broken}\n\n", "event mismatch": "event: response.completed\ndata: {\"type\":\"response.created\"}\n\n", "unknown": "data: {\"type\":\"response.new_semantic_event\"}\n\n",
		"text conflict":           prefix + delta + strings.Replace(final, `"text":"hello"`, `"text":"different"`, 1),
		"wrong item id":           prefix + strings.Replace(delta, `"item_id":"m"`, `"item_id":"other"`, 1) + final,
		"missing index":           prefix + strings.Replace(delta, `,"content_index":0`, "", 1) + final,
		"negative index":          prefix + strings.Replace(delta, `"content_index":0`, `"content_index":-1`, 1) + final,
		"unknown index":           prefix + strings.Replace(delta, `"output_index":0`, `"output_index":1`, 1) + final,
		"delta missing":           prefix + strings.Replace(delta, `,"delta":"hello"`, "", 1) + final,
		"delta after text done":   prefix + delta + responseEvent("response.output_text.done", `,"output_index":0,"item_id":"m","content_index":0,"text":"hello"`) + delta + final,
		"done text conflict":      prefix + delta + responseEvent("response.output_text.done", `,"output_index":0,"item_id":"m","content_index":0,"text":"no"`) + final,
		"duplicate item":          prefix + prefix + final,
		"item index beyond final": prefix + responseEvent("response.completed", `,"response":{"status":"completed","output":[]}`),
		"item final identity":     prefix + strings.Replace(final, `"id":"m"`, `"id":"changed"`, 1),
		"item done conflict":      prefix + responseEvent("response.output_item.done", `,"output_index":0,"item":{"id":"m","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"wrong","annotations":[]}]}`) + final,
		"refusal event":           prefix + responseEvent("response.refusal.delta", `,"output_index":0,"item_id":"m","content_index":0,"delta":"no"`) + final,
		"unsupported content":     prefix + responseEvent("response.content_part.added", `,"output_index":0,"item_id":"m","content_index":0,"part":{"type":"refusal","refusal":"no"}`) + final,
		"response id changed":     responseEvent("response.created", `,"response":{"id":"original"}`) + strings.Replace(final, `"status":"completed","output"`, `"id":"changed","status":"completed","output"`, 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c := newResponses(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })
			got, err := c.StreamModel(t.Context(), api.ModelRequest{}, func(string) {})
			if err == nil || !reflect.DeepEqual(got, api.ModelResponse{}) {
				t.Fatalf("response=%+v err=%v", got, err)
			}
		})
	}
	// Mutate independent tool/summary streams; no full completion may be returned.
	for name, oldNew := range map[string][2]string{
		"arguments delta":           {"\"delta\":\"1}\"", "\"delta\":\"3}\""},
		"arguments done":            {"\"arguments\":\"{\\\"y\\\":2}\"", "\"arguments\":\"{\\\"y\\\":9}\""},
		"summary delta":             {"\"delta\":\"brief\"", "\"delta\":\"different\""},
		"call identity":             {"\"call_id\":\"call1\",\"name\":\"first\",\"arguments\":\"\"", "\"call_id\":\"other\",\"name\":\"first\",\"arguments\":\"\""},
		"added unsupported content": {`"role":"assistant","status":"in_progress","content":[]`, `"role":"assistant","status":"in_progress","content":[],"refusal":"lost"`},
		"added summary conflict":    {`"summary":[],"encrypted_content":null`, `"summary":[{"type":"summary_text","text":"wrong"}],"encrypted_content":null`},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := responsesStreamFixture()
			changed := strings.Replace(fixture, oldNew[0], oldNew[1], 1)
			if changed == fixture {
				t.Fatal("test fixture mutation did not match")
			}
			c := newResponses(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, changed) })
			if _, err := c.StreamModel(t.Context(), api.ModelRequest{}, nil); err == nil {
				t.Fatal("conflicting stream accepted")
			}
		})
	}
}

func TestResponsesStreamTerminalDetails(t *testing.T) {
	for _, tc := range []struct{ event, fields, want string }{
		{"response.failed", `,"response":{"status":"failed","error":{"message":"deployment unavailable"}}`, "deployment unavailable"},
		{"response.incomplete", `,"response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}`, "max_output_tokens"},
	} {
		t.Run(tc.event, func(t *testing.T) {
			c := newResponses(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, responseEvent(tc.event, tc.fields))
			})
			_, err := c.StreamModel(t.Context(), api.ModelRequest{}, nil)
			if err == nil || !strings.Contains(err.Error(), "terminal "+tc.event) || strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want terminal category without provider text", err)
			}
		})
	}
}

func TestResponsesStreamFinalOnlyAndCancellation(t *testing.T) {
	t.Run("final only, no usage", func(t *testing.T) {
		c := newResponses(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, responseEvent("response.completed", `,"response":{"status":"completed","output":[]}`))
		})
		got, err := c.StreamModel(t.Context(), api.ModelRequest{Model: "explicit"}, nil)
		if err != nil || got.Usage != (api.Usage{Model: "explicit"}) {
			t.Fatalf("response=%+v err=%v", got, err)
		}
	})
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			arrived := make(chan struct{})
			release := make(chan struct{})
			c := newResponses(t, func(w http.ResponseWriter, r *http.Request) {
				defer close(release)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				if stream {
					_, _ = io.WriteString(w, ": waiting\n\n")
					w.(http.Flusher).Flush()
				}
				close(arrived)
				<-r.Context().Done()
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var err error
				if stream {
					_, err = c.StreamModel(ctx, api.ModelRequest{}, nil)
				} else {
					_, err = c.Model(ctx, api.ModelRequest{})
				}
				done <- err
			}()
			<-arrived
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("err=%v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("cancellation did not unblock")
			}
			<-release
		})
	}
}
