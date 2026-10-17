package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"github.com/agent-primitives/sessions/api"
)

// StreamModel emits provisional text deltas only. The authoritative completed
// response, not the accumulated chunks, is returned for durable recording.
func (c *ResponsesClient) StreamModel(ctx context.Context, req api.ModelRequest, onChunk func(string)) (api.ModelResponse, error) {
	model, body, err := c.buildResponsesRequest(req, true)
	if err != nil {
		return api.ModelResponse{}, err
	}
	resp, err := c.client.post(ctx, body)
	if err != nil {
		return api.ModelResponse{}, responsesIOError("request", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return api.ModelResponse{}, fmt.Errorf("openai responses: HTTP status %d", resp.StatusCode)
	}
	state := responsesStream{items: map[int]*responsesStreamItem{}}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)
	var data strings.Builder
	eventName := ""
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if data.Len() > 0 {
				payload := strings.TrimSuffix(data.String(), "\n")
				if payload != "[DONE]" {
					completed, err := state.event(eventName, []byte(payload), onChunk)
					if err != nil {
						return api.ModelResponse{}, fmt.Errorf("openai responses: stream: %w", err)
					}
					if completed != nil {
						if err := ctx.Err(); err != nil {
							return api.ModelResponse{}, err
						}
						return decodeResponses(model, completed)
					}
				}
			}
			data.Reset()
			eventName = ""
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			if data.Len()+len(value) > 32<<20 {
				return api.ModelResponse{}, fmt.Errorf("openai responses: SSE record exceeds 32 MiB")
			}
			data.WriteString(value)
			data.WriteByte('\n')
		case "event":
			eventName = value
			// SSE id/retry and unknown framing fields are not model content.
		}
	}
	if err := scanner.Err(); err != nil {
		return api.ModelResponse{}, responsesIOError("read stream", err)
	}
	if err := ctx.Err(); err != nil {
		return api.ModelResponse{}, err
	}
	return api.ModelResponse{}, fmt.Errorf("openai responses: EOF without response.completed")
}

type responsesStream struct {
	id    string
	items map[int]*responsesStreamItem
}

type responsesStreamItem struct {
	added responsesItem
	done  *responsesItem
	args  responsesStreamText
	texts map[responsesTextKey]*responsesStreamText
}

type responsesTextKey struct {
	kind  string
	index int
}

type responsesStreamText struct {
	deltas    strings.Builder
	sawDelta  bool
	done      *string
	addedPart json.RawMessage
	donePart  json.RawMessage
}

func (t *responsesStreamText) delta(value string) error {
	if t.done != nil || t.donePart != nil {
		return fmt.Errorf("delta after completed text/arguments")
	}
	t.sawDelta = true
	t.deltas.WriteString(value)
	return nil
}

func (t *responsesStreamText) finish(value string) error {
	if t.done != nil {
		return fmt.Errorf("duplicate completed text/arguments")
	}
	if t.sawDelta && t.deltas.String() != value {
		return fmt.Errorf("completed text/arguments conflict with deltas")
	}
	t.done = &value
	return nil
}

func (t *responsesStreamText) matches(value string) error {
	if (t.sawDelta && t.deltas.String() != value) || (t.done != nil && *t.done != value) {
		return fmt.Errorf("streamed text/arguments conflict with final output")
	}
	return nil
}

type responsesEvent struct {
	Type         string          `json:"type"`
	OutputIndex  *int            `json:"output_index"`
	ContentIndex *int            `json:"content_index"`
	SummaryIndex *int            `json:"summary_index"`
	ItemID       string          `json:"item_id"`
	Item         json.RawMessage `json:"item"`
	Part         json.RawMessage `json:"part"`
	Delta        *string         `json:"delta"`
	Text         *string         `json:"text"`
	Arguments    *string         `json:"arguments"`
	Response     json.RawMessage `json:"response"`
}

