package openai_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/model/openai"
)

func TestResponsesDeterministicErrors(t *testing.T) {
	t.Run("parameters", func(t *testing.T) {
		c := newResponses(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid request reached provider") })
		req := api.ModelRequest{Params: map[string]string{"reasoning_effort": "invalid", "max_output_tokens": "-1"}}
		for i := 0; i < 100; i++ {
			_, err := c.Model(t.Context(), req)
			if err == nil || !strings.Contains(err.Error(), "max_output_tokens must be a positive decimal integer") {
				t.Fatalf("iteration %d: first invalid parameter was not the sorted first key: %v", i, err)
			}
		}
	})
	t.Run("output indexes", func(t *testing.T) {
		fixture := responseEvent("response.output_item.added", `,"output_index":0,"item":{"type":"message","id":"m0","role":"assistant","content":[]}`) +
			responseEvent("response.output_item.added", `,"output_index":2,"item":{"type":"message","id":"m2","role":"assistant","content":[]}`) +
			responseEvent("response.completed", `,"response":{"status":"completed","output":[{"type":"message","id":"changed","role":"assistant","content":[]}]}`)
		c := newResponses(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, fixture) })
		for i := 0; i < 100; i++ {
			_, err := c.StreamModel(t.Context(), api.ModelRequest{}, nil)
			if err == nil || !strings.Contains(err.Error(), "conflicting output item identity/type") {
				t.Fatalf("iteration %d: first invalid output was not index zero: %v", i, err)
			}
		}
	})
	t.Run("text keys", func(t *testing.T) {
		for _, tc := range []struct{ name, fixture, want string }{
			{
				"content indexes",
				responseEvent("response.output_item.added", `,"output_index":0,"item":{"type":"message","id":"m","role":"assistant","content":[]}`) +
					responseEvent("response.output_text.delta", `,"output_index":0,"item_id":"m","content_index":1,"delta":"absent"`) +
					responseEvent("response.output_text.delta", `,"output_index":0,"item_id":"m","content_index":0,"delta":"conflict"`) +
					responseEvent("response.completed", `,"response":{"status":"completed","output":[{"type":"message","id":"m","role":"assistant","content":[{"type":"output_text","text":"final"}]}]}`),
				"streamed text/arguments conflict with final output",
			},
			{
				"kind before index",
				responseEvent("response.output_item.added", `,"output_index":0,"item":{"type":"reasoning","id":"r","summary":[]}`) +
					responseEvent("response.reasoning_summary_text.delta", `,"output_index":0,"item_id":"r","summary_index":0,"delta":"conflict"`) +
					responseEvent("response.reasoning_text.delta", `,"output_index":0,"item_id":"r","content_index":1,"delta":"absent"`) +
					responseEvent("response.completed", `,"response":{"status":"completed","output":[{"type":"reasoning","id":"r","encrypted_content":"cipher","summary":[{"type":"summary_text","text":"final"}]}]}`),
				"observed content absent from final output",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				c := newResponses(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, tc.fixture) })
				for i := 0; i < 100; i++ {
					_, err := c.StreamModel(t.Context(), api.ModelRequest{}, nil)
					if err == nil || err.Error() != "openai responses: stream: "+tc.want {
						t.Fatalf("iteration %d: text error was not sorted by kind/index: %v", i, err)
					}
				}
			})
		}
	})
	t.Run("argument numbers", func(t *testing.T) {
		for _, args := range []string{
			`{"a":9007199254740993,"b":1e400}`,
			`{"nested":{"a":9007199254740993,"b":1e400}}`,
			`{"nested":[{"a":9007199254740993,"b":1e400}]}`,
			`{"nested":[9007199254740993,1e400]}`,
		} {
			for _, streaming := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t", args, streaming), func(t *testing.T) {
					body := fmt.Sprintf(`{"status":"completed","output":[{"type":"function_call","id":"f","call_id":"c","name":"f","arguments":%q}]}`, args)
					if streaming {
						body = responseEvent("response.completed", `,"response":`+body)
					}
					c := newResponses(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })
					for i := 0; i < 100; i++ {
						var err error
						if streaming {
							_, err = c.StreamModel(t.Context(), api.ModelRequest{}, nil)
						} else {
							_, err = c.Model(t.Context(), api.ModelRequest{})
						}
						if err == nil || !strings.HasSuffix(err.Error(), "function arguments: integer outside exactly representable +/-2^53 domain") {
							t.Fatalf("iteration %d: first invalid number was not in key/array order: %v", i, err)
						}
					}
				})
			}
		}
	})
}

