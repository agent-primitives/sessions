package openai

import (
	"errors"
	"strings"
	"testing"

	"github.com/agent-primitives/sessions/api"
)

// The Responses request builder refuses these choices before HTTP, so only a direct call reaches
// them. Catches the shared check passing a completion it cannot judge for another adapter.
func TestCheckToolResponseRefusesUncheckableChoice(t *testing.T) {
	tools := []api.ToolDefinition{{Name: "lookup"}}
	call := []api.Part{{ToolCall: &api.ToolCall{ID: "c", Tool: "lookup"}}}
	for name, choice := range map[string]*api.ToolChoice{
		"unknown mode":     {Mode: "ANY"},
		"named under AUTO": {Mode: api.ToolChoiceAuto, Name: "lookup"},
		"named, no mode":   {Name: "lookup"},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkToolResponse(tools, choice, call)
			if !errors.Is(err, ErrUnsupportedPart) || !strings.Contains(err.Error(), "cannot be checked") {
				t.Fatalf("err = %v, want ErrUnsupportedPart that cannot be checked", err)
			}
		})
	}
	if err := checkToolResponse(tools, nil, call); err != nil {
		t.Fatalf("nil choice: err = %v, want accepted", err)
	}
}
