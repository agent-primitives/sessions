package conformance_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
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

const responsesReasoning = `{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"ciphertext==","status":"completed"}`

type responsesReport struct {
	requests  []api.ModelRequest
	responses []api.ModelResponse
}

type responsesHarness struct {
	reports chan responsesReport
	change  string
}

func (responsesHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "responses-tools", Capabilities: api.Capabilities{
		Resumability: api.ResumabilityStatelessReplay, ReasoningReplay: true,
	}}, nil
}

func (h responsesHarness) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	var report responsesReport
	defer func() { h.reports <- report }()
	req := api.ModelRequest{
		Model: "responses-model", Messages: start.Inputs,
		Params: map[string]string{"reasoning_effort": "low", "max_output_tokens": "512"},
		Tools: []api.ToolDefinition{{Name: "lookup", Description: "Look up weather", InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}},
		}}},
		ToolChoice: &api.ToolChoice{Mode: api.ToolChoiceRequired, Name: "lookup"},
	}
	switch h.change {
	case "params":
		req.Params["reasoning_effort"] = "high"
	case "tools":
		req.Tools[0].Description = "Different description"
	case "choice":
		req.ToolChoice = &api.ToolChoice{Mode: api.ToolChoiceAuto}
	}
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
		if turn != 0 {
			continue
		}
		// Keep the full model message, including opaque reasoning, rather than rebuilding text.
		message := response.Message
		if h.change == "reasoning" {
			message.Parts = append([]api.Part(nil), message.Parts...)
			r := *message.Parts[0].Reasoning
			r.Opaque = []byte(strings.ReplaceAll(string(r.Opaque), "ciphertext", "changed"))
			message.Parts[0].Reasoning = &r
		}
		req.Messages = append(append([]api.Message(nil), req.Messages...), message)
		calls := 0
		for _, p := range response.Message.Parts {
			if p.ToolCall == nil {
				continue
			}
			calls++
			call := *p.ToolCall // Do not mutate the completion already committed by the host.
			call.Mediation, call.IdempotencyKey = api.MediationControllerMediated, "weather-1"
			result, err := sink.ToolCall(ctx, call)
			if err != nil {
				return err
			}
			req.Messages = append(req.Messages, api.Message{Role: "tool", Parts: []api.Part{{ToolResult: &result}}})
		}
		if calls != 1 {
			return fmt.Errorf("expected one function call, got %d", calls)
		}
		req.ToolChoice = &api.ToolChoice{Mode: api.ToolChoiceNone}
	}
	return nil
}

func responsesFixture(followup bool) string {
	output := `[` + responsesReasoning + `,{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"city\":\"Paris\"}","status":"completed"}]`
	if followup {
		output = `[{"type":"message","id":"msg_2","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Paris is sunny","annotations":[]}]}]`
	}
	return `{"id":"resp_1","status":"completed","model":"responses-model","output":` + output + `,"usage":{"input_tokens":7,"output_tokens":3,"output_tokens_details":{"reasoning_tokens":1}}}`
}

