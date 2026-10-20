package conformance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/model/openai"
	"github.com/agent-primitives/sessions/sqlitelog"
)

type modelSelectionHarness []string

func (modelSelectionHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "model-selection", Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}}, nil
}

func (h modelSelectionHarness) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	for _, model := range h {
		if _, err := sink.Model(ctx, api.ModelRequest{Model: model, Messages: start.Inputs}); err != nil {
			return err
		}
	}
	return nil
}

var modelSelectionPaths = []struct {
	name      string
	responses bool
	streaming bool
}{
	{"chat/plain", false, false}, {"chat/stream", false, true},
	{"responses/plain", true, false}, {"responses/stream", true, true},
}

// A real adapter and HTTP server keep model selection observable beyond the ModelFunc boundary.
func modelSelectionClient(t *testing.T, responses, streaming bool, refuseEcho *atomic.Bool) (controller.ModelFunc, []controller.Option, chan string) {
	t.Helper()
	models := make(chan string, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if body.Stream != streaming {
			t.Errorf("HTTP stream = %v, want %v", body.Stream, streaming)
		}
		models <- body.Model
		if body.Model == "echo" && refuseEcho != nil && refuseEcho.Swap(false) {
			http.Error(w, `{"error":{"code":"model_not_found","message":"unknown model"}}`, http.StatusBadRequest)
			return
		}
		if responses {
			fixture := `{"id":"resp_1","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"answer","annotations":[]}]}]}`
			if streaming {
				writeResponsesStream(w, fixture)
			} else {
				fmt.Fprint(w, fixture)
			}
		} else if streaming {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		} else {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}]}`)
		}
	}))
	t.Cleanup(server.Close)
	var model controller.ModelFunc
	var stream controller.StreamFunc
	var err error
	options := []openai.Option{openai.WithBaseURL(server.URL), openai.WithModel("configured-provider")}
	if responses {
		var client *openai.ResponsesClient
		client, err = openai.NewResponses(options...)
		if err == nil {
			model, stream = client.Model, client.StreamModel
		}
	} else {
		var client *openai.Client
		client, err = openai.New(options...)
		if err == nil {
			model, stream = client.Model, client.StreamModel
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	var opts []controller.Option
	if streaming {
		opts = append(opts, controller.WithStreamingModel(stream))
		model = func(ctx context.Context, req api.ModelRequest) (api.ModelResponse, error) {
			return stream(ctx, req, func(string) {})
		}
	}
	return model, opts, models
}

func checkHTTPModels(t *testing.T, models chan string, want ...string) {
	t.Helper()
	var got []string
	for len(models) > 0 {
		got = append(got, <-models)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("HTTP models = %q, want %q", got, want)
	}
}

func checkJournalModels(t *testing.T, log eventlog.Store, want ...string) []eventlog.Record {
	t.Helper()
	records, err := log.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range records {
		if r.Event.ModelCall != nil {
			got = append(got, r.Event.ModelCall.Model)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("journal models = %q, want %q", got, want)
	}
	if err := log.Verify(); err != nil {
		t.Fatal(err)
	}
	return records
}

func checkModelSelectionReplay(t *testing.T, c *controller.Controller, log eventlog.Store, h modelSelectionHarness, models chan string) {
	t.Helper()
	before, err := log.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Replay(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	checkHTTPModels(t, models) // Replay must never reach the actual HTTP boundary.
	after, err := log.Read(1)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("replay changed journal: err=%v", err)
	}
}

// Catches substituting the adapter fallback for an admitted literal, and sticky exact selection
// leaking into the next no-policy execution on the same adapter. WithModelDefault alone is inert.
func TestControllerModelSelection(t *testing.T) {
	for _, path := range modelSelectionPaths {
		t.Run(path.name, func(t *testing.T) {
			for _, tc := range []struct{ name, requested, defaultModel, want string }{
				{"explicit-echo", "echo", "session-default", "echo"},
				{"empty-uses-echo-default", "", "echo", "echo"},
				{"ordinary-override", "ordinary-model", "session-default", "ordinary-model"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					model, opts, models := modelSelectionClient(t, path.responses, path.streaming, nil)
					store, _ := openFile(t)
					defer store.Close()
					log := store.Session("policy")
					policyOpts := append([]controller.Option{}, opts...)
					policyOpts = append(policyOpts, controller.WithModelPolicy(api.ModelPolicy{Default: tc.defaultModel, Allowed: []string{"echo", "session-default", "ordinary-model"}}))
					c, err := controller.New(log, model, policyOpts...)
					if err != nil {
						t.Fatal(err)
					}
					h := modelSelectionHarness{tc.requested}
					if err := c.Exec(t.Context(), h, nil, 0); err != nil {
						t.Fatal(err)
					}
					checkHTTPModels(t, models, tc.want)
					checkJournalModels(t, log, tc.want)
					checkModelSelectionReplay(t, c, log, h, models)

					legacyLog := store.Session("no-policy")
					legacyOpts := append(append([]controller.Option{}, opts...), controller.WithModelDefault("echo"))
					legacy, err := controller.New(legacyLog, model, legacyOpts...)
					if err != nil {
						t.Fatal(err)
					}
					legacyHarness := modelSelectionHarness{"echo", ""}
					if err := legacy.Exec(t.Context(), legacyHarness, nil, 0); err != nil {
						t.Fatal(err)
					}
					checkHTTPModels(t, models, "configured-provider", "configured-provider")
					checkJournalModels(t, legacyLog, "echo", "")
					checkModelSelectionReplay(t, legacy, legacyLog, legacyHarness, models)
				})
			}
		})
	}
}

// The outer policy call supplies ambient exact metadata. A nested no-policy controller must
// explicitly mask it rather than letting an unrelated invocation inherit policy behavior.
func TestControllerModelSelectionMasksAmbientExact(t *testing.T) {
	for _, path := range modelSelectionPaths {
		t.Run(path.name, func(t *testing.T) {
			model, opts, models := modelSelectionClient(t, path.responses, path.streaming, nil)
			legacyLog := eventlog.AsStore(eventlog.New())
			legacy, err := controller.New(legacyLog, model, opts...)
			if err != nil {
				t.Fatal(err)
			}
			nested := func(ctx context.Context, req api.ModelRequest) (api.ModelResponse, error) {
				if err := legacy.Exec(ctx, modelSelectionHarness{"echo", ""}, nil, 0); err != nil {
					return api.ModelResponse{}, err
				}
				return model(ctx, req)
			}
			outerOpts := []controller.Option{controller.WithModelPolicy(api.ModelPolicy{Default: "echo", Allowed: []string{"echo"}})}
			if path.streaming {
				// The nested call still uses the real streaming adapter via legacy's options.
				outerOpts = append(outerOpts, controller.WithStreamingModel(func(ctx context.Context, req api.ModelRequest, _ func(string)) (api.ModelResponse, error) {
					return nested(ctx, req)
				}))
			}
			outer, err := controller.New(eventlog.AsStore(eventlog.New()), nested, outerOpts...)
			if err != nil {
				t.Fatal(err)
			}
			if err := outer.Exec(t.Context(), modelSelectionHarness{"echo"}, nil, 0); err != nil {
				t.Fatal(err)
			}
			checkHTTPModels(t, models, "configured-provider", "configured-provider", "echo")
			checkJournalModels(t, legacyLog, "echo", "")
		})
	}
}

// A provider may not serve the literal policy model. Refusal must leave an unanswered intent,
// not a successful substituted turn; retrying after the provider is fixed answers that same call.
func TestControllerModelSelectionProviderRefusalRetry(t *testing.T) {
	for _, path := range modelSelectionPaths {
		t.Run(path.name, func(t *testing.T) {
			var refuseEcho atomic.Bool
			refuseEcho.Store(true)
			model, opts, models := modelSelectionClient(t, path.responses, path.streaming, &refuseEcho)
			log := eventlog.AsStore(eventlog.New())
			policyOpts := append(append([]controller.Option{}, opts...), controller.WithModelPolicy(api.ModelPolicy{Default: "echo", Allowed: []string{"echo"}}))
			c, err := controller.New(log, model, policyOpts...)
			if err != nil {
				t.Fatal(err)
			}
			h := modelSelectionHarness{""}
			if err := c.Exec(t.Context(), h, nil, 0); err == nil {
				t.Fatal("provider refusal unexpectedly completed the execution")
			}
			checkHTTPModels(t, models, "echo")
			before := checkJournalModels(t, log, "echo")
			if len(before) != 3 || before[2].Event.Kind != api.EventError {
				t.Fatalf("refusal must leave START, MODEL_CALL, ERROR; got %#v", before)
			}
			resuming, err := controller.New(log, model, opts...)
			if err != nil {
				t.Fatal(err)
			}
			if resumed, err := resuming.Resume(t.Context(), h); !resumed || err != nil {
				t.Fatalf("retry Resume = %v, %v", resumed, err)
			}
			checkHTTPModels(t, models, "echo")
			after := checkJournalModels(t, log, "echo")
			if len(after) != 5 || after[3].Event.Kind != api.EventOutput || after[4].Event.Kind != api.EventEnd || !reflect.DeepEqual(before, after[:3]) {
				t.Fatal("retry did not answer the original intent and preserve its prefix")
			}
			checkModelSelectionReplay(t, resuming, log, h, models)
		})
	}
}

// Catches deriving exact selection from today's policy rather than the invocation's recorded
// default, both when re-driving an unanswered intent and when advancing past a completed prefix.
func TestControllerModelSelectionResumeWithoutPolicy(t *testing.T) {
	for _, path := range modelSelectionPaths {
		t.Run(path.name, func(t *testing.T) {
			for _, cut := range []struct {
				name    string
				fromSeq int64
			}{{"unanswered-intent", 3}, {"completed-prefix-new-call", 4}} {
				t.Run(cut.name, func(t *testing.T) {
					model, opts, models := modelSelectionClient(t, path.responses, path.streaming, nil)
					store, dbPath := openFile(t)
					log := store.Session("recovery")
					policyOpts := append(append([]controller.Option{}, opts...), controller.WithModelPolicy(api.ModelPolicy{Default: "echo", Allowed: []string{"echo"}}))
					c, err := controller.New(log, model, policyOpts...)
					if err != nil {
						t.Fatal(err)
					}
					h := modelSelectionHarness{"", "echo"}
					if err := c.Exec(t.Context(), h, nil, 0); err != nil {
						t.Fatal(err)
					}
					checkHTTPModels(t, models, "echo", "echo")
					before := checkJournalModels(t, log, "echo", "echo")
					if err := store.Close(); err != nil {
						t.Fatal(err)
					}
					// No INPUT: START=1, first CALL=2, first OUTPUT=3, next CALL=4.
					truncateFrom(t, dbPath, "recovery", cut.fromSeq)
					store, err = sqlitelog.Open(dbPath)
					if err != nil {
						t.Fatal(err)
					}
					defer store.Close()
					log = store.Session("recovery")
					resuming, err := controller.New(log, model, opts...) // Today's host removed policy.
					if err != nil {
						t.Fatal(err)
					}
					if resumed, err := resuming.Resume(t.Context(), h); !resumed || err != nil {
						t.Fatalf("Resume = %v, %v", resumed, err)
					}
					wantHTTP := []string{"echo"}
					if cut.fromSeq == 3 {
						wantHTTP = append(wantHTTP, "echo")
					}
					checkHTTPModels(t, models, wantHTTP...)
					after := checkJournalModels(t, log, "echo", "echo")
					if !reflect.DeepEqual(before[:cut.fromSeq-1], after[:cut.fromSeq-1]) {
						t.Fatal("Resume changed recorded prefix")
					}
					checkModelSelectionReplay(t, resuming, log, h, models)
				})
			}
		})
	}
}
