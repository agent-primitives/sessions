package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"

	"github.com/agent-primitives/sessions/api"
)

func responsesUnsupported(format string, args ...any) error {
	return fmt.Errorf("%w: Responses: %s", ErrUnsupportedPart, fmt.Sprintf(format, args...))
}

func (c *ResponsesClient) buildResponsesRequest(ctx context.Context, req api.ModelRequest, stream bool) (string, []byte, error) {
	model := c.client.requestModel(ctx, req.Model)
	body := map[string]any{"model": model, "store": false, "include": []string{"reasoning.encrypted_content"}}
	if stream {
		body["stream"] = true
	}
	for _, key := range slices.Sorted(maps.Keys(req.Params)) {
		value := req.Params[key]
		switch key {
		case "reasoning_effort":
			switch value {
			case "none", "minimal", "low", "medium", "high", "xhigh", "max":
				body["reasoning"] = map[string]string{"effort": value}
			default:
				return model, nil, responsesUnsupported("invalid reasoning_effort")
			}
		case "max_output_tokens":
			if value == "" || strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
				return model, nil, responsesUnsupported("max_output_tokens must be a positive decimal integer")
			}
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n <= 0 {
				return model, nil, responsesUnsupported("max_output_tokens must be a positive int64")
			}
			body[key] = n
		default:
			return model, nil, responsesUnsupported("unsupported parameter")
		}
	}
	if err := validateTools(req.Tools, req.ToolChoice); err != nil {
		return model, nil, responsesUnsupported("%v", err)
	}
	tools := make([]map[string]any, 0, len(req.Tools))
	for _, tool := range req.Tools {
		t := map[string]any{"type": "function", "name": tool.Name, "description": tool.Description, "strict": false}
		if tool.InputSchema != nil {
			raw, err := responsesMarshal(tool.InputSchema)
			if err != nil {
				return model, nil, responsesUnsupported("tool schema: %v", err)
			}
			if _, err := toolArguments(string(raw)); err != nil {
				return model, nil, responsesUnsupported("tool schema: %v", err)
			}
			t["parameters"] = tool.InputSchema
		}
		tools = append(tools, t)
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	if choice := req.ToolChoice; choice != nil {
		if choice.Name != "" {
			body["tool_choice"] = map[string]string{"type": "function", "name": choice.Name}
		} else {
			switch choice.Mode {
			case "":
			case api.ToolChoiceAuto, api.ToolChoiceNone, api.ToolChoiceRequired:
				body["tool_choice"] = strings.ToLower(string(choice.Mode))
			}
		}
	}
	input, err := responsesInput(model, req.Messages)
	if err != nil {
		return model, nil, err
	}
	raw, err := responsesMarshal(body)
	if err != nil {
		return model, nil, responsesUnsupported("encode request: %v", err)
	}
	// Marshal would compact and HTML-escape json.RawMessage. Append the validated input
	// items directly so opaque reasoning is replayed byte-for-byte, not reconstructed.
	raw = append(raw[:len(raw)-1], `,"input":[`...)
	for i, item := range input {
		if i > 0 {
			raw = append(raw, ',')
		}
		raw = append(raw, item...)
	}
	raw = append(raw, ']', '}')
	return model, raw, nil
}

func responsesPartKinds(p api.Part) int {
	n := 0
	for _, set := range []bool{p.Text != nil, p.File != nil, p.Data != nil, p.Reasoning != nil, p.ToolCall != nil, p.ToolResult != nil} {
		if set {
			n++
		}
	}
	return n
}

