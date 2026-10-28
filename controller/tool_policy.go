package controller

import (
	"context"
	"encoding"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"strconv"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/authn"
	"github.com/agent-primitives/sessions/wire"
)

// ErrToolPolicyRefused is a transient refusal from the WithToolPolicy hook: the executor was not
// called and no receipt was written. On an approved REQUIRES_APPROVAL call the decision stays
// recoverable and the next Resume asks the hook again, as for ErrMissingToolExecutor. On an
// ordinary call it behaves like an executor error: the TOOL_CALL intent stays without a receipt.
// A verdict the controller does not know, including the zero value, is refused the same way.
var ErrToolPolicyRefused = errors.New("controller: tool call refused by host policy")

// policyDenied is the model-facing Content of a POLICY_DENIED receipt. The hook's reason is kept
// in ToolResult.Error.
const policyDenied = "Tool execution refused by host policy."

// ToolPolicyVerdict is the outcome of a WithToolPolicy hook.
type ToolPolicyVerdict int

const (
	// ToolPolicyUnspecified is the zero value. It is not a decision: the controller refuses the
	// call transiently (ErrToolPolicyRefused), so a hook that forgets to set a verdict fails closed
	// without a durable consequence.
	ToolPolicyUnspecified ToolPolicyVerdict = iota
	// ToolPolicyAllow lets the controller call the executor.
	ToolPolicyAllow
	// ToolPolicyTransientRefusal refuses this attempt only, for a condition that may clear (a
	// credential refresh failed, a provider is down). Nothing is recorded for the refusal.
	ToolPolicyTransientRefusal
	// ToolPolicyDefinitiveRefusal refuses the call for good (a grant was revoked, policy says no).
	// The controller records a POLICY_DENIED receipt, which recovery and replay serve instead of
	// asking again.
	ToolPolicyDefinitiveRefusal
)

// ToolPolicyDecision is what a WithToolPolicy hook returns.
type ToolPolicyDecision struct {
	Verdict ToolPolicyVerdict
	// Reason explains a refusal. A definitive refusal journals it in the receipt's Error and sends
	// it to the harness. A transient refusal returns it in the error, which reaches the harness and
	// the Exec or Resume caller and, when it ends an ordinary call's turn, is journaled in the
	// EVENT_ERROR description. It must not carry secrets.
	Reason string
}

// ToolPolicyApproval is the committed decision that authorizes an approved REQUIRES_APPROVAL call.
type ToolPolicyApproval struct {
	// Decision is a copy of the APPROVAL_RESULT payload. Approved is always true here: a denial
	// never reaches the executor or the hook. VerifiedApprover is the approver the host's
	// authentication verified when it journaled the decision, zero when it did not; unlike the
	// harness, the hook receives it.
	Decision api.ApprovalResult
	// DecisionSeq is the journal sequence of that APPROVAL_RESULT record.
	DecisionSeq int64
}

// ToolPolicyInput is what the controller passes to a WithToolPolicy hook.
type ToolPolicyInput struct {
	// Scope is the session identity the executor will receive.
	Scope       ToolCallContext
	ExecutionID string
	// Call is the call the executor would receive, in its journaled form: ID, tool, mediation and
	// key are the executor's, and Args are rebuilt for each hook call from the JSON form the
	// TOOL_CALL records, so numbers are float64 and typed Go containers become map[string]any and
	// []any. The hook holds no reference into the executor's call, so changing Call changes
	// nothing. A call whose arguments the journal cannot represent is refused transiently
	// (ErrToolPolicyRefused) without calling the hook. So is a call whose journal form differs
	// from what the executor receives, whether the executor reads the Go values in Args or the
	// JSON encoding/json writes for them, anywhere in the argument maps, slices, arrays, pointers
	// and interfaces: the hook would judge one value while the executor receives another. That
	// covers a number of magnitude at least 2^53 that a float64 cannot hold exactly, such as the
	// integer 2^53+1 in a Go integer type or a json.Number in any spelling ("9007199254740993",
	// "9007199254740993e0", "9007199254740993.0"); a json.Number below 2^53 whose decimal value
	// differs from the JSON encoding/json writes for its journal float64, such as
	// "0.30000000000000001" journaled as 0.3; a float32 whose JSON form a float64 reads as a
	// different number, such as float32(0.7) or float32(1e20); and a string or map key that is not
	// valid UTF-8. Another spelling of the same value ("3e-1" for 0.3) is not refused; neither is a
	// nil slice or map journaled as an empty one, or a nil pointer journaled as null. Args must be
	// JSON-shaped (package wire): a value the guard cannot compare with its journal form is refused
	// too. That is a struct encoding/json writes field by field, such as struct{ Amount int64 },
	// and any value or non-string map key whose type has a MarshalJSON or MarshalText method, such
	// as time.Time, big.Int, net.IP or json.RawMessage: the journal holds only the method's output,
	// while the executor receives the Go value.
	Call api.ToolCall
	// Caller is the verified caller the authn interceptors installed on the context passed to
	// Exec or Resume. It is empty when authentication is off or the controller runs without them.
	// For an approved call it is the caller of the Resume (or input-less Exec) that recovers the
	// call, not the approver, which is Approval.Decision.VerifiedApprover.
	Caller api.CallerIdentity
	// Approval is the committed decision for an approved REQUIRES_APPROVAL call, nil otherwise.
	// The decision's Event.actor is caller-supplied and not included.
	Approval *ToolPolicyApproval
}

