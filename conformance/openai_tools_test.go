package conformance_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/model/openai"
	"github.com/agent-primitives/sessions/sqlitelog"
)

type openaiToolReport struct {
	requests  []api.ModelRequest
	responses []api.ModelResponse
}
type openaiToolHarness struct{ reports chan openaiToolReport }

func (openaiToolHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "openai-tools", Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}}, nil
}
func (h openaiToolHarness) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	var report openaiToolReport
	defer func() { h.reports <- report }() // Run executes on the remote server's goroutine.
	req := api.ModelRequest{Model: "test-model", Messages: start.Inputs,
		Tools: []api.ToolDefinition{{Name: "lookup", Description: "Look up a city", InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []any{"city"},
		}}}, ToolChoice: &api.ToolChoice{Mode: api.ToolChoiceRequired, Name: "lookup"}}
	for turn := 0; turn < 2; turn++ {
		report.requests = append(report.requests, req)
		response, err := sink.Model(ctx, req)
		if err != nil {
			return err
		}
		report.responses = append(report.responses, response)
		if err := sink.Usage(ctx, response.Usage); err != nil {
			return err
		}
		if turn == 0 {
			req.Messages = append(append([]api.Message(nil), req.Messages...), response.Message)
			for _, part := range response.Message.Parts {
				if part.ToolCall == nil {
					continue
				}
				call := *part.ToolCall // Never mutate the model's original assistant message.
				call.Mediation, call.IdempotencyKey = api.MediationControllerMediated, "lookup-key"
				result, err := sink.ToolCall(ctx, call)
				if err != nil {
					return err
				}
				req.Messages = append(req.Messages, api.Message{Role: "tool", Parts: []api.Part{{ToolResult: &result}}})
			}
			req.ToolChoice = &api.ToolChoice{Mode: api.ToolChoiceNone}
		}
	}
	return nil
}

// A rejected provider completion must not become a journaled output or a host effect.
// Exercise Harness.Connect too: adapter errors are copied into durable ERROR descriptions.
func assertOpenAICompletionRefused(t *testing.T, streaming bool, body, private string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, body)
	}))
	defer server.Close()
	client, err := openai.New(openai.WithBaseURL(server.URL), openai.WithModel("test-model"), openai.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	var toolCalls atomic.Int64
	opts := []controller.Option{controller.WithSessionUID("refused"), controller.WithToolExecutor(func(_ context.Context, _ controller.ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
		toolCalls.Add(1)
		return api.ToolResult{ID: call.ID}, nil
	})}
	if streaming {
		opts = append(opts, controller.WithStreamingModel(client.StreamModel))
	}
	h := openaiToolHarness{reports: make(chan openaiToolReport, 1)}
	har := wireHarnessFrom(t, h)
	store, _ := openFile(t)
	defer store.Close()
	journal := store.Session("refused")
	c, err := controller.New(journal, client.Model, opts...)
	if err != nil {
		t.Fatal(err)
	}
	err = c.Exec(t.Context(), har, []api.Message{*api.TextMessage("user", "weather?")}, 0)
	if err == nil || !strings.Contains(err.Error(), openai.ErrUnsupportedPart.Error()) {
		t.Errorf("err=%v, want refused completion", err)
	}
	if private != "" && err != nil && strings.Contains(err.Error(), private) {
		t.Errorf("argument payload leaked into returned error: %v", err)
	}
	if report := <-h.reports; len(report.responses) != 0 {
		t.Errorf("harness received rejected completion: %+v", report.responses)
	}
	if toolCalls.Load() != 0 {
		t.Errorf("executor called %d times for rejected completion", toolCalls.Load())
	}
	records, err := journal.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	var errorsRecorded int
	for _, record := range records {
		switch record.Event.Kind {
		case api.EventOutput, api.EventToolCall, api.EventToolResult:
			t.Errorf("rejected completion produced %s", record.Event.Kind)
		case api.EventError:
			errorsRecorded++
			if record.Event.Err == nil {
				t.Error("ERROR has no description")
			} else if private != "" && strings.Contains(record.Event.Err.Description, private) {
				t.Errorf("argument payload leaked into journal ERROR: %s", record.Event.Err.Description)
			}
		}
	}
	if errorsRecorded == 0 {
		t.Error("missing durable ERROR for refused completion")
	}
	if err := journal.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenAIToolArgumentErrorPrivacy(t *testing.T) {
	const private = "987654321098765432109876543210e10000"
	const message = `{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"private\":987654321098765432109876543210e10000}"}}]}`
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
			body := fmt.Sprintf(`{"choices":[{"message":%s,"finish_reason":"tool_calls"}]}`, message)
			if streaming {
				body = fmt.Sprintf("data: {\"choices\":[{\"delta\":%s,\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", message)
			}
			assertOpenAICompletionRefused(t, streaming, body, private)
		})
	}
}

