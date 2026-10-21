package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

//go:embed web/index.html web/styles.css web/flow.js web/app.js
var webFS embed.FS

// safeName is what a session UID, harness, atespace or namespace may be before it reaches a
// kubectl argument or a log pattern: no leading '-', no shell or regexp metacharacters.
var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// maxInput bounds one turn's text in characters (runes); the page is for a demo, not a document
// upload. The page's maxlength counts UTF-16 units, never fewer than runes, so it stays inside.
const maxInput = 4000

// lane is one agentsessionsd Deployment, the harness it serves, and a client to its Sessions API.
type lane struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	Harness string `json:"harness"`
	Daemon  string `json:"daemon"`
	Addr    string `json:"addr"`
	// Tier is what the harness declares to the daemon: STATELESS_REPLAY or REQUIRES_MEMORY_SNAPSHOT.
	Tier     string `json:"tier"`
	Fidelity string `json:"fidelity"`
	c        *client.Client
}

type server struct {
	cfg   config
	lanes []*lane
	kube  *kube
	now   func() time.Time

	mu     sync.Mutex
	owner  map[string]*lane // session UID -> the lane whose daemon holds it
	acts   []*activity
	nextID int
}

func newServer(cfg config) (*server, error) {
	s := &server{cfg: cfg, now: time.Now, owner: map[string]*lane{}}
	for _, l := range []*lane{
		{Name: "stateless", Label: "Stateless tier", Harness: cfg.Harness, Daemon: "agentsessionsd", Addr: cfg.Stateless,
			Tier: "STATELESS_REPLAY", Fidelity: "MEMORY"},
		{Name: "memory", Label: "Memory tier", Harness: cfg.MemoryHarness, Daemon: "agentsessionsd-memory", Addr: cfg.Memory,
			Tier: "REQUIRES_MEMORY_SNAPSHOT", Fidelity: "MEMORY"},
		{Name: "volumes", Label: "Memory tier, VOLUMES template", Harness: cfg.MemoryHarness, Daemon: "agentsessionsd-volumes",
			Addr: cfg.Volumes, Tier: "REQUIRES_MEMORY_SNAPSHOT", Fidelity: "VOLUMES"},
	} {
		if l.Addr == "" {
			continue
		}
		c, err := client.Dial(l.Addr, client.WithProject(cfg.Project))
		if err != nil {
			s.close()
			return nil, err
		}
		l.c = c
		s.lanes = append(s.lanes, l)
	}
	if cfg.Kubectl != "" {
		s.kube = &kube{kubectl: cfg.Kubectl, ate: cfg.KubectlAte, kubeconfig: cfg.Kubeconfig, context: cfg.Context,
			namespace: cfg.Namespace, atespace: cfg.Atespace}
	}
	return s, nil
}

func (s *server) close() {
	for _, l := range s.lanes {
		if l.c != nil {
			_ = l.c.Close()
		}
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/sessions", s.handleSessions)
	mux.HandleFunc("GET /api/journal", s.handleJournal)
	mux.HandleFunc("GET /api/actor", s.handleActor)
	mux.HandleFunc("GET /api/activity", s.handleActivity)
	mux.HandleFunc("GET /api/trace", s.handleTrace)
	mux.HandleFunc("POST /api/create", s.handleCreate)
	mux.HandleFunc("POST /api/exec", s.handleExec)
	mux.HandleFunc("POST /api/suspend", s.handleLifecycle("suspend"))
	mux.HandleFunc("POST /api/resume", s.handleLifecycle("resume"))
	mux.HandleFunc("POST /api/fork", s.handleFork)
	mux.HandleFunc("POST /api/replay", s.handleReplay)
	return guard(mux)
}

// guard keeps other web pages out of the loopback bridge. It is not authentication: anything
// running as the user on this machine can still call it. It refuses a Host that is not a loopback
// name (DNS rebinding), a cross-origin Origin, and a POST that is not JSON (so a form on another
// site cannot send one without a CORS preflight, which the bridge never answers).
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; img-src 'self' data:; frame-ancestors 'none'")
		h.Set("Referrer-Policy", "no-referrer")
		if !loopbackHost(r.Host) {
			http.Error(w, "the lifecycle page answers only on a loopback host name", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost {
			ct := r.Header.Get("Content-Type")
			if mt, _, _ := strings.Cut(ct, ";"); strings.TrimSpace(mt) != "application/json" {
				http.Error(w, "want Content-Type: application/json", http.StatusUnsupportedMediaType)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		}
		next.ServeHTTP(w, r)
	})
}

func loopbackHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func (s *server) handleConfig(w http.ResponseWriter, r *http.Request) {
	type laneStatus struct {
		*lane
		Up    bool   `json:"up"`
		Error string `json:"error,omitempty"`
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	out := make([]laneStatus, len(s.lanes))
	var wg sync.WaitGroup
	for i, l := range s.lanes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = laneStatus{lane: l, Up: true}
			if _, err := l.c.ListSessionsPage(ctx, s.cfg.Project, 1, ""); err != nil {
				out[i].Up, out[i].Error = false, grpcMessage(err)
			}
		}()
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{
		"lanes":    out,
		"atespace": s.cfg.Atespace,
		"cluster":  s.kube != nil,
		"replay":   s.cfg.Router != "",
		"project":  s.cfg.Project,
	})
}

