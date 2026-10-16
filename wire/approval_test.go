package wire_test

import (
	"encoding/hex"
	"reflect"
	"testing"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/wire"
	"google.golang.org/protobuf/proto"
)

func TestApprovalWireContract(t *testing.T) {
	for _, tc := range []struct {
		name   string
		event  api.Event
		legacy string
	}{
		{"legacy receipt", api.Event{Kind: api.EventToolResult, Result: &api.ToolResult{ID: "c1"}}, "38055a040a026331"},
		{"legacy denial", api.Event{Kind: api.EventApprovalResult, ApprovalResult: &api.ApprovalResult{ToolCallID: "c1", Reason: "no"}}, "38076a080a0263311a026e6f"},
		{"decision", api.Event{ExecutionID: "e1", Kind: api.EventApprovalResult, Actor: api.IdentityRef{Principal: "human", Issuer: "issuer", Subject: "subject"}, ApprovalResult: &api.ApprovalResult{ToolCallID: "c1", Approved: true, RequestSeq: 9007199254740993}}, ""},
		{"park", api.Event{ExecutionID: "e1", Kind: api.EventParked, Parked: &api.ApprovalRef{ExecutionID: "e1", ToolCallID: "c1", RequestSeq: 9007199254740993}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := proto.MarshalOptions{Deterministic: true}.Marshal(wire.EventToProto(tc.event))
			if err != nil {
				t.Fatal(err)
			}
			if tc.legacy != "" && hex.EncodeToString(b) != tc.legacy {
				t.Fatalf("legacy bytes = %x, want %s", b, tc.legacy)
			}
			var decoded v1.Event
			if err := proto.Unmarshal(b, &decoded); err != nil {
				t.Fatal(err)
			}
			if got := wire.EventFromProto(&decoded); !reflect.DeepEqual(got, tc.event) {
				t.Fatalf("roundtrip = %#v, want %#v", got, tc.event)
			}
		})
	}
	for _, code := range []api.ToolResultCode{api.ToolResultCodeUnspecified, api.ToolResultCodeApprovalDenied, api.ToolResultCodeExecutorError, 99} {
		want := &api.ToolResult{ID: "c1", Code: code, IsError: code != api.ToolResultCodeUnspecified, ApprovalRequestSeq: 9007199254740993, ApprovalDecisionSeq: 9007199254740994}
		pb := wire.ToolResultToProto(want)
		if pb.GetCode() != v1.ToolResult_Code(code) || !reflect.DeepEqual(wire.ToolResultFromProto(pb), want) {
			t.Fatalf("receipt conversion = %v", pb)
		}
		want.Approval = &api.ApprovalResult{ToolCallID: "other", Approved: true, RequestSeq: 42}
		if !proto.Equal(pb, wire.ToolResultToProto(want)) {
			t.Fatal("transient approval leaked into receipt")
		}
		want.Approval = nil
		message := &api.Message{Role: "tool", Parts: []api.Part{{ToolResult: want}}}
		if got := wire.MessageFromProto(wire.MessageToProto(message)); !reflect.DeepEqual(got, message) {
			t.Fatalf("model receipt = %#v", got)
		}
	}
	if wire.ToolResultToProto(nil) != nil || wire.ToolResultFromProto(nil) != nil {
		t.Fatal("nil converters must preserve absence")
	}
	for _, approved := range []*bool{nil, proto.Bool(false), proto.Bool(true)} {
		want := &v1.ApproveRequest{Session: "s1", ExecutionId: "e1", ToolCallId: "c1", RequestSeq: 9007199254740993, Approved: approved, Reason: "reviewed", Identity: &v1.IdentityRef{Principal: "human"}}
		b, err := proto.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		var got v1.ApproveRequest
		if err := proto.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(&got, want) || (got.Approved == nil) != (approved == nil) {
			t.Fatalf("decision presence = %v, want %v", &got, want)
		}
	}
}
