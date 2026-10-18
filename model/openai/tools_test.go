package openai_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/model/openai"
)

func TestModelMapsToolRequests(t *testing.T) {
	for _, tc := range []struct {
		name   string
		choice *api.ToolChoice
		wire   any
	}{
		{"absent", nil, nil},
		{"default", &api.ToolChoice{}, nil},
		{"auto", &api.ToolChoice{Mode: api.ToolChoiceAuto}, "auto"},
		{"none", &api.ToolChoice{Mode: api.ToolChoiceNone}, "none"},
		{"required", &api.ToolChoice{Mode: api.ToolChoiceRequired}, "required"},
		{"named", &api.ToolChoice{Mode: api.ToolChoiceRequired, Name: "weather"}, map[string]any{"type": "function", "function": map[string]any{"name": "weather"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Error(err)
				}
				_, _ = io.WriteString(w, completion("ok"))
			})
			req := api.ModelRequest{ToolChoice: tc.choice, Tools: []api.ToolDefinition{
				{Name: "weather", Description: "forecast", InputSchema: map[string]any{"type": "object"}},
				{Name: "empty", InputSchema: map[string]any{}}, {Name: "unspecified"},
			}, Messages: []api.Message{
				*api.TextMessage("user", "weather?"),
				{Role: "assistant", Parts: []api.Part{{Text: &api.TextPart{Text: "checking"}},
					{ToolCall: &api.ToolCall{ID: "c1", Tool: "weather", Args: map[string]any{"city": "Paris"}}},
					{ToolCall: &api.ToolCall{ID: "c2", Tool: "empty"}}}},
				{Role: "tool", Parts: []api.Part{{ToolResult: &api.ToolResult{ID: "c1", Output: map[string]any{"audit": "only"}, Content: []api.Part{{Text: &api.TextPart{Text: "sun"}}, {Text: &api.TextPart{Text: "ny"}}}}}}},
				{Role: "tool", Parts: []api.Part{{ToolResult: &api.ToolResult{ID: "c2"}}}},
			}}
			if _, err := c.Model(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			var want map[string]any
			if err := json.Unmarshal([]byte(`{"model":"test-model","messages":[{"role":"user","content":"weather?"},{"role":"assistant","content":"checking","tool_calls":[{"id":"c1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Paris\"}"}},{"id":"c2","type":"function","function":{"name":"empty","arguments":"{}"}}]},{"role":"tool","content":"sunny","tool_call_id":"c1"},{"role":"tool","content":"","tool_call_id":"c2"}],"tools":[{"type":"function","function":{"name":"weather","description":"forecast","parameters":{"type":"object"}}},{"type":"function","function":{"name":"empty","parameters":{}}},{"type":"function","function":{"name":"unspecified"}}]}`), &want); err != nil {
				t.Fatal(err)
			}
			if tc.wire != nil {
				want["tool_choice"] = tc.wire
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("request = %#v, want %#v", got, want)
			}
		})
	}
}

