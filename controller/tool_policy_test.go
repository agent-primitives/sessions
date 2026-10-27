package controller_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"net"
	"net/netip"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/authn"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
)

// toolPolicy returns verdicts in order (then allows) and records what it was asked.
type toolPolicy struct {
	verdicts []controller.ToolPolicyDecision
	inputs   []controller.ToolPolicyInput
}

func (p *toolPolicy) hook(_ context.Context, in controller.ToolPolicyInput) controller.ToolPolicyDecision {
	p.inputs = append(p.inputs, in)
	if len(p.verdicts) == 0 {
		return controller.ToolPolicyDecision{Verdict: controller.ToolPolicyAllow}
	}
	d := p.verdicts[0]
	p.verdicts = p.verdicts[1:]
	return d
}

func policyCall() api.ToolCall {
	return api.ToolCall{ID: "call-1", Tool: "send", Mediation: api.MediationControllerMediated, IdempotencyKey: "key-1", Args: map[string]any{"to": "a"}}
}

func logKinds(t *testing.T, log eventlog.Store) []api.EventKind {
	t.Helper()
	recs, err := log.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]api.EventKind, 0, len(recs))
	for _, r := range recs {
		kinds = append(kinds, r.Event.Kind)
	}
	return kinds
}

func toolResults(t *testing.T, log eventlog.Store) []api.ToolResult {
	t.Helper()
	recs, err := log.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	var out []api.ToolResult
	for _, r := range recs {
		if r.Event.Kind == api.EventToolResult {
			out = append(out, *r.Event.Result)
		}
	}
	return out
}

// With no hook, and with a hook that allows, an ordinary call journals the same records and
// receipt, and the executor runs once with the same scope and call.
func TestToolPolicyAllowMatchesNoHook(t *testing.T) {
	run := func(opts ...controller.Option) (eventlog.Store, []api.ToolCall) {
		log := memStore(t)
		var seen []api.ToolCall
		executor := func(_ context.Context, scope controller.ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
			if scope.SessionUID != "s" {
				t.Errorf("scope = %+v", scope)
			}
			seen = append(seen, call)
			return api.ToolResult{Output: map[string]any{"text": "sent"}}, nil
		}
		c, err := controller.New(log, echoModel, append([]controller.Option{controller.WithSessionUID("s"), controller.WithToolExecutor(executor)}, opts...)...)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Exec(t.Context(), &callHarness{calls: []api.ToolCall{policyCall()}}, []api.Message{msg("go")}, 0); err != nil {
			t.Fatal(err)
		}
		return log, seen
	}
	baseLog, baseCalls := run()
	policy := &toolPolicy{}
	hookLog, hookCalls := run(controller.WithToolPolicy(policy.hook))
	if !reflect.DeepEqual(logKinds(t, baseLog), logKinds(t, hookLog)) || !reflect.DeepEqual(toolResults(t, baseLog), toolResults(t, hookLog)) {
		t.Fatalf("allow changed the journal: %v %+v vs %v %+v", logKinds(t, baseLog), toolResults(t, baseLog), logKinds(t, hookLog), toolResults(t, hookLog))
	}
	if len(baseCalls) != 1 || !reflect.DeepEqual(baseCalls, hookCalls) {
		t.Fatalf("executor calls: no hook %+v, allow %+v", baseCalls, hookCalls)
	}
	if len(policy.inputs) != 1 {
		t.Fatalf("hook calls = %d, want 1", len(policy.inputs))
	}
	in := policy.inputs[0]
	if in.Scope.SessionUID != "s" || in.ExecutionID == "" || !reflect.DeepEqual(in.Call, policyCall()) || in.Approval != nil || in.Caller != (api.CallerIdentity{}) {
		t.Fatalf("hook input = %+v", in)
	}
}