// ToolPolicyFunc is an optional host check that runs after the controller's own gates and right
// before the executor, on every executor dispatch: an ordinary CONTROLLER_MEDIATED call (live, or
// re-driven by Resume) and an approved REQUIRES_APPROVAL call on recovery. It runs after the
// TOOL_CALL intent is journaled and, for an approved call, after the call is checked against its
// journaled hash. Replay and recorded receipts never call it, and a denied approval does not
// reach it. ctx is the per-call context the harness passed to ToolCall; a hook that cannot decide,
// including because ctx is done, returns ToolPolicyTransientRefusal. Like the executor it runs
// inside the serialized sink and must not re-enter it.
type ToolPolicyFunc func(ctx context.Context, in ToolPolicyInput) ToolPolicyDecision

// WithToolPolicy installs a pre-dispatch tool policy hook. Without it the executor is called as
// before. A hook needs an executor to have any effect: with no executor the call fails before
// the hook as it does today.
func WithToolPolicy(policy ToolPolicyFunc) Option {
	return func(c *Controller) { c.toolPolicy = policy }
}

// checkToolPolicy asks the hook about call. It returns (nil, nil) to dispatch, a POLICY_DENIED
// receipt to record without dispatching, or an ErrToolPolicyRefused error to stop without a
// receipt. The receipt carries the call ID; the caller adds approval sequences.
func (s *liveSink) checkToolPolicy(ctx context.Context, call api.ToolCall, approval *ToolPolicyApproval) (*api.ToolResult, error) {
	if s.c.toolPolicy == nil {
		return nil, nil
	}
	in := ToolPolicyInput{
		Scope:       ToolCallContext{SessionUID: s.c.sessionUID},
		ExecutionID: s.executionID,
		Call:        call,
		Approval:    approval,
	}
	// Rebuild the arguments from the journal's JSON form rather than cloning them: a clone shares
	// typed containers (map[string]string) with the executor's call, so a hook could change what
	// runs, and an approved call would no longer match the hash checked before the hook.
	in.Call.Args = wire.ToolCallFromProto(wire.ToolCallToProto(&call)).Args
	if call.Args != nil && in.Call.Args == nil {
		return nil, fmt.Errorf("%w: tool arguments cannot be represented in the journal", ErrToolPolicyRefused)
	}
	// Compare the executor's arguments with the copy the hook will judge, before the hook can
	// change that copy.
	if !journalHolds(reflect.ValueOf(call.Args), in.Call.Args) {
		return nil, fmt.Errorf("%w: tool arguments hold a value the guard cannot verify against the journal", ErrToolPolicyRefused)
	}
	if s.runCtx != nil {
		in.Caller, _ = authn.Caller(s.runCtx)
	}
	decision := s.c.toolPolicy(ctx, in)
	switch decision.Verdict {
	case ToolPolicyAllow:
		return nil, nil
	case ToolPolicyDefinitiveRefusal:
		reason := decision.Reason
		if reason == "" {
			reason = policyDenied
		}
		return &api.ToolResult{ID: call.ID, IsError: true, Code: api.ToolResultCodePolicyDenied, Error: reason, Content: api.TextMessage("tool", policyDenied).Parts}, nil
	case ToolPolicyTransientRefusal:
		if decision.Reason == "" {
			return nil, ErrToolPolicyRefused
		}
		return nil, fmt.Errorf("%w: %s", ErrToolPolicyRefused, decision.Reason)
	default:
		return nil, fmt.Errorf("%w: unknown verdict %d", ErrToolPolicyRefused, decision.Verdict)
	}
}

var (
	jsonNumberType    = reflect.TypeFor[json.Number]()
	jsonMarshalerType = reflect.TypeFor[json.Marshaler]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
)

