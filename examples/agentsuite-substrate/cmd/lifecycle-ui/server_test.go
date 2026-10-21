package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/client"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/harnesswire"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

// The refusal as the sessions service and the substrate runtime write it for a memory-tier session
// whose suspend completed a VOLUMES snapshot (session/service.go checkpointRefusal wrapping
// runtime/substrate requireCheckpoint, then api.ErrMemoryStateLost).
const refusalMsg = `suspend: session "sess-abc": runtime: checkpoint does not meet the required contract: ` +
	`actor agentsuite-demo/sess-abc: required MEMORY with a durable URI, observed VOLUMES (URI present: true); ` +
	`actor remains suspended: runtime: session's in-memory harness state is lost: the checkpoint cannot restore the session's in-RAM state`

// fakeSessions is the part of the Sessions API the bridge calls, in memory.
type fakeSessions struct {
	v1.UnimplementedSessionsServer
	mu         sync.Mutex
	sessions   map[string]*v1.Session
	log        map[string][]*v1.LogRecord
	suspendErr error
	forks      int
}

func newFake() *fakeSessions {
	return &fakeSessions{sessions: map[string]*v1.Session{}, log: map[string][]*v1.LogRecord{}}
}

func (f *fakeSessions) CreateSession(_ context.Context, r *v1.CreateSessionRequest) (*v1.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	uid := fmt.Sprintf("sess-%d", len(f.sessions)+1)
	s := &v1.Session{Metadata: &v1.ResourceMetadata{Uid: uid, Name: r.GetSession().GetMetadata().GetName(),
		Project: r.GetSession().GetMetadata().GetProject()}, Harness: r.GetSession().GetHarness(),
		ComputeState: v1.ComputeState_COMPUTE_NONE}
	f.sessions[uid] = s
	return s, nil
}

func (f *fakeSessions) GetSession(_ context.Context, r *v1.GetSessionRequest) (*v1.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.sessions[r.GetUid()]; ok {
		return proto.Clone(s).(*v1.Session), nil
	}
	return nil, status.Error(codes.NotFound, "no session")
}

func (f *fakeSessions) ListSessions(_ context.Context, r *v1.ListSessionsRequest) (*v1.ListSessionsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &v1.ListSessionsResponse{}
	for _, s := range f.sessions {
		if s.GetMetadata().GetProject() == r.GetProject() {
			out.Sessions = append(out.Sessions, s)
		}
	}
	return out, nil
}

// Exec journals a valid turn (EXECUTION_START, INPUT, MODEL_CALL, OUTPUT, END) and streams it.
func (f *fakeSessions) Exec(r *v1.ExecRequest, stream v1.Sessions_ExecServer) error {
	f.mu.Lock()
	s, ok := f.sessions[r.GetSession()]
	f.mu.Unlock()
	if !ok {
		return status.Error(codes.NotFound, "no session")
	}
	if err := stream.Send(&v1.ExecUpdate{Update: &v1.ExecUpdate_Session{Session: s}}); err != nil {
		return err
	}
	text := r.GetInputs()[0].GetParts()[0].GetText().GetText()
	recs := turn(f.log[r.GetSession()], text)
	f.mu.Lock()
	f.log[r.GetSession()] = recs
	s.ComputeState = v1.ComputeState_COMPUTE_LIVE
	f.mu.Unlock()
	for _, rec := range recs[len(recs)-5:] {
		if err := stream.Send(&v1.ExecUpdate{Update: &v1.ExecUpdate_Record{Record: rec}}); err != nil {
			return err
		}
	}
	return nil
}

func turn(prev []*v1.LogRecord, text string) []*v1.LogRecord {
	recs := make([]eventlog.Record, 0, len(prev))
	for _, p := range prev {
		recs = append(recs, eventlog.RecordFromProto(p))
	}
	l := eventlog.NewFrom(recs)
	exec := fmt.Sprintf("x%d", len(prev))
	for _, ev := range []api.Event{
		{Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{Harness: "triage"}},
		{Kind: api.EventInput, Message: &api.Message{Role: "user", Parts: []api.Part{{Text: &api.TextPart{Text: text}}}}},
		{Kind: api.EventModelCall, ModelCall: &api.ModelCall{Model: "triage-model"}},
		{Kind: api.EventOutput, Message: &api.Message{Role: "assistant", Parts: []api.Part{{Text: &api.TextPart{Text: "1) " + text}}}}},
		{Kind: api.EventEnd, End: &api.HarnessEnd{State: "COMPLETED"}},
	} {
		ev.ExecutionID = exec
		ev.Timestamp = time.Unix(1, 0).UTC()
		if _, err := l.Append(l.Head(), 0, ev); err != nil {
			panic(err)
		}
	}
	out := make([]*v1.LogRecord, 0)
	for _, r := range l.Snapshot() {
		out = append(out, eventlog.RecordToProto(r))
	}
	return out
}