// The hook gets a copy of the call: changing its args cannot change what the executor runs.
func TestToolPolicyCannotChangeTheCall(t *testing.T) {
	log := memStore(t)
	var seen api.ToolCall
	executor := func(_ context.Context, _ controller.ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
		seen = call
		return api.ToolResult{}, nil
	}
	hook := func(_ context.Context, in controller.ToolPolicyInput) controller.ToolPolicyDecision {
		in.Call.Args["to"] = "attacker"
		return controller.ToolPolicyDecision{Verdict: controller.ToolPolicyAllow}
	}
	c, err := controller.New(log, echoModel, controller.WithSessionUID("s"), controller.WithToolExecutor(executor), controller.WithToolPolicy(hook))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Exec(t.Context(), &callHarness{calls: []api.ToolCall{policyCall()}}, []api.Message{msg("go")}, 0); err != nil {
		t.Fatal(err)
	}
	if seen.Args["to"] != "a" {
		t.Fatalf("executor saw args %v", seen.Args)
	}
}

type staticVerifier struct{ id api.CallerIdentity }

func (v staticVerifier) Verify(context.Context) (authn.VerifiedCaller, error) {
	return authn.VerifiedCaller{Identity: v.id, ValidUntil: time.Now().Add(time.Hour)}, nil
}