func responsesInput(model string, messages []api.Message) ([][]byte, error) {
	input := [][]byte{}
	calls, results, reasoningIDs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	add := func(item any) error {
		raw, err := responsesMarshal(item)
		if err == nil {
			input = append(input, raw)
		}
		return err
	}
	for mi, m := range messages {
		role := m.Role
		if role == "" {
			role = "user"
		}
		switch role {
		case "system", "user", "assistant", "tool":
			if role == "tool" && len(m.Parts) == 0 {
				return nil, responsesUnsupported("message %d requires model-facing tool results", mi)
			}
		default:
			return nil, responsesUnsupported("message %d role is unsupported", mi)
		}
		content := []map[string]string{}
		flush := func() error {
			if len(content) == 0 {
				return nil
			}
			err := add(map[string]any{"type": "message", "role": role, "content": content})
			content = nil
			return err
		}
		lastOrdinal := int32(-1)
		for pi, p := range m.Parts {
			if responsesPartKinds(p) != 1 {
				return nil, responsesUnsupported("message %d part %d must have exactly one kind", mi, pi)
			}
			if p.Text != nil && role != "tool" {
				kind := "input_text"
				if role == "assistant" {
					kind = "output_text"
				}
				content = append(content, map[string]string{"type": kind, "text": p.Text.Text})
				continue
			}
			if err := flush(); err != nil {
				return nil, responsesUnsupported("message text: %v", err)
			}
			switch {
			case p.Reasoning != nil && role == "assistant":
				r := p.Reasoning
				item, err := parseResponsesItem(r.Opaque)
				if err != nil || item.Type != "reasoning" || validateResponsesReasoning(item) != nil {
					return nil, responsesUnsupported("malformed or missing encrypted reasoning state")
				}
				if r.Provider != "openai" || r.ModelID != model || r.ItemID != item.ID || r.Ordinal < 0 || r.Ordinal <= lastOrdinal || r.OpaqueURI != "" || r.OpaqueDigest != "" || reasoningIDs[item.ID] {
					return nil, responsesUnsupported("incompatible reasoning metadata")
				}
				lastOrdinal = r.Ordinal
				reasoningIDs[item.ID] = true
				input = append(input, r.Opaque)
			case p.ToolCall != nil && role == "assistant":
				tc := p.ToolCall
				if tc.ID == "" || tc.Tool == "" || calls[tc.ID] {
					return nil, responsesUnsupported("empty or duplicate tool call identity/name")
				}
				calls[tc.ID] = true
				args := tc.Args
				if args == nil {
					args = map[string]any{}
				}
				raw, err := responsesMarshal(args)
				if err != nil {
					return nil, responsesUnsupported("function arguments: %v", err)
				}
				if _, err = toolArguments(string(raw)); err != nil {
					return nil, responsesUnsupported("function arguments: %v", err)
				}
				if err := add(map[string]any{"type": "function_call", "call_id": tc.ID, "name": tc.Tool, "arguments": string(raw)}); err != nil {
					return nil, err
				}
			case p.ToolResult != nil && role == "tool":
				tr := p.ToolResult
				if tr.ID == "" || results[tr.ID] || len(tr.Content) == 0 {
					return nil, responsesUnsupported("tool result requires unique call ID and model-facing Content")
				}
				results[tr.ID] = true
				var text strings.Builder
				for _, part := range tr.Content {
					if responsesPartKinds(part) != 1 || part.Text == nil {
						return nil, responsesUnsupported("tool result Content must be text")
					}
					text.WriteString(part.Text.Text)
				}
				if err := add(map[string]any{"type": "function_call_output", "call_id": tr.ID, "output": text.String()}); err != nil {
					return nil, err
				}
			default:
				return nil, responsesUnsupported("message %d part %d is unsupported for its role", mi, pi)
			}
		}
		if err := flush(); err != nil {
			return nil, responsesUnsupported("message text: %v", err)
		}
		// Preserve an explicit empty text message rather than removing history.
		if len(m.Parts) == 0 && role != "tool" {
			if err := add(map[string]any{"type": "message", "role": role, "content": []any{}}); err != nil {
				return nil, err
			}
		}
	}
	return input, nil
}