func (s *responsesStream) event(eventName string, raw []byte, onChunk func(string)) ([]byte, error) {
	var e responsesEvent
	if err := responsesUnmarshal(raw, &e); err != nil {
		return nil, fmt.Errorf("malformed event: %w", err)
	}
	if e.Type == "" || (eventName != "" && eventName != e.Type) {
		return nil, fmt.Errorf("missing/conflicting SSE event type")
	}
	switch e.Type {
	case "response.created", "response.in_progress":
		// Explicitly allowed lifecycle events. They carry no finalized content.
		var response struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		if len(e.Response) == 0 || bytes.Equal(e.Response, []byte("null")) {
			return nil, fmt.Errorf("missing lifecycle response")
		}
		if err := responsesUnmarshal(e.Response, &response); err != nil {
			return nil, err
		}
		if response.Status != "" && response.Status != "in_progress" {
			return nil, fmt.Errorf("unexpected lifecycle response status")
		}
		if err := s.responseID(response.ID); err != nil {
			return nil, err
		}
		return nil, nil
	case "error", "response.failed", "response.incomplete":
		// Only the allowlisted terminal kind is safe to journal; provider messages can echo input.
		return nil, fmt.Errorf("terminal %s", e.Type)
	case "response.completed":
		if err := s.validateFinal(e.Response); err != nil {
			return nil, err
		}
		return e.Response, nil
	case "response.output_item.added":
		if e.OutputIndex == nil || *e.OutputIndex < 0 || s.items[*e.OutputIndex] != nil {
			return nil, fmt.Errorf("invalid/duplicate output index")
		}
		item, err := parseResponsesItem(e.Item)
		if err != nil {
			return nil, err
		}
		if item.ID == "" {
			return nil, fmt.Errorf("missing added item identity")
		}
		for _, other := range s.items {
			if other.added.ID == item.ID {
				return nil, fmt.Errorf("duplicate item identity")
			}
		}
		switch item.Type {
		case "message":
			if err := responsesKeys(e.Item, "type", "id", "status", "role", "content"); err != nil {
				return nil, err
			}
			if item.Role != "assistant" {
				return nil, fmt.Errorf("unsupported message role")
			}
			for _, part := range item.Content {
				if _, err := responsesOutputText(part, true); err != nil {
					return nil, err
				}
			}
		case "function_call":
			if err := responsesKeys(e.Item, "type", "id", "status", "call_id", "name", "arguments"); err != nil {
				return nil, err
			}
			if item.CallID == "" || item.Name == "" {
				return nil, fmt.Errorf("missing function call identity/name")
			}
		case "reasoning": // Encrypted content on added events is not replayable state.
		default:
			return nil, fmt.Errorf("unsupported added item type")
		}
		s.items[*e.OutputIndex] = &responsesStreamItem{added: item, texts: map[responsesTextKey]*responsesStreamText{}}
		return nil, nil
	}
	// Every semantic event below must refer to a known item and stable index/ID.
	if e.OutputIndex == nil || *e.OutputIndex < 0 {
		return nil, fmt.Errorf("missing/invalid output index")
	}
	item := s.items[*e.OutputIndex]
	if item == nil {
		return nil, fmt.Errorf("event for unknown output index")
	}
	if item.done != nil {
		return nil, fmt.Errorf("event after output item completion")
	}
	if e.Type == "response.output_item.done" {
		done, err := parseResponsesItem(e.Item)
		if err != nil {
			return nil, err
		}
		if err := responsesSameIdentity(item.added, done); err != nil {
			return nil, err
		}
		item.done = &done
		return nil, nil
	}
	if e.ItemID == "" || e.ItemID != item.added.ID {
		return nil, fmt.Errorf("conflicting/missing event item identity")
	}
	switch e.Type {
	case "response.function_call_arguments.delta", "response.function_call_arguments.done":
		if item.added.Type != "function_call" {
			return nil, fmt.Errorf("arguments event for non-function item")
		}
		if e.Type == "response.function_call_arguments.delta" {
			if e.Delta == nil {
				return nil, fmt.Errorf("missing arguments delta")
			}
			return nil, item.args.delta(*e.Delta)
		}
		if e.Arguments == nil {
			return nil, fmt.Errorf("missing completed arguments")
		}
		return nil, item.args.finish(*e.Arguments)
	case "response.output_text.delta", "response.output_text.done", "response.content_part.added", "response.content_part.done":
		if item.added.Type != "message" {
			return nil, fmt.Errorf("text event for non-message item")
		}
		return nil, item.textEvent(e, "content", e.ContentIndex, onChunk)
	case "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		if item.added.Type != "reasoning" {
			return nil, fmt.Errorf("summary event for non-reasoning item")
		}
		return nil, item.textEvent(e, "summary", e.SummaryIndex, nil)
	case "response.reasoning_text.delta", "response.reasoning_text.done":
		if item.added.Type != "reasoning" {
			return nil, fmt.Errorf("reasoning event for non-reasoning item")
		}
		return nil, item.textEvent(e, "reasoning", e.ContentIndex, nil)
	default:
		return nil, fmt.Errorf("unsupported semantic event")
	}
}

func (s *responsesStream) responseID(id string) error {
	if s.id != "" && s.id != id {
		return fmt.Errorf("conflicting response identity")
	}
	if id != "" {
		s.id = id
	}
	return nil
}

func (item *responsesStreamItem) textEvent(e responsesEvent, kind string, index *int, onChunk func(string)) error {
	if index == nil || *index < 0 {
		return fmt.Errorf("missing/invalid %s index", kind)
	}
	key := responsesTextKey{kind: kind, index: *index}
	text := item.texts[key]
	if text == nil {
		text = &responsesStreamText{}
		item.texts[key] = text
	}
	switch {
	case strings.HasSuffix(e.Type, ".delta"):
		if e.Delta == nil {
			return fmt.Errorf("missing text delta")
		}
		if err := text.delta(*e.Delta); err != nil {
			return err
		}
		if onChunk != nil && *e.Delta != "" {
			onChunk(*e.Delta)
		}
	case strings.HasSuffix(e.Type, "part.added"):
		if text.addedPart != nil || text.donePart != nil || text.done != nil || text.sawDelta {
			return fmt.Errorf("duplicate/out-of-order content part")
		}
		if _, err := responsesStreamPartText(e.Part, kind); err != nil {
			return err
		}
		text.addedPart = e.Part
	case strings.HasSuffix(e.Type, "part.done"):
		if text.donePart != nil {
			return fmt.Errorf("duplicate completed content part")
		}
		value, err := responsesStreamPartText(e.Part, kind)
		if err != nil {
			return err
		}
		if err := text.matches(value); err != nil {
			return err
		}
		text.donePart = e.Part
	default:
		if e.Text == nil {
			return fmt.Errorf("missing completed text")
		}
		return text.finish(*e.Text)
	}
	return nil
}

