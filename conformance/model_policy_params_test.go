package conformance_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/model/openai"
)

type modelParamsHarness struct{ model string }

func (modelParamsHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "model-params"}, nil
}

func (h modelParamsHarness) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	if start.Model != "allowed" {
		return fmt.Errorf("Start.Model = %q, want allowed", start.Model)
	}
	_, err := sink.Model(ctx, api.ModelRequest{Model: h.model, Params: map[string]string{"temperature": "0.5"}})
	return err
}

// Policy admission must not bypass Chat's Params refusal; a forbidden model must still refuse
// before intent, even when the request would also fail adapter admission.
func TestModelPolicyPreservesChatParamsRefusal(t *testing.T) {
	for _, tc := range []struct {
		name, model string
		want        error
		intents     int
	}{
		{"default", "", openai.ErrUnsupportedPart, 1},
		{"forbidden override", "forbidden", api.ErrModelNotAllowed, 0},
	} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, streaming), func(t *testing.T) {
				var providerCalls atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					providerCalls.Add(1)
					_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"unexpected"},"finish_reason":"stop"}]}`)
				}))
				defer server.Close()
				client, err := openai.New(openai.WithBaseURL(server.URL), openai.WithModel("adapter-fallback"), openai.WithHTTPClient(server.Client()))
				if err != nil {
					t.Fatal(err)
				}
				opts := []controller.Option{controller.WithModelPolicy(api.ModelPolicy{Default: "allowed", Allowed: []string{"allowed"}})}
				if streaming {
					opts = append(opts, controller.WithStreamingModel(client.StreamModel))
				}
				log := eventlog.AsStore(eventlog.New())
				c, err := controller.New(log, client.Model, opts...)
				if err != nil {
					t.Fatal(err)
				}
				if err := c.Exec(t.Context(), modelParamsHarness{model: tc.model}, nil, 0); !errors.Is(err, tc.want) {
					t.Fatalf("Exec = %v, want %v", err, tc.want)
				}
				records, err := log.Read(1)
				if err != nil {
					t.Fatal(err)
				}
				intents, failures := 0, 0
				for _, r := range records {
					switch r.Event.Kind {
					case api.EventModelCall:
						intents++
						if r.Event.ModelCall.Model != "allowed" {
							t.Fatalf("intent model = %q", r.Event.ModelCall.Model)
						}
					case api.EventError:
						failures++
					case api.EventOutput, api.EventToolCall, api.EventToolResult, api.EventEnd:
						t.Fatalf("refused request recorded %s", r.Event.Kind)
					}
				}
				if intents != tc.intents || failures != 1 || providerCalls.Load() != 0 {
					t.Fatalf("intents/errors/provider calls = %d/%d/%d", intents, failures, providerCalls.Load())
				}
				if err := log.Verify(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
