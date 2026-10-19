package openai_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/model/openai"
)

func newResponses(t *testing.T, h http.HandlerFunc, opts ...openai.Option) *openai.ResponsesClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	base := []openai.Option{openai.WithBaseURL(srv.URL), openai.WithModel("test-model")}
	c, err := openai.NewResponses(append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const responsesText = `{"id":"resp_1","object":"response","created_at":1741476542,"status":"completed","model":"provider-alias","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello","annotations":[]}]}],"usage":{"input_tokens":7,"output_tokens":11,"output_tokens_details":{"reasoning_tokens":3}},"error":null,"incomplete_details":null}`

func assertJSON(t *testing.T, got, want []byte) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got JSON: %v: %s", err, got)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("want JSON: %v", err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("JSON got %s\nwant %s", got, want)
	}
}

// Catches the wrong protocol/default path, implicit state, and mutation of the hashed request.
func TestResponsesTextRequest(t *testing.T) {
	var posted []byte
	c := newResponses(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" || r.Method != "POST" {
			t.Errorf("request = %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("api-key") != "local-secret" {
			t.Errorf("headers = %v", r.Header)
		}
		posted, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, responsesText)
	}, openai.WithHeader("api-key", "local-secret"))
	req := api.ModelRequest{Model: "echo", Messages: []api.Message{*api.TextMessage("system", "rules"), {Parts: []api.Part{{Text: &api.TextPart{Text: "question"}}, {Text: &api.TextPart{Text: "?"}}}}}, Params: map[string]string{"reasoning_effort": "low", "max_output_tokens": "4096"}}
	before, _ := json.Marshal(req)
	got, err := c.Model(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, posted, []byte(`{"model":"test-model","store":false,"include":["reasoning.encrypted_content"],"reasoning":{"effort":"low"},"max_output_tokens":4096,"input":[{"type":"message","role":"system","content":[{"type":"input_text","text":"rules"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"question"},{"type":"input_text","text":"?"}]}]}`))
	after, _ := json.Marshal(req)
	if string(before) != string(after) {
		t.Fatal("mutated request")
	}
	want := api.ModelResponse{Message: *api.TextMessage("assistant", "hello"), Usage: api.Usage{Model: "test-model", InputTokens: 7, OutputTokens: 11, ReasoningTokens: 3}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("response = %#v, want %#v", got, want)
	}
	if !strings.HasSuffix(c.Endpoint(), "/responses") {
		t.Fatalf("endpoint = %s", c.Endpoint())
	}
}