func (f *fakeSessions) Replay(r *v1.ReplayRequest, stream v1.Sessions_ReplayServer) error {
	f.mu.Lock()
	recs := f.log[r.GetSession()]
	f.mu.Unlock()
	for _, rec := range recs {
		if err := stream.Send(rec); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeSessions) Suspend(_ context.Context, r *v1.SuspendRequest) (*v1.Session, error) {
	if f.suspendErr != nil {
		return nil, f.suspendErr
	}
	return f.setCompute(r.GetSession(), v1.ComputeState_COMPUTE_COLD)
}

func (f *fakeSessions) Resume(_ context.Context, r *v1.ResumeRequest) (*v1.Session, error) {
	return f.setCompute(r.GetSession(), v1.ComputeState_COMPUTE_LIVE)
}

func (f *fakeSessions) setCompute(uid string, cs v1.ComputeState) (*v1.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[uid]
	if !ok {
		return nil, status.Error(codes.NotFound, "no session")
	}
	s.ComputeState = cs
	return proto.Clone(s).(*v1.Session), nil
}

func (f *fakeSessions) Fork(_ context.Context, r *v1.ForkRequest) (*v1.ForkResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forks++
	uid := fmt.Sprintf("child-%d", f.forks)
	c := &v1.Session{Metadata: &v1.ResourceMetadata{Uid: uid}, ParentUid: r.GetSession(), ForkSeq: r.GetAtSeq()}
	f.sessions[uid] = c
	return &v1.ForkResponse{Children: []*v1.Session{c}}, nil
}

// testServer serves the bridge over httptest with one lane per fake, named in order.
func testServer(t *testing.T, fakes map[string]*fakeSessions) (*server, *httptest.Server) {
	t.Helper()
	s := &server{cfg: config{Project: "lifecycle-ui", Atespace: "agentsuite-demo", Namespace: "agentsuite-demo"},
		now: time.Now, owner: map[string]*lane{}}
	for _, name := range []string{"stateless", "memory", "volumes"} {
		f, ok := fakes[name]
		if !ok {
			continue
		}
		lis := bufconn.Listen(1 << 20)
		g := grpc.NewServer()
		v1.RegisterSessionsServer(g, f)
		go func() { _ = g.Serve(lis) }()
		t.Cleanup(g.Stop)
		c, err := client.Dial("passthrough:///bufnet", client.WithDialOptions(grpc.WithContextDialer(
			func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) })))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		tier := "STATELESS_REPLAY"
		if name != "stateless" {
			tier = "REQUIRES_MEMORY_SNAPSHOT"
		}
		s.lanes = append(s.lanes, &lane{Name: name, Harness: "triage", Daemon: "agentsessionsd-" + name, Addr: "bufnet", Tier: tier, c: c})
	}
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)
	return s, ts
}

