package openai

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Decoder and transport errors can contain peer payloads or credential-bearing URLs.
// Model errors become durable journal text, so retain categories, never those causes.
func responsesJSONError(err error) error {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return errors.New("JSON value has the wrong type or is out of range")
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return errors.New("invalid JSON syntax")
	}
	return errors.New("invalid JSON value")
}

func responsesUnmarshal(raw []byte, dst any) error {
	if err := json.Unmarshal(raw, dst); err != nil {
		return responsesJSONError(err)
	}
	return nil
}

func responsesMarshal(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, errors.New("value cannot be encoded as JSON")
	}
	return raw, nil
}

func responsesIOError(stage string, err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("openai responses: %s: %w", stage, context.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("openai responses: %s: %w", stage, context.DeadlineExceeded)
	case errors.Is(err, bufio.ErrTooLong):
		return fmt.Errorf("openai responses: %s: %w", stage, bufio.ErrTooLong)
	default:
		return fmt.Errorf("openai responses: %s failed", stage)
	}
}
