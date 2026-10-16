package client_test

import (
	"context"
	"net"
	"testing"

	"github.com/aramase/agentsessions/api"
	v1 "github.com/aramase/agentsessions/api/genpb"
	"github.com/aramase/agentsessions/client"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

type approvalSDKServer struct {
	v1.UnimplementedSessionsServer
	records, fail bool
	request       *v1.ApproveRequest
	resumes       int
}

func (s *approvalSDKServer) Exec(_ *v1.ExecRequest, stream v1.Sessions_ExecServer) error {
	for _, update := range []*v1.ExecUpdate{
		{Update: &v1.ExecUpdate_Session{Session: &v1.Session{Metadata: &v1.ResourceMetadata{Uid: "known"}, LastSeq: 10}}},
		{Update: &v1.ExecUpdate_Record{Record: &v1.LogRecord{Seq: 11}}},
		{Update: &v1.ExecUpdate_Session{Session: &v1.Session{Metadata: &v1.ResourceMetadata{Uid: "known"}, LastSeq: 12, PendingApproval: &v1.ApprovalRef{ExecutionId: "e", ToolCallId: "c", RequestSeq: 12}}}},
	} {
		if update.GetRecord() != nil && !s.records {
			continue
		}
		if update.GetSession().GetLastSeq() == 12 && s.fail {
			return status.Error(codes.Aborted, "stale")
		}
		if err := stream.Send(update); err != nil {
			return err
		}
	}
	return nil
}
func (s *approvalSDKServer) Approve(_ context.Context, q *v1.ApproveRequest) (*v1.ApproveResponse, error) {
	s.request = q
	return &v1.ApproveResponse{Decision: &v1.LogRecord{Seq: 14}}, nil
}
func (s *approvalSDKServer) Resume(context.Context, *v1.ResumeRequest) (*v1.Session, error) {
	s.resumes++
	return nil, nil
}

func TestApprovalSDK(t *testing.T) {
	for _, tc := range []struct {
		name          string
		records, fail bool
		cursor        int64
		frames        int
	}{
		{"latest beyond records", true, false, 12, 2}, {"query without records", false, false, 12, 2}, {"error retains initial", false, true, 10, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := &approvalSDKServer{records: tc.records, fail: tc.fail}
			lis := bufconn.Listen(1 << 20)
			grpcServer := grpc.NewServer()
			v1.RegisterSessionsServer(grpcServer, server)
			go grpcServer.Serve(lis)
			t.Cleanup(grpcServer.Stop)
			c, err := client.Dial("passthrough:///approval", client.WithDialOptions(grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) })))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { c.Close(); lis.Close() })
			frames := 0
			turn, err := c.Exec(t.Context(), client.ExecOptions{Session: "known", OnSession: func(*v1.Session) { frames++ }})
			wantCode := codes.OK
			if tc.fail {
				wantCode = codes.Aborted
			}
			if status.Code(err) != wantCode || turn == nil || turn.LastSeq != tc.cursor || turn.Session.GetLastSeq() != tc.cursor || turn.Session.GetMetadata().GetUid() != "known" || frames != tc.frames {
				t.Fatalf("turn=%+v frames=%d err=%v", turn, frames, err)
			}
			if tc.records || tc.fail {
				return
			} // Forwarding has one owner, not a stream-shape matrix.
			for _, approved := range []bool{false, true} {
				decision := api.ApprovalDecision{ExecutionID: "e", ToolCallID: "c", RequestSeq: 12, Approved: approved, Reason: "reviewed", Identity: api.IdentityRef{Principal: "human", Issuer: "issuer", Subject: "subject"}}
				response, err := c.Approve(t.Context(), "known", decision)
				q := server.request
				if err != nil || response.GetDecision().GetSeq() != 14 || q == nil || q.Approved == nil || q.GetApproved() != approved || q.Session != "known" || q.ExecutionId != "e" || q.ToolCallId != "c" || q.RequestSeq != 12 || q.Reason != "reviewed" || !proto.Equal(q.Identity, &v1.IdentityRef{Principal: "human", Issuer: "issuer", Subject: "subject"}) || server.resumes != 0 {
					t.Fatalf("SDK forwarding/implicit resume: %v %v %v", q, response, err)
				}
			}
		})
	}
}
