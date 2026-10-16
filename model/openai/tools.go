package openai

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agent-primitives/sessions/api"
)

func toChatTools(defs []api.ToolDefinition) ([]chatToolDefinition, error) {
	var tools []chatToolDefinition
	for _, def := range defs {
		if def.Name == "" {
			return nil, fmt.Errorf("%w: tool name is required", ErrUnsupportedPart)
		}
		tool := chatToolDefinition{Type: "function"}
		tool.Function.Name, tool.Function.Description = def.Name, def.Description
		if def.InputSchema != nil {
			tool.Function.Parameters = &def.InputSchema // preserve explicit {} versus omitted
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

func toChatToolChoice(choice *api.ToolChoice) (any, error) {
	if choice == nil {
		return nil, nil
	}
	if choice.Name != "" {
		if choice.Mode != api.ToolChoiceRequired {
			return nil, fmt.Errorf("%w: named tool choice requires REQUIRED", ErrUnsupportedPart)
		}
		return map[string]any{"type": "function", "function": map[string]string{"name": choice.Name}}, nil
	}
	switch choice.Mode {
	case "":
		return nil, nil
	case api.ToolChoiceAuto:
		return "auto", nil
	case api.ToolChoiceNone:
		return "none", nil
	case api.ToolChoiceRequired:
		return "required", nil
	default:
		return nil, fmt.Errorf("%w: unknown tool choice mode", ErrUnsupportedPart)
	}
}

// Top-level reasoning remains journal-only provider state. Every other part must have a faithful
// Chat Completions representation; in particular, assistant text must precede its tool calls.
func toChatMessages(msgs []api.Message) ([]chatMessage, error) {
	out := make([]chatMessage, 0, len(msgs))
	for _, m := range msgs {
		role := m.Role
		if role == "" {
			role = "user"
		}
		if role == "tool" {
			if len(m.Parts) != 1 || !onePart(m.Parts[0]) || m.Parts[0].ToolResult == nil {
				return nil, fmt.Errorf("%w: tool message requires one tool result", ErrUnsupportedPart)
			}
			message, err := toolResultMessage(m.Parts[0].ToolResult)
			if err != nil {
				return nil, err
			}
			out = append(out, message)
			continue
		}
		message := chatMessage{Role: role}
		var text strings.Builder
		ids := map[string]bool{}
		for _, p := range m.Parts {
			if !onePart(p) {
				return nil, fmt.Errorf("%w: part must have exactly one known content field", ErrUnsupportedPart)
			}
			switch {
			case p.Text != nil:
				if len(message.ToolCalls) != 0 {
					return nil, fmt.Errorf("%w: text after a tool call cannot be represented", ErrUnsupportedPart)
				}
				text.WriteString(p.Text.Text)
			case p.Reasoning != nil:
				// Opaque replay state, not resent to this endpoint.
			case p.ToolCall != nil:
				call := p.ToolCall
				if role != "assistant" || call.ID == "" || call.Tool == "" || ids[call.ID] || call.Mediation != "" || call.IdempotencyKey != "" {
					return nil, fmt.Errorf("%w: assistant tool call requires unique ID/name and no execution metadata", ErrUnsupportedPart)
				}
				args := call.Args
				if args == nil {
					args = map[string]any{}
				}
				encoded, err := json.Marshal(args)
				if err != nil {
					return nil, fmt.Errorf("%w: encode tool arguments: %w", ErrUnsupportedPart, err)
				}
				wire := chatToolCall{ID: call.ID, Type: "function"}
				wire.Function.Name, wire.Function.Arguments = call.Tool, string(encoded)
				message.ToolCalls = append(message.ToolCalls, wire)
				ids[call.ID] = true
			default:
				return nil, fmt.Errorf("%w: only text, reasoning and assistant tool calls are supported here", ErrUnsupportedPart)
			}
		}
		message.Content = text.String()
		out = append(out, message)
	}
	return out, nil
}

func onePart(p api.Part) bool {
	n := 0
	for _, present := range []bool{p.Text != nil, p.Reasoning != nil, p.File != nil, p.Data != nil, p.ToolCall != nil, p.ToolResult != nil} {
		if present {
			n++
		}
	}
	return n == 1
}

func toolResultMessage(result *api.ToolResult) (chatMessage, error) {
	if result.ID == "" {
		return chatMessage{}, fmt.Errorf("%w: tool result ID is required", ErrUnsupportedPart)
	}
	// Content is explicitly model-facing. Never guess text from a durable receipt or fetch a URI;
	// the harness must render that content. Receipt metadata is not extra model text.
	if len(result.Content) == 0 && (result.Output != nil || result.OutputURI != "" || result.OutputDigest != "" || result.IsError || result.Error != "" || result.Code != api.ToolResultCodeUnspecified || result.ApprovalRequestSeq != 0 || result.ApprovalDecisionSeq != 0) {
		return chatMessage{}, fmt.Errorf("%w: nonempty tool receipt requires model-facing Content", ErrUnsupportedPart)
	}
	var text strings.Builder
	for _, p := range result.Content {
		if !onePart(p) || p.Text == nil {
			return chatMessage{}, fmt.Errorf("%w: tool result Content must be text-only", ErrUnsupportedPart)
		}
		text.WriteString(p.Text.Text)
	}
	return chatMessage{Role: "tool", Content: text.String(), ToolCallID: result.ID}, nil
}

func toolCallParts(calls []chatToolCall) ([]api.Part, error) {
	parts := make([]api.Part, 0, len(calls))
	ids := map[string]bool{}
	for _, call := range calls {
		if call.Type != "function" || call.ID == "" || call.Function.Name == "" || ids[call.ID] {
			return nil, fmt.Errorf("%w: completion tool call requires function type and unique ID/name", ErrUnsupportedPart)
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return nil, fmt.Errorf("%w: decode tool arguments: %w", ErrUnsupportedPart, err)
		}
		if args == nil {
			return nil, fmt.Errorf("%w: tool arguments must be a JSON object", ErrUnsupportedPart)
		}
		parts = append(parts, api.Part{ToolCall: &api.ToolCall{ID: call.ID, Tool: call.Function.Name, Args: args}})
		ids[call.ID] = true
	}
	return parts, nil
}

// Only accept a finalized tool completion, never a truncated-but-parseable call or empty intent.
func checkToolFinish(reason string, count int) error {
	if reason == "function_call" || (reason == "tool_calls") != (count > 0) {
		return fmt.Errorf("%w: incomplete or unsupported tool completion", ErrUnsupportedPart)
	}
	return nil
}

func legacyFunctionCall(raw json.RawMessage) bool {
	return len(raw) != 0 && string(raw) != "null"
}

type toolDeltaBuffer struct {
	kind, id, name string
	args           strings.Builder
}

func appendToolDeltas(calls map[int]*toolDeltaBuffer, deltas []chatToolDelta) error {
	for _, d := range deltas {
		if d.Index == nil || *d.Index < 0 || d.Type != "" && d.Type != "function" {
			return fmt.Errorf("%w: tool delta requires a nonnegative index and function type", ErrUnsupportedPart)
		}
		call := calls[*d.Index]
		if call == nil {
			call = &toolDeltaBuffer{}
			calls[*d.Index] = call
		}
		// ID/name are metadata, not argument fragments. Identical repeats are harmless; a
		// changed identity at the same index must not be guessed or concatenated into a new call.
		if (d.ID != "" && call.id != "" && d.ID != call.id) ||
			(d.Function.Name != "" && call.name != "" && d.Function.Name != call.name) {
			return fmt.Errorf("%w: tool call identity changed within an index", ErrUnsupportedPart)
		}
		if d.Type != "" {
			call.kind = d.Type
		}
		if d.ID != "" {
			call.id = d.ID
		}
		if d.Function.Name != "" {
			call.name = d.Function.Name
		}
		call.args.WriteString(d.Function.Arguments)
	}
	return nil
}

// ---- Chat Completions function-tool wire types ----

type chatToolDefinition struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  *map[string]any `json:"parameters,omitempty"`
	} `json:"function"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatToolDelta struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