// Catches reordering across reasoning/tool items, call_id confusion, receipt leakage,
// strict-mode schema rewriting, and loss of provider-owned reasoning bytes.
func TestResponsesOrderedToolsAndReasoning(t *testing.T) {
	const reasoning = `{"type":"reasoning", "id":"rs_1","summary":[{"type":"summary_text","text":"brief"}],"encrypted_content":"ciphertext==","future_field":"\u0061"}`
	const call = `{"type":"function_call","id":"fc_item","call_id":"call_1","name":"lookup","arguments":"{\"n\":2}","status":"completed"}`
	var posted []byte
	c := newResponses(t, func(w http.ResponseWriter, r *http.Request) {
		posted, _ = io.ReadAll(r.Body)
		_, _ = fmt.Fprintf(w, `{"status":"completed","output":[%s,%s,{"type":"message","id":"msg_2","role":"assistant","status":"completed","content":[{"type":"output_text","text":"done","annotations":[]}]}],"usage":null}`, reasoning, call)
	})
	first, err := c.Model(t.Context(), api.ModelRequest{Model: "explicit", Tools: []api.ToolDefinition{{Name: "lookup"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Message.Parts) != 3 {
		t.Fatalf("parts = %+v", first.Message.Parts)
	}
	rp := first.Message.Parts[0].Reasoning
	if rp == nil || string(rp.Opaque) != reasoning || rp.Provider != "openai" || rp.ModelID != "explicit" || rp.ItemID != "rs_1" || rp.Ordinal != 0 || rp.ValidityScope != "" {
		t.Fatalf("reasoning = %+v", rp)
	}
	tc := first.Message.Parts[1].ToolCall
	if tc == nil || tc.ID != "call_1" || tc.Tool != "lookup" || !reflect.DeepEqual(tc.Args, map[string]any{"n": float64(2)}) {
		t.Fatalf("call = %+v", tc)
	}
	if first.Usage != (api.Usage{Model: "explicit"}) {
		t.Fatal(first.Usage)
	}
	rp.ValidityScope = "execution-1"
	rp.Summary = []api.Part{{Text: &api.TextPart{Text: "not authoritative"}}}
	req := api.ModelRequest{Model: "explicit", Messages: []api.Message{{Role: "assistant", Parts: append([]api.Part{{Text: &api.TextPart{Text: "before"}}}, first.Message.Parts...)}, {Role: "tool", Parts: []api.Part{{ToolResult: &api.ToolResult{ID: "call_1", Output: map[string]any{"DO_NOT_SEND": "receipt"}, Error: "DO_NOT_SEND", OutputURI: "secret://receipt", Content: []api.Part{{Text: &api.TextPart{Text: "result "}}, {Text: &api.TextPart{Text: "text"}}}}}}}}, Tools: []api.ToolDefinition{{Name: "lookup", Description: "Lookup", InputSchema: map[string]any{}}, {Name: "other"}}, ToolChoice: &api.ToolChoice{Mode: api.ToolChoiceRequired, Name: "lookup"}}
	before, _ := json.Marshal(req)
	if _, err := c.Model(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(req)
	if string(before) != string(after) {
		t.Fatal("mutated request")
	}
	if !strings.Contains(string(posted), reasoning) {
		t.Fatalf("raw reasoning not verbatim: %s", posted)
	}
	assertJSON(t, posted, []byte(`{"model":"explicit","store":false,"include":["reasoning.encrypted_content"],"tools":[{"type":"function","name":"lookup","description":"Lookup","parameters":{},"strict":false},{"type":"function","name":"other","description":"","strict":false}],"tool_choice":{"type":"function","name":"lookup"},"input":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"before"}]},`+reasoning+`,{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"n\":2}"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]},{"type":"function_call_output","call_id":"call_1","output":"result text"}]}`))
}

// Every refusal must happen before HTTP, including mixed-kind parts and missing model-facing results.
func TestResponsesRequestRefusals(t *testing.T) {
	validReason := func() *api.ReasoningPart {
		return &api.ReasoningPart{Provider: "openai", ModelID: "test-model", ItemID: "rs", Opaque: []byte(`{"type":"reasoning","id":"rs","encrypted_content":"cipher","summary":[]}`)}
	}
	reasoningReq := func(change func(*api.ReasoningPart)) api.ModelRequest {
		r := validReason()
		change(r)
		return api.ModelRequest{Messages: []api.Message{{Role: "assistant", Parts: []api.Part{{Reasoning: r}}}}}
	}
	cases := map[string]api.ModelRequest{
		"unknown param":               {Params: map[string]string{"temperature": "1"}},
		"empty effort":                {Params: map[string]string{"reasoning_effort": ""}},
		"invalid effort":              {Params: map[string]string{"reasoning_effort": "HIGH"}},
		"zero tokens":                 {Params: map[string]string{"max_output_tokens": "0"}},
		"negative tokens":             {Params: map[string]string{"max_output_tokens": "-1"}},
		"decimal tokens":              {Params: map[string]string{"max_output_tokens": "1.5"}},
		"exponent tokens":             {Params: map[string]string{"max_output_tokens": "1e2"}},
		"token overflow":              {Params: map[string]string{"max_output_tokens": "9223372036854775808"}},
		"empty name":                  {Tools: []api.ToolDefinition{{}}},
		"duplicate name":              {Tools: []api.ToolDefinition{{Name: "f"}, {Name: "f"}}},
		"schema not JSON":             {Tools: []api.ToolDefinition{{Name: "f", InputSchema: map[string]any{"x": make(chan int)}}}},
		"choice mode":                 {ToolChoice: &api.ToolChoice{Mode: "invalid"}},
		"choice unnamed mode":         {ToolChoice: &api.ToolChoice{Name: "f"}},
		"choice name with auto":       {Tools: []api.ToolDefinition{{Name: "f"}}, ToolChoice: &api.ToolChoice{Mode: api.ToolChoiceAuto, Name: "f"}},
		"choice unknown name":         {ToolChoice: &api.ToolChoice{Mode: api.ToolChoiceRequired, Name: "f"}},
		"foreign reasoning":           reasoningReq(func(r *api.ReasoningPart) { r.Provider = "anthropic" }),
		"cross model reasoning":       reasoningReq(func(r *api.ReasoningPart) { r.ModelID = "other" }),
		"external reasoning":          reasoningReq(func(r *api.ReasoningPart) { r.OpaqueURI = "blob://state" }),
		"reasoning digest":            reasoningReq(func(r *api.ReasoningPart) { r.OpaqueDigest = "digest" }),
		"old Chat reasoning":          reasoningReq(func(r *api.ReasoningPart) { r.Opaque = []byte("old reasoning") }),
		"reasoning identity":          reasoningReq(func(r *api.ReasoningPart) { r.ItemID = "different" }),
		"reasoning ordinal":           reasoningReq(func(r *api.ReasoningPart) { r.Ordinal = -1 }),
		"missing encrypted reasoning": reasoningReq(func(r *api.ReasoningPart) { r.Opaque = []byte(`{"type":"reasoning","id":"rs","summary":[]}`) }),
	}
	for name, part := range map[string]api.Part{"empty": {}, "file": {File: &api.FilePart{}}, "data": {Data: map[string]any{}}, "mixed": {Text: &api.TextPart{Text: "text"}, File: &api.FilePart{}}, "tool in user": {ToolCall: &api.ToolCall{ID: "c", Tool: "f"}}, "reasoning in user": {Reasoning: validReason()}} {
		cases[name] = api.ModelRequest{Messages: []api.Message{{Role: "user", Parts: []api.Part{part}}}}
	}
	cases["unsupported role"] = api.ModelRequest{Messages: []api.Message{*api.TextMessage("developer", "x")}}
	cases["empty tool message"] = api.ModelRequest{Messages: []api.Message{{Role: "tool"}}}
	for name, result := range map[string]*api.ToolResult{"no content": {ID: "c", Output: map[string]any{"x": "receipt"}}, "no result id": {Content: []api.Part{{Text: &api.TextPart{}}}}, "result data": {ID: "c", Content: []api.Part{{Data: map[string]any{}}}}, "result mixed": {ID: "c", Content: []api.Part{{Text: &api.TextPart{}, Reasoning: validReason()}}}} {
		cases[name] = api.ModelRequest{Messages: []api.Message{{Role: "tool", Parts: []api.Part{{ToolResult: result}}}}}
	}
	for name, call := range map[string]*api.ToolCall{"no call id": {Tool: "f"}, "no call name": {ID: "c"}, "invalid args": {ID: "c", Tool: "f", Args: map[string]any{"x": math.NaN()}}} {
		cases[name] = api.ModelRequest{Messages: []api.Message{{Role: "assistant", Parts: []api.Part{{ToolCall: call}}}}}
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			var hits atomic.Int32
			c := newResponses(t, func(w http.ResponseWriter, _ *http.Request) { hits.Add(1); _, _ = io.WriteString(w, responsesText) })
			_, err := c.Model(t.Context(), req)
			if !errors.Is(err, openai.ErrUnsupportedPart) {
				t.Fatalf("err=%v, want unsupported part", err)
			}
			if hits.Load() != 0 {
				t.Fatal("invalid request reached HTTP")
			}
		})
	}
}

func TestResponsesChoicesEffortsAndEmptyResult(t *testing.T) {
	for _, mode := range []api.ToolChoiceMode{"", api.ToolChoiceAuto, api.ToolChoiceNone, api.ToolChoiceRequired} {
		t.Run(string(mode), func(t *testing.T) {
			var posted map[string]any
			c := newResponses(t, func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&posted)
				if mode == api.ToolChoiceRequired {
					_, _ = io.WriteString(w, `{"status":"completed","output":[{"type":"function_call","id":"fc","call_id":"c2","name":"f","arguments":"{}"}]}`)
					return
				}
				_, _ = io.WriteString(w, responsesText)
			})
			_, err := c.Model(t.Context(), api.ModelRequest{Tools: []api.ToolDefinition{{Name: "f"}}, ToolChoice: &api.ToolChoice{Mode: mode}, Messages: []api.Message{{Role: "tool", Parts: []api.Part{{ToolResult: &api.ToolResult{ID: "c", Content: []api.Part{{Text: &api.TextPart{Text: ""}}}}}}}}})
			if err != nil {
				t.Fatal(err)
			}
			want := strings.ToLower(string(mode))
			if mode == "" {
				if _, ok := posted["tool_choice"]; ok {
					t.Fatal(posted)
				}
			} else if posted["tool_choice"] != want {
				t.Fatal(posted)
			}
		})
	}
	for _, effort := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"} {
		t.Run(effort, func(t *testing.T) {
			c := newResponses(t, func(w http.ResponseWriter, r *http.Request) {
				var b map[string]any
				_ = json.NewDecoder(r.Body).Decode(&b)
				if !reflect.DeepEqual(b["reasoning"], map[string]any{"effort": effort}) {
					t.Error(b)
				}
				_, _ = io.WriteString(w, responsesText)
			})
			if _, err := c.Model(t.Context(), api.ModelRequest{Params: map[string]string{"reasoning_effort": effort}}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Unsupported output may not be silently dropped while supported text is returned.
func TestResponsesOutputRefusals(t *testing.T) {
	cases := map[string]string{
		"missing status": `{"output":[]}`, "failed": `{"status":"failed","output":[]}`, "incomplete": `{"status":"incomplete","output":[]}`, "missing output": `{"status":"completed","output_text":"lost"}`, "null output": `{"status":"completed","output":null}`,
		"error": `{"status":"completed","output":[],"error":{"message":"bad"}}`, "incomplete details": `{"status":"completed","output":[],"incomplete_details":{"reason":"max_output_tokens"}}`,
	}
	for name, item := range map[string]string{
		"unknown item": `{"type":"web_search_call","id":"ws"}`, "no id": `{"type":"message","role":"assistant","content":[]}`, "wrong role": `{"type":"message","id":"m","role":"user","content":[]}`, "unfinished message": `{"type":"message","id":"m","role":"assistant","status":"in_progress","content":[]}`, "missing content": `{"type":"message","id":"m","role":"assistant"}`,
		"refusal": `{"type":"message","id":"m","role":"assistant","content":[{"type":"refusal","refusal":"no"}]}`, "annotation": `{"type":"message","id":"m","role":"assistant","content":[{"type":"output_text","text":"x","annotations":[{"type":"url_citation"}]}]}`, "missing text": `{"type":"message","id":"m","role":"assistant","content":[{"type":"output_text"}]}`, "audio": `{"type":"message","id":"m","role":"assistant","content":[{"type":"audio","data":"x"}]}`,
		"no encrypted": `{"type":"reasoning","id":"rs","summary":[]}`, "empty encrypted": `{"type":"reasoning","id":"rs","encrypted_content":""}`, "wrong encrypted type": `{"type":"reasoning","id":"rs","encrypted_content":123}`, "bad reasoning summary": `{"type":"reasoning","id":"rs","encrypted_content":"cipher","summary":[{"type":"refusal","refusal":"no"}]}`,
		"missing call id": `{"type":"function_call","id":"fc","name":"f","arguments":"{}"}`, "missing call name": `{"type":"function_call","id":"fc","call_id":"c","arguments":"{}"}`, "malformed args": `{"type":"function_call","id":"fc","call_id":"c","name":"f","arguments":"{"}`, "array args": `{"type":"function_call","id":"fc","call_id":"c","name":"f","arguments":"[]"}`, "null args": `{"type":"function_call","id":"fc","call_id":"c","name":"f","arguments":"null"}`, "trailing args": `{"type":"function_call","id":"fc","call_id":"c","name":"f","arguments":"{} {}"}`,
	} {
		cases[name] = `{"status":"completed","output":[` + item + `]}`
	}
	cases["duplicate item id"] = `{"status":"completed","output":[{"type":"function_call","id":"fc","call_id":"c1","name":"f","arguments":"{}"},{"type":"function_call","id":"fc","call_id":"c2","name":"f","arguments":"{}"}]}`
	cases["duplicate call id"] = `{"status":"completed","output":[{"type":"function_call","id":"fc1","call_id":"c","name":"f","arguments":"{}"},{"type":"function_call","id":"fc2","call_id":"c","name":"f","arguments":"{}"}]}`
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c := newResponses(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })
			got, err := c.Model(t.Context(), api.ModelRequest{Tools: []api.ToolDefinition{{Name: "f"}}})
			if err == nil || !reflect.DeepEqual(got, api.ModelResponse{}) {
				t.Fatalf("response=%+v err=%v", got, err)
			}
		})
	}
}

