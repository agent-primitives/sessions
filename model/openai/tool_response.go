package openai

import (
	"fmt"

	"github.com/agent-primitives/sessions/api"
)

// checkToolResponse refuses a completion whose tool calls break the request's tool constraints
// (api.ModelRequest.Tools and ToolChoice): a call to a tool that was not offered, a call under
// NONE, a call to a tool other than a named choice, or no call under REQUIRED. A provider can
// return any of these; the harness would otherwise forward the call to the host as if the request
// allowed it. The error wraps ErrUnsupportedPart and never quotes the returned name or arguments,
// because the host may journal the error text. It is protocol neutral so every adapter in this
// package can apply it to the decoded parts before returning the completion.
func checkToolResponse(tools []api.ToolDefinition, choice *api.ToolChoice, parts []api.Part) error {
	refuse := func(reason string) error {
		return fmt.Errorf("%w: completion %s", ErrUnsupportedPart, reason)
	}
	var mode api.ToolChoiceMode
	var name string
	if choice != nil {
		mode, name = choice.Mode, choice.Name
	}
	switch mode {
	case "", api.ToolChoiceAuto, api.ToolChoiceNone, api.ToolChoiceRequired:
	default:
		return refuse("cannot be checked against an unsupported tool choice mode")
	}
	if name != "" && mode != api.ToolChoiceRequired {
		return refuse("cannot be checked against a named tool choice without REQUIRED")
	}
	offered := make(map[string]bool, len(tools))
	for _, tool := range tools {
		offered[tool.Name] = true
	}
	calls := 0
	for _, p := range parts {
		if p.ToolCall == nil {
			continue
		}
		calls++
		switch {
		case mode == api.ToolChoiceNone:
			return refuse("calls a tool under tool choice NONE")
		case !offered[p.ToolCall.Tool]:
			return refuse("calls a tool the request did not offer")
		case name != "" && p.ToolCall.Tool != name:
			return refuse("calls a tool other than the named tool choice")
		}
	}
	if mode == api.ToolChoiceRequired && calls == 0 {
		return refuse("has no tool call under tool choice REQUIRED")
	}
	return nil
}
