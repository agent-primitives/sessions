package agentsessionsd

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	v1 "github.com/aramase/agentsessions/api/genpb"
)

// recvStream serves queued ExecRequests to RecvMsg.
type recvStream struct {
	grpc.ServerStream
	reqs []*v1.ExecRequest
}

func (s *recvStream) Context() context.Context { return context.Background() }

func (s *recvStream) RecvMsg(m any) error {
	if len(s.reqs) == 0 {
		return errors.New("EOF")
	}
	dst := m.(*v1.ExecRequest)
	proto.Reset(dst)
	proto.Merge(dst, s.reqs[0])
	s.reqs = s.reqs[1:]
	return nil
}

func input() []*v1.Message { return []*v1.Message{{Role: "user"}} }

// The hook sees the turn's harness and rewrites the first request's config; it skips other methods
// and input-less (recovery) Execs, and its error refuses the Exec with INVALID_ARGUMENT.
func TestExecConfigInterceptor(t *testing.T) {
	var seen []string
	f := func(harness string, config []byte) ([]byte, error) {
		seen = append(seen, harness)
		if string(config) == "bad" {
			return nil, errors.New("refused")
		}
		return []byte(harness + ":" + string(config)), nil
	}
	harnessOf := func(req *v1.ExecRequest) (string, error) {
		if req.GetHarness() != "" {
			return req.GetHarness(), nil
		}
		return "of-" + req.GetSession(), nil
	}
	icpt := execConfigInterceptor(f, harnessOf)
	recv := func(method string, reqs ...*v1.ExecRequest) (*v1.ExecRequest, error) {
		var got v1.ExecRequest
		err := icpt(nil, &recvStream{reqs: reqs}, &grpc.StreamServerInfo{FullMethod: method}, func(_ any, ss grpc.ServerStream) error {
			return ss.RecvMsg(&got)
		})
		return &got, err
	}
	exec := v1.Sessions_Exec_FullMethodName

	got, err := recv(exec, &v1.ExecRequest{Session: "s1", Inputs: input(), Config: []byte("c")})
	if err != nil || string(got.GetConfig()) != "of-s1:c" {
		t.Fatalf("session harness: config %q, err %v", got.GetConfig(), err)
	}
	if got, _ := recv(exec, &v1.ExecRequest{Session: "s1", Harness: "writer", Inputs: input()}); string(got.GetConfig()) != "writer:" {
		t.Fatalf("override harness: config %q", got.GetConfig())
	}
	before := len(seen)
	if got, _ := recv(exec, &v1.ExecRequest{Session: "s1", Config: []byte("c")}); string(got.GetConfig()) != "c" {
		t.Fatalf("recovery Exec config rewritten to %q", got.GetConfig())
	}
	if got, _ := recv("/agentsessions.v1.Sessions/Replay", &v1.ExecRequest{Session: "s1", Inputs: input(), Config: []byte("c")}); string(got.GetConfig()) != "c" {
		t.Fatalf("another method's request rewritten to %q", got.GetConfig())
	}
	if len(seen) != before {
		t.Fatalf("the hook ran for a recovery Exec or another method: %v", seen[before:])
	}
	if _, err := recv(exec, &v1.ExecRequest{Session: "s1", Inputs: input(), Config: []byte("bad")}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("refused config: err %v, want INVALID_ARGUMENT", err)
	}
}