func TestOpenAIRefusesToolCallsCompletedAfterFinish(t *testing.T) {
	const finish = "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n"
	for _, tc := range []struct{ name, body string }{
		{"entire call after finish", finish + `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}}]}` + "\n\n"},
		{"argument suffix after finish", `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"n\":"}}]}}]}` + "\n\n" + finish + `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}` + "\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertOpenAICompletionRefused(t, true, tc.body+"data: [DONE]\n\n", "")
		})
	}
}

// Exercise the real HTTP adapter through Harness.Connect, durable recovery, and replay.
func TestOpenAIToolsWireRecoveryAndReplay(t *testing.T) {
	var finalized [][]api.ModelResponse
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
			check := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			equal := func(got, want any) {
				t.Helper()
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("got %#v, want %#v", got, want)
				}
			}
			var providerCalls, toolCalls atomic.Int64
			bodies := make(chan map[string]any, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := providerCalls.Add(1)
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if n <= 2 {
					bodies <- body
				}
				message, finish := `{"role":"assistant","content":"Looking up","tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"city\":\"Paris\"}"}}]}`, "tool_calls"
				if n == 2 {
					message, finish = `{"role":"assistant","content":"Paris is sunny"}`, "stop"
				}
				usage := `{"prompt_tokens":7,"completion_tokens":3,"completion_tokens_details":{"reasoning_tokens":1}}`
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":%s}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":%q}]}\n\ndata: {\"choices\":[],\"usage\":%s}\n\ndata: [DONE]\n\n", message, finish, usage)
				} else {
					fmt.Fprintf(w, `{"choices":[{"message":%s,"finish_reason":%q}],"usage":%s}`, message, finish, usage)
				}
			}))
			defer server.Close()
			client, err := openai.New(openai.WithBaseURL(server.URL), openai.WithModel("test-model"), openai.WithHTTPClient(server.Client()))
			check(err)
			executor := func(_ context.Context, _ controller.ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
				toolCalls.Add(1)
				if call.ID != "call-1" || call.Tool != "lookup" || call.Args["city"] != "Paris" || call.IdempotencyKey != "lookup-key" || call.Mediation != api.MediationControllerMediated {
					t.Errorf("host tool call = %#v", call)
				}
				return api.ToolResult{ID: call.ID, Content: []api.Part{{Text: &api.TextPart{Text: "sunny"}}}, Output: map[string]any{"receipt": "private-metadata"}}, nil
			}
			var chunks strings.Builder
			opts := []controller.Option{controller.WithSessionUID("tools"), controller.WithToolExecutor(executor), controller.WithObserver(controller.Observer{OnDelta: func(d api.Delta) { chunks.WriteString(d.Chunk) }})}
			if streaming {
				opts = append(opts, controller.WithStreamingModel(client.StreamModel))
			}
			h := openaiToolHarness{reports: make(chan openaiToolReport, 1)}
			har := wireHarnessFrom(t, h)
			store, path := openFile(t)
			defer func() { store.Close() }()
			journal := store.Session("tools")
			c, err := controller.New(journal, client.Model, opts...)
			check(err)
			check(c.Exec(t.Context(), har, []api.Message{*api.TextMessage("user", "weather?")}, 0))
			live := <-h.reports
			want := []api.ModelResponse{
				{Message: api.Message{Role: "assistant", Parts: []api.Part{{Text: &api.TextPart{Text: "Looking up"}}, {ToolCall: &api.ToolCall{ID: "call-1", Tool: "lookup", Args: map[string]any{"city": "Paris"}}}}}, Usage: api.Usage{Model: "test-model", InputTokens: 7, OutputTokens: 3, ReasoningTokens: 1}},
				{Message: *api.TextMessage("assistant", "Paris is sunny"), Usage: api.Usage{Model: "test-model", InputTokens: 7, OutputTokens: 3, ReasoningTokens: 1}},
			}
			equal(live.responses, want)
			for i := 0; i < 2; i++ {
				body := <-bodies
				var expected map[string]any
				check(json.Unmarshal([]byte(`{"model":"test-model","messages":[{"role":"user","content":"weather?"}],"tools":[{"type":"function","function":{"name":"lookup","description":"Look up a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],"tool_choice":{"type":"function","function":{"name":"lookup"}}}`), &expected))
				if i == 1 {
					expected["tool_choice"] = "none"
					check(json.Unmarshal([]byte(`{"messages":[{"role":"user","content":"weather?"},{"role":"assistant","content":"Looking up","tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"city\":\"Paris\"}"}}]},{"role":"tool","tool_call_id":"call-1","content":"sunny"}]}`), &expected))
				}
				if streaming {
					expected["stream"], expected["stream_options"] = true, map[string]any{"include_usage": true}
				}
				equal(body, expected)
			}
			if streaming && chunks.String() != "Looking upParis is sunny" || !streaming && chunks.Len() != 0 {
				t.Fatalf("text-only chunks = %q", chunks.String())
			}
			records, err := journal.Read(1)
			check(err)
			modelIndex, outputIndex, usageIndex, endIndex := 0, 0, 0, -1
			for i, record := range records {
				switch event := record.Event; event.Kind {
				case api.EventModelCall:
					raw, err := json.Marshal(live.requests[modelIndex])
					check(err)
					equal(event.ModelCall.InputHash, fmt.Sprintf("%x", sha256.Sum256(raw)))
					modelIndex++
				case api.EventOutput:
					equal(event.Message, &want[outputIndex].Message)
					outputIndex++
				case api.EventUsage:
					equal(event.Usage, &want[usageIndex].Usage)
					usageIndex++
				case api.EventEnd:
					endIndex = i
				}
			}
			if modelIndex != 2 || outputIndex != 2 || usageIndex != 2 || endIndex != len(records)-1 {
				t.Fatal("incomplete live journal")
			}
			check(store.Close())
			truncateFrom(t, path, "tools", records[endIndex].Seq) // Lose only END, not a completed-resume no-op.
			reopened, err := sqlitelog.Open(path)
			check(err)
			store = reopened
			journal = store.Session("tools")
			for _, replay := range []bool{false, true} {
				fresh, err := controller.New(journal, client.Model, opts...)
				check(err)
				if replay {
					_, err = fresh.Replay(t.Context(), har)
				} else {
					var redrove bool
					redrove, err = fresh.Resume(t.Context(), har)
					equal(redrove, true)
				}
				check(err)
				equal(<-h.reports, live)
				if providerCalls.Load() != 2 || toolCalls.Load() != 1 || fresh.ModelInvocations() != 0 || fresh.ToolInvocations() != 0 {
					t.Fatal("recovery repeated a provider/tool effect")
				}
				after, err := journal.Read(1)
				check(err)
				equal(after[:endIndex], records[:endIndex])
				check(journal.Verify())
			}
			finalized = append(finalized, live.responses)
		})
	}
	if len(finalized) != 2 || !reflect.DeepEqual(finalized[0], finalized[1]) {
		t.Fatal("streaming changed finalized tool responses or usage")
	}
}
