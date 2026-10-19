package openai_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/model/openai"
)

// Values a provider controls. A refusal must not quote them: the host may journal the error text.
const (
	refusedName = "delete-all"
	refusedArg  = "arg-secret-7f3"
	refusedText = "text-secret-91c"
	refusedID   = "call_zq9"
)

func toolResponseCall(id, name string) string {
	return `{"type":"function_call","id":"fc_` + id + `","call_id":"` + id + `","name":"` + name + `","arguments":"{\"path\":\"` + refusedArg + `\"}","status":"completed"}`
}

const toolResponseText = `{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"` + refusedText + `","annotations":[]}]}`

func toolResponseFinal(items ...string) string {
	return `{"id":"resp","status":"completed","output":[` + strings.Join(items, ",") + `]}`
}

// toolResponseStream frames a completed response as the SSE events a provider sends for it.
func toolResponseStream(items ...string) string {
	out := responseEvent("response.created", `,"response":{"id":"resp","status":"in_progress","output":[]}`)
	for i, item := range items {
		index := `,"output_index":` + strconv.Itoa(i)
		out += responseEvent("response.output_item.added", index+`,"item":`+item)
		out += responseEvent("response.output_item.done", index+`,"item":`+item)
	}
	return out + responseEvent("response.completed", `,"response":`+toolResponseFinal(items...))
}

// Catches a completion that breaks the request's tool constraints being returned as a success:
// the harness would forward the call to the host, or treat a REQUIRED turn as answered.
func TestResponsesRefuseToolResponse(t *testing.T) {
	offered := []api.ToolDefinition{{Name: "lookup"}, {Name: "wipe-disk"}}
	required := &api.ToolChoice{Mode: api.ToolChoiceRequired}
	named := &api.ToolChoice{Mode: api.ToolChoiceRequired, Name: "lookup"}
	for _, tc := range []struct {
		name   string
		tools  []api.ToolDefinition
		choice *api.ToolChoice
		output []string
		reason string // empty: the completion is accepted
	}{
		{name: "not offered", tools: []api.ToolDefinition{{Name: "lookup"}}, output: []string{toolResponseCall(refusedID, refusedName)}, reason: "calls a tool the request did not offer"},
		{name: "not offered after an offered call", tools: offered, choice: &api.ToolChoice{Mode: api.ToolChoiceAuto}, output: []string{toolResponseCall("call_ok", "lookup"), toolResponseCall(refusedID, refusedName)}, reason: "calls a tool the request did not offer"},
		{name: "AUTO with no tools", choice: &api.ToolChoice{Mode: api.ToolChoiceAuto}, output: []string{toolResponseCall(refusedID, refusedName)}, reason: "calls a tool the request did not offer"},
		{name: "named choice, other offered tool", tools: offered, choice: named, output: []string{toolResponseCall(refusedID, "wipe-disk")}, reason: "calls a tool other than the named tool choice"},
		{name: "named choice, second call differs", tools: offered, choice: named, output: []string{toolResponseCall("call_ok", "lookup"), toolResponseCall(refusedID, "wipe-disk")}, reason: "calls a tool other than the named tool choice"},
		{name: "NONE", tools: offered, choice: &api.ToolChoice{Mode: api.ToolChoiceNone}, output: []string{toolResponseText, toolResponseCall(refusedID, "wipe-disk")}, reason: "calls a tool under tool choice NONE"},
		{name: "REQUIRED, text only", tools: offered, choice: required, output: []string{toolResponseText}, reason: "has no tool call under tool choice REQUIRED"},
		{name: "named REQUIRED, text only", tools: offered, choice: named, output: []string{toolResponseText}, reason: "has no tool call under tool choice REQUIRED"},
		{name: "REQUIRED, empty output", tools: offered, choice: required, reason: "has no tool call under tool choice REQUIRED"},
		{name: "provider default, offered call", tools: offered, output: []string{toolResponseText, toolResponseCall("call_ok", "wipe-disk")}},
		{name: "REQUIRED, offered calls", tools: offered, choice: required, output: []string{toolResponseCall("call_ok", "wipe-disk"), toolResponseCall("call_ok2", "lookup")}},
		{name: "named choice, named call", tools: offered, choice: named, output: []string{toolResponseText, toolResponseCall("call_ok", "lookup")}},
		{name: "NONE, text only", tools: offered, choice: &api.ToolChoice{Mode: api.ToolChoiceNone}, output: []string{toolResponseText}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newResponses(t, func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["stream"] == true {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, toolResponseStream(tc.output...))
					return
				}
				_, _ = io.WriteString(w, toolResponseFinal(tc.output...))
			})
			req := api.ModelRequest{Tools: tc.tools, ToolChoice: tc.choice}
			for _, path := range []struct {
				name string
				call func() (api.ModelResponse, error)
			}{
				{"Model", func() (api.ModelResponse, error) { return c.Model(t.Context(), req) }},
				{"StreamModel", func() (api.ModelResponse, error) { return c.StreamModel(t.Context(), req, func(string) {}) }},
			} {
				t.Run(path.name, func(t *testing.T) {
					got, err := path.call()
					if tc.reason == "" {
						if err != nil {
							t.Fatalf("err = %v, want accepted", err)
						}
						if len(got.Message.Parts) != len(tc.output) {
							t.Fatalf("parts = %+v, want %d", got.Message.Parts, len(tc.output))
						}
						return
					}
					if !errors.Is(err, openai.ErrUnsupportedPart) || !strings.Contains(err.Error(), tc.reason) {
						t.Fatalf("err = %v, want ErrUnsupportedPart with %q", err, tc.reason)
					}
					for _, leaked := range []string{refusedName, refusedArg, refusedText, refusedID, "wipe-disk", "lookup"} {
						if strings.Contains(err.Error(), leaked) {
							t.Fatalf("err = %q quotes provider value %q", err, leaked)
						}
					}
					if !reflect.DeepEqual(got, api.ModelResponse{}) {
						t.Fatalf("refused completion returned %+v", got)
					}
				})
			}
		})
	}
}