func post(t *testing.T, ts *httptest.Server, path string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+path, "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func postActivity(t *testing.T, ts *httptest.Server, path string, body any) activity {
	t.Helper()
	resp := post(t, ts, path, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: %s", path, resp.Status)
	}
	var a activity
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestLifecycleThroughTheBridge(t *testing.T) {
	_, ts := testServer(t, map[string]*fakeSessions{"stateless": newFake()})

	created := postActivity(t, ts, "/api/create", map[string]string{"lane": "stateless", "name": "demo"})
	if !created.OK || created.Session == "" || created.After.Harness != "triage" {
		t.Fatalf("create: %+v", created)
	}
	sid := created.Session

	// Exec streams the session frame, every record, then the finished activity.
	resp := post(t, ts, "/api/exec", map[string]string{"session": sid, "input": "my laptop will not boot"})
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("exec content type %q", ct)
	}
	var types []string
	var final activity
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var frame struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(sc.Bytes(), &frame); err != nil {
			t.Fatal(err)
		}
		types = append(types, frame.Type)
		if frame.Type == "activity" {
			if err := json.Unmarshal(frame.Data, &final); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := strings.Join(types, ","); got != "session,record,record,record,record,record,activity" {
		t.Fatalf("exec frames %s", got)
	}
	if !final.OK || final.Output != "1) my laptop will not boot" || len(final.Records) != 5 {
		t.Fatalf("exec activity: %+v", final)
	}

	// The journal is read back over Replay and its chain checked by the bridge.
	var j journal
	r, err := http.Get(ts.URL + "/api/journal?session=" + sid)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Body.Close() }()
	if err := json.NewDecoder(r.Body).Decode(&j); err != nil {
		t.Fatal(err)
	}
	if !j.Verified || len(j.Records) != 5 || j.Records[1].Kind != "INPUT" || j.Records[1].Text != "my laptop will not boot" {
		t.Fatalf("journal: %+v", j)
	}

	forked := postActivity(t, ts, "/api/fork", map[string]any{"session": sid, "atSeq": 5})
	if !forked.OK || forked.Child != "child-1" || forked.AtSeq != 5 {
		t.Fatalf("fork: %+v", forked)
	}
	// The child is found on the lane its parent was forked on.
	if a := postActivity(t, ts, "/api/suspend", map[string]string{"session": forked.Child}); !a.OK || a.Lane != "stateless" {
		t.Fatalf("suspend child: %+v", a)
	}
}

func TestRefusalIsSplitIntoItsFields(t *testing.T) {
	f := newFake()
	f.suspendErr = status.Error(codes.FailedPrecondition, refusalMsg)
	_, ts := testServer(t, map[string]*fakeSessions{"volumes": f})
	sid := postActivity(t, ts, "/api/create", map[string]string{"lane": "volumes"}).Session

	a := postActivity(t, ts, "/api/suspend", map[string]string{"session": sid})
	if a.OK || a.Code != "FailedPrecondition" || a.Message != refusalMsg {
		t.Fatalf("suspend: ok=%v code=%q message=%q", a.OK, a.Code, a.Message)
	}
	want := refusal{Op: "suspend", Session: "sess-abc", Actor: "agentsuite-demo/sess-abc", Required: "MEMORY",
		Observed: "VOLUMES", Suspended: true, MemoryLost: true}
	if a.Refusal == nil || *a.Refusal != want {
		t.Fatalf("refusal = %+v, want %+v", a.Refusal, want)
	}
	if a.After == nil || a.After.UID != sid {
		t.Fatalf("a refusal still reports the session as it was left: %+v", a.After)
	}
}

func TestParseRefusal(t *testing.T) {
	// A FAILED_PRECONDITION that is not a fidelity refusal has no fields to show.
	if r := parseRefusal(`fork: memory-tier harness forks only at the head`); r != nil {
		t.Errorf("unrelated refusal parsed as %+v", r)
	}
	// The VOLUMES-only Snapshot refusal names the actor only: it suspended nothing.
	r := parseRefusal(`suspend: session "s1": runtime: checkpoint does not meet the required contract: ` +
		`actor ns/s1: VOLUMES-only backend cannot capture a memory-required checkpoint; no actor was suspended`)
	if r == nil || r.Actor != "ns/s1" || r.Session != "s1" || r.Required != "" || r.Observed != "" || r.Suspended {
		t.Errorf("VOLUMES-only refusal parsed as %+v", r)
	}
}

func TestReplayIsRefusedWhereItCannotRun(t *testing.T) {
	s, ts := testServer(t, map[string]*fakeSessions{"memory": newFake()})
	sid := postActivity(t, ts, "/api/create", map[string]string{"lane": "memory"}).Session
	// No router: the page says how to turn replay on, and records nothing.
	if resp := post(t, ts, "/api/replay", map[string]string{"session": sid}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("replay without a router: %s", resp.Status)
	}
	s.cfg.Router = "127.0.0.1:1"
	a := postActivity(t, ts, "/api/replay", map[string]string{"session": sid})
	if a.OK || a.FailedAt != "bridge" || !a.Declined || a.Replay != nil || !strings.Contains(a.Message, "only the stateless tier") {
		t.Fatalf("memory-tier replay: %+v", a)
	}
	// The trace names no call the bridge did not make, and no hop past it as reached.
	tr := s.trace(context.Background(), &a)
	if b := tr.Nodes["bridge"]; b.State != "declined" || strings.Contains(b.Detail, "then controller.Replay") ||
		!strings.Contains(b.Detail, "Sessions.Replay not called: ") || !strings.Contains(b.Detail, "only the stateless tier") {
		t.Errorf("bridge: %+v", b)
	}
	for _, n := range []string{"daemon", "ateapi", "worker", "actor", "router", "model"} {
		if ev := tr.Nodes[n]; ev.State != "skipped" {
			t.Errorf("%s: %+v, want skipped", n, ev)
		}
	}
}