// The same completed output is sent as JSON or as SSE, with argument/text fragments before it.
func writeResponsesStream(w http.ResponseWriter, fixture string) {
	w.Header().Set("Content-Type", "text/event-stream")
	sequence := 0
	emit := func(kind string, fields map[string]any) {
		fields["type"], fields["sequence_number"] = kind, sequence
		sequence++
		body, _ := json.Marshal(fields)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, body)
	}
	var response map[string]any
	_ = json.Unmarshal([]byte(fixture), &response)
	emit("response.created", map[string]any{"response": map[string]any{"id": "resp_1", "status": "in_progress", "output": []any{}}})
	for index, raw := range response["output"].([]any) {
		item := raw.(map[string]any)
		added := make(map[string]any)
		for k, v := range item {
			added[k] = v
		}
		added["status"] = "in_progress"
		switch item["type"] {
		case "reasoning":
			added["encrypted_content"] = nil // Only completed encrypted bytes are authoritative.
		case "function_call":
			added["arguments"] = ""
		case "message":
			added["content"] = []any{}
		}
		emit("response.output_item.added", map[string]any{"output_index": index, "item": added})
		switch item["type"] {
		case "function_call":
			args := item["arguments"].(string)
			for _, fragment := range []string{args[:5], args[5:]} {
				emit("response.function_call_arguments.delta", map[string]any{"output_index": index, "item_id": item["id"], "delta": fragment})
			}
			emit("response.function_call_arguments.done", map[string]any{"output_index": index, "item_id": item["id"], "arguments": args})
		case "message":
			content := item["content"].([]any)[0].(map[string]any)
			emit("response.content_part.added", map[string]any{"output_index": index, "item_id": item["id"], "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
			for _, fragment := range []string{"Paris is ", "sunny"} {
				emit("response.output_text.delta", map[string]any{"output_index": index, "item_id": item["id"], "content_index": 0, "delta": fragment})
			}
			emit("response.output_text.done", map[string]any{"output_index": index, "item_id": item["id"], "content_index": 0, "text": content["text"]})
			emit("response.content_part.done", map[string]any{"output_index": index, "item_id": item["id"], "content_index": 0, "part": content})
		}
		emit("response.output_item.done", map[string]any{"output_index": index, "item": json.RawMessage(mustResponsesJSON(item))})
	}
	// Use the fixture's exact completed reasoning JSON, not a map re-encoding of its bytes.
	emit("response.completed", map[string]any{"response": json.RawMessage(fixture)})
}

func mustResponsesJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// Even an HTTP 200 containing a complete-looking call must not execute it if the response failed.
func TestResponsesIncompleteNeverExecutesTool(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
			var providerCalls, toolCalls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				providerCalls.Add(1)
				fixture := strings.Replace(responsesFixture(false), `"status":"completed"`, `"status":"incomplete"`, 1)
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":%s}\n\n", fixture)
				} else {
					fmt.Fprint(w, fixture)
				}
			}))
			defer server.Close()
			client, err := openai.NewResponses(openai.WithBaseURL(server.URL), openai.WithModel("responses-model"))
			if err != nil {
				t.Fatal(err)
			}
			store, _ := openFile(t)
			defer store.Close()
			journal := store.Session("incomplete")
			opts := []controller.Option{controller.WithSessionUID("incomplete"), controller.WithToolExecutor(func(context.Context, controller.ToolCallContext, api.ToolCall) (api.ToolResult, error) {
				toolCalls.Add(1)
				return api.ToolResult{}, nil
			})}
			if streaming {
				opts = append(opts, controller.WithStreamingModel(client.StreamModel))
			}
			c, err := controller.New(journal, client.Model, opts...)
			if err != nil {
				t.Fatal(err)
			}
			h := responsesHarness{reports: make(chan responsesReport, 1)}
			err = c.Exec(t.Context(), wireHarnessFrom(t, h), []api.Message{*api.TextMessage("user", "weather?")}, 0)
			<-h.reports
			if err == nil || !strings.Contains(err.Error(), "incomplete") {
				t.Fatalf("want incomplete response failure, got %v", err)
			}
			if providerCalls.Load() != 1 || toolCalls.Load() != 0 {
				t.Fatalf("provider=%d tool=%d; incomplete response was retried or executed", providerCalls.Load(), toolCalls.Load())
			}
			records, err := journal.Read(1)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range records {
				switch r.Event.Kind {
				case api.EventOutput, api.EventToolCall, api.EventToolResult, api.EventUsage, api.EventEnd:
					t.Fatalf("incomplete response committed %s", r.Event.Kind)
				}
			}
			if records[len(records)-1].Event.Kind != api.EventError {
				t.Fatal("failed execution did not record ERROR")
			}
			if err := journal.Verify(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResponsesWireRecoveryAndReplay(t *testing.T) {
	var finalized [][]api.ModelResponse
	for _, streaming := range []bool{false, true} {
		for _, cut := range []string{"end", "trailing-model"} {
			t.Run(fmt.Sprintf("stream=%t/cut=%s", streaming, cut), func(t *testing.T) {
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
				bodies := make(chan map[string]any, 4)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					providerCalls.Add(1)
					if r.URL.Path != "/responses" {
						t.Errorf("path = %s", r.URL.Path)
					}
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					bodies <- body
					followup := body["tool_choice"] == "none"
					fixture := responsesFixture(followup)
					if streaming {
						writeResponsesStream(w, fixture)
					} else {
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, fixture)
					}
				}))
				defer server.Close()
				client, err := openai.NewResponses(openai.WithBaseURL(server.URL), openai.WithModel("responses-model"))
				check(err)
				executor := func(_ context.Context, scope controller.ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
					toolCalls.Add(1)
					equal(scope.SessionUID, "responses")
					equal(call, api.ToolCall{ID: "call_1", Tool: "lookup", Args: map[string]any{"city": "Paris"}, Mediation: api.MediationControllerMediated, IdempotencyKey: "weather-1"})
					return api.ToolResult{ID: call.ID, Content: []api.Part{{Text: &api.TextPart{Text: "sunny"}}}, Output: map[string]any{"receipt": "not-model-input"}}, nil
				}
				var chunks strings.Builder
				opts := []controller.Option{controller.WithSessionUID("responses"), controller.WithToolExecutor(executor), controller.WithObserver(controller.Observer{OnDelta: func(delta api.Delta) { chunks.WriteString(delta.Chunk) }})}
				if streaming {
					opts = append(opts, controller.WithStreamingModel(client.StreamModel))
				}
				h := responsesHarness{reports: make(chan responsesReport, 1)}
				har := wireHarnessFrom(t, h)
				store, path := openFile(t)
				defer func() { store.Close() }()
				journal := store.Session("responses")
				c, err := controller.New(journal, client.Model, opts...)
				check(err)
				check(c.Exec(t.Context(), har, []api.Message{*api.TextMessage("user", "weather?")}, 0))
				live := <-h.reports
				if len(live.responses) != 2 {
					t.Fatalf("responses = %#v", live.responses)
				}
				first := live.responses[0]
				equal(len(first.Message.Parts), 2)
				equal(first.Message.Parts[0].Reasoning, &api.ReasoningPart{Provider: "openai", ModelID: "responses-model", Opaque: []byte(responsesReasoning), ItemID: "rs_1"})
				equal(first.Message.Parts[1].ToolCall, &api.ToolCall{ID: "call_1", Tool: "lookup", Args: map[string]any{"city": "Paris"}})
				equal(live.responses[1].Message, *api.TextMessage("assistant", "Paris is sunny"))
				for _, response := range live.responses {
					equal(response.Usage, api.Usage{Model: "responses-model", InputTokens: 7, OutputTokens: 3, ReasoningTokens: 1})
				}
				for i := 0; i < 2; i++ {
					body := <-bodies
					equal(body["model"], "responses-model")
					equal(body["store"], false)
					equal(body["include"], []any{"reasoning.encrypted_content"})
					equal(body["reasoning"], map[string]any{"effort": "low"})
					equal(body["max_output_tokens"], float64(512))
					tools := body["tools"].([]any)
					equal(len(tools), 1)
					tool := tools[0].(map[string]any)
					equal(tool["type"], "function")
					equal(tool["strict"], false)
					if i == 1 {
						items := body["input"].([]any)
						equal(len(items), 4)
						var reasoning map[string]any
						check(json.Unmarshal([]byte(responsesReasoning), &reasoning))
						equal(items[1], reasoning)
						call := items[2].(map[string]any)
						equal(call["type"], "function_call")
						equal(call["call_id"], "call_1")
						equal(items[3], map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "sunny"})
					}
					if _, exists := body["previous_response_id"]; exists {
						t.Fatal("adapter used provider-stored state")
					}
				}
				if streaming {
					equal(chunks.String(), "Paris is sunny")
				} else {
					equal(chunks.String(), "")
				}
				records, err := journal.Read(1)
				check(err)
				var outputIndex, modelIndex, usageIndex int
				cutAt := records[len(records)-1].Seq
				for _, record := range records {
					switch event := record.Event; event.Kind {
					case api.EventModelCall:
						requestJSON, err := json.Marshal(live.requests[modelIndex])
						check(err)
						equal(event.ModelCall.InputHash, fmt.Sprintf("%x", sha256.Sum256(requestJSON)))
						modelIndex++
					case api.EventOutput:
						if cut == "trailing-model" && outputIndex == 1 {
							cutAt = record.Seq
						}
						equal(event.Message, &live.responses[outputIndex].Message)
						outputIndex++
					case api.EventUsage:
						equal(event.Usage, &live.responses[usageIndex].Usage)
						usageIndex++
					}
				}
				equal(modelIndex, 2)
				equal(outputIndex, 2)
				equal(usageIndex, 2)
				check(journal.Verify())
				// I0 divergence is detected before adapter/executor IO, including opaque-state changes.
				for _, change := range []string{"params", "tools", "choice", "reasoning"} {
					changed := responsesHarness{reports: make(chan responsesReport, 1), change: change}
					fresh, err := controller.New(journal, client.Model, opts...)
					check(err)
					_, err = fresh.Replay(t.Context(), wireHarnessFrom(t, changed))
					<-changed.reports
					if !errors.Is(err, controller.ErrReplayDiverged) {
						t.Fatalf("change %s: expected divergence, got %v", change, err)
					}
					equal(providerCalls.Load(), int64(2))
					equal(toolCalls.Load(), int64(1))
				}
				check(store.Close())
				truncateFrom(t, path, "responses", cutAt)
				store, err = sqlitelog.Open(path)
				check(err)
				journal = store.Session("responses")
				chunks.Reset()
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
					wantCalls := int64(2)
					if cut == "trailing-model" {
						wantCalls++
					}
					equal(providerCalls.Load(), wantCalls)
					equal(toolCalls.Load(), int64(1))
					if replay || cut == "end" {
						equal(fresh.ModelInvocations(), 0)
					}
					equal(fresh.ToolInvocations(), 0)
					check(journal.Verify())
				}
				if cut == "end" {
					equal(chunks.String(), "")
				}
				finalized = append(finalized, live.responses)
			})
		}
	}
	for _, responses := range finalized[1:] {
		if !reflect.DeepEqual(responses, finalized[0]) {
			t.Fatal("streaming or recovery changed finalized parts/usage")
		}
	}
}