func TestModelToolPartsMatchStreamedCompletion(t *testing.T) {
	plain := `{"choices":[{"finish_reason":"tool_calls","message":{"reasoning_content":"r","content":"checking","tool_calls":[{"id":"c0","type":"function","function":{"name":"empty","arguments":"{}"}},{"id":"c1","type":"function","function":{"name":"weather","arguments":"{\"n\":2}"}}]}}],"usage":{"prompt_tokens":3,"completion_tokens":5,"completion_tokens_details":{"reasoning_tokens":2}}}`
	frames := []string{
		`{"choices":[{"delta":{"reasoning_content":"r","content":"checking","tool_calls":[{"index":1,"id":"c1","type":"function","function":{"name":"weather","arguments":"{\"n\":"}},{"index":0,"id":"c0","type":"function","function":{"arguments":"{"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"empty","arguments":"}"}},{"index":1,"id":"c1","type":"function","function":{"name":"weather","arguments":"2}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":5,"completion_tokens_details":{"reasoning_tokens":2}}}`,
	}
	want := api.ModelResponse{Message: api.Message{Role: "assistant", Parts: []api.Part{
		{Reasoning: &api.ReasoningPart{Provider: "openai", ModelID: "test-model", Opaque: []byte("r")}},
		{Text: &api.TextPart{Text: "checking"}},
		{ToolCall: &api.ToolCall{ID: "c0", Tool: "empty", Args: map[string]any{}}},
		{ToolCall: &api.ToolCall{ID: "c1", Tool: "weather", Args: map[string]any{"n": float64(2)}}},
	}}, Usage: api.Usage{Model: "test-model", InputTokens: 3, OutputTokens: 5, ReasoningTokens: 2}}
	for _, stream := range []bool{false, true} {
		c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
			if !stream {
				_, _ = io.WriteString(w, plain)
				return
			}
			for _, frame := range frames {
				_, _ = io.WriteString(w, "data: "+frame+"\n\n")
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		})
		var got api.ModelResponse
		var chunks strings.Builder
		var err error
		if stream {
			got, err = c.StreamModel(t.Context(), api.ModelRequest{}, func(s string) { chunks.WriteString(s) })
		} else {
			got, err = c.Model(t.Context(), api.ModelRequest{})
		}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("stream=%v response=%+v err=%v, want %+v", stream, got, err, want)
		}
		if stream && chunks.String() != "checking" {
			t.Fatalf("tool fragments leaked into text callback: %q", chunks.String())
		}
	}
}

// A named REQUIRED choice may finish with stop, but that exception must not admit
// a different, multiple, malformed, legacy, or unfinished call in either HTTP path.
func TestNamedToolCompletionFinish(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, tc := range []struct {
			name, finish, tool, id, kind, args string
			count                              int
			choice                             *api.ToolChoice
			legacy, wantOK                     bool
		}{
			{name: "named stop", finish: "stop", wantOK: true},
			{name: "ordinary tool_calls", finish: "tool_calls", wantOK: true},
			{name: "wrong name", finish: "stop", tool: "other"},
			{name: "two calls", finish: "stop", count: 2},
			{name: "length", finish: "length"},
			{name: "content_filter", finish: "content_filter"},
			{name: "empty finish"},
			{name: "legacy", finish: "stop", legacy: true},
			{name: "malformed arguments", finish: "stop", args: "{"},
			{name: "null arguments", finish: "stop", args: "null"},
			{name: "array arguments", finish: "stop", args: "[]"},
			{name: "missing ID", finish: "stop", id: "missing"},
			{name: "wrong type", finish: "stop", kind: "custom"},
			{name: "auto stop", finish: "stop", choice: &api.ToolChoice{Mode: api.ToolChoiceAuto}},
			{name: "unnamed required stop", finish: "stop", choice: &api.ToolChoice{Mode: api.ToolChoiceRequired}},
		} {
			t.Run(fmt.Sprintf("stream=%t/%s", streaming, tc.name), func(t *testing.T) {
				tool, id, kind, args, count := "weather", "c", "function", "{}", 1
				if tc.tool != "" {
					tool = tc.tool
				}
				if tc.id == "missing" {
					id = ""
				}
				if tc.kind != "" {
					kind = tc.kind
				}
				if tc.args != "" {
					args = tc.args
				}
				if tc.count != 0 {
					count = tc.count
				}
				calls := make([]any, 0, count)
				for i := 0; i < count; i++ {
					callID := id
					if i > 0 {
						callID = fmt.Sprintf("c%d", i)
					}
					calls = append(calls, map[string]any{"index": i, "id": callID, "type": kind, "function": map[string]any{"name": tool, "arguments": args}})
				}
				message := map[string]any{"tool_calls": calls}
				if tc.legacy {
					message = map[string]any{"function_call": map[string]any{"name": tool, "arguments": args}}
				}
				key := "message"
				if streaming {
					key = "delta"
				}
				body, err := json.Marshal(map[string]any{"choices": []any{map[string]any{key: message, "finish_reason": tc.finish}}})
				if err != nil {
					t.Fatal(err)
				}
				c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
					if streaming {
						fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", body)
					} else {
						_, _ = w.Write(body)
					}
				})
				choice := tc.choice
				if choice == nil {
					choice = &api.ToolChoice{Mode: api.ToolChoiceRequired, Name: "weather"}
				}
				req := api.ModelRequest{Tools: []api.ToolDefinition{{Name: "weather"}}, ToolChoice: choice}
				var got api.ModelResponse
				if streaming {
					got, err = c.StreamModel(t.Context(), req, nil)
				} else {
					got, err = c.Model(t.Context(), req)
				}
				if !tc.wantOK {
					if !errors.Is(err, openai.ErrUnsupportedPart) || len(got.Message.Parts) != 0 {
						t.Fatalf("response=%+v err=%v, want unsupported and no completion", got, err)
					}
					return
				}
				want := api.Message{Role: "assistant", Parts: []api.Part{{ToolCall: &api.ToolCall{ID: "c", Tool: "weather", Args: map[string]any{}}}}}
				if err != nil || !reflect.DeepEqual(got.Message, want) {
					t.Fatalf("message=%+v err=%v, want %+v", got.Message, err, want)
				}
			})
		}
	}
}

