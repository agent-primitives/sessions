package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"strings"
	"testing"

	v1 "github.com/agent-primitives/sessions/api/genpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

type approvalCLIRecorder struct {
	v1.UnimplementedSessionsServer
	pending *v1.ApprovalRef
	request *v1.ApproveRequest
	calls   []string
	fail    bool
}

func (s *approvalCLIRecorder) GetSession(_ context.Context, q *v1.GetSessionRequest) (*v1.Session, error) {
	s.calls = append(s.calls, "get")
	return &v1.Session{PendingApproval: s.pending}, nil
}
func (s *approvalCLIRecorder) Approve(_ context.Context, q *v1.ApproveRequest) (*v1.ApproveResponse, error) {
	s.calls = append(s.calls, "decision")
	s.request = q
	return &v1.ApproveResponse{Decision: &v1.LogRecord{Seq: 11, ContentHash: "original"}}, nil
}
func (s *approvalCLIRecorder) Resume(context.Context, *v1.ResumeRequest) (*v1.Session, error) {
	s.calls = append(s.calls, "resume")
	if s.fail {
		return nil, status.Error(codes.Unavailable, "offline")
	}
	return &v1.Session{LastSeq: 15, ExecState: v1.ExecState_EXEC_COMPLETED}, nil
}
func captureApprovalOutput(t *testing.T, run func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old; r.Close() }()
	runErr := run()
	w.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), runErr
}
func TestCLIApproval(t *testing.T) {
	full := []string{"--execution", "e", "--tool-call", "c", "--request-seq", "9"}
	for _, tc := range []struct {
		name                          string
		run                           func([]string) error
		args                          []string
		pending, fail, approved, json bool
		calls, errorText              string
	}{
		{"approve discover UID first", cmdApprove, []string{"uid", "--reason", "reviewed", "--actor", "human", "--identity-issuer", "issuer", "--identity-subject", "subject"}, true, false, true, false, "get,decision,resume", ""},
		{"deny explicit JSON", cmdDeny, append([]string{"--session", "uid", "--json"}, full...), false, false, false, true, "decision,resume", ""},
		{"committed recovery failure", cmdApprove, append([]string{"--session", "uid"}, full...), false, true, true, false, "decision,resume", "decision committed"},
		{"committed recovery failure JSON", cmdApprove, append([]string{"--session", "uid", "--json"}, full...), false, true, true, true, "decision,resume", "decision committed"},
		{"partial tuple", cmdApprove, []string{"--session", "uid", "--execution", "e"}, false, false, false, false, "", "full"},
		{"nonpositive sequence", cmdApprove, []string{"--session", "uid", "--execution", "e", "--tool-call", "c", "--request-seq", "0"}, false, false, false, false, "", "positive"},
		{"duplicate UID", cmdApprove, []string{"--session", "uid", "other"}, false, false, false, false, "", "exactly one"},
		{"missing UID", cmdApprove, nil, false, false, false, false, "", "required"},
		{"no pending retry guidance", cmdDeny, []string{"--session", "uid"}, false, false, false, false, "get", "Resume"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &approvalCLIRecorder{fail: tc.fail}
			if tc.pending {
				s.pending = &v1.ApprovalRef{ExecutionId: "e", ToolCallId: "c", RequestSeq: 9}
			}
			lis, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			srv := grpc.NewServer()
			v1.RegisterSessionsServer(srv, s)
			go srv.Serve(lis)
			t.Cleanup(srv.Stop)
			args := append([]string{"--server", lis.Addr().String()}, tc.args...)
			if tc.name == "approve discover UID first" {
				args = append([]string{"uid", "--server", lis.Addr().String()}, tc.args[1:]...)
			}
			output, err := captureApprovalOutput(t, func() error { return tc.run(args) })
			if strings.Join(s.calls, ",") != tc.calls || tc.errorText == "" && err != nil || tc.errorText != "" && (err == nil || !strings.Contains(err.Error(), tc.errorText)) {
				t.Fatalf("calls=%v err=%v", s.calls, err)
			}
			if s.request != nil {
				q := s.request
				if q.Session != "uid" || q.ExecutionId != "e" || q.ToolCallId != "c" || q.RequestSeq != 9 || q.Approved == nil || q.GetApproved() != tc.approved {
					t.Fatalf("decision=%v", q)
				}
				if tc.pending && (q.Reason != "reviewed" || q.Identity.GetPrincipal() != "human" || q.Identity.GetIssuer() != "issuer" || q.Identity.GetSubject() != "subject") {
					t.Fatalf("provenance=%v", q)
				}
			}
			if tc.fail && (status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "offline")) {
				t.Fatalf("lost committed cause: %v", err)
			}
			if tc.fail && output != "" {
				t.Fatalf("recovery failure printed success: %q", output)
			}
			if tc.json && !tc.fail {
				var got v1.ApproveResponse
				if err := protojson.Unmarshal([]byte(output), &got); err != nil || got.Decision.GetContentHash() != "original" || got.Session.GetLastSeq() != 15 || got.Session.GetExecState() != v1.ExecState_EXEC_COMPLETED {
					t.Fatalf("JSON=%s err=%v", output, err)
				}
				var fields map[string]map[string]any
				if err := json.Unmarshal([]byte(output), &fields); err != nil {
					t.Fatal(err)
				}
				if fields["decision"]["content_hash"] != "original" || fields["session"]["last_seq"] != "15" || fields["decision"]["contentHash"] != nil {
					t.Fatalf("approval JSON has wrong field names: %v", fields)
				}
			} else if tc.errorText == "" && !strings.Contains(output, "execution=COMPLETED") {
				t.Fatalf("approval output hides execution state: %s", output)
			}
		})
	}
	for _, run := range []func([]string) error{cmdApprove, cmdDeny} {
		if err := run([]string{"--help"}); err != nil {
			t.Fatal(err)
		}
	}
}