// toolArguments validates numbers before converting to the neutral float64
// domain. Parsing directly as float64 would silently round 2^53+1 into 2^53.
func toolArguments(raw string) (map[string]any, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, responsesJSONError(err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("arguments contain trailing JSON")
	}
	obj, ok := value.(map[string]any)
	if !ok || obj == nil {
		return nil, fmt.Errorf("arguments must be a JSON object")
	}
	if err := toolArgumentNumbers(value); err != nil {
		return nil, err
	}
	var out map[string]any
	if err := responsesUnmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func toolArgumentNumbers(value any) error {
	switch v := value.(type) {
	case json.Number:
		f, err := v.Float64()
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return fmt.Errorf("number outside float64 domain")
		}
		// Only numbers near the boundary need exact arithmetic. Rat also handles
		// decimal/exponent spellings of integers, which ParseInt would miss.
		if math.Abs(f) >= 1<<53 {
			r, ok := new(big.Rat).SetString(string(v))
			if !ok {
				return fmt.Errorf("invalid number")
			}
			if r.IsInt() && new(big.Int).Abs(r.Num()).Cmp(new(big.Int).Lsh(big.NewInt(1), 53)) > 0 {
				return fmt.Errorf("integer outside exactly representable +/-2^53 domain")
			}
		}
	case map[string]any:
		for _, key := range slices.Sorted(maps.Keys(v)) {
			if err := toolArgumentNumbers(v[key]); err != nil {
				return err
			}
		}
	case []any:
		for _, x := range v {
			if err := toolArgumentNumbers(x); err != nil {
				return err
			}
		}
	}
	return nil
}

type responsesItem struct {
	Type      string            `json:"type"`
	ID        string            `json:"id"`
	Status    string            `json:"status"`
	Role      string            `json:"role"`
	Content   []json.RawMessage `json:"content"`
	CallID    string            `json:"call_id"`
	Name      string            `json:"name"`
	Arguments *string           `json:"arguments"`
	Encrypted *string           `json:"encrypted_content"`
	Summary   []json.RawMessage `json:"summary"`
	raw       json.RawMessage
}

func parseResponsesItem(raw []byte) (responsesItem, error) {
	var item responsesItem
	err := responsesUnmarshal(raw, &item)
	item.raw = raw
	return item, err
}

