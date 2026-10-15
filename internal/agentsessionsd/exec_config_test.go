package agentsessionsd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/aramase/agentsessions/api"
	v1 "github.com/aramase/agentsessions/api/genpb"
	"github.com/aramase/agentsessions/client"
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

// The hook sees the turn's harness and rewrites the first request's config, with or without inputs;
// it skips other methods, and its error refuses the Exec with INVALID_ARGUMENT.
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
	// An input-less Exec starts a new turn with its config unless an approval is pending, so the
	// hook sees it too.
	if got, err := recv(exec, &v1.ExecRequest{Session: "s1", Config: []byte("c")}); err != nil || string(got.GetConfig()) != "of-s1:c" {
		t.Fatalf("input-less Exec: config %q, err %v", got.GetConfig(), err)
	}
	if _, err := recv(exec, &v1.ExecRequest{Session: "s1", Config: []byte("bad")}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("refused config on an input-less Exec: err %v, want INVALID_ARGUMENT", err)
	}
	before := len(seen)
	if got, _ := recv("/agentsessions.v1.Sessions/Replay", &v1.ExecRequest{Session: "s1", Inputs: input(), Config: []byte("c")}); string(got.GetConfig()) != "c" {
		t.Fatalf("another method's request rewritten to %q", got.GetConfig())
	}
	if len(seen) != before {
		t.Fatalf("the hook ran for another method: %v", seen[before:])
	}
	if _, err := recv(exec, &v1.ExecRequest{Session: "s1", Inputs: input(), Config: []byte("bad")}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("refused config: err %v, want INVALID_ARGUMENT", err)
	}
}

// configHarness outputs the execution config it was started with.
type configHarness struct{}

func (configHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "cfg", Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}}, nil
}

func (configHarness) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	return sink.Output(ctx, "config:"+string(start.Config))
}

// Through Run and the real Sessions service, the hook covers every Exec that starts a turn: a
// client-set key the hook refuses is refused with or without inputs, and an input-less Exec runs
// with the config the hook returned, which is what the harness sees and the journal records.
func TestRunExecConfigCoversInputlessExec(t *testing.T) {
	h := serveHarness(t, configHarness{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	addrs := make(chan string, 1)
	hook := func(harness string, config []byte) ([]byte, error) {
		if strings.Contains(string(config), "member") {
			return nil, errors.New("only the host sets member")
		}
		return []byte(`{"member":"` + harness + `"}`), nil
	}
	done := make(chan error, 1)
	go func() {
		done <- Run([]string{"-journal", t.TempDir() + "/journal.db", "-addr", "127.0.0.1:0", "-model", "m", "-harness", "cfg=" + h},
			WithExecConfig(hook), withContext(ctx), withListening(func(addr string) { addrs <- addr }))
	}()
	var addr string
	select {
	case addr = <-addrs:
	case err := <-done:
		t.Fatalf("Run returned before serving: %v", err)
	}
	c, err := client.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	tctx, tcancel := context.WithTimeout(ctx, 10*time.Second)
	defer tcancel()

	first, err := c.Exec(tctx, client.ExecOptions{Harness: "cfg", Inputs: []string{"hi"}})
	if err != nil || first.Output != `config:{"member":"cfg"}` {
		t.Fatalf("with inputs: output %q, err %v", first.Output, err)
	}
	uid := first.Session.GetMetadata().GetUid()
	for _, inputs := range [][]string{{"hi"}, nil} {
		_, err := c.Exec(tctx, client.ExecOptions{Session: uid, Inputs: inputs, Config: []byte(`{"member":"client-chosen"}`)})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("client-set member with %d inputs: err %v, want INVALID_ARGUMENT", len(inputs), err)
		}
	}
	second, err := c.Exec(tctx, client.ExecOptions{Session: uid})
	if err != nil || second.Output != `config:{"member":"cfg"}` {
		t.Fatalf("input-less Exec: output %q, err %v", second.Output, err)
	}
	records, err := c.Replay(tctx, uid, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	starts := 0
	for _, rec := range records {
		if start := rec.GetEvent().GetExecutionStart(); start != nil {
			starts++
			if string(start.GetConfig()) != `{"member":"cfg"}` {
				t.Fatalf("EXECUTION_START %d config = %q, want the hook's", starts, start.GetConfig())
			}
		}
	}
	if starts != 2 {
		t.Fatalf("%d EXECUTION_START records, want 2 (the refused Execs start no turn)", starts)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}