// A suspended (or never placed) stateless session is declined before Sessions.Replay: routing the
// replay's turns would make atenet-router wake the actor outside Sessions. Nothing reaches the
// router, the session stays COMPUTE_COLD, and after a visible Resume the replay goes ahead.
func TestReplayOfASessionThatIsNotLiveIsDeclined(t *testing.T) {
	f := newFake()
	s, ts := testServer(t, map[string]*fakeSessions{"stateless": f})
	router, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = router.Close() })
	dialed := make(chan struct{}, 1)
	go func() {
		for {
			c, err := router.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
			select {
			case dialed <- struct{}{}:
			default:
			}
		}
	}()
	s.cfg.Router = router.Addr().String()
	sid := postActivity(t, ts, "/api/create", map[string]string{"lane": "stateless"}).Session

	// Created, no turn yet: no actor to drive.
	a := postActivity(t, ts, "/api/replay", map[string]string{"session": sid})
	if a.OK || !a.Declined || a.FailedAt != "bridge" || a.Replay != nil || !strings.Contains(a.Message, "COMPUTE_NONE, not COMPUTE_LIVE") {
		t.Fatalf("replay before a turn: %+v", a)
	}

	resp := post(t, ts, "/api/exec", map[string]string{"session": sid, "input": "hi"})
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	if sus := postActivity(t, ts, "/api/suspend", map[string]string{"session": sid}); !sus.OK || sus.After.ComputeState != "COMPUTE_COLD" {
		t.Fatalf("suspend: %+v", sus)
	}
	a = postActivity(t, ts, "/api/replay", map[string]string{"session": sid})
	if a.OK || !a.Declined || a.FailedAt != "bridge" || a.Replay != nil ||
		!strings.Contains(a.Message, "suspended (COMPUTE_COLD)") || !strings.Contains(a.Message, "Resume the session first") {
		t.Fatalf("replay while suspended: %+v", a)
	}
	if a.After == nil || a.After.ComputeState != "COMPUTE_COLD" {
		t.Fatalf("after a declined replay: %+v", a.After)
	}
	select {
	case <-dialed:
		t.Fatal("a declined replay reached the router")
	case <-time.After(100 * time.Millisecond):
	}
	if got, _ := f.GetSession(context.Background(), &v1.GetSessionRequest{Uid: sid}); got.GetComputeState() != v1.ComputeState_COMPUTE_COLD {
		t.Fatalf("session compute %s after a declined replay", got.GetComputeState())
	}
	tr := s.trace(context.Background(), &a)
	if b := tr.Nodes["bridge"]; b.State != "declined" || !strings.Contains(b.Detail, "Sessions.Replay not called: ") ||
		!strings.Contains(b.Detail, "COMPUTE_COLD") {
		t.Errorf("bridge: %+v", b)
	}
	for _, n := range []string{"daemon", "ateapi", "worker", "actor", "router", "model"} {
		if ev := tr.Nodes[n]; ev.State != "skipped" {
			t.Errorf("%s: %+v, want skipped", n, ev)
		}
	}

	// Resumed through Sessions, the replay is no longer declined (it then dials the router).
	if res := postActivity(t, ts, "/api/resume", map[string]string{"session": sid}); !res.OK || res.After.ComputeState != "COMPUTE_LIVE" {
		t.Fatalf("resume: %+v", res)
	}
	a = postActivity(t, ts, "/api/replay", map[string]string{"session": sid})
	if a.Declined || a.Replay == nil || a.Replay.Turns != 1 {
		t.Fatalf("replay after resume: %+v", a)
	}
}