func responsesKeys(raw []byte, allowed ...string) error {
	var fields map[string]json.RawMessage
	if err := responsesUnmarshal(raw, &fields); err != nil {
		return err
	}
	for key := range fields {
		found := false
		for _, a := range allowed {
			if a == key {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("unsupported item/content field")
		}
	}
	return nil
}

func validateResponsesReasoning(item responsesItem) error {
	if item.Type != "reasoning" || item.ID == "" || item.Encrypted == nil || *item.Encrypted == "" || (item.Status != "" && item.Status != "completed") {
		return fmt.Errorf("reasoning requires identity and completed encrypted state")
	}
	for _, raw := range item.Summary {
		var p struct {
			Type string  `json:"type"`
			Text *string `json:"text"`
		}
		if err := responsesUnmarshal(raw, &p); err != nil {
			return err
		}
		if p.Type != "summary_text" || p.Text == nil {
			return fmt.Errorf("unsupported reasoning summary")
		}
		if err := responsesKeys(raw, "type", "text"); err != nil {
			return err
		}
	}
	return nil
}

func responsesOutputText(raw []byte, requireText bool) (string, error) {
	var p struct {
		Type        string            `json:"type"`
		Text        *string           `json:"text"`
		Annotations []json.RawMessage `json:"annotations"`
	}
	if err := responsesUnmarshal(raw, &p); err != nil {
		return "", err
	}
	if p.Type != "output_text" || (requireText && p.Text == nil) || len(p.Annotations) > 0 {
		return "", fmt.Errorf("unsupported output content/refusal/annotations")
	}
	if err := responsesKeys(raw, "type", "text", "annotations", "logprobs"); err != nil {
		return "", err
	}
	if p.Text == nil {
		return "", nil
	}
	return *p.Text, nil
}

func decodeResponses(model string, raw []byte) (api.ModelResponse, error) {
	fail := func(err error) (api.ModelResponse, error) {
		return api.ModelResponse{}, fmt.Errorf("openai responses: %w", err)
	}
	var out struct {
		Status     string            `json:"status"`
		Output     []json.RawMessage `json:"output"`
		Error      json.RawMessage   `json:"error"`
		Incomplete json.RawMessage   `json:"incomplete_details"`
		Usage      struct {
			Input   int64 `json:"input_tokens"`
			Output  int64 `json:"output_tokens"`
			Details struct {
				Reasoning int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	if err := responsesUnmarshal(raw, &out); err != nil {
		return fail(fmt.Errorf("decode response: %w", err))
	}
	present := func(r json.RawMessage) bool { return len(r) > 0 && !bytes.Equal(bytes.TrimSpace(r), []byte("null")) }
	if out.Status != "completed" || out.Output == nil || present(out.Error) || present(out.Incomplete) {
		switch out.Status {
		case "failed", "incomplete", "cancelled", "queued", "in_progress":
			return fail(fmt.Errorf("response is not a completed output (status %s)", out.Status))
		default:
			return fail(fmt.Errorf("response is not a completed output"))
		}
	}
	msg := api.Message{Role: "assistant"}
	ids, calls := map[string]bool{}, map[string]bool{}
	for index, rawItem := range out.Output {
		item, err := parseResponsesItem(rawItem)
		if err != nil {
			return fail(fmt.Errorf("output item %d: %w", index, err))
		}
		if item.ID == "" || ids[item.ID] || (item.Status != "" && item.Status != "completed") {
			return fail(fmt.Errorf("invalid/duplicate output item identity or status at %d", index))
		}
		ids[item.ID] = true
		switch item.Type {
		case "message":
			if item.Role != "assistant" || item.Content == nil {
				return fail(fmt.Errorf("output message requires assistant role and content"))
			}
			if err := responsesKeys(rawItem, "type", "id", "status", "role", "content"); err != nil {
				return fail(err)
			}
			for _, content := range item.Content {
				text, err := responsesOutputText(content, true)
				if err != nil {
					return fail(err)
				}
				msg.Parts = append(msg.Parts, api.Part{Text: &api.TextPart{Text: text}})
			}
		case "function_call":
			if item.CallID == "" || item.Name == "" || calls[item.CallID] || item.Arguments == nil {
				return fail(fmt.Errorf("invalid/duplicate function call identity, name or arguments"))
			}
			if err := responsesKeys(rawItem, "type", "id", "status", "call_id", "name", "arguments"); err != nil {
				return fail(err)
			}
			args, err := toolArguments(*item.Arguments)
			if err != nil {
				return fail(fmt.Errorf("function arguments: %w", err))
			}
			calls[item.CallID] = true
			msg.Parts = append(msg.Parts, api.Part{ToolCall: &api.ToolCall{ID: item.CallID, Tool: item.Name, Args: args}})
		case "reasoning":
			if err := validateResponsesReasoning(item); err != nil {
				return fail(err)
			}
			msg.Parts = append(msg.Parts, api.Part{Reasoning: &api.ReasoningPart{Provider: "openai", ModelID: model, ItemID: item.ID, Ordinal: int32(index), Opaque: append([]byte(nil), rawItem...)}})
		default:
			return fail(fmt.Errorf("unsupported output item type"))
		}
	}
	if out.Usage.Input < 0 || out.Usage.Output < 0 || out.Usage.Details.Reasoning < 0 {
		return fail(fmt.Errorf("negative usage"))
	}
	return api.ModelResponse{Message: msg, Usage: api.Usage{Model: model, InputTokens: out.Usage.Input, OutputTokens: out.Usage.Output, ReasoningTokens: out.Usage.Details.Reasoning}}, nil
}
