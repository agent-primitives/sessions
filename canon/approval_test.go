package canon_test

import (
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/aramase/agentsessions/api"
	v1 "github.com/aramase/agentsessions/api/genpb"
	"github.com/aramase/agentsessions/canon"
	"google.golang.org/protobuf/proto"
)

func TestApprovalCanonicalVectors(t *testing.T) {
	// Literal bytes/hashes from the pre-contract shape, not converter-derived expectations.
	for _, tc := range []struct {
		name        string
		event       api.Event
		bytes, hash string
	}{
		{"legacy receipt", api.Event{Kind: api.EventToolResult, Result: &api.ToolResult{ID: "c1"}},
			`{"event":{"kind":"EVENT_TOOL_RESULT","result":{"id":"c1"}},"prev_hash":"","seq":"1"}`, "2eddf9826b382796691c15da8ee7dd6af7b41dbab55c949f131ed2933e0b0d8e"},
		{"legacy decision", api.Event{Kind: api.EventApprovalResult, ApprovalResult: &api.ApprovalResult{ToolCallID: "c1", Reason: "no"}},
			`{"event":{"approval_result":{"reason":"no","tool_call_id":"c1"},"kind":"EVENT_APPROVAL_RESULT"},"prev_hash":"","seq":"1"}`, "c36a909b1ea5f243536ff6b4f1770e99e47b35cb1dfb94c2e943b85a42206bea"},
		{"correlated receipt", api.Event{ExecutionID: "e1", Kind: api.EventToolResult, Result: &api.ToolResult{ID: "c1", IsError: true, Code: api.ToolResultCodeApprovalDenied, ApprovalRequestSeq: 9007199254740993, ApprovalDecisionSeq: 9007199254740994}},
			`{"execution_id":"e1","kind":"EVENT_TOOL_RESULT","result":{"approval_decision_seq":"9007199254740994","approval_request_seq":"9007199254740993","code":"APPROVAL_DENIED","id":"c1","is_error":true}}`, ""},
		{"decision provenance", api.Event{ExecutionID: "e1", Kind: api.EventApprovalResult, Actor: api.IdentityRef{Principal: "human"}, ApprovalResult: &api.ApprovalResult{ToolCallID: "c1", Reason: "no", RequestSeq: 9007199254740993}},
			`{"actor":{"principal":"human"},"approval_result":{"reason":"no","request_seq":"9007199254740993","tool_call_id":"c1"},"execution_id":"e1","kind":"EVENT_APPROVAL_RESULT"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b []byte
			var err error
			if tc.hash != "" {
				b, err = canon.Record("", 1, tc.event)
			} else {
				b, err = canon.Event(tc.event)
			}
			if err != nil || string(b) != tc.bytes {
				t.Fatalf("canonical bytes = %s, %v; want %s", b, err, tc.bytes)
			}
			if tc.hash != "" {
				h, err := canon.HashRecord("", 1, tc.event)
				if err != nil || h != tc.hash {
					t.Fatalf("legacy hash = %s, %v; want %s", h, err, tc.hash)
				}
			}
		})
	}
	for _, tc := range []struct {
		name          string
		message       proto.Message
		bytes, digest string
	}{
		{"session", &v1.Session{Metadata: &v1.ResourceMetadata{Project: "tenant", Name: "name", Uid: "s1", Version: 7}, Harness: "echo", Model: "m", LastSeq: 99,
			Identity: &v1.IdentityRef{Principal: "p", Issuer: "i", Subject: "s"}, Origin: &v1.Origin{Source: "source", Subject: "subject", Uri: "uri", Attributes: map[string]string{"context": "value"}}, Labels: map[string]string{"label": "value"}, Annotations: map[string]string{"note": "value"}},
			`{"annotations":{"note":"value"},"harness":"echo","identity":{"issuer":"i","principal":"p","subject":"s"},"labels":{"label":"value"},"last_seq":"99","metadata":{"name":"name","project":"tenant","uid":"s1","version":"7"},"model":"m","origin":{"attributes":{"context":"value"},"source":"source","subject":"subject","uri":"uri"}}`, ""},
		{"registry", &v1.HarnessSpec{DescriptorId: "echo", Capabilities: &v1.Capabilities{Resumability: v1.Resumability_RESUMABILITY_STATELESS_REPLAY}, Placement: &v1.HarnessSpec_Remote{Remote: &v1.RemotePlacement{Address: "127.0.0.1:9000"}}},
			`{"capabilities":{"resumability":"RESUMABILITY_STATELESS_REPLAY"},"descriptor_id":"echo","remote":{"address":"127.0.0.1:9000"}}`, "sha256:f0c96c8b67d5dc75be557f64ccd59496eaf9cfb102754ee54e9d4c571eb2eafc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := canon.Proto(tc.message)
			if err != nil || string(b) != tc.bytes {
				t.Fatalf("compatibility bytes = %s, %v; want %s", b, err, tc.bytes)
			}
			if tc.digest != "" && "sha256:"+fmt.Sprintf("%x", sha256.Sum256(b)) != tc.digest {
				t.Fatal("registry digest changed")
			}
		})
	}
	// The transient carrier cannot affect either receipt or tool-message history hashes.
	result := &api.ToolResult{ID: "c1", Code: api.ToolResultCodeApprovalDenied, ApprovalRequestSeq: 42, ApprovalDecisionSeq: 43}
	for _, event := range []api.Event{{Kind: api.EventToolResult, Result: result}, {Kind: api.EventInput, Message: &api.Message{Role: "tool", Parts: []api.Part{{ToolResult: result}}}}} {
		before, err := canon.HashRecord("", 1, event)
		if err != nil {
			t.Fatal(err)
		}
		result.Approval = &api.ApprovalResult{ToolCallID: "c1", Approved: true, RequestSeq: 42}
		after, err := canon.HashRecord("", 1, event)
		if err != nil || before != after {
			t.Fatalf("transient hash changed: %s -> %s, %v", before, after, err)
		}
		result.Approval = nil
	}
}