func TestReplayableCompute(t *testing.T) {
	for cs := range v1.ComputeState_name {
		err := replayableCompute(v1.ComputeState(cs))
		if want := v1.ComputeState(cs) == v1.ComputeState_COMPUTE_LIVE; (err == nil) != want {
			t.Errorf("%s: err %v", v1.ComputeState(cs), err)
		}
	}
}

// A replay that stops after the journal read, here because the fake model's stats forward is
// gone, says what it established and nothing more: no re-run, no zero-call claim, no verdict on
// the model, and the failure is the bridge's, not the daemon's.
func TestFailedReplayClaimsNoSuccess(t *testing.T) {
	s, ts := testServer(t, map[string]*fakeSessions{"stateless": newFake()})
	sid := postActivity(t, ts, "/api/create", map[string]string{"lane": "stateless"}).Session
	resp := post(t, ts, "/api/exec", map[string]string{"session": sid, "input": "hi"})
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	stats := httptest.NewServer(http.NotFoundHandler())
	stats.Close()
	s.cfg.Router = "127.0.0.1:1"
	s.cfg.ModelStats = stats.URL + "/stats"

	a := postActivity(t, ts, "/api/replay", map[string]string{"session": sid})
	if a.OK || a.FailedAt != "bridge" || !strings.Contains(a.Message, "read fake model stats") {
		t.Fatalf("replay: ok=%v failedAt=%q message=%q", a.OK, a.FailedAt, a.Message)
	}
	want := replayResult{Records: 5, Turns: 1, ControllerCalls: -1, FakeBefore: -1, FakeAfter: -1}
	if r := a.Replay; r == nil || r.Records != want.Records || r.Turns != want.Turns ||
		r.ControllerCalls != -1 || r.FakeBefore != -1 || r.FakeAfter != -1 || r.ModelCalled || r.Outputs != nil {
		t.Fatalf("replay result %+v, want %+v", a.Replay, want)
	}

	tr := s.trace(context.Background(), &a)
	if m := tr.Nodes["model"]; m.State != "unknown" || !strings.Contains(m.Detail, "not established") ||
		strings.Contains(m.Detail, "counted 0") {
		t.Errorf("model: %+v", m)
	}
	if b := tr.Nodes["bridge"]; b.State != "failed" || !strings.Contains(b.Detail, "read fake model stats") ||
		strings.Contains(b.Detail, "then controller.Replay") || !strings.Contains(b.Detail, "controller.Replay did not run") {
		t.Errorf("bridge: %+v", b)
	}
	if d := tr.Nodes["daemon"]; d.State == "refused" {
		t.Errorf("daemon: %+v, want the Replay RPC it served left as traced", d)
	}
}

