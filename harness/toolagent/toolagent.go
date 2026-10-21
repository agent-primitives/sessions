// Package toolagent provides a reference harness that lets the model call tools, each one gated
// on a human decision. Every call it makes is REQUIRES_APPROVAL: the host parks the turn, and only
// an approved call reaches the host's tool executor. The harness never runs a tool itself, holds
// no credentials, and keeps no state beyond the journal, so it is STATELESS_REPLAY.
//
// Mediation is fixed by the harness, not by per-execution config: a caller that could choose it
// could also skip the approval.
package toolagent

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/agent-primitives/sessions/api"
)

// MaxModelCalls bounds the model calls in one turn, so a model that keeps calling tools cannot
// loop forever. A turn that reaches it fails.
const MaxModelCalls = 8

// ErrTooManyModelCalls is returned when a turn reaches MaxModelCalls without a final answer.
var ErrTooManyModelCalls = errors.New("toolagent: too many model calls in one turn")

// ErrInvalidToolCall is returned for a model tool call the harness cannot gate: no call ID, or an
// ID the turn already used. The host would refuse the call anyway; this names the cause.
var ErrInvalidToolCall = errors.New("toolagent: model tool call needs a unique ID")

// Harness offers Tools to the model on every call, turns each tool call in a completion into a
// REQUIRES_APPROVAL ToolCall, and feeds the result back until the model answers without one.
type Harness struct {
	Model string
	// Tools are offered to the model in this order. Each name needs a host executor route for an
	// approved call to run.
	Tools []api.ToolDefinition
}

var _ api.Harness = Harness{}

// PublishTool is the tool the documented executor example (docs/tool-executor-example.py) serves:
// publish one text. harnessnode offers it with HARNESS_KIND=tool. Each call returns a new value, so
// no caller can change another's schema.
func PublishTool() api.ToolDefinition {
	return api.ToolDefinition{
		Name:        "publish",
		Description: "Publish a short text. Each call needs a human approval before it runs.",
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"text": map[string]any{"type": "string", "description": "The text to publish."}},
			"required":             []any{"text"},
			"additionalProperties": false,
		},
	}
}

// Describe declares the model, the gated tools and the stateless replay contract.
func (h Harness) Describe(context.Context) (api.Descriptor, error) {
	specs := make([]api.ToolSpec, 0, len(h.Tools))
	for _, tool := range h.Tools {
		specs = append(specs, api.ToolSpec{Name: tool.Name, Description: tool.Description, Mediation: api.MediationRequiresApproval})
	}
	return api.Descriptor{
		ID:      "tool",
		Version: "1", // bump when execution or config interpretation changes, not for host releases
		Models:  []string{h.Model},
		Tools:   specs,
		Capabilities: api.Capabilities{
			Resumability: api.ResumabilityStatelessReplay,
			ForkSafe:     true,
		},
	}, nil
}

// Run builds the context from the journal, then alternates model calls and gated tool calls. On
// Resume the host serves recorded completions and receipts, so the same calls are re-emitted with
// the same IDs, keys and arguments; only an approved call that has no receipt runs.
func (h Harness) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	prompt, err := parseSystemPrompt(start.Config)
	if err != nil {
		return err
	}
	messages := make([]api.Message, 0, 1+len(start.History)+len(start.Inputs))
	if prompt != "" {
		messages = append(messages, *api.TextMessage("system", prompt))
	}
	messages = append(messages, historyMessages(start.History)...)
	messages = append(messages, start.Inputs...)
	model := start.Model
	if model == "" {
		model = h.Model
	}
	used := map[string]bool{}
	for range MaxModelCalls {
		resp, err := sink.Model(ctx, api.ModelRequest{Model: model, Messages: messages, Tools: h.Tools})
		if err != nil {
			return err
		}
		var calls []*api.ToolCall
		for _, part := range resp.Message.Parts {
			if part.ToolCall != nil {
				calls = append(calls, part.ToolCall)
			}
		}
		if len(calls) == 0 {
			return nil // the host recorded the completion as the turn's output
		}
		messages = append(messages, resp.Message)
		for _, requested := range calls {
			if requested.ID == "" || used[requested.ID] {
				return ErrInvalidToolCall
			}
			used[requested.ID] = true
			// Copy: the completion's part can share this pointer with the recorded output.
			call := *requested
			call.Mediation = api.MediationRequiresApproval
			call.IdempotencyKey = idempotencyKey(start.ExecutionID, call.ID)
			result, err := sink.ToolCall(ctx, call)
			if err != nil {
				return err
			}
			messages = append(messages, toolMessage(call.ID, result))
		}
	}
	return ErrTooManyModelCalls
}

