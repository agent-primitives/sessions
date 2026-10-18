package conformance_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/model/openai"
)

type responsesPrivacyHarness struct{ request api.ModelRequest }

func (responsesPrivacyHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "responses-privacy"}, nil
}

func (h responsesPrivacyHarness) Run(ctx context.Context, _ *api.Start, sink api.EventSink) error {
	_, err := sink.Model(ctx, h.request)
	return err
}

type responsesPrivacyTransport func(*http.Request) (*http.Response, error)

func (f responsesPrivacyTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type responsesPrivacyReader struct{ message string }

func (r responsesPrivacyReader) Read([]byte) (int, error) { return 0, errors.New(r.message) }

// Raw decoder, provider and transport errors must not cross the model seam into durable ERROR text.
func TestResponsesDiagnosticPrivacy(t *testing.T) {
	const marker = "fictional-private-payload-marker"
	const number = "987654321098765432109876543210e10000"
	prefix := "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\n"
	cases := []struct {
		name, body, want string
		status           int
		streamOnly       bool
		transportError   bool
		readerError      bool
		request          api.ModelRequest
		noHTTP           bool
	}{
		{name: "usage range", body: `{"status":"completed","output":[],"usage":{"input_tokens":` + number + `}}`, want: "decode response"},
		{name: "unknown status", body: `{"status":"` + marker + `","output":[]}`, want: "not a completed output"},
		{name: "unknown output type", body: `{"status":"completed","output":[{"type":"` + marker + `","id":"item"}]}`, want: "unsupported output item type"},
		{name: "unknown output field", body: `{"status":"completed","output":[{"type":"message","id":"m","role":"assistant","content":[],"` + marker + `":true}]}`, want: "unsupported item/content field"},
		{name: "unknown content field", body: `{"status":"completed","output":[{"type":"message","id":"m","role":"assistant","content":[{"type":"output_text","text":"fictional","` + marker + `":true}]}]}`, want: "unsupported item/content field"},
		{name: "HTTP JSON body", body: `{"error":{"message":"` + marker + `"}}`, status: 503, want: "HTTP status 503"},
		{name: "HTTP raw body", body: marker, status: 429, want: "HTTP status 429"},
		{name: "HTTP bounded identifiers", body: `{"error":{"code":"invalid_value","param":"input[1].content[0].type","message":"` + marker + `"}}`, status: 400, want: "HTTP status 400 (code=invalid_value, param=input[1].content[0].type)"},
		{name: "HTTP unsafe identifiers", body: `{"error":{"code":"` + marker + `","param":"https://private.invalid/` + marker + `"}}`, status: 400, want: "HTTP status 400"},
		{name: "HTTP oversized identifiers", body: `{"error":{"code":"` + strings.Repeat("x", 65) + `","param":"` + strings.Repeat("y", 129) + `","message":"` + marker + `"}}`, status: 400, want: "HTTP status 400"},
		{name: "failed message", body: "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"" + marker + "\"}}}\n\n", streamOnly: true, want: "terminal response.failed"},
		{name: "incomplete reason", body: "data: {\"type\":\"response.incomplete\",\"response\":{\"incomplete_details\":{\"reason\":\"" + marker + "\"}}}\n\n", streamOnly: true, want: "terminal response.incomplete"},
		{name: "provider error event", body: "data: {\"type\":\"error\",\"message\":\"" + marker + "\"}\n\n", streamOnly: true, want: "terminal error"},
		{name: "stream index range", body: "data: {\"type\":\"response.output_item.added\",\"output_index\":" + number + "}\n\n", streamOnly: true, want: "malformed event"},
		{name: "stream content index range", body: prefix + "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"item_id\":\"m\",\"content_index\":" + number + ",\"delta\":\"fictional\"}\n\n", streamOnly: true, want: "malformed event"},
		{name: "unknown event", body: prefix + "data: {\"type\":\"" + marker + "\",\"output_index\":0,\"item_id\":\"m\"}\n\n", streamOnly: true, want: "unsupported semantic event"},
		{name: "transport detail", transportError: true, want: "request failed"},
		{name: "reader detail", readerError: true, want: "read"},
		{name: "unknown parameter", request: api.ModelRequest{Params: map[string]string{marker: "fictional"}}, noHTTP: true, want: "parameter"},
		{name: "unknown role", request: api.ModelRequest{Messages: []api.Message{*api.TextMessage(marker, "fictional")}}, noHTTP: true, want: "role"},
		{name: "unknown choice", request: api.ModelRequest{ToolChoice: &api.ToolChoice{Mode: api.ToolChoiceMode(marker)}}, noHTTP: true, want: "tool choice mode"},
	}
	for _, tc := range cases {
		for _, streaming := range []bool{false, true} {
			if tc.streamOnly && !streaming {
				continue
			}
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, streaming), func(t *testing.T) {
				var hits, tools atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					hits.Add(1)
					if tc.status != 0 {
						w.WriteHeader(tc.status)
						fmt.Fprint(w, tc.body)
					} else if tc.streamOnly {
						fmt.Fprint(w, tc.body)
					} else if streaming {
						fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", tc.body)
					} else {
						fmt.Fprint(w, tc.body)
					}
				}))
				defer server.Close()
				options := []openai.Option{openai.WithModel("fictional-model"), openai.WithBaseURL(server.URL + "/" + marker)}
				if tc.transportError || tc.readerError {
					options = append(options, openai.WithHTTPClient(&http.Client{Transport: responsesPrivacyTransport(func(*http.Request) (*http.Response, error) {
						hits.Add(1)
						if tc.transportError {
							return nil, errors.New(marker)
						}
						return &http.Response{StatusCode: 200, Body: io.NopCloser(responsesPrivacyReader{message: marker}), Header: make(http.Header)}, nil
					})}))
				}
				client, err := openai.NewResponses(options...)
				if err != nil {
					t.Fatal(err)
				}
				store, _ := openFile(t)
				defer store.Close()
				journal := store.Session("privacy")
				opts := []controller.Option{controller.WithSessionUID("privacy"), controller.WithToolExecutor(func(context.Context, controller.ToolCallContext, api.ToolCall) (api.ToolResult, error) {
					tools.Add(1)
					return api.ToolResult{}, nil
				})}
				if streaming {
					opts = append(opts, controller.WithStreamingModel(client.StreamModel))
				}
				c, err := controller.New(journal, client.Model, opts...)
				if err != nil {
					t.Fatal(err)
				}
				err = c.Exec(t.Context(), responsesPrivacyHarness{request: tc.request}, nil, 0)
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("expected %q failure, got %v", tc.want, err)
				}
				checkPrivate := func(text string) {
					t.Helper()
					if strings.Contains(text, marker) || strings.Contains(text, number) || strings.Contains(text, server.URL) {
						t.Error("diagnostic exposed payload or endpoint data")
					}
				}
				checkPrivate(err.Error())
				records, err := journal.Read(1)
				if err != nil {
					t.Fatal(err)
				}
				errorCount := 0
				for _, record := range records {
					switch record.Event.Kind {
					case api.EventError:
						errorCount++
						checkPrivate(record.Event.Err.Description)
						if !strings.Contains(record.Event.Err.Description, tc.want) {
							t.Fatalf("journal error lost diagnostic category: %s", record.Event.Err.Description)
						}
					case api.EventOutput, api.EventToolCall, api.EventToolResult, api.EventEnd:
						t.Fatalf("failed call recorded %s", record.Event.Kind)
					}
				}
				wantHits := int64(1)
				if tc.noHTTP {
					wantHits = 0
				}
				if errorCount != 1 || hits.Load() != wantHits || tools.Load() != 0 {
					t.Fatalf("errors=%d requests=%d tools=%d", errorCount, hits.Load(), tools.Load())
				}
				if err := journal.Verify(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