func TestToolArgumentErrorsDoNotExposePayload(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, tc := range []struct{ name, args, private string }{
			{"range", `{"private":987654321098765432109876543210e10000}`, "987654321098765432109876543210e10000"},
			{"syntax", `{"private":"fixture-private-value",}`, "fixture-private-value"},
			{"type", `"fixture-private-value"`, "fixture-private-value"},
		} {
			t.Run(fmt.Sprintf("stream=%t/%s", streaming, tc.name), func(t *testing.T) {
				encoded, err := json.Marshal(tc.args)
				if err != nil {
					t.Fatal(err)
				}
				message := fmt.Sprintf(`{"tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"weather","arguments":%s}}]}`, encoded)
				c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
					if streaming {
						fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":%s,\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", message)
					} else {
						fmt.Fprintf(w, `{"choices":[{"message":%s,"finish_reason":"tool_calls"}]}`, message)
					}
				})
				var got api.ModelResponse
				if streaming {
					got, err = c.StreamModel(t.Context(), api.ModelRequest{}, nil)
				} else {
					got, err = c.Model(t.Context(), api.ModelRequest{})
				}
				if !errors.Is(err, openai.ErrUnsupportedPart) || len(got.Message.Parts) != 0 {
					t.Fatalf("response=%+v err=%v, want unsupported and no completion", got, err)
				}
				if strings.Contains(err.Error(), tc.private) {
					t.Fatalf("argument payload leaked into error: %v", err)
				}
				var syntax *json.SyntaxError
				var typeError *json.UnmarshalTypeError
				if errors.As(err, &syntax) || errors.As(err, &typeError) {
					t.Fatalf("raw decoder error escaped: %v", err)
				}
				if !strings.Contains(err.Error(), "decode tool arguments") {
					t.Fatalf("missing argument diagnostic: %v", err)
				}
			})
		}
	}
}