// The hook receives the verified caller the authn interceptors installed on the Exec context.
func TestToolPolicyReceivesVerifiedCaller(t *testing.T) {
	caller := api.CallerIdentity{Issuer: "https://issuer.example", Subject: "alice"}
	unary, _, err := authn.NewInterceptors(authn.Required, staticVerifier{id: caller})
	if err != nil {
		t.Fatal(err)
	}
	log := memStore(t)
	policy := &toolPolicy{}
	c, err := controller.New(log, echoModel, controller.WithSessionUID("s"), controller.WithToolPolicy(policy.hook),
		controller.WithToolExecutor(func(context.Context, controller.ToolCallContext, api.ToolCall) (api.ToolResult, error) {
			return api.ToolResult{}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unary(t.Context(), nil, &grpc.UnaryServerInfo{}, func(ctx context.Context, _ any) (any, error) {
		return nil, c.Exec(ctx, &callHarness{calls: []api.ToolCall{policyCall()}}, []api.Message{msg("go")}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	if len(policy.inputs) != 1 || policy.inputs[0].Caller != caller {
		t.Fatalf("hook inputs = %+v, want caller %+v", policy.inputs, caller)
	}
}

// A definitive refusal on an ordinary call journals a POLICY_DENIED receipt after the intent and
// hands it to the harness as a result, without calling the executor. Replay serves it without
// the hook or the executor.
func TestToolPolicyDefinitiveRefusalOrdinaryCall(t *testing.T) {
	log := memStore(t)
	effects := 0
	executor := func(context.Context, controller.ToolCallContext, api.ToolCall) (api.ToolResult, error) {
		effects++
		return api.ToolResult{}, nil
	}
	policy := &toolPolicy{verdicts: []controller.ToolPolicyDecision{{Verdict: controller.ToolPolicyDefinitiveRefusal, Reason: "policy says no"}}}
	c, err := controller.New(log, echoModel, controller.WithSessionUID("s"), controller.WithToolExecutor(executor), controller.WithToolPolicy(policy.hook))
	if err != nil {
		t.Fatal(err)
	}
	h := &callHarness{calls: []api.ToolCall{policyCall()}}
	if err := c.Exec(t.Context(), h, []api.Message{msg("go")}, 0); err != nil {
		t.Fatal(err)
	}
	want := api.ToolResult{ID: "call-1", IsError: true, Code: api.ToolResultCodePolicyDenied, Error: "policy says no", Content: api.TextMessage("tool", "Tool execution refused by host policy.").Parts}
	if effects != 0 || len(h.results) != 1 || !reflect.DeepEqual(h.results[0], want) {
		t.Fatalf("effects=%d harness results=%+v", effects, h.results)
	}
	if got := toolResults(t, log); !reflect.DeepEqual(got, []api.ToolResult{want}) {
		t.Fatalf("journaled receipts = %+v", got)
	}
	wantKinds := []api.EventKind{api.EventExecutionStart, api.EventInput, api.EventToolCall, api.EventToolResult, api.EventEnd}
	if got := logKinds(t, log); !reflect.DeepEqual(got, wantKinds) {
		t.Fatalf("journal = %v, want %v", got, wantKinds)
	}
	replay := &callHarness{calls: []api.ToolCall{policyCall()}}
	if _, err := c.Replay(t.Context(), replay); err != nil {
		t.Fatal(err)
	}
	if effects != 0 || len(policy.inputs) != 1 || !reflect.DeepEqual(replay.results, []api.ToolResult{want}) {
		t.Fatalf("replay: effects=%d hook calls=%d results=%+v", effects, len(policy.inputs), replay.results)
	}
	if resumed, err := c.Resume(t.Context(), &callHarness{calls: []api.ToolCall{policyCall()}}); resumed || err != nil {
		t.Fatalf("Resume of a completed turn = %v, %v", resumed, err)
	}
	if err := controller.VerifyJournal(log); err != nil {
		t.Fatal(err)
	}
}

// A transient refusal on an ordinary call behaves like an executor error: the intent stays
// without a receipt and the error reaches the harness. A Resume that re-drives the call asks the
// hook again; a definitive refusal there is receipted and not asked again.
func TestToolPolicyTransientRefusalOrdinaryCall(t *testing.T) {
	log := memStore(t)
	effects := 0
	executor := func(context.Context, controller.ToolCallContext, api.ToolCall) (api.ToolResult, error) {
		effects++
		return api.ToolResult{}, nil
	}
	policy := &toolPolicy{verdicts: []controller.ToolPolicyDecision{
		{Verdict: controller.ToolPolicyTransientRefusal, Reason: "provider outage"},
		{Verdict: controller.ToolPolicyDefinitiveRefusal, Reason: "grant revoked"},
	}}
	c, err := controller.New(log, echoModel, controller.WithSessionUID("s"), controller.WithToolExecutor(executor), controller.WithToolPolicy(policy.hook), dedupAll)
	if err != nil {
		t.Fatal(err)
	}
	err = c.Exec(t.Context(), &callHarness{calls: []api.ToolCall{policyCall()}}, []api.Message{msg("go")}, 0)
	if !errors.Is(err, controller.ErrToolPolicyRefused) || !strings.Contains(err.Error(), "provider outage") {
		t.Fatalf("Exec = %v, want ErrToolPolicyRefused with the reason", err)
	}
	if effects != 0 || len(toolResults(t, log)) != 0 {
		t.Fatalf("transient refusal: effects=%d receipts=%+v", effects, toolResults(t, log))
	}

	resumed, err := c.Resume(t.Context(), &callHarness{calls: []api.ToolCall{policyCall()}})
	if !resumed || err != nil {
		t.Fatalf("Resume = %v, %v", resumed, err)
	}
	receipts := toolResults(t, log)
	if effects != 0 || len(policy.inputs) != 2 || len(receipts) != 1 || receipts[0].Code != api.ToolResultCodePolicyDenied || receipts[0].Error != "grant revoked" {
		t.Fatalf("re-drive: effects=%d hook calls=%d receipts=%+v", effects, len(policy.inputs), receipts)
	}
	for i, in := range policy.inputs {
		if !reflect.DeepEqual(in.Call, policyCall()) || in.Approval != nil {
			t.Fatalf("hook input %d = %+v", i, in)
		}
	}
	before := logKinds(t, log)
	if resumed, err := c.Resume(t.Context(), &callHarness{calls: []api.ToolCall{policyCall()}}); resumed || err != nil {
		t.Fatalf("second Resume = %v, %v", resumed, err)
	}
	if effects != 0 || len(policy.inputs) != 2 || !reflect.DeepEqual(logKinds(t, log), before) {
		t.Fatalf("receipted refusal re-asked: effects=%d hook calls=%d", effects, len(policy.inputs))
	}
	replay := &callHarness{calls: []api.ToolCall{policyCall()}}
	if _, err := c.Replay(t.Context(), replay); err != nil {
		t.Fatal(err)
	}
	if effects != 0 || len(policy.inputs) != 2 || !reflect.DeepEqual(replay.results, receipts) {
		t.Fatalf("replay: effects=%d hook calls=%d results=%+v, want %+v", effects, len(policy.inputs), replay.results, receipts)
	}
	if err := controller.VerifyJournal(log); err != nil {
		t.Fatal(err)
	}
}

// POLICY_DENIED is host-owned: an executor reply or a harness report carrying it is refused, as
// for APPROVAL_DENIED.
func TestToolPolicyDeniedCodeIsHostOwned(t *testing.T) {
	denied := api.ToolResult{IsError: true, Code: api.ToolResultCodePolicyDenied}
	log := memStore(t)
	c, err := controller.New(log, echoModel, controller.WithSessionUID("s"),
		controller.WithToolExecutor(func(context.Context, controller.ToolCallContext, api.ToolCall) (api.ToolResult, error) {
			return denied, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Exec(t.Context(), &callHarness{calls: []api.ToolCall{policyCall()}}, []api.Message{msg("go")}, 0); !errors.Is(err, controller.ErrApprovalReceiptReport) {
		t.Fatalf("executor-forged POLICY_DENIED = %v, want ErrApprovalReceiptReport", err)
	}
	c, err = controller.New(memStore(t), echoModel, controller.WithSessionUID("s"))
	if err != nil {
		t.Fatal(err)
	}
	reporter := &reportHarness{result: api.ToolResult{ID: "r1", IsError: true, Code: api.ToolResultCodePolicyDenied}}
	if err := c.Exec(t.Context(), reporter, []api.Message{msg("go")}, 0); !errors.Is(err, controller.ErrApprovalReceiptReport) {
		t.Fatalf("reported POLICY_DENIED = %v, want ErrApprovalReceiptReport", err)
	}
}

type reportHarness struct{ result api.ToolResult }

func (*reportHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "tools", Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}}, nil
}

func (h *reportHarness) Run(ctx context.Context, _ *api.Start, sink api.EventSink) error {
	return sink.Report(ctx, h.result)
}

// typedArgsCall nests typed Go containers in its arguments. The journal accepts them through its
// JSON fallback, and a shallow clone of the arguments would share them.
func typedArgsCall() api.ToolCall {
	call := policyCall()
	call.Args = map[string]any{"nested": map[string]string{"to": "original"}, "list": []string{"original"}, "count": 2}
	return call
}

// mutateArgs changes every nested container the hook can reach, whatever its Go type.
func mutateArgs(args map[string]any) {
	for k, v := range args {
		switch v := v.(type) {
		case map[string]string:
			v["to"] = "mutated"
		case map[string]any:
			v["to"] = "mutated"
		case []string:
			v[0] = "mutated"
		case []any:
			v[0] = "mutated"
		default:
			args[k] = "mutated"
		}
	}
}

// A hook that mutates nested arguments, including typed containers a shallow clone would share,
// changes neither the call the executor runs nor the journal. The hook sees the journaled form of
// the arguments.
func TestToolPolicyCannotChangeNestedArgs(t *testing.T) {
	for _, store := range []struct {
		name string
		new  func(*testing.T) eventlog.Store
	}{
		{"sqlite", memStore},
		{"memory", func(*testing.T) eventlog.Store { return eventlog.AsStore(eventlog.New()) }},
	} {
		t.Run(store.name, func(t *testing.T) {
			log := store.new(t)
			var seen []api.ToolCall
			executor := func(_ context.Context, _ controller.ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
				seen = append(seen, call)
				return api.ToolResult{}, nil
			}
			var hookArgs map[string]any
			hook := func(_ context.Context, in controller.ToolPolicyInput) controller.ToolPolicyDecision {
				hookArgs = map[string]any{"nested": map[string]any{"to": "original"}, "list": []any{"original"}, "count": float64(2)}
				if !reflect.DeepEqual(in.Call.Args, hookArgs) {
					t.Errorf("hook args = %#v, want the journaled form %#v", in.Call.Args, hookArgs)
				}
				mutateArgs(in.Call.Args)
				return controller.ToolPolicyDecision{Verdict: controller.ToolPolicyAllow}
			}
			c, err := controller.New(log, echoModel, controller.WithSessionUID("s"), controller.WithToolExecutor(executor), controller.WithToolPolicy(hook))
			if err != nil {
				t.Fatal(err)
			}
			h := &callHarness{calls: []api.ToolCall{typedArgsCall()}}
			if err := c.Exec(t.Context(), h, []api.Message{msg("go")}, 0); err != nil {
				t.Fatal(err)
			}
			if hookArgs == nil || len(seen) != 1 || !reflect.DeepEqual(seen[0], typedArgsCall()) || !reflect.DeepEqual(h.calls[0], typedArgsCall()) {
				t.Fatalf("hook changed the call: executor saw %#v, harness holds %#v", seen, h.calls[0])
			}
			if err := controller.VerifyJournal(log); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A call whose arguments the journal cannot represent is refused transiently without asking the
// hook or calling the executor: the hook could not see what would run.
func TestToolPolicyRefusesUnrepresentableArgs(t *testing.T) {
	log := memStore(t)
	effects := 0
	executor := func(context.Context, controller.ToolCallContext, api.ToolCall) (api.ToolResult, error) {
		effects++
		return api.ToolResult{}, nil
	}
	policy := &toolPolicy{}
	c, err := controller.New(log, echoModel, controller.WithSessionUID("s"), controller.WithToolExecutor(executor), controller.WithToolPolicy(policy.hook))
	if err != nil {
		t.Fatal(err)
	}
	call := policyCall()
	call.Args = map[string]any{"ch": make(chan int)}
	err = c.Exec(t.Context(), &callHarness{calls: []api.ToolCall{call}}, []api.Message{msg("go")}, 0)
	if !errors.Is(err, controller.ErrToolPolicyRefused) {
		t.Fatalf("Exec = %v, want ErrToolPolicyRefused", err)
	}
	if effects != 0 || len(policy.inputs) != 0 || len(toolResults(t, log)) != 0 {
		t.Fatalf("effects=%d hook calls=%d receipts=%+v", effects, len(policy.inputs), toolResults(t, log))
	}
}

// A call whose journal form differs from what the executor receives, read as Go values or as the
// JSON encoding/json writes, is refused transiently before the hook: the hook would judge one value
// while the executor receives another. So is a value the guard cannot compare: a struct
// encoding/json writes field by field, and a value or map key whose type has a MarshalJSON or
// MarshalText method. Values the journal holds exactly still reach the hook and the executor.
func TestToolPolicyRefusesInexactArgs(t *testing.T) {
	over := int64(1<<53 + 1)
	for _, tc := range []struct {
		name    string
		args    map[string]any
		refused bool
	}{
		{"int64 above 2^53", map[string]any{"amount": over}, true},
		{"nested in a typed map", map[string]any{"limits": map[string]int64{"amount": over}}, true},
		{"nested in a slice", map[string]any{"list": []any{1.0, map[string]any{"amount": over}}}, true},
		{"through a pointer", map[string]any{"amount": &over}, true},
		{"uint64", map[string]any{"amount": uint64(1<<63 + 1)}, true},
		{"json.Number", map[string]any{"amount": json.Number("9007199254740993")}, true},
		{"json.Number exponent", map[string]any{"amount": json.Number("9007199254740993e0")}, true},
		{"json.Number decimal integer", map[string]any{"amount": json.Number("9007199254740993.0")}, true},
		{"negative json.Number exponent", map[string]any{"amount": json.Number("-90071992547409.93e2")}, true},
		{"json.Number fraction above 2^53", map[string]any{"amount": json.Number("9007199254740994.5")}, true},
		{"json.Number exponent beyond float64 precision", map[string]any{"amount": json.Number("1e300")}, true},
		{"json.Number fraction past the journal float", map[string]any{"amount": json.Number("0.30000000000000001")}, true},
		{"negative json.Number fraction past the journal float", map[string]any{"amount": json.Number("-0.30000000000000001")}, true},
		{"json.Number fraction below the journal float", map[string]any{"amount": json.Number("0.29999999999999999")}, true},
		{"json.Number exponent past the journal float", map[string]any{"amount": json.Number("30000000000000001e-17")}, true},
		{"big.Int above 2^53", map[string]any{"amount": new(big.Int).SetInt64(1<<53 + 1)}, true},
		{"json.Marshaler number in a slice", map[string]any{"list": []marshaledNumber{"9007199254740993"}}, true},
		{"json.Marshaler number in a named-byte slice", map[string]any{"list": []byteNumber{1}}, true},
		{"json.Marshaler number in a named-byte array", map[string]any{"list": [1]byteNumber{1}}, true},
		{"float32 beside a typed sibling", map[string]any{"amount": float32(1e20), "labels": map[string]string{"k": "v"}}, true},
		{"float32", map[string]any{"amount": float32(1e20)}, true},
		{"float32 fraction", map[string]any{"amount": float32(0.7)}, true},
		{"float32 power of two written in fewer digits", map[string]any{"amount": float32(1 << 60)}, true},
		{"invalid UTF-8 string", map[string]any{"path": "a\xffb"}, true},
		{"invalid UTF-8 key", map[string]any{"paths": map[string]any{"a\xffb": true}}, true},
		{"integer-keyed map", map[string]any{"byID": map[int]int64{1: over}}, true},
		{"int64 in a struct", map[string]any{"p": struct{ Amount int64 }{over}}, true},
		{"float32 in a struct", map[string]any{"p": struct{ Rate float32 }{1e20}}, true},
		{"invalid UTF-8 in a struct", map[string]any{"p": struct{ Memo string }{"a\xffb"}}, true},
		{"pointer to a struct", map[string]any{"p": &struct{ Amount int64 }{over}}, true},
		{"struct of exact values", map[string]any{"p": struct{ Amount int64 }{1}}, true},
		{"2^53", map[string]any{"amount": int64(1 << 53)}, false},
		{"exact int64 above 2^53", map[string]any{"amount": int64(1 << 60)}, false},
		{"min int64", map[string]any{"amount": int64(math.MinInt64)}, false},
		{"float64", map[string]any{"amount": float64(1<<60 + 1<<10)}, false},
		{"fractional json.Number", map[string]any{"amount": json.Number("0.1")}, false},
		{"json.Number fraction in its shortest form", map[string]any{"amount": json.Number("0.3")}, false},
		{"json.Number fraction in another spelling", map[string]any{"amount": json.Number("3.000e-1")}, false},
		{"small json.Number", map[string]any{"amount": json.Number("1e-7")}, false},
		{"json.Number exponent 2^53", map[string]any{"amount": json.Number("9007199254740992e0")}, false},
		{"json.Number decimal 2^53", map[string]any{"amount": json.Number("9007199254740992.0")}, false},
		{"json.Number exact exponent", map[string]any{"amount": json.Number("1.152921504606846976e18")}, false},
		{"exact big.Int above 2^53", map[string]any{"amount": new(big.Int).Lsh(big.NewInt(1), 60)}, true},
		{"integer type written as a string", map[string]any{"amount": stringInt(1 << 53)}, true},
		{"text marshaler bytes", map[string]any{"ip": net.ParseIP("10.0.0.1")}, true},
		{"json.Marshaler struct", map[string]any{"at": time.Unix(0, 0).UTC()}, true},
		{"json.RawMessage", map[string]any{"amount": json.RawMessage("1")}, true},
		{"text marshaler map key", map[string]any{"byAddr": map[netip.Addr]int{netip.MustParseAddr("10.0.0.1"): 1}}, true},
		{"string key with a text marshaler", map[string]any{"byName": map[textName]int{"a": 1}}, false},
		{"bytes", map[string]any{"blob": []byte{0xff, 0x01}}, false},
		{"byte array", map[string]any{"blob": [2]byte{0xff, 0x01}}, false},
		{"exact float32", map[string]any{"amount": float32(0.5), "big": float32(1 << 24)}, false},
		{"exact float32 beside a typed sibling", map[string]any{"amount": float32(1 << 24), "labels": map[string]string{"k": "v"}}, false},
		{"integer-keyed exact map", map[string]any{"byID": map[int]string{1: "a"}}, false},
		{"nil pointer and bool", map[string]any{"amount": (*int64)(nil), "ok": true}, false},
		{"nil pointer to a json.Marshaler", map[string]any{"at": (*time.Time)(nil)}, false},
		{"nil containers", map[string]any{"list": []any(nil), "typed": []string(nil), "map": map[string]any(nil), "blob": []byte(nil)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := memStore(t)
			effects := 0
			executor := func(context.Context, controller.ToolCallContext, api.ToolCall) (api.ToolResult, error) {
				effects++
				return api.ToolResult{}, nil
			}
			policy := &toolPolicy{}
			c, err := controller.New(log, echoModel, controller.WithSessionUID("s"), controller.WithToolExecutor(executor), controller.WithToolPolicy(policy.hook))
			if err != nil {
				t.Fatal(err)
			}
			call := policyCall()
			call.Args = tc.args
			err = c.Exec(t.Context(), &callHarness{calls: []api.ToolCall{call}}, []api.Message{msg("go")}, 0)
			if !tc.refused {
				if err != nil || effects != 1 || len(policy.inputs) != 1 {
					t.Fatalf("Exec = %v, effects=%d hook calls=%d; want the call to run", err, effects, len(policy.inputs))
				}
				return
			}
			if !errors.Is(err, controller.ErrToolPolicyRefused) || !strings.Contains(err.Error(), "cannot verify") {
				t.Fatalf("Exec = %v, want ErrToolPolicyRefused for a value the guard cannot verify", err)
			}
			if effects != 0 || len(policy.inputs) != 0 || len(toolResults(t, log)) != 0 {
				t.Fatalf("effects=%d hook calls=%d receipts=%+v", effects, len(policy.inputs), toolResults(t, log))
			}
		})
	}
}

// A value written by its own MarshalJSON or MarshalText method is refused before the hook, since
// the guard can compare only that output and the executor receives the Go value: an upper-bound
// policy that allows the written 1 must not run an executor that reads 100.
func TestToolPolicyRefusesMarshalerArgs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		amount any
	}{
		{"json.Marshaler", jsonAmount(100)},
		{"pointer to a json.Marshaler", func() *jsonAmount { a := jsonAmount(100); return &a }()},
		{"encoding.TextMarshaler", textAmount(100)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := memStore(t)
			var effects []any
			executor := func(_ context.Context, _ controller.ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
				effects = append(effects, call.Args["amount"])
				return api.ToolResult{}, nil
			}
			var judged []any
			limit := func(_ context.Context, in controller.ToolPolicyInput) controller.ToolPolicyDecision {
				judged = append(judged, in.Call.Args["amount"])
				f, ok := in.Call.Args["amount"].(float64)
				if s, isText := in.Call.Args["amount"].(string); isText {
					var err error
					f, err = strconv.ParseFloat(s, 64)
					ok = err == nil
				}
				if ok && f <= 10 {
					return controller.ToolPolicyDecision{Verdict: controller.ToolPolicyAllow}
				}
				return controller.ToolPolicyDecision{Verdict: controller.ToolPolicyDefinitiveRefusal, Reason: "over the limit"}
			}
			c, err := controller.New(log, echoModel, controller.WithSessionUID("s"), controller.WithToolExecutor(executor), controller.WithToolPolicy(limit))
			if err != nil {
				t.Fatal(err)
			}
			call := policyCall()
			call.Args = map[string]any{"amount": tc.amount}
			err = c.Exec(t.Context(), &callHarness{calls: []api.ToolCall{call}}, []api.Message{msg("go")}, 0)
			if !errors.Is(err, controller.ErrToolPolicyRefused) || !strings.Contains(err.Error(), "cannot verify") {
				t.Fatalf("Exec = %v (hook judged %v, executor received %v), want ErrToolPolicyRefused for a marshaler value", err, judged, effects)
			}
			if len(judged) != 0 || len(effects) != 0 || len(toolResults(t, log)) != 0 {
				t.Fatalf("hook judged %v, executor received %v, receipts=%+v", judged, effects, toolResults(t, log))
			}
		})
	}
}

// jsonAmount writes a JSON number other than its value.
type jsonAmount int64

func (jsonAmount) MarshalJSON() ([]byte, error) { return []byte("1"), nil }

// textAmount writes text other than its value.
type textAmount int64

func (textAmount) MarshalText() ([]byte, error) { return []byte("1"), nil }

// stringInt is an integer type whose JSON form is a string.
type stringInt int64

func (n stringInt) MarshalJSON() ([]byte, error) {
	return json.Marshal(strconv.FormatInt(int64(n), 10))
}

// byteNumber is a byte type whose JSON form is a number a float64 cannot hold exactly. A slice
// of it is a JSON array, not base64 text, because the element has a MarshalJSON method.
type byteNumber uint8

func (byteNumber) MarshalJSON() ([]byte, error) { return []byte("9007199254740993"), nil }

// marshaledNumber writes its literal as a JSON number.
type marshaledNumber string

func (n marshaledNumber) MarshalJSON() ([]byte, error) { return []byte(n), nil }

// textName is a string key type with a MarshalText method that encoding/json does not call for
// a key, since it writes a string-kind key as is.
type textName string

func (textName) MarshalText() ([]byte, error) { return []byte("other"), nil }

// An executor reply carrying POLICY_DENIED is refused on a call Resume re-drives too, with no
// receipt, so only a hook's refusal ever records the code.
func TestToolPolicyDeniedCodeIsHostOwnedOnRedrive(t *testing.T) {
	log := memStore(t)
	replies := []error{errors.New("transport lost"), nil}
	executor := func(context.Context, controller.ToolCallContext, api.ToolCall) (api.ToolResult, error) {
		err := replies[0]
		replies = replies[1:]
		return api.ToolResult{IsError: true, Code: api.ToolResultCodePolicyDenied}, err
	}
	c, err := controller.New(log, echoModel, controller.WithSessionUID("s"), controller.WithToolExecutor(executor), dedupAll)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Exec(t.Context(), &callHarness{calls: []api.ToolCall{policyCall()}}, []api.Message{msg("go")}, 0); err == nil {
		t.Fatal("Exec with a failing executor succeeded")
	}
	if _, err := c.Resume(t.Context(), &callHarness{calls: []api.ToolCall{policyCall()}}); !errors.Is(err, controller.ErrApprovalReceiptReport) {
		t.Fatalf("re-driven POLICY_DENIED reply = %v, want ErrApprovalReceiptReport", err)
	}
	if len(replies) != 0 || len(toolResults(t, log)) != 0 {
		t.Fatalf("executor replies left=%d receipts=%+v", len(replies), toolResults(t, log))
	}
	if err := controller.VerifyJournal(log); err != nil {
		t.Fatal(err)
	}
}
