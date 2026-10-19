package openai

import (
	"errors"
	"strings"
	"testing"

	"github.com/agent-primitives/sessions/api"
)

func TestValidateCompletionToolCallsRejectsUnknownToolChoiceMode(t *testing.T) {
	err := validateCompletionToolCalls(nil, nil, &api.ToolChoice{Mode: "NEW_MODE"})
	if !errors.Is(err, ErrUnsupportedPart) || !strings.Contains(err.Error(), "unsupported tool choice mode") {
		t.Fatalf("err=%v, want unsupported tool choice mode", err)
	}
}