func TestDecodeErrorsDoNotExposePayload(t *testing.T) {
	const private = "987654321098765432109876543210"
	for _, tc := range []struct {
		name, body, want string
		streaming        bool
	}{
		{"index range", `{"choices":[{"delta":{"content":"must not emit","tool_calls":[{"index":` + private + `}]}}]}`, "JSON value has the wrong type or is out of range for field choices.delta.tool_calls.index", true},
		{"index exponent", `{"choices":[{"delta":{"tool_calls":[{"index":` + private + `e10000}]}}]}`, "JSON value has the wrong type or is out of range for field choices.delta.tool_calls.index", true},
		{"index type", `{"choices":[{"delta":{"tool_calls":[{"index":"` + private + `"}]}}]}`, "JSON value has the wrong type or is out of range for field choices.delta.tool_calls.index", true},
		{"stream syntax", `{"choices":[],"` + private + `":}`, "invalid JSON syntax", true},
		{"response syntax", `{"choices":[],"` + private + `":}`, "invalid JSON syntax", false},
		{"stream usage", `{"usage":{"prompt_tokens":` + private + `e10000}}`, "JSON value has the wrong type or is out of range for field usage.prompt_tokens", true},
		{"response usage", `{"usage":{"prompt_tokens":` + private + `e10000}}`, "JSON value has the wrong type or is out of range for field usage.prompt_tokens", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
				if tc.streaming {
					fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", tc.body)
				} else {
					fmt.Fprint(w, tc.body)
				}
			})
			var got api.ModelResponse
			var err error
			stage := "decode response"
			if tc.streaming {
				stage = "decode stream chunk"
				got, err = c.StreamModel(t.Context(), api.ModelRequest{}, func(string) { t.Error("rejected chunk emitted text") })
			} else {
				got, err = c.Model(t.Context(), api.ModelRequest{})
			}
			if err == nil || !strings.Contains(err.Error(), stage+": "+tc.want) || len(got.Message.Parts) != 0 {
				t.Fatalf("response=%+v err=%v, want %s: %s and no completion", got, err, stage, tc.want)
			}
			var syntax *json.SyntaxError
			var typeError *json.UnmarshalTypeError
			if strings.Contains(err.Error(), private) || errors.As(err, &syntax) || errors.As(err, &typeError) {
				t.Fatalf("raw decoder diagnostic escaped: %v", err)
			}
		})
	}
}

func TestModelRefusesMalformedToolCompletions(t *testing.T) {
	for _, tc := range []struct{ name, id, kind, args, finish string }{
		{"invalid JSON", "c", "function", "{", "tool_calls"},
		{"null", "c", "function", "null", "tool_calls"},
		{"array", "c", "function", "[]", "tool_calls"},
		{"scalar", "c", "function", `"x"`, "tool_calls"},
		{"multiple values", "c", "function", "{} {}", "tool_calls"},
		{"missing ID", "", "function", "{}", "tool_calls"},
		{"missing name", "c", "function", "{}", "tool_calls"},
		{"empty calls", "c", "function", "{}", "tool_calls"},
		{"legacy", "c", "function", "{}", "function_call"},
		{"custom tool", "c", "custom", "{}", "tool_calls"},
		{"truncated", "c", "function", "{}", "length"},
		{"duplicate ID", "c", "function", "{}", "tool_calls"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call := map[string]any{"id": tc.id, "type": tc.kind, "function": map[string]any{"name": "weather", "arguments": tc.args}}
			calls := []any{call}
			if tc.name == "duplicate ID" {
				calls = append(calls, call)
			}
			if tc.name == "missing name" {
				call["function"].(map[string]any)["name"] = ""
			}
			if tc.name == "empty calls" {
				calls = nil
			}
			message := map[string]any{"tool_calls": calls}
			if tc.name == "legacy" {
				message = map[string]any{"function_call": call["function"]}
			}
			body, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"finish_reason": tc.finish, "message": message}}})
			if err != nil {
				t.Fatal(err)
			}
			c := newClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) })
			if _, err := c.Model(t.Context(), api.ModelRequest{}); !errors.Is(err, openai.ErrUnsupportedPart) {
				t.Fatalf("err=%v, want unsupported completion", err)
			}
		})
	}
}

