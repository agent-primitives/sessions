package controller

import (
	"fmt"
	"reflect"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/wire"
)

// matchToolCall compares only the observable request, not raw Go numeric types or map ordering.
// Mismatch errors name the field without exposing arguments or idempotency keys to the journal.
func matchToolCall(emitted api.ToolCall, recorded *api.ToolCall) error {
	if recorded == nil {
		return fmt.Errorf("%w: recorded tool call missing its payload", ErrReplayDiverged)
	}
	// These are main's existing host-execution guards, applied to recorded evidence before either
	// serving a receipt or re-driving an intent. Preserve the key/mediation causes for errors.Is.
	switch recorded.Mediation {
	case api.MediationControllerMediated:
	case api.MediationRequiresApproval:
		if recorded.ID == "" {
			return fmt.Errorf("%w: recorded approval call has no ID", ErrReplayDiverged)
		}
	default:
		return fmt.Errorf("%w: invalid recorded tool mediation: %w", ErrReplayDiverged, ErrUnmediatedToolCall)
	}
	if recorded.IdempotencyKey == "" {
		return fmt.Errorf("%w: invalid recorded tool key: %w", ErrReplayDiverged, ErrMissingIdempotencyKey)
	}
	switch {
	case emitted.ID != recorded.ID:
		return fmt.Errorf("%w: tool call ID mismatch", ErrReplayDiverged)
	case emitted.Tool != recorded.Tool:
		return fmt.Errorf("%w: tool name mismatch", ErrReplayDiverged)
	case emitted.Mediation != recorded.Mediation:
		return fmt.Errorf("%w: tool mediation mismatch", ErrReplayDiverged)
	case emitted.IdempotencyKey != recorded.IdempotencyKey:
		return fmt.Errorf("%w: tool idempotency key mismatch", ErrReplayDiverged)
	}
	// Use the journal's existing wire conversion only for comparison; do not rewrite live args
	// or introduce a new argument domain. Its whole-map JSON fallback can turn nested nil
	// containers into null when a sibling has a typed value. Preserve those legacy distinctions
	// and the conversion's existing precision limitations rather than reinterpret old evidence.
	want := wire.ToolCallFromProto(wire.ToolCallToProto(recorded)).Args
	got := wire.ToolCallFromProto(wire.ToolCallToProto(&emitted)).Args
	if recorded.Args != nil && want == nil || emitted.Args != nil && got == nil {
		return fmt.Errorf("%w: tool arguments could not be represented in the journal", ErrReplayDiverged)
	}
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("%w: tool arguments mismatch", ErrReplayDiverged)
	}
	return nil
}

func recordedToolResult(call *api.ToolCall, result *api.ToolResult) (api.ToolResult, error) {
	if result == nil {
		return api.ToolResult{}, fmt.Errorf("%w: recorded tool result missing its payload", ErrReplayDiverged)
	}
	if result.ID != call.ID {
		return api.ToolResult{}, fmt.Errorf("%w: recorded tool result correlation mismatch", ErrReplayDiverged)
	}
	return *result, nil
}

// cloneArgs deep-copies the JSON-shaped containers of tool arguments (maps and []any), so a copy
// checked against the journal cannot be changed through a map or slice another holder shares.
// Scalars are values; other Go types are outside the JSON-shaped tool domain and are not copied.
func cloneArgs(args map[string]any) map[string]any {
	if args == nil {
		return nil
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[k] = cloneArg(v)
	}
	return out
}

func cloneArg(v any) any {
	switch v := v.(type) {
	case map[string]any:
		return cloneArgs(v)
	case []any:
		if v == nil {
			return v
		}
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = cloneArg(e)
		}
		return out
	default:
		return v
	}
}
