package openai_test

import (
	"encoding/json"
	"errors"
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