func TestStreamCompletionFreezesAtFinish(t *testing.T) {
	const complete = `{"content":"before","tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"weather","arguments":"{}"}}]}`
	const final = `{"choices":[{"delta":` + complete + `,"finish_reason":"tool_calls"}]}`
	const textFinal = `{"choices":[{"delta":{"content":"before"},"finish_reason":"stop"}]}`
	for _, tc := range []struct{ name, first, tail string }{
		{"content", final, `{"choices":[{"delta":{"content":"after"}}]}`},
		{"reasoning", final, `{"choices":[{"delta":{"reasoning_content":"after"}}]}`},
		{"tool", final, `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":""}}]}}]}`},
		{"legacy", final, `{"choices":[{"delta":{"function_call":{"name":"weather","arguments":"{}"}}}]}`},
		{"contradictory reason", textFinal, `{"choices":[{"delta":{},"finish_reason":"length"}]}`},
		{"content with repeated reason", textFinal, `{"choices":[{"delta":{"content":"after"},"finish_reason":"stop"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, "data: %s\n\ndata: %s\n\ndata: [DONE]\n\n", tc.first, tc.tail)
			})
			var chunks strings.Builder
			got, err := c.StreamModel(t.Context(), api.ModelRequest{}, func(s string) { chunks.WriteString(s) })
			if !errors.Is(err, openai.ErrUnsupportedPart) || len(got.Message.Parts) != 0 {
				t.Errorf("response=%+v err=%v, want unsupported and no completion", got, err)
			}
			if chunks.String() != "before" {
				t.Errorf("post-finish text reached callback: %q", chunks.String())
			}
		})
	}
}

func TestStreamFinalFrameDeltaAndTrailingUsage(t *testing.T) {
	for _, done := range []bool{false, true} {
		t.Run(fmt.Sprintf("done=%t", done), func(t *testing.T) {
			c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
				for _, frame := range []string{
					`{"choices":[{"delta":{"content":"checking","tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"weather","arguments":"{\"n\":"}}]}}]}`,
					`{"choices":[{"delta":{"content":"done","tool_calls":[{"index":0,"function":{"arguments":"2}"}}]},"finish_reason":"tool_calls"}]}`,
					`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4}}`,
					`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
					`{"choices":[{"delta":{"role":"assistant"}}]}`,
					`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":7,"completion_tokens":9}}`,
				} {
					fmt.Fprintf(w, "data: %s\n\n", frame)
				}
				if done {
					_, _ = io.WriteString(w, "data: [DONE]\n\n")
				}
			})
			var chunks strings.Builder
			got, err := c.StreamModel(t.Context(), api.ModelRequest{}, func(s string) { chunks.WriteString(s) })
			want := api.ModelResponse{Message: api.Message{Role: "assistant", Parts: []api.Part{
				{Text: &api.TextPart{Text: "checkingdone"}},
				{ToolCall: &api.ToolCall{ID: "c", Tool: "weather", Args: map[string]any{"n": float64(2)}}},
			}}, Usage: api.Usage{Model: "test-model", InputTokens: 7, OutputTokens: 9}}
			if err != nil || !reflect.DeepEqual(got, want) || chunks.String() != "checkingdone" {
				t.Fatalf("response=%+v err=%v chunks=%q, want %+v", got, err, chunks.String(), want)
			}
		})
	}
}

func TestStreamRefusesUnmappedOrIncompleteToolDeltas(t *testing.T) {
	for name, delta := range map[string]string{
		"missing index": `{"tool_calls":[{"id":"c","type":"function","function":{"name":"weather","arguments":"{}"}}]}`,
		"negative":      `{"tool_calls":[{"index":-1,"id":"c","type":"function","function":{"name":"weather","arguments":"{}"}}]}`,
		"gap":           `{"tool_calls":[{"index":999999999,"id":"c","type":"function","function":{"name":"weather","arguments":"{}"}}]}`,
		"custom":        `{"tool_calls":[{"index":0,"id":"c","type":"custom"}]}`,
		"bad args":      `{"tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"weather","arguments":"{"}}]}`,
		"no finish":     `{"tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"weather","arguments":"{}"}}]}`,
		"legacy":        `{"function_call":{"name":"weather","arguments":"{}"}}`,
		"empty calls":   `{"tool_calls":[]}`,
		"changed ID":    `{"tool_calls":[{"index":0,"id":"c0","type":"function","function":{"name":"weather","arguments":"{"}},{"index":0,"id":"c1","function":{"arguments":"}"}}]}`,
		"changed name":  `{"tool_calls":[{"index":0,"id":"c0","type":"function","function":{"name":"weather","arguments":"{"}},{"index":0,"function":{"name":"other","arguments":"}"}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":"+delta+"}]}\n\n")
				if name != "no finish" {
					_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
				}
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			})
			if _, err := c.StreamModel(t.Context(), api.ModelRequest{}, nil); !errors.Is(err, openai.ErrUnsupportedPart) {
				t.Fatalf("err=%v, want unsupported stream", err)
			}
		})
	}
}