// journalHolds reports whether j, the journal form of the argument value v, is the value an
// executor receives in v, whether it reads v's Go values or the JSON encoding/json writes for v.
// It walks v and j together through maps, slices, arrays, pointers and interfaces, and compares
// every scalar: integers and floats exactly (a float32 must also equal the float64 its shorter
// JSON form parses to), json.Number literals with numberHolds, and strings byte for byte
// (encoding/json replaces invalid UTF-8). A nil slice or map may be journaled as null or as an
// empty value, and a nil slice that encoding/json writes as base64 also as "". A value the guard
// cannot compare does not hold: a struct written field by field, and any value, map key
// included, whose type has a MarshalJSON or MarshalText method (time.Time, big.Int,
// json.RawMessage), since only the method's output reaches the journal and the executor may read
// the Go value. A nil pointer, which encoding/json writes as null without calling a method, holds
// against null. The caller has already converted v to j, which fails on cyclic values, so the
// walk terminates.
func journalHolds(v reflect.Value, j any) bool {
	if !v.IsValid() {
		return j == nil
	}
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			return j == nil
		}
		return journalHolds(v.Elem(), j)
	}
	if hasMarshaler(v.Type()) {
		return false
	}
	switch v.Kind() {
	case reflect.Map:
		m, ok := j.(map[string]any)
		if v.IsNil() {
			return j == nil || ok && len(m) == 0
		}
		if !ok || len(m) != v.Len() {
			return false
		}
		for it := v.MapRange(); it.Next(); {
			k, ok := jsonKey(it.Key())
			if !ok {
				return false
			}
			e, ok := m[k]
			if !ok || !journalHolds(it.Value(), e) {
				return false
			}
		}
		return true
	case reflect.Slice:
		base64Text := v.Type().Elem().Kind() == reflect.Uint8 && !hasMarshaler(v.Type().Elem())
		if v.IsNil() {
			l, isList := j.([]any)
			return j == nil || base64Text && j == "" || isList && len(l) == 0
		}
		if base64Text {
			s, ok := j.(string) // encoding/json writes bytes as base64 text
			return ok && s == base64.StdEncoding.EncodeToString(v.Bytes())
		}
		fallthrough
	case reflect.Array:
		l, ok := j.([]any)
		if !ok || len(l) != v.Len() {
			return false
		}
		for i := range l {
			if !journalHolds(v.Index(i), l[i]) {
				return false
			}
		}
		return true
	case reflect.Bool:
		b, ok := j.(bool)
		return ok && b == v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		f, ok := j.(float64)
		return ok && ratEqual(new(big.Rat).SetInt64(v.Int()), f)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		f, ok := j.(float64)
		return ok && ratEqual(new(big.Rat).SetUint64(v.Uint()), f)
	case reflect.Float32:
		// encoding/json writes the shortest text that reads back as the same float32, which a
		// float64 reader can parse to a different number (float32(0.7) is written 0.7).
		f, ok := j.(float64)
		written, err := strconv.ParseFloat(strconv.FormatFloat(v.Float(), 'g', -1, 32), 64)
		return ok && err == nil && f == v.Float() && f == written
	case reflect.Float64:
		f, ok := j.(float64)
		return ok && f == v.Float()
	case reflect.String:
		if v.Type() == jsonNumberType {
			f, ok := j.(float64)
			return ok && numberHolds(v.String(), f)
		}
		s, ok := j.(string)
		return ok && s == v.String()
	}
	return false // a struct, func, chan or complex number: outside the JSON-shaped argument domain
}

// hasMarshaler reports whether t or *t has a MarshalJSON or MarshalText method. encoding/json
// writes such a value with the method (on an addressable value, also a pointer method), so the
// journal holds the method's output, which the guard cannot compare with the Go value. The
// pointer's method set includes t's, and refusing a type whose pointer method encoding/json would
// skip on a non-addressable value only fails closed.
func hasMarshaler(t reflect.Type) bool {
	p := reflect.PointerTo(t)
	return p.Implements(jsonMarshalerType) || p.Implements(textMarshalerType)
}

// jsonKey returns the object key encoding/json writes for the map key k. A key type with a
// MarshalText or MarshalJSON method is refused like such a value (journalHolds).
func jsonKey(k reflect.Value) (string, bool) {
	if k.Kind() == reflect.String {
		return k.String(), true // encoding/json uses a string key as is, whatever its methods
	}
	if hasMarshaler(k.Type()) {
		return "", false
	}
	switch k.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(k.Int(), 10), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.FormatUint(k.Uint(), 10), true
	}
	return "", false
}

// numberHolds reports whether the JSON number literal s has the value of the journal number f, so
// that each journal float stands for exactly one accepted decimal value. Below 2^53 in magnitude
// that is the value of f's shortest encoding, the JSON encoding/json writes for f: "0.3" and
// "3e-1" hold against 0.3, but "0.30000000000000001", which a float64 reader also rounds to 0.3,
// does not. At or above 2^53 every float64 is an integer and its shortest encoding may round it
// ("1152921504606847000" for 2^60), so s must equal f exactly, whatever the spelling
// ("9007199254740992e0", "9007199254740992.0"). A literal that does not parse is refused.
func numberHolds(s string, f float64) bool {
	r, ok := new(big.Rat).SetString(s)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return false
	}
	if math.Abs(f) >= 1<<53 {
		return ratEqual(r, f)
	}
	shortest, ok := new(big.Rat).SetString(strconv.FormatFloat(f, 'g', -1, 64))
	return ok && r.Cmp(shortest) == 0
}

// ratEqual reports whether f is exactly r. NaN and infinities are never equal.
func ratEqual(r *big.Rat, f float64) bool {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return false
	}
	return r.Cmp(new(big.Rat).SetFloat64(f)) == 0
}