func responsesStreamPartText(raw []byte, kind string) (string, error) {
	if kind == "content" {
		return responsesOutputText(raw, true)
	}
	var part struct {
		Type string  `json:"type"`
		Text *string `json:"text"`
	}
	if err := responsesUnmarshal(raw, &part); err != nil {
		return "", err
	}
	expected := "summary_text"
	if kind == "reasoning" {
		expected = "reasoning_text"
	}
	if part.Type != expected || part.Text == nil {
		return "", fmt.Errorf("unsupported %s part", kind)
	}
	if err := responsesKeys(raw, "type", "text"); err != nil {
		return "", err
	}
	return *part.Text, nil
}

func responsesSameIdentity(a, b responsesItem) error {
	if a.ID != b.ID || a.Type != b.Type || a.Role != b.Role || a.CallID != b.CallID || a.Name != b.Name {
		return fmt.Errorf("conflicting output item identity/type")
	}
	return nil
}

func responsesJSONEqual(a, b []byte) bool {
	var av, bv any
	ad, bd := json.NewDecoder(bytes.NewReader(a)), json.NewDecoder(bytes.NewReader(b))
	ad.UseNumber()
	bd.UseNumber()
	return ad.Decode(&av) == nil && bd.Decode(&bv) == nil && reflect.DeepEqual(av, bv)
}

func (s *responsesStream) validateFinal(raw []byte) error {
	// Decode with the shared final codec before reconciling observations; this
	// validates status, usage and every output item even if no deltas arrived.
	if _, err := decodeResponses("", raw); err != nil {
		return err
	}
	var final struct {
		ID     string            `json:"id"`
		Output []json.RawMessage `json:"output"`
	}
	if err := responsesUnmarshal(raw, &final); err != nil {
		return err
	}
	if err := s.responseID(final.ID); err != nil {
		return err
	}
	for index, observed := range s.items {
		if index >= len(final.Output) {
			return fmt.Errorf("observed item absent from final output")
		}
		item, err := parseResponsesItem(final.Output[index])
		if err != nil {
			return err
		}
		if err := responsesSameIdentity(observed.added, item); err != nil {
			return err
		}
		if observed.done != nil && !responsesJSONEqual(observed.done.raw, item.raw) {
			return fmt.Errorf("completed item conflicts with final output")
		}
		if item.Type == "function_call" {
			if err := observed.args.matches(*item.Arguments); err != nil {
				return err
			}
			if observed.added.Arguments != nil && !strings.HasPrefix(*item.Arguments, *observed.added.Arguments) {
				return fmt.Errorf("added function arguments conflict with final output")
			}
		}
		for i, part := range observed.added.Content {
			if i >= len(item.Content) {
				return fmt.Errorf("added content absent from final output")
			}
			kind := "content"
			if item.Type == "reasoning" {
				kind = "reasoning"
			}
			before, err := responsesStreamPartText(part, kind)
			if err != nil {
				return err
			}
			after, err := responsesStreamPartText(item.Content[i], kind)
			if err != nil || !strings.HasPrefix(after, before) {
				return fmt.Errorf("added content conflicts with final output")
			}
		}
		for i, part := range observed.added.Summary {
			if i >= len(item.Summary) {
				return fmt.Errorf("added summary absent from final output")
			}
			before, err := responsesStreamPartText(part, "summary")
			if err != nil {
				return err
			}
			after, err := responsesStreamPartText(item.Summary[i], "summary")
			if err != nil || !strings.HasPrefix(after, before) {
				return fmt.Errorf("added summary conflicts with final output")
			}
		}
		for key, text := range observed.texts {
			parts := item.Content
			if key.kind == "summary" {
				parts = item.Summary
			}
			if key.index >= len(parts) {
				return fmt.Errorf("observed content absent from final output")
			}
			value, err := responsesStreamPartText(parts[key.index], key.kind)
			if err != nil {
				return err
			}
			if err := text.matches(value); err != nil {
				return err
			}
			if text.donePart != nil && !responsesJSONEqual(text.donePart, parts[key.index]) {
				return fmt.Errorf("completed content conflicts with final output")
			}
			if text.addedPart != nil {
				prefix, err := responsesStreamPartText(text.addedPart, key.kind)
				if err != nil || !strings.HasPrefix(value, prefix) {
					return fmt.Errorf("added content conflicts with final output")
				}
			}
		}
	}
	return nil
}