// A replay whose controller runs but cannot reach the actor (the router forward is gone) reports
// the counters it read and leaves the zero-call claim unmade.
func TestReplayThatCannotReachTheActor(t *testing.T) {
	s, ts := testServer(t, map[string]*fakeSessions{"stateless": newFake()})
	sid := postActivity(t, ts, "/api/create", map[string]string{"lane": "stateless"}).Session
	resp := post(t, ts, "/api/exec", map[string]string{"session": sid, "input": "hi"})
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	stats := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"calls":3}`))
	}))
	t.Cleanup(stats.Close)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("replay called the model")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(model.Close)
	router, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Router = router.Addr().String()
	_ = router.Close()
	s.cfg.ModelURL = model.URL
	s.cfg.ModelStats = stats.URL

	a := postActivity(t, ts, "/api/replay", map[string]string{"session": sid})
	if a.OK || a.FailedAt != "bridge" || !strings.Contains(a.Message, "replay:") {
		t.Fatalf("replay: ok=%v failedAt=%q message=%q", a.OK, a.FailedAt, a.Message)
	}
	if r := a.Replay; r == nil || r.ControllerCalls != 0 || r.FakeBefore != 3 || r.FakeAfter != -1 || r.ModelCalled {
		t.Fatalf("replay result %+v", a.Replay)
	}
	tr := s.trace(context.Background(), &a)
	if m := tr.Nodes["model"]; m.State != "unknown" ||
		!strings.Contains(m.Detail, "fake model counter 3 before, not read after") {
		t.Errorf("model: %+v", m)
	}
	// The controller ran here, so the bridge names it, and where the call stopped.
	if b := tr.Nodes["bridge"]; b.State != "failed" || !strings.Contains(b.Detail, "then controller.Replay through atenet-router") ||
		!strings.Contains(b.Detail, "stopped here: replay:") {
		t.Errorf("bridge: %+v", b)
	}
}

// echoActor gives a session a journal the echo harness wrote on "triage" and serves that harness
// over gRPC on loopback, standing in for the session's actor behind atenet-router. It returns the
// address to use as the router.
func echoActor(t *testing.T, f *fakeSessions, uid string) string {
	t.Helper()
	live := eventlog.New()
	c, err := controller.New(eventlog.AsStore(live), echoagent.Model, controller.WithSessionUID(uid), controller.WithHarness("triage"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Exec(context.Background(), echoagent.Harness{}, []api.Message{*api.TextMessage("user", "ping")}, 0); err != nil {
		t.Fatal(err)
	}
	var protos []*v1.LogRecord
	for _, r := range live.Snapshot() {
		protos = append(protos, eventlog.RecordToProto(r))
	}
	f.mu.Lock()
	f.log[uid] = protos
	f.mu.Unlock()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer()
	v1.RegisterHarnessServer(g, harnesswire.NewServer(echoagent.Harness{}))
	go func() { _ = g.Serve(lis) }()
	t.Cleanup(g.Stop)
	return lis.Addr().String()
}

// counterServer is the fake model's stats endpoint, answering each read with the next value.
func counterServer(t *testing.T, reads ...int64) string {
	t.Helper()
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if n >= len(reads) {
			t.Errorf("fake model stats read %d times, want %d", n+1, len(reads))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, `{"calls":%d}`, reads[n])
		n++
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// The fake model's counter is shared by every daemon: a turn another tab or `ui.sh check` sends
// during a replay moves it. A replay whose controller counted no call still fails when it moves,
// but the move is not attributed to the replay: the model is drawn unknown, not called.
func TestReplayCounterMoveIsNotAttributedToTheReplay(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("replay called the model")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(model.Close)
	for _, c := range []struct {
		name  string
		reads []int64
	}{
		{"counter held", []int64{3, 3}},
		{"another session's turn moved the counter", []int64{3, 4}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFake()
			s, ts := testServer(t, map[string]*fakeSessions{"stateless": f})
			sid := postActivity(t, ts, "/api/create", map[string]string{"lane": "stateless"}).Session
			if _, err := f.setCompute(sid, v1.ComputeState_COMPUTE_LIVE); err != nil {
				t.Fatal(err)
			}
			s.cfg.Router = echoActor(t, f, sid)
			s.cfg.ModelURL = model.URL
			s.cfg.ModelStats = counterServer(t, c.reads...)

			a := postActivity(t, ts, "/api/replay", map[string]string{"session": sid})
			r := a.Replay
			if r == nil || r.ControllerCalls != 0 || r.FakeBefore != c.reads[0] || r.FakeAfter != c.reads[1] || r.ModelCalled {
				t.Fatalf("replay result %+v (ok=%v message=%q)", r, a.OK, a.Message)
			}
			tr := s.trace(context.Background(), &a)
			m := tr.Nodes["model"]
			if c.reads[0] == c.reads[1] {
				if !a.OK || len(r.Outputs) != 1 || r.Outputs[0] != "echo:ping" || m.State != "skipped" {
					t.Fatalf("held replay: ok=%v message=%q outputs=%q model=%+v", a.OK, a.Message, r.Outputs, m)
				}
				return
			}
			if a.OK || a.FailedAt != "bridge" || strings.Contains(a.Message, "replay called the model") ||
				!strings.Contains(a.Message, "fake model's counter moved 3 -> 4") || !strings.Contains(a.Message, "not established") {
				t.Fatalf("replay: ok=%v failedAt=%q message=%q", a.OK, a.FailedAt, a.Message)
			}
			if m.State != "unknown" || strings.Contains(m.Detail, "called the model") ||
				!strings.Contains(m.Detail, "cannot be attributed") || !strings.Contains(m.Detail, "fake model counter 3 -> 4") {
				t.Errorf("model: %+v", m)
			}
		})
	}
}

// The bridge's part in a replay names controller.Replay only when the controller ran.
func TestReplayBridgeDetail(t *testing.T) {
	const ran = "Sessions.Replay (journal), then controller.Replay through atenet-router"
	for _, c := range []struct {
		name string
		a    activity
		want string
	}{
		{"held", activity{OK: true, Replay: &replayResult{ControllerCalls: 0}}, ran},
		{"controller failed", activity{FailedAt: "bridge", Replay: &replayResult{ControllerCalls: 0}}, ran},
		{"declined", activity{FailedAt: "bridge", Declined: true}, "Sessions.Replay not called"},
		{"Replay RPC failed", activity{Code: "Unavailable"}, "Sessions.Replay (journal) failed; controller.Replay did not run"},
		{"nothing to re-run", activity{FailedAt: "bridge"},
			"Sessions.Replay (journal); nothing to re-run or the chain did not verify, so controller.Replay did not run"},
		{"stopped before the controller", activity{FailedAt: "bridge", Replay: &replayResult{ControllerCalls: -1}},
			"Sessions.Replay (journal), verified; controller.Replay did not run"},
	} {
		c.a.Op = "replay"
		if got := bridgeDetail(&c.a); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// The model node in a replay's trace: skipped only when the replay held.
func TestReplayModel(t *testing.T) {
	for _, c := range []struct {
		name   string
		a      activity
		state  string
		detail string
	}{
		{"held", activity{OK: true, Replay: &replayResult{FakeBefore: 3, FakeAfter: 3}}, "skipped",
			"controller counted 0 model calls; fake model counter 3 -> 3"},
		{"stopped before the journal", activity{}, "skipped", "before the controller ran"},
		{"stopped after the fake counter", activity{Replay: &replayResult{ControllerCalls: -1, FakeBefore: 3, FakeAfter: -1}},
			"unknown", "not established: the controller's replay did not run; fake model counter 3 before, not read after"},
		{"controller failed", activity{Replay: &replayResult{ControllerCalls: 0, FakeBefore: -1, FakeAfter: -1}},
			"unknown", "not established: controller counted 0 model calls"},
		{"called the model", activity{Replay: &replayResult{ControllerCalls: 1, FakeBefore: 3, FakeAfter: 4, ModelCalled: true}},
			"observed", "the replay called the model: controller counted 1 model calls; fake model counter 3 -> 4"},
		{"shared counter moved", activity{Replay: &replayResult{ControllerCalls: 0, FakeBefore: 3, FakeAfter: 4}},
			"unknown", "cannot be attributed to the replay and zero model calls is not established: controller counted 0 model calls; fake model counter 3 -> 4"},
	} {
		got := replayModel(&c.a)
		if got.State != c.state || !strings.Contains(got.Detail, c.detail) {
			t.Errorf("%s: %+v, want %s with %q", c.name, got, c.state, c.detail)
		}
	}
}

func TestLoadJournalRefusesNothingToReplay(t *testing.T) {
	if _, _, _, err := loadJournal("s", nil); err == nil {
		t.Error("empty journal: want an error")
	}
	recs := turn(nil, "hi")
	if _, _, turns, err := loadJournal("s", recs); err != nil || turns != 1 {
		t.Errorf("one turn: turns=%d err=%v", turns, err)
	}
	if _, _, _, err := loadJournal("s", recs[:4]); err == nil || !strings.Contains(err.Error(), "no completed turn") {
		t.Errorf("no END: err = %v", err)
	}
	tampered := turn(nil, "hi")
	tampered[1].GetEvent().GetMessage().GetParts()[0].GetText().Text = "edited"
	if _, _, _, err := loadJournal("s", tampered); err == nil || !strings.Contains(err.Error(), "verify") {
		t.Errorf("tampered: err = %v", err)
	}
	if j := journalView("s", tampered); j.Verified || j.Error == "" {
		t.Errorf("tampered journal view: %+v", j)
	}
}

func TestBadRequests(t *testing.T) {
	_, ts := testServer(t, map[string]*fakeSessions{"stateless": newFake()})
	for _, c := range []struct {
		path string
		body any
		code int
	}{
		{"/api/create", map[string]string{"lane": "volumes"}, http.StatusBadRequest},
		{"/api/create", map[string]string{"lane": "stateless", "extra": "x"}, http.StatusBadRequest},
		{"/api/suspend", map[string]string{"session": "--kubeconfig=/etc/x"}, http.StatusBadRequest},
		{"/api/suspend", map[string]string{"session": "nope"}, http.StatusNotFound},
		{"/api/exec", map[string]string{"session": "sess-1", "input": "  "}, http.StatusBadRequest},
		{"/api/exec", map[string]string{"session": "sess-1", "input": strings.Repeat("x", maxInput+1)}, http.StatusBadRequest},
		{"/api/exec", map[string]string{"session": "sess-1", "input": strings.Repeat("é", maxInput+1)}, http.StatusBadRequest},
		{"/api/fork", map[string]any{"session": "sess-1", "atSeq": -1}, http.StatusBadRequest},
	} {
		if resp := post(t, ts, c.path, c.body); resp.StatusCode != c.code {
			t.Errorf("%s %v: %s, want %d", c.path, c.body, resp.Status, c.code)
		}
	}
}

// The limit counts characters, as the page's maxlength does, not bytes.
func TestExecInputLimitCountsCharacters(t *testing.T) {
	_, ts := testServer(t, map[string]*fakeSessions{"stateless": newFake()})
	created := postActivity(t, ts, "/api/create", map[string]string{"lane": "stateless", "name": "demo"})
	resp := post(t, ts, "/api/exec", map[string]string{"session": created.Session, "input": strings.Repeat("é", maxInput)})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("%d two-byte characters: %s, want 200", maxInput, resp.Status)
	}
}

func TestGuard(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := guard(ok)
	for _, c := range []struct {
		name, method, host, origin, ctype string
		code                              int
	}{
		{"page", "GET", "127.0.0.1:8090", "", "", http.StatusNoContent},
		{"localhost", "GET", "localhost:8090", "", "", http.StatusNoContent},
		{"ipv6 loopback", "GET", "[::1]:8090", "", "", http.StatusNoContent},
		{"rebinding host", "GET", "evil.example:8090", "", "", http.StatusForbidden},
		{"lan address", "GET", "192.168.1.5:8090", "", "", http.StatusForbidden},
		{"same-origin post", "POST", "127.0.0.1:8090", "http://127.0.0.1:8090", "application/json", http.StatusNoContent},
		{"cross-origin post", "POST", "127.0.0.1:8090", "http://evil.example", "application/json", http.StatusForbidden},
		{"form post", "POST", "127.0.0.1:8090", "", "application/x-www-form-urlencoded", http.StatusUnsupportedMediaType},
		{"text post", "POST", "127.0.0.1:8090", "", "text/plain", http.StatusUnsupportedMediaType},
	} {
		r := httptest.NewRequest(c.method, "/api/x", strings.NewReader("{}"))
		r.Host = c.host
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if c.ctype != "" {
			r.Header.Set("Content-Type", c.ctype)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.code {
			t.Errorf("%s: %d, want %d", c.name, w.Code, c.code)
		}
	}
}

func TestParseFlags(t *testing.T) {
	for _, c := range []struct {
		args []string
		ok   bool
	}{
		{[]string{"-stateless", "127.0.0.1:18080"}, true},
		{[]string{"-listen", "localhost:9000", "-volumes", "127.0.0.1:1"}, true},
		{[]string{"-listen", "[::1]:9000", "-memory", "127.0.0.1:1"}, true},
		{[]string{"-listen", "0.0.0.0:8090", "-stateless", "127.0.0.1:1"}, false},
		{[]string{"-listen", ":8090", "-stateless", "127.0.0.1:1"}, false},
		{[]string{"-listen", "10.0.0.4:8090", "-stateless", "127.0.0.1:1"}, false},
		{[]string{}, false},
		{[]string{"-stateless", "127.0.0.1:1", "-harness", "-x"}, false},
		{[]string{"-stateless", "127.0.0.1:1", "-atespace", "a b"}, false},
		{[]string{"-stateless", "127.0.0.1:1", "extra"}, false},
	} {
		_, err := parseFlags(c.args)
		if (err == nil) != c.ok {
			t.Errorf("parseFlags(%q): err = %v, want ok=%v", c.args, err, c.ok)
		}
	}
	c, err := parseFlags([]string{"-stateless", "127.0.0.1:1"})
	if err != nil || c.MemoryHarness != "triage-memory" {
		t.Errorf("default memory harness: %q, %v", c.MemoryHarness, err)
	}
}
