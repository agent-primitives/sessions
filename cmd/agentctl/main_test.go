package main

import (
	"net"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	v1 "github.com/aramase/agentsessions/api/genpb"
	"github.com/aramase/agentsessions/sqlitelog"
	"github.com/aramase/agentsessions/wire"
)

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestExecHarnessFlag(t *testing.T) {
	for _, tt := range []struct {
		name    string
		session string
		harness string
	}{
		{name: "host default"},
		{name: "create chat", harness: "chat"},
		{name: "session default", session: "existing"},
		{name: "turn override", session: "existing", harness: "chat"},
		{name: "unknown harness", harness: "unknown"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			requests := make(chan *v1.ExecRequest, 1)
			lis, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			srv := grpc.NewServer()
			v1.RegisterSessionsServer(srv, &execRecorder{requests: requests})
			go func() { _ = srv.Serve(lis) }()
			t.Cleanup(srv.Stop)

			args := []string{"--server", lis.Addr().String(), "--input", "hello"}
			if tt.session != "" {
				args = append(args, "--session", tt.session)
			}
			if tt.harness != "" {
				args = append(args, "--harness", tt.harness)
			}
			err = cmdExec(args)
			if tt.harness == "unknown" {
				if status.Code(err) != codes.InvalidArgument {
					t.Fatalf("unknown harness error = %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			select {
			case req := <-requests:
				if req.GetHarness() != tt.harness || req.GetSession() != tt.session {
					t.Fatalf("exec request = %v", req)
				}
				if len(req.GetInputs()) != 1 || wire.MessageFromProto(req.Inputs[0]).Text() != "hello" {
					t.Fatalf("inputs = %v", req.GetInputs())
				}
				if req.ExpectedLastSeq != nil {
					t.Fatal("default exec unexpectedly enabled CAS")
				}
			default:
				t.Fatal("exec sent no request")
			}
		})
	}
}

type execRecorder struct {
	v1.UnimplementedSessionsServer
	requests chan *v1.ExecRequest
}

func (s *execRecorder) Exec(req *v1.ExecRequest, stream v1.Sessions_ExecServer) error {
	s.requests <- req
	if req.GetHarness() == "unknown" {
		return status.Error(codes.InvalidArgument, "unknown harness")
	}
	return stream.Send(&v1.ExecUpdate{Update: &v1.ExecUpdate_Session{Session: &v1.Session{
		Metadata: &v1.ResourceMetadata{Uid: "session-uid"},
		Harness:  req.GetHarness(),
	}}})
}

// Embedded mode refuses a journal in which another host registered the harness name it serves, and
// opens one whose registrations use other names.
func TestEmbeddedRefusesRegistrationCollision(t *testing.T) {
	for _, tc := range []struct {
		name    string
		wantErr bool
	}{
		{name: "echo", wantErr: true},
		{name: "suite-agent"},
	} {
		journal := filepath.Join(t.TempDir(), "journal.db")
		store, err := sqlitelog.Open(journal)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.RegisterHarness(sqlitelog.HarnessRecord{Name: tc.name, UID: "u", Spec: "{}", SpecDigest: "d"}); err != nil {
			t.Fatal(err)
		}
		_ = store.Close()
		err = cmdList([]string{"--journal", journal})
		if tc.wantErr {
			if err == nil || !strings.Contains(err.Error(), "collides with a static harness name") {
				t.Errorf("registration %q: got %v, want a collision error", tc.name, err)
			}
		} else if err != nil {
			t.Errorf("registration %q: %v", tc.name, err)
		}
	}
}

// replay -v prints each record's chain links and the model call and usage fields, so the journal's
// digests and token counts can be read without a client of the API.
func TestFormatRecordVerbose(t *testing.T) {
	for _, tt := range []struct {
		r    *v1.LogRecord
		want string
	}{{
		r: &v1.LogRecord{Seq: 1, ContentHash: "sha256:0123456789abcdef0123", Event: &v1.Event{
			Kind: v1.EventKind_EVENT_INPUT, ExecutionId: "e1",
			Body: &v1.Event_Message{Message: &v1.Message{Role: "user", Parts: []*v1.Part{{Part: &v1.Part_Text{Text: &v1.TextPart{Text: "hi"}}}}}},
		}},
		want: "seq=1   EVENT_INPUT          hash=0123456789abcdef prev=- exec=e1  user: hi",
	}, {
		r: &v1.LogRecord{Seq: 2, ContentHash: "abc", PrevHash: "sha256:0123456789abcdef0123", Event: &v1.Event{
			Kind: v1.EventKind_EVENT_MODEL_CALL, Body: &v1.Event_Model{Model: &v1.ModelCall{Model: "gpt", InputHash: "sha256:fedcba98765432100000"}},
		}},
		want: "seq=2   EVENT_MODEL_CALL     hash=abc prev=0123456789abcdef  model=gpt input_hash=fedcba9876543210",
	}, {
		r: &v1.LogRecord{Seq: 3, ContentHash: "def", PrevHash: "abc", Event: &v1.Event{
			Kind: v1.EventKind_EVENT_USAGE, Body: &v1.Event_Usage{Usage: &v1.Usage{Model: "gpt", InputTokens: 12, OutputTokens: 5, ReasoningTokens: 1}},
		}},
		want: "seq=3   EVENT_USAGE          hash=def prev=abc  model=gpt input_tokens=12 output_tokens=5 reasoning_tokens=1",
	}, {
		r: &v1.LogRecord{Seq: 4, ContentHash: "c4", PrevHash: "def", Event: &v1.Event{
			Kind: v1.EventKind_EVENT_TOOL_CALL, Body: &v1.Event_Tool{Tool: &v1.ToolCall{Id: "call-1", Tool: "agentsuite.invoke", Mediation: v1.Mediation_MEDIATION_REQUIRES_APPROVAL,
				Args: mustStruct(t, map[string]any{"task": "review it", "agent": "reviewer"})}},
		}},
		want: `seq=4   EVENT_TOOL_CALL      hash=c4 prev=def  id=call-1 tool=agentsuite.invoke mediation=REQUIRES_APPROVAL args={"agent":"reviewer","task":"review it"}`,
	}, {
		r: &v1.LogRecord{Seq: 5, ContentHash: "c5", PrevHash: "c4", Event: &v1.Event{
			Kind: v1.EventKind_EVENT_APPROVAL_REQUEST, Body: &v1.Event_Approval{Approval: &v1.ApprovalRequest{ToolCallId: "call-1", Reason: "delegation"}},
		}},
		want: `seq=5   EVENT_APPROVAL_REQUEST hash=c5 prev=c4  tool_call=call-1 reason="delegation"`,
	}, {
		r: &v1.LogRecord{Seq: 6, ContentHash: "c6", PrevHash: "c5", Event: &v1.Event{
			Kind: v1.EventKind_EVENT_APPROVAL_RESULT, Body: &v1.Event_ApprovalResult{ApprovalResult: &v1.ApprovalResult{ToolCallId: "call-1", Reason: "no", RequestSeq: 5}},
		}},
		want: `seq=6   EVENT_APPROVAL_RESULT hash=c6 prev=c5  tool_call=call-1 approved=false reason="no" request_seq=5`,
	}, {
		r: &v1.LogRecord{Seq: 7, ContentHash: "c7", PrevHash: "c6", Event: &v1.Event{
			Kind: v1.EventKind_EVENT_TOOL_RESULT, Body: &v1.Event_Result{Result: &v1.ToolResult{Id: "call-1", Code: v1.ToolResult_APPROVAL_DENIED, IsError: true, Error: "denied"}},
		}},
		want: `seq=7   EVENT_TOOL_RESULT    hash=c7 prev=c6  id=call-1 code=APPROVAL_DENIED error="denied"`,
	}, {
		r: &v1.LogRecord{Seq: 8, ContentHash: "c8", PrevHash: "c7", Event: &v1.Event{
			Kind: v1.EventKind_EVENT_TOOL_RESULT, Body: &v1.Event_Result{Result: &v1.ToolResult{Id: "call-2", Output: mustStruct(t, map[string]any{"session": "sess-1", "agent": "writer"})}},
		}},
		want: `seq=8   EVENT_TOOL_RESULT    hash=c8 prev=c7  id=call-2 output={"agent":"writer","session":"sess-1"}`,
	}} {
		if got := formatRecordVerbose(tt.r); got != tt.want {
			t.Errorf("got  %q\nwant %q", got, tt.want)
		}
	}
}