func TestResponsesArgumentNumberBoundaries(t *testing.T) {
	for _, tc := range []struct {
		number string
		ok     bool
	}{{"9007199254740992", true}, {"-9007199254740992", true}, {"9007199254740993", false}, {"-9007199254740993", false}, {"9007199254740994", false}, {"9.007199254740993e15", false}, {"9007199254740993.0", false}, {"1e400", false}, {"1.25", true}, {"2e3", true}} {
		t.Run(tc.number, func(t *testing.T) {
			c := newResponses(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintf(w, `{"status":"completed","output":[{"type":"function_call","id":"fc","call_id":"c","name":"f","arguments":%q}]}`, `{"nested":[{"n":`+tc.number+`}]}`)
			})
			got, err := c.Model(t.Context(), api.ModelRequest{Tools: []api.ToolDefinition{{Name: "f"}}})
			if (err == nil) != tc.ok {
				t.Fatalf("err=%v, success want %v", err, tc.ok)
			}
			if tc.ok {
				if len(got.Message.Parts) != 1 || got.Message.Parts[0].ToolCall == nil {
					t.Fatalf("missing tool call: %+v", got)
				}
				if _, ok := got.Message.Parts[0].ToolCall.Args["nested"].([]any)[0].(map[string]any)["n"].(float64); !ok {
					t.Fatal("not float64")
				}
			}
		})
	}
	for _, n := range []any{int64(9007199254740993), uint64(9007199254740993), json.Number("9007199254740993"), math.Inf(1)} {
		t.Run(fmt.Sprintf("request_%v", n), func(t *testing.T) {
			c := newResponses(t, func(w http.ResponseWriter, _ *http.Request) {
				t.Error("out of range request reached server")
				_, _ = io.WriteString(w, responsesText)
			})
			_, err := c.Model(t.Context(), api.ModelRequest{Messages: []api.Message{{Role: "assistant", Parts: []api.Part{{ToolCall: &api.ToolCall{ID: "c", Tool: "f", Args: map[string]any{"n": n}}}}}}})
			if err == nil {
				t.Fatal("accepted unsafe number")
			}
		})
	}
}

func TestResponsesConstructorOptions(t *testing.T) {
	if _, err := openai.NewResponses(); err == nil {
		t.Fatal("missing model accepted")
	}
	c, err := openai.NewResponses(openai.WithModel("m"), openai.WithBaseURL("https://azure.invalid/openai/v1?tenant=x#drop"), openai.WithPath("responses?api-version=2025-04-01"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Endpoint() != "https://azure.invalid/openai/v1/responses?tenant=x&api-version=2025-04-01" {
		t.Fatal(c.Endpoint())
	}
	for _, opts := range [][]openai.Option{{openai.WithBaseURL("relative")}, {openai.WithBaseURL("https://x.invalid?api-version=1"), openai.WithPath("/responses?api-version=2")}} {
		if _, err := openai.NewResponses(append([]openai.Option{openai.WithModel("m")}, opts...)...); err == nil {
			t.Fatal("invalid endpoint accepted")
		}
	}
}