func TestResponsesHTTPDiagnosticReadFailure(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			c, err := openai.NewResponses(openai.WithModel("test-model"), openai.WithHTTPClient(&http.Client{Transport: responsesRoundTripper(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 400, Body: io.NopCloser(iotest.ErrReader(errors.New("private reader detail"))), Header: make(http.Header)}, nil
			})}))
			if err != nil {
				t.Fatal(err)
			}
			if streaming {
				_, err = c.StreamModel(t.Context(), api.ModelRequest{}, nil)
			} else {
				_, err = c.Model(t.Context(), api.ModelRequest{})
			}
			if err == nil || err.Error() != "openai responses: HTTP status 400" {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestResponsesHTTPDiagnosticIdentifiers(t *testing.T) {
	const status = "openai responses: HTTP status 400"
	const safe = `{"error":{"code":"invalid_value","param":"input[1].content[0].type","message":"DO NOT ECHO","type":"invalid_request_error","innererror":{"message":"DO NOT ECHO"}}}`
	cases := []struct{ name, body, suffix string }{
		{"safe", safe, " (code=invalid_value, param=input[1].content[0].type)"},
		{"null code", `{"error":{"code":null,"param":"input"}}`, " (param=input)"},
		{"null param", `{"error":{"code":"invalid_value","param":null}}`, " (code=invalid_value)"},
		{"wrong code type", `{"error":{"code":42,"param":"input"}}`, " (param=input)"},
		{"wrong param type", `{"error":{"code":"invalid_value","param":{}}}`, " (code=invalid_value)"},
		{"code boundary", `{"error":{"code":"` + strings.Repeat("c", 64) + `"}}`, " (code=" + strings.Repeat("c", 64) + ")"},
		{"param boundary", `{"error":{"param":"` + strings.Repeat("p", 128) + `"}}`, " (param=" + strings.Repeat("p", 128) + ")"},
		{"long code", `{"error":{"code":"` + strings.Repeat("c", 65) + `","param":"input"}}`, " (param=input)"},
		{"long param", `{"error":{"code":"invalid_value","param":"` + strings.Repeat("p", 129) + `"}}`, " (code=invalid_value)"},
		{"empty", `{"error":{"code":"","param":""}}`, ""},
		{"url", `{"error":{"code":"https://private.invalid","param":"input/private"}}`, ""},
		{"controls", `{"error":{"code":"bad\ncode","param":"input\u0000"}}`, ""},
		{"unicode", `{"error":{"code":"é","param":"输入"}}`, ""},
		{"hyphenated secret", `{"error":{"code":"sk-private","param":"sk-private"}}`, ""},
		{"malformed", `{"error":{"code":"invalid_value"}`, ""},
		{"trailing JSON", safe + `{}`, ""},
		{"envelope boundary", safe + strings.Repeat(" ", (16<<10)-len(safe)), " (code=invalid_value, param=input[1].content[0].type)"},
		{"oversize envelope", safe + strings.Repeat(" ", (16<<10)+1-len(safe)), ""},
	}
	for _, tc := range cases {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, streaming), func(t *testing.T) {
				c := newResponses(t, func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, tc.body)
				})
				var err error
				if streaming {
					_, err = c.StreamModel(t.Context(), api.ModelRequest{}, nil)
				} else {
					_, err = c.Model(t.Context(), api.ModelRequest{})
				}
				if err == nil || err.Error() != status+tc.suffix {
					t.Fatalf("err=%v, want %s", err, status+tc.suffix)
				}
			})
		}
	}
}