// sessionView is the part of a Session the page shows.
type sessionView struct {
	UID          string `json:"uid"`
	Name         string `json:"name,omitempty"`
	Lane         string `json:"lane"`
	Harness      string `json:"harness"`
	LastSeq      int64  `json:"lastSeq"`
	ComputeState string `json:"computeState"`
	ExecState    string `json:"execState"`
	ParentUID    string `json:"parentUid,omitempty"`
	ForkSeq      int64  `json:"forkSeq,omitempty"`
	Created      string `json:"created,omitempty"`
}

func viewSession(l *lane, s *v1.Session) *sessionView {
	if s == nil {
		return nil
	}
	v := &sessionView{
		UID: s.GetMetadata().GetUid(), Name: s.GetMetadata().GetName(), Lane: l.Name, Harness: s.GetHarness(),
		LastSeq: s.GetLastSeq(), ComputeState: s.GetComputeState().String(), ExecState: s.GetExecState().String(),
		ParentUID: s.GetParentUid(), ForkSeq: s.GetForkSeq(),
	}
	if t := s.GetMetadata().GetCreateTime(); t != nil {
		v.Created = t.AsTime().UTC().Format(time.RFC3339)
	}
	return v
}

func (s *server) handleSessions(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	views := []*sessionView{}
	errs := map[string]string{}
	for _, l := range s.lanes {
		list, err := l.c.ListSessions(ctx, s.cfg.Project)
		if err != nil {
			errs[l.Name] = grpcMessage(err)
			continue
		}
		for _, sess := range list {
			v := viewSession(l, sess)
			s.setOwner(v.UID, l)
			views = append(views, v)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": views, "errors": errs})
}

func (s *server) setOwner(uid string, l *lane) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owner[uid] = l
}

// laneOf finds the daemon that holds uid: the one it was created or listed on, else the first lane
// whose GetSession finds it.
func (s *server) laneOf(ctx context.Context, uid string) (*lane, error) {
	if !safeName.MatchString(uid) {
		return nil, errBadRequest("session: want a session UID")
	}
	s.mu.Lock()
	l := s.owner[uid]
	s.mu.Unlock()
	if l != nil {
		return l, nil
	}
	for _, l := range s.lanes {
		if _, err := l.c.GetSession(ctx, uid); err == nil {
			s.setOwner(uid, l)
			return l, nil
		}
	}
	return nil, errNotFound("session " + uid + " is on no lane this page serves")
}

func (s *server) laneNamed(name string) (*lane, error) {
	for _, l := range s.lanes {
		if l.Name == name {
			return l, nil
		}
	}
	return nil, errBadRequest(fmt.Sprintf("lane %q is not served", name))
}

// activity is one lifecycle call made through the page, kept in memory for the timeline.
type activity struct {
	ID         int           `json:"id"`
	Op         string        `json:"op"`
	Lane       string        `json:"lane"`
	Session    string        `json:"session"`
	Child      string        `json:"child,omitempty"`
	AtSeq      int64         `json:"atSeq,omitempty"`
	Input      string        `json:"input,omitempty"`
	Started    time.Time     `json:"started"`
	DurationMS int64         `json:"durationMs"`
	OK         bool          `json:"ok"`
	Code       string        `json:"code,omitempty"`
	Message    string        `json:"message,omitempty"`
	Refusal    *refusal      `json:"refusal,omitempty"`
	Output     string        `json:"output,omitempty"`
	Records    []recordView  `json:"records,omitempty"`
	Replay     *replayResult `json:"replay,omitempty"`
	After      *sessionView  `json:"after,omitempty"`
}

func (s *server) begin(op string, l *lane, uid string) *activity {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	return &activity{ID: s.nextID, Op: op, Lane: l.Name, Session: uid, Started: s.now()}
}

// finish records the call's outcome and keeps the activity for the timeline.
func (s *server) finish(a *activity, err error) *activity {
	a.DurationMS = s.now().Sub(a.Started).Milliseconds()
	a.OK = err == nil
	if err != nil {
		a.Code = status.Code(err).String()
		a.Message = grpcMessage(err)
		if status.Code(err) == codes.FailedPrecondition {
			a.Refusal = parseRefusal(a.Message)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acts = append(s.acts, a)
	return a
}

func (s *server) activityByID(id int) *activity {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.acts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

func (s *server) handleActivity(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	acts := append([]*activity{}, s.acts...)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"activity": acts})
}

func (s *server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Lane string `json:"lane"`
		Name string `json:"name"`
	}
	if !decode(w, r, &body) {
		return
	}
	l, err := s.laneNamed(body.Lane)
	if err != nil {
		writeError(w, err)
		return
	}
	name := strings.TrimSpace(body.Name)
	if len(name) > 63 {
		writeError(w, errBadRequest("name: at most 63 characters"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	a := s.begin("create", l, "")
	sess, err := l.c.CreateSession(ctx, &v1.Session{
		Metadata: &v1.ResourceMetadata{Project: s.cfg.Project, Name: name},
		Harness:  l.Harness,
	})
	if err == nil {
		a.After = viewSession(l, sess)
		a.Session = a.After.UID
		s.setOwner(a.Session, l)
	}
	writeJSON(w, http.StatusOK, s.finish(a, err))
}

func (s *server) handleLifecycle(op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Session string `json:"session"`
		}
		if !decode(w, r, &body) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		l, err := s.laneOf(ctx, body.Session)
		if err != nil {
			writeError(w, err)
			return
		}
		a := s.begin(op, l, body.Session)
		var sess *v1.Session
		if op == "suspend" {
			sess, err = l.c.Suspend(ctx, body.Session)
		} else {
			sess, err = l.c.Resume(ctx, body.Session, false)
		}
		if err == nil {
			a.After = viewSession(l, sess)
		} else {
			a.After = s.current(ctx, l, body.Session)
		}
		writeJSON(w, http.StatusOK, s.finish(a, err))
	}
}

// current reads the session after a failed call, so the timeline shows what the refusal left.
func (s *server) current(ctx context.Context, l *lane, uid string) *sessionView {
	sess, err := l.c.GetSession(ctx, uid)
	if err != nil {
		return nil
	}
	return viewSession(l, sess)
}

func (s *server) handleFork(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Session string `json:"session"`
		AtSeq   int64  `json:"atSeq"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.AtSeq < 0 {
		writeError(w, errBadRequest("atSeq: want 0 (the head) or a committed seq"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	l, err := s.laneOf(ctx, body.Session)
	if err != nil {
		writeError(w, err)
		return
	}
	a := s.begin("fork", l, body.Session)
	a.AtSeq = body.AtSeq
	children, err := l.c.Fork(ctx, body.Session, client.ForkOptions{AtSeq: body.AtSeq, Count: 1})
	if err == nil && len(children) == 1 {
		a.After = viewSession(l, children[0])
		a.Child = a.After.UID
		s.setOwner(a.Child, l)
	} else if err == nil {
		err = status.Errorf(codes.Internal, "fork returned %d children, want 1", len(children))
	}
	writeJSON(w, http.StatusOK, s.finish(a, err))
}

// handleExec runs one turn and streams it to the page as NDJSON: the session frame, each committed
// record and each delta as the Exec stream delivers them, then the finished activity.
func (s *server) handleExec(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Session string `json:"session"`
		Input   string `json:"input"`
	}
	if !decode(w, r, &body) {
		return
	}
	input := strings.TrimSpace(body.Input)
	if input == "" || utf8.RuneCountInString(input) > maxInput {
		writeError(w, errBadRequest(fmt.Sprintf("input: want 1 to %d characters", maxInput)))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	l, err := s.laneOf(ctx, body.Session)
	if err != nil {
		writeError(w, err)
		return
	}
	a := s.begin("exec", l, body.Session)
	a.Input = input
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	flusher, _ := w.(http.Flusher)
	send := func(v any) {
		_ = enc.Encode(v)
		if flusher != nil {
			flusher.Flush()
		}
	}
	err = s.exec(ctx, l, a, input, func(kind string, v any) { send(map[string]any{"type": kind, "data": v}) })
	a.After = s.current(ctx, l, body.Session)
	send(map[string]any{"type": "activity", "data": s.finish(a, err)})
}

func (s *server) exec(ctx context.Context, l *lane, a *activity, input string, emit func(string, any)) error {
	stream, err := l.c.Sessions().Exec(ctx, &v1.ExecRequest{
		Session: a.Session,
		Inputs:  []*v1.Message{{Role: "user", Parts: []*v1.Part{{Part: &v1.Part_Text{Text: &v1.TextPart{Text: input}}}}}},
	})
	if err != nil {
		return err
	}
	var out strings.Builder
	for {
		u, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			a.Output = out.String()
			return nil
		}
		if err != nil {
			a.Output = out.String()
			return err
		}
		switch {
		case u.GetRecord() != nil:
			rv := viewRecord(u.GetRecord())
			a.Records = append(a.Records, rv)
			if rv.Kind == "OUTPUT" {
				out.Reset()
				out.WriteString(rv.Text)
			}
			emit("record", rv)
		case u.GetDelta() != nil:
			emit("delta", u.GetDelta().GetChunk())
		case u.GetSession() != nil:
			emit("session", viewSession(l, u.GetSession()))
		}
	}
}

func (s *server) handleJournal(w http.ResponseWriter, r *http.Request) {
	uid := r.URL.Query().Get("session")
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	l, err := s.laneOf(ctx, uid)
	if err != nil {
		writeError(w, err)
		return
	}
	recs, err := l.c.Replay(ctx, uid, 1, 0)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, journalView(uid, recs))
}

func (s *server) handleActor(w http.ResponseWriter, r *http.Request) {
	uid := r.URL.Query().Get("session")
	if !safeName.MatchString(uid) {
		writeError(w, errBadRequest("session: want a session UID"))
		return
	}
	if s.kube == nil || s.kube.ate == "" {
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "error": "cluster reads are off: start the page with -kubectl and -kubectl-ate"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	a, err := s.kube.actor(ctx, uid)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": true, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": true, "actor": a})
}

func (s *server) handleTrace(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.URL.Query().Get("id"))
	if err != nil {
		writeError(w, errBadRequest("id: want an activity id"))
		return
	}
	a := s.activityByID(id)
	if a == nil {
		writeError(w, errNotFound("no activity "+strconv.Itoa(id)))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, s.trace(ctx, a))
}

// httpError is a refusal of the page's own request, before any Sessions call.
type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

func errBadRequest(msg string) error { return &httpError{http.StatusBadRequest, msg} }
func errNotFound(msg string) error   { return &httpError{http.StatusNotFound, msg} }

func writeError(w http.ResponseWriter, err error) {
	var he *httpError
	code := http.StatusBadGateway
	msg := grpcMessage(err)
	if errors.As(err, &he) {
		code, msg = he.code, he.msg
	} else if st, ok := status.FromError(err); ok {
		msg = st.Code().String() + ": " + st.Message()
	}
	writeJSON(w, code, map[string]string{"error": msg})
}

// grpcMessage is a gRPC status's message without the "rpc error: code = ..." wrapper, or the error.
func grpcMessage(err error) string {
	if st, ok := status.FromError(err); ok {
		return st.Message()
	}
	return err.Error()
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, errBadRequest("request body: "+err.Error()))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
