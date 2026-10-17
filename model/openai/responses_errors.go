package openai

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Only bounded identifier fields may accompany an HTTP status; never echo messages
// or decoder errors. Invalid fields are omitted independently, not truncated.
func responsesHTTPError(resp *http.Response) error {
	status := fmt.Sprintf("openai responses: HTTP status %d", resp.StatusCode)
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (16<<10)+1))
	if err != nil || len(raw) > 16<<10 {
		return errors.New(status)
	}
	var envelope struct {
		Error struct {
			Code  json.RawMessage `json:"code"`
			Param json.RawMessage `json:"param"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return errors.New(status)
	}
	var fields []string
	for _, field := range []struct {
		name  string
		raw   json.RawMessage
		limit int
	}{
		{"code", envelope.Error.Code, 64},
		{"param", envelope.Error.Param, 128},
	} {
		var value string
		if json.Unmarshal(field.raw, &value) != nil || len(value) == 0 || len(value) > field.limit {
			continue
		}
		valid := true
		for _, ch := range value {
			if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' {
				continue
			}
			if field.name == "param" && (ch == '.' || ch == '[' || ch == ']') {
				continue
			}
			valid = false
			break
		}
		if valid {
			fields = append(fields, field.name+"="+value)
		}
	}
	if len(fields) != 0 {
		status += " (" + strings.Join(fields, ", ") + ")"
	}
	return errors.New(status)
}

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
