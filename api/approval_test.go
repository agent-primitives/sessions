package api_test

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/agent-primitives/sessions/api"
)

func TestIsParkUnwind(t *testing.T) {
	ref := api.ApprovalRef{ExecutionID: "e1", ToolCallID: "c1", RequestSeq: 3}
	park := &api.ApprovalParkedError{Ref: ref}
	other := &api.ApprovalParkedError{Ref: api.ApprovalRef{ExecutionID: "e2", ToolCallID: "c1", RequestSeq: 3}}
	failure := errors.New("run failed")
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"failure", failure, false},
		{"bare sentinel", api.ErrApprovalParked, false},
		{"nil park", (*api.ApprovalParkedError)(nil), false},
		{"park", park, true},
		{"wrong request", other, false},
		{"wrapped", fmt.Errorf("run: %w", park), true},
		{"joined matching", errors.Join(park, fmt.Errorf("run: %w", park)), true},
		{"joined mismatch", errors.Join(park, other), false},
		{"joined failure", errors.Join(park, failure), false},
		{"nested failure", fmt.Errorf("run: %w", errors.Join(park, errors.Join(park, failure))), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := api.IsParkUnwind(tc.err, ref); got != tc.want {
				t.Fatalf("IsParkUnwind=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestApprovalContract(t *testing.T) {
	encode := func(v any) string {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	ref := api.ApprovalRef{ExecutionID: "e1", ToolCallID: "c1", RequestSeq: 9007199254740993}
	for _, wrapped := range []bool{false, true} {
		var err error = &api.ApprovalParkedError{Ref: ref}
		if wrapped {
			err = fmt.Errorf("run: %w", err)
		}
		var parked *api.ApprovalParkedError
		if !errors.Is(err, api.ErrApprovalParked) || !errors.As(err, &parked) || parked.Ref != ref || errors.Is(errors.New(err.Error()), api.ErrApprovalParked) {
			t.Fatalf("park correlation/sentinel: %v, %#v", err, parked)
		}
	}
	for _, tc := range []struct {
		value api.ApprovalResult
		want  string
	}{
		{api.ApprovalResult{ToolCallID: "c1", Reason: "no"}, `{"ToolCallID":"c1","Approved":false,"Reason":"no"}`},
		{api.ApprovalResult{ToolCallID: "c1", Reason: "no", RequestSeq: 42}, `{"ToolCallID":"c1","Approved":false,"Reason":"no","RequestSeq":42}`},
	} {
		if got := encode(tc.value); got != tc.want {
			t.Fatalf("decision JSON = %s, want %s", got, tc.want)
		}
	}
	input := func(result api.ToolResult) api.ModelRequest {
		return api.ModelRequest{Model: "m", Messages: []api.Message{{Role: "tool", Parts: []api.Part{{ToolResult: &result}}}}}
	}
	// Independent pre-contract JSON/hash; never derive this pin from a converter.
	const legacy = `{"Model":"m","Messages":[{"Role":"tool","Parts":[{"Text":null,"File":null,"Data":null,"Reasoning":null,"ToolResult":{"ID":"c1","Output":null,"OutputURI":"","OutputDigest":"","IsError":false,"Error":""}}]}],"Params":null}`
	for _, transient := range []*api.ApprovalResult{nil, {ToolCallID: "other", Approved: true, RequestSeq: 42}} {
		got := encode(input(api.ToolResult{ID: "c1", Approval: transient}))
		if got != legacy || fmt.Sprintf("%x", sha256.Sum256([]byte(got))) != "e519b8f957d02755a004b638c9d819f925e8bb9b65fa06a71bfd4f121b95472a" {
			t.Fatalf("legacy input changed: %s", got)
		}
	}
	base := api.ToolResult{ID: "c1", IsError: true, Code: api.ToolResultCodeApprovalDenied, ApprovalRequestSeq: ref.RequestSeq, ApprovalDecisionSeq: ref.RequestSeq + 1}
	before := encode(input(base))
	for name, change := range map[string]func(*api.ToolResult){
		"code":     func(r *api.ToolResult) { r.Code = api.ToolResultCodeExecutorError },
		"request":  func(r *api.ToolResult) { r.ApprovalRequestSeq-- },
		"decision": func(r *api.ToolResult) { r.ApprovalDecisionSeq-- },
	} {
		t.Run(name, func(t *testing.T) {
			r := base
			change(&r)
			if encode(input(r)) == before {
				t.Fatal("durable receipt field omitted from model input")
			}
		})
	}
}