// idempotencyKey is unique per session: a model's call IDs need not be unique across turns, and
// the executor deduplicates on session UID plus key.
func idempotencyKey(executionID, callID string) string {
	return executionID + "/" + callID
}

// toolMessage renders a result for the model: text Content as is, otherwise the error or the
// output as JSON. Receipt fields (codes, approval sequence numbers) are journal evidence, not
// model input.
func toolMessage(id string, result api.ToolResult) api.Message {
	text := ""
	for _, part := range result.Content {
		if part.Text != nil {
			text += part.Text.Text
		}
	}
	if text == "" {
		switch {
		case result.IsError && result.Error != "":
			text = "Tool error: " + result.Error
		case result.IsError:
			text = "Tool error."
		case result.Output != nil:
			// encoding/json sorts map keys, so a replayed turn renders the same request.
			if data, err := json.Marshal(result.Output); err == nil {
				text = string(data)
			} else {
				text = "Tool returned output that is not JSON."
			}
		default:
			text = "Tool returned no output."
		}
	}
	return api.Message{Role: "tool", Parts: []api.Part{{ToolResult: &api.ToolResult{ID: id, IsError: result.IsError, Content: api.TextMessage("tool", text).Parts}}}}
}

// historyMessages rebuilds earlier turns for the model: inputs, completions and the results of
// their tool calls. A tool call with no result in the journal (a turn that failed or is still
// parked) is dropped from its completion, because a provider refuses an unanswered call. Each
// result answers one call: a later completion in the same execution that reused an answered ID
// failed its turn (Run refuses it after the host journaled it), so it is dropped too.
func historyMessages(history []api.Event) []api.Message {
	type callKey struct{ execution, id string }
	answered := map[callKey]api.ToolResult{}
	for _, event := range history {
		if event.Kind == api.EventToolResult && event.Result != nil {
			answered[callKey{event.ExecutionID, event.Result.ID}] = *event.Result
		}
	}
	offered := map[callKey]bool{}
	var messages []api.Message
	for _, event := range history {
		switch event.Kind {
		case api.EventInput:
			if event.Message != nil {
				messages = append(messages, *event.Message)
			}
		case api.EventOutput:
			if event.Message == nil {
				continue
			}
			message := api.Message{Role: event.Message.Role}
			for _, part := range event.Message.Parts {
				if part.ToolCall != nil {
					key := callKey{event.ExecutionID, part.ToolCall.ID}
					if _, ok := answered[key]; !ok || offered[key] {
						continue
					}
					offered[key] = true
				}
				message.Parts = append(message.Parts, part)
			}
			if len(message.Parts) > 0 {
				messages = append(messages, message)
			}
		case api.EventToolResult:
			key := callKey{event.ExecutionID, ""}
			if event.Result != nil {
				key.id = event.Result.ID
			}
			if offered[key] {
				messages = append(messages, toolMessage(key.id, answered[key]))
				delete(offered, key)
				delete(answered, key)
			}
		}
	}
	return messages
}

func parseSystemPrompt(config []byte) (string, error) {
	if len(config) == 0 {
		return "", nil
	}
	// Retain unknown values as raw JSON so future fields are ignored, not interpreted.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(config, &fields); err != nil || fields == nil {
		// Decoder errors can include config content; only fixed diagnostics may be journaled.
		return "", errors.New("toolagent: config must be a JSON object")
	}
	raw, ok := fields["system_prompt"]
	if !ok {
		return "", nil
	}
	var prompt *string
	if err := json.Unmarshal(raw, &prompt); err != nil || prompt == nil {
		return "", errors.New("toolagent: system_prompt must be a string")
	}
	return *prompt, nil
}
