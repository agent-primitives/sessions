package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/agent-primitives/sessions/eventlog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
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
		Project: r.GetSession().GetMetadata().GetProject()}, Harness: r.GetSession().GetHarness()}
	f.sessions[uid] = s
	return s, nil
}

func (f *fakeSessions) GetSession(_ context.Context, r *v1.GetSessionRequest) (*v1.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.sessions[r.GetUid()]; ok {
		return s, nil
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
	return f.GetSession(context.Background(), &v1.GetSessionRequest{Uid: r.GetSession()})
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
	if a.OK || !strings.Contains(a.Message, "only the stateless tier") {
		t.Fatalf("memory-tier replay: %+v", a)
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
