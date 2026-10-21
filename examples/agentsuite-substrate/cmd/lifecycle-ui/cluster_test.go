package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	atepb "github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/protobuf/encoding/protojson"
)

// fakeCluster answers the bridge's kubectl calls from canned output and records every call.
type fakeCluster struct {
	calls [][]string
	out   map[string]string // joined args prefix -> output
	errs  map[string]string // joined args prefix -> the error kubectl reports, checked first
}

func (f *fakeCluster) exec(_ context.Context, bin string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{bin}, args...))
	key := strings.Join(args, " ")
	for prefix, msg := range f.errs {
		if strings.HasPrefix(key, prefix) {
			return nil, errors.New(msg)
		}
	}
	best := ""
	for prefix := range f.out {
		if strings.HasPrefix(key, prefix) && len(prefix) > len(best) {
			best = prefix
		}
	}
	if best == "" {
		return nil, errors.New("not found")
	}
	return []byte(f.out[best]), nil
}

func TestClusterReadsAreReadOnly(t *testing.T) {
	f := &fakeCluster{out: map[string]string{"get": "", "logs": ""}}
	k := &kube{kubectl: "kubectl", ate: "kubectl-ate", kubeconfig: "/tmp/kc", context: "kind-demo", exec: f.exec}
	for _, verb := range []string{"apply", "delete", "create", "edit", "patch", "exec", "port-forward", "scale", "--kubeconfig=/x", ""} {
		if _, err := k.run(context.Background(), false, verb, "x"); err == nil {
			t.Errorf("verb %q ran", verb)
		}
		if _, err := k.run(context.Background(), true, verb, "x"); err == nil {
			t.Errorf("plugin verb %q ran", verb)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("refused verbs reached the cluster: %q", f.calls)
	}
	if _, err := k.run(context.Background(), true, "get", "actor", "s1"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.calls[0], " ")
	if got != "kubectl-ate get actor s1 --kubeconfig /tmp/kc --context kind-demo" {
		t.Errorf("call = %q", got)
	}
}

// actorJSON is the plugin's output shape for an actor (ateapipb.Actor in protojson).
const actorJSON = `{
  "metadata": {"atespace": "agentsuite-demo", "name": "sess-abc", "uid": "a-1"},
  "actorTemplate": {"atespace": "agentsuite-demo", "name": "support-desk-triage-memory-1234-vol"},
  "status": {
    "state": "ACTOR_STATE_SUSPENDED",
    "workerAssignment": {"workerPod": "agentsuite-demo-microvm-0"},
    "snapshots": [
      {"generation": 1, "fidelity": "SNAPSHOT_FIDELITY_MEMORY", "durableSnapshot": {"status": "SNAPSHOT_STORAGE_STATUS_COMPLETED", "object": {"snapshotUri": "gs://x/1"}}},
      {"generation": 2, "fidelity": "SNAPSHOT_FIDELITY_VOLUMES", "durableSnapshot": {"status": "SNAPSHOT_STORAGE_STATUS_COMPLETED", "object": {"snapshotUri": "gs://x/2"}}}
    ]
  }
}`

func TestParseActor(t *testing.T) {
	a, err := parseActor([]byte(actorJSON))
	if err != nil {
		t.Fatal(err)
	}
	if a.State != "SUSPENDED" || a.Worker != "agentsuite-demo-microvm-0" || a.Template != "support-desk-triage-memory-1234-vol" || a.UID != "a-1" {
		t.Errorf("actor = %+v", a)
	}
	if len(a.Snapshots) != 2 || a.Snapshots[0].Generation != 2 || a.Snapshots[0].Fidelity != "VOLUMES" || a.Snapshots[0].Storage != "COMPLETED" {
		t.Errorf("snapshots, newest first = %+v", a.Snapshots)
	}
	b, _ := json.Marshal(a)
	if strings.Contains(string(b), "gs://") {
		t.Errorf("the page's actor view carries a snapshot URI: %s", b)
	}
}

// t0 is when the traced call started; at(d) is a kubectl --timestamps prefix d after it.
var t0 = time.Date(2026, 10, 21, 2, 18, 0, 0, time.UTC)

func at(d time.Duration) string { return t0.Add(d).Format(time.RFC3339Nano) + " " }

func traceServer(f *fakeCluster) *server {
	return &server{
		cfg:   config{Namespace: "agentsuite-demo", Atespace: "agentsuite-demo"},
		lanes: []*lane{{Name: "volumes", Daemon: "agentsessionsd-volumes"}},
		kube:  &kube{kubectl: "kubectl", ate: "kubectl-ate", namespace: "agentsuite-demo", atespace: "agentsuite-demo", exec: f.exec},
	}
}

func TestTraceRefusedSuspend(t *testing.T) {
	f := &fakeCluster{out: map[string]string{
		"logs -n agentsuite-demo deploy/agentsessionsd-volumes": at(-time.Second) + `msg="operation finished" component=session operation=exec session_uid=sess-abc phase=finish outcome=success
` + at(10*time.Millisecond) + `level=INFO msg="operation started" request_id=r1 component=session operation=suspend session_uid=sess-abc phase=start
` + at(90*time.Millisecond) + `level=WARN msg="operation finished" request_id=r1 component=session operation=suspend session_uid=sess-abc phase=finish outcome=error`,
		"get pods -n ate-system":      "pod/ate-api-server-0\npod/atenet-router-1\npod/atelet-x\n",
		"get pods -n agentsuite-demo": "pod/agentsessionsd-volumes-1\npod/fakemodel-2\npod/agentsuite-demo-microvm-0\n",
		// The previous call's CreateActor, inside --since-time's whole second but before this call.
		"logs -n ate-system ate-api-server-0": at(-600*time.Millisecond) + `msg="Handle RPC" method=/ateapi.Control/CreateActor actor=sess-abc
` + at(20*time.Millisecond) + `msg="Handle RPC" method=/ateapi.Control/GetActor actor=sess-abc
` + at(30*time.Millisecond) + `msg="Handle RPC" method=/ateapi.Control/SuspendActor actor=sess-abc`,
		"logs -n agentsuite-demo agentsuite-demo-microvm-0": at(-500*time.Millisecond) + `{"msg":"Handle RPC","method":"/ateom.Ateom/RestoreWorkload","actor_name":"sess-abc"}
` + at(40*time.Millisecond) + `{"time":"t3","msg":"Handle RPC","method":"/ateom.Ateom/CheckpointWorkload","actor_name":"sess-other"}
` + at(50*time.Millisecond) + `{"time":"t4","msg":"Handle RPC","method":"/ateom.Ateom/CheckpointWorkload","actor_name":"sess-abc"}`,
		"get actor sess-abc": actorJSON,
	}}
	s := traceServer(f)
	a := &activity{ID: 7, Op: "suspend", Lane: "volumes", Session: "sess-abc", Started: t0, DurationMS: 100, Code: "FailedPrecondition", Message: refusalMsg}
	tr := s.trace(context.Background(), a)
	want := map[string]string{"browser": "observed", "bridge": "observed", "daemon": "refused", "ateapi": "observed",
		"worker": "observed", "actor": "observed", "router": "skipped", "model": "skipped"}
	for n, st := range want {
		if tr.Nodes[n].State != st {
			t.Errorf("%s: %s (%s), want %s", n, tr.Nodes[n].State, tr.Nodes[n].Detail, st)
		}
	}
	// The call's own method, not the previous call's CreateActor or this call's GetActor; the worker
	// line is this session's, not another's.
	if !strings.Contains(tr.Nodes["ateapi"].Detail, "SuspendActor") {
		t.Errorf("ate-api evidence %q", tr.Nodes["ateapi"].Detail)
	}
	if !strings.Contains(tr.Nodes["worker"].Detail, "t4") {
		t.Errorf("worker evidence %q, want the sess-abc CheckpointWorkload line", tr.Nodes["worker"].Detail)
	}
	if !strings.Contains(tr.Nodes["actor"].Detail, "SUSPENDED") || !strings.Contains(tr.Nodes["actor"].Detail, "VOLUMES") {
		t.Errorf("actor evidence %q", tr.Nodes["actor"].Detail)
	}
	if !strings.Contains(tr.Nodes["daemon"].Detail, "FailedPrecondition") {
		t.Errorf("daemon evidence %q", tr.Nodes["daemon"].Detail)
	}
	for _, c := range f.calls {
		if c[1] != "get" && c[1] != "logs" {
			t.Errorf("trace ran %q", c)
		}
	}
}

// A line from before the call, from after it, or without a timestamp is not evidence for it.
func TestTraceMissingHopIsNotAssumed(t *testing.T) {
	f := &fakeCluster{out: map[string]string{
		"logs -n agentsuite-demo deploy/agentsessionsd-volumes": at(10*time.Millisecond) + `msg="operation finished" component=session operation=resume session_uid=sess-other phase=finish
` + at(-time.Second) + `msg="operation finished" component=session operation=resume session_uid=sess-abc phase=finish`,
		"get pods -n ate-system":      "pod/ate-api-server-0\npod/atenet-router-1\n",
		"get pods -n agentsuite-demo": "pod/agentsuite-demo-microvm-0\n",
		"logs -n ate-system":          at(10*time.Second) + `method=/ateapi.Control/ResumeActor actor=sess-abc`,
		"logs -n agentsuite-demo":     `method=/ateom.Ateom/RestoreWorkload actor_name=sess-abc`,
	}}
	s := traceServer(f)
	tr := s.trace(context.Background(), &activity{ID: 1, Op: "resume", Lane: "volumes", Session: "sess-abc", OK: true, Started: t0, DurationMS: 50})
	for _, n := range []string{"daemon", "ateapi", "worker", "actor"} {
		if tr.Nodes[n].State != "missing" {
			t.Errorf("%s: %s (%s), want missing", n, tr.Nodes[n].State, tr.Nodes[n].Detail)
		}
	}
}

// attachCluster is a later turn on a RUNNING actor: the daemon reads it with GetActor and
// creates, resumes and restores nothing; the router routes the turn.
func attachCluster(apiLog string) *fakeCluster {
	return &fakeCluster{out: map[string]string{
		"logs -n agentsuite-demo deploy/agentsessionsd": at(400*time.Millisecond) + `msg="operation finished" component=session operation=exec session_uid=sess-abc phase=finish outcome=success`,
		"get pods -n ate-system":                        "pod/ate-api-server-0\npod/atenet-router-1\n",
		"get pods -n agentsuite-demo":                   "pod/agentsessionsd-1\npod/agentsuite-demo-microvm-0\n",
		"logs -n ate-system ate-api-server-0":           apiLog,
		// Turn 1's restore, inside --since-time's whole second but before this call.
		"logs -n agentsuite-demo agentsuite-demo-microvm-0": at(-700*time.Millisecond) + `{"msg":"Handle RPC","method":"/ateom.Ateom/RestoreWorkload","actor_name":"sess-abc"}`,
		"logs -n ate-system atenet-router-1":                at(30*time.Millisecond) + `{"msg":"Route ok","actor":"sess-abc","targetAddr":"10.244.0.21:443"}`,
		"get actor sess-abc":                                strings.Replace(actorJSON, "ACTOR_STATE_SUSPENDED", "ACTOR_STATE_RUNNING", 1),
	}}
}

func TestTraceExecAttachIsNotARestore(t *testing.T) {
	f := attachCluster(at(-650*time.Millisecond) + `{"msg":"Handle RPC","method":"/ateapi.Control/ResumeActor","req":"sess-abc"}
` + at(15*time.Millisecond) + `{"msg":"Handle RPC","method":"/ateapi.Control/GetActor","req":"sess-abc"}
` + at(25*time.Millisecond) + `{"msg":"Handle RPC","method":"/ateapi.Control/ResumeActor","req":"sess-abc","resp":{"actor":{"name":"sess-abc"}}}`)
	s := &server{
		cfg:   config{Namespace: "agentsuite-demo", Atespace: "agentsuite-demo"},
		lanes: []*lane{{Name: "stateless", Daemon: "agentsessionsd"}},
		kube:  &kube{kubectl: "kubectl", ate: "kubectl-ate", namespace: "agentsuite-demo", atespace: "agentsuite-demo", exec: f.exec},
	}
	a := &activity{ID: 3, Op: "exec", Lane: "stateless", Session: "sess-abc", OK: true, Started: t0, DurationMS: 400,
		Records: []recordView{{Kind: "INPUT"}, {Kind: "MODEL_CALL"}}}
	tr := s.trace(context.Background(), a)
	for n, st := range map[string]string{"daemon": "observed", "ateapi": "observed", "worker": "skipped", "router": "observed", "actor": "observed", "model": "observed"} {
		if tr.Nodes[n].State != st {
			t.Errorf("%s: %s (%s), want %s", n, tr.Nodes[n].State, tr.Nodes[n].Detail, st)
		}
	}
	// Not the previous call's ResumeActor, nor the router's ResumeActor that resumed nothing.
	if got := tr.Nodes["ateapi"].Method; got != "GetActor" {
		t.Errorf("ate-api method %q, want GetActor", got)
	}
	if d := tr.Nodes["ateapi"].Detail; !strings.Contains(d, "attached as it was") || strings.Contains(d, "already RUNNING") {
		t.Errorf("ate-api detail %q", d)
	}
	if tr.Nodes["worker"].Method != "" || !strings.Contains(tr.Nodes["worker"].Detail, "only GetActor") {
		t.Errorf("worker: %+v", tr.Nodes["worker"])
	}
}

// A pod whose logs could not be read is not evidence that nothing happened there: a later turn
// whose worker logs are unreadable is not drawn as "nothing restored", and an ate-api that showed
// only GetActor while another replica went unread does not say the actor was attached as it was.
// A line found in a pod that was read still counts.
func TestTraceUnreadLogsAreUnknown(t *testing.T) {
	const denied = `Error from server (Forbidden): pods "x" is forbidden: User "u" cannot get resource "pods/log"`
	getOnly := at(15*time.Millisecond) + `{"msg":"Handle RPC","method":"/ateapi.Control/GetActor","req":"sess-abc"}`
	exec := func(f *fakeCluster) traceView {
		s := traceServer(f)
		s.lanes = []*lane{{Name: "stateless", Daemon: "agentsessionsd"}}
		return s.trace(context.Background(), &activity{ID: 3, Op: "exec", Lane: "stateless", Session: "sess-abc", OK: true, Started: t0, DurationMS: 400})
	}

	t.Run("worker logs unreadable", func(t *testing.T) {
		f := attachCluster(getOnly)
		f.errs = map[string]string{"logs -n agentsuite-demo agentsuite-demo-microvm-0": denied}
		tr := exec(f)
		if w := tr.Nodes["worker"]; w.State != "unknown" || !strings.Contains(w.Detail, "agentsuite-demo-microvm-0") ||
			!strings.Contains(w.Detail, "Forbidden") || strings.Contains(w.Detail, "no workload restored") {
			t.Errorf("worker: %+v", w)
		}
		if a := tr.Nodes["ateapi"]; a.State != "observed" || a.Method != "GetActor" {
			t.Errorf("ate-api: %+v", a)
		}
	})

	t.Run("one ate-api replica unreadable", func(t *testing.T) {
		f := attachCluster(getOnly)
		f.out["get pods -n ate-system"] = "pod/ate-api-server-0\npod/ate-api-server-1\npod/atenet-router-1\n"
		f.errs = map[string]string{"logs -n ate-system ate-api-server-1": "context deadline exceeded"}
		tr := exec(f)
		a := tr.Nodes["ateapi"]
		if a.State != "unknown" || !strings.Contains(a.Detail, "ate-api-server-1: context deadline exceeded") ||
			strings.Contains(a.Detail, "attached as it was") {
			t.Errorf("ate-api: %+v", a)
		}
		// Only the turn-1 restore before the call: not found, and not turned into "skipped".
		if w := tr.Nodes["worker"]; w.State != "missing" {
			t.Errorf("worker: %+v", w)
		}
	})

	t.Run("a line in a pod that was read", func(t *testing.T) {
		f := attachCluster(at(20*time.Millisecond) + `{"msg":"Handle RPC","method":"/ateapi.Control/CreateActor","req":"sess-abc"}`)
		f.out["get pods -n ate-system"] = "pod/ate-api-server-0\npod/ate-api-server-1\npod/atenet-router-1\n"
		f.errs = map[string]string{"logs -n ate-system ate-api-server-1": "context deadline exceeded"}
		tr := exec(f)
		if a := tr.Nodes["ateapi"]; a.State != "observed" || a.Method != "CreateActor" {
			t.Errorf("ate-api: %+v", a)
		}
	})

	// A worker pod that has not started is named as such, apart from a log that could not be
	// read, and the hop stays unknown.
	t.Run("worker pod not started", func(t *testing.T) {
		f := attachCluster(getOnly)
		f.out["get pods -n agentsuite-demo"] = "pod/agentsessionsd-1\npod/agentsuite-demo-microvm-0\npod/agentsuite-demo-microvm-1\npod/agentsuite-demo-microvm-2\n"
		f.errs = map[string]string{
			"logs -n agentsuite-demo agentsuite-demo-microvm-1": `Error from server (BadRequest): container "worker" in pod "agentsuite-demo-microvm-1" is waiting to start: ContainerCreating`,
			"logs -n agentsuite-demo agentsuite-demo-microvm-2": denied,
		}
		w := exec(f).Nodes["worker"]
		if w.State != "unknown" || strings.Contains(w.Detail, "no workload restored") {
			t.Fatalf("worker: %+v", w)
		}
		want := "this hop is not known; logs not read: agentsuite-demo-microvm-2: " + denied +
			"; pod not started: agentsuite-demo-microvm-1: "
		if !strings.HasPrefix(w.Detail, want) || !strings.Contains(w.Detail, "ContainerCreating") {
			t.Errorf("worker detail %q, want prefix %q", w.Detail, want)
		}

		delete(f.errs, "logs -n agentsuite-demo agentsuite-demo-microvm-2")
		f.out["logs -n agentsuite-demo agentsuite-demo-microvm-2"] = ""
		w = exec(f).Nodes["worker"]
		if w.State != "unknown" || !strings.HasPrefix(w.Detail, "this hop is not known; pod not started: agentsuite-demo-microvm-1: ") ||
			strings.Contains(w.Detail, "logs not read") {
			t.Errorf("worker: %+v", w)
		}
	})

	t.Run("router logs unreadable", func(t *testing.T) {
		f := attachCluster(getOnly)
		f.errs = map[string]string{"logs -n ate-system atenet-router-1": denied}
		tr := exec(f)
		if r := tr.Nodes["router"]; r.State != "unknown" || !strings.Contains(r.Detail, "Forbidden") {
			t.Errorf("router: %+v", r)
		}
	})
}

// Resume on a session whose actor is already RUNNING: the daemon's ResumeActor gets resumed=false
// back and restores nothing. That line is the call's own (no turn is routed during a Resume), so
// ate-api is observed with the no-op labelled, and the worker is skipped, not missing.
func TestTraceResumeOfARunningActor(t *testing.T) {
	for _, c := range []struct {
		name, lane, daemon, apiLog, method string
		reads                              []actorRead
	}{
		{
			// The page's refresh read the actor just before the call: its own GetActor is stamped
			// before the call's window, and the daemon's GetActor lands inside the read's span, so
			// the read is paired with the daemon's line and that line is left out. The no-op
			// ResumeActor alone still identifies the call.
			name: "resume only", lane: "volumes", daemon: "agentsessionsd-volumes",
			reads: []actorRead{{uid: "sess-abc", from: t0.Add(-300 * time.Millisecond), to: t0.Add(-200 * time.Millisecond)}},
			apiLog: at(-290*time.Millisecond) + `{"msg":"Handle RPC","method":"/ateapi.Control/GetActor","req":"sess-abc","t":"bridge"}
` + at(10*time.Millisecond) + `{"msg":"Handle RPC","method":"/ateapi.Control/GetActor","req":"sess-abc","t":"daemon"}
` + at(20*time.Millisecond) + `{"msg":"Handle RPC","method":"/ateapi.Control/ResumeActor","req":"sess-abc","resp":{"actor":{"name":"sess-abc"}}}`,
			method: noopResumeMethod,
		},
		{
			// Every lane reads the actor before resuming it (runtime/substrate requireRestoreSnapshot).
			name: "read then resume", lane: "stateless", daemon: "agentsessionsd",
			apiLog: at(10*time.Millisecond) + `{"msg":"Handle RPC","method":"/ateapi.Control/GetActor","req":"sess-abc"}
` + at(20*time.Millisecond) + `{"msg":"Handle RPC","method":"/ateapi.Control/ResumeActor","req":"sess-abc","resp":{"actor":{"name":"sess-abc"}}}`,
			method: "GetActor + " + noopResumeMethod,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := attachCluster(c.apiLog)
			f.out["logs -n agentsuite-demo deploy/"+c.daemon] = at(60*time.Millisecond) + `msg="operation finished" component=session operation=resume session_uid=sess-abc phase=finish outcome=success`
			s := traceServer(f)
			s.lanes = []*lane{{Name: c.lane, Daemon: c.daemon}}
			s.kube.reads = c.reads
			tr := s.trace(context.Background(), &activity{ID: 4, Op: "resume", Lane: c.lane, Session: "sess-abc", OK: true, Started: t0, DurationMS: 60})
			for n, st := range map[string]string{"daemon": "observed", "ateapi": "observed", "worker": "skipped", "actor": "observed", "router": "skipped"} {
				if tr.Nodes[n].State != st {
					t.Errorf("%s: %s (%s), want %s", n, tr.Nodes[n].State, tr.Nodes[n].Detail, st)
				}
			}
			ev := tr.Nodes["ateapi"]
			if ev.Method != c.method || !strings.Contains(ev.Detail, "already RUNNING") || strings.Contains(ev.Detail, "attached") {
				t.Errorf("ate-api: %+v, want %q", ev, c.method)
			}
			if w := tr.Nodes["worker"]; !strings.Contains(w.Detail, "no workload restored") || strings.Contains(w.Detail, "atunnel") {
				t.Errorf("worker: %+v", w)
			}
		})
	}
}

// A Resume of a suspended actor reports the ResumeActor that resumed it, not the daemon's read
// before it (runtime/substrate requireRestoreSnapshot) nor a no-op next to it.
func TestTraceResumeOfASuspendedActor(t *testing.T) {
	f := attachCluster(at(5*time.Millisecond) + `{"msg":"Handle RPC","method":"/ateapi.Control/GetActor","req":"sess-abc"}
` + at(10*time.Millisecond) + `{"msg":"Handle RPC","method":"/ateapi.Control/ResumeActor","req":"sess-abc","resp":{"actor":{"name":"sess-abc"},"resumed":true}}
` + at(30*time.Millisecond) + `{"msg":"Handle RPC","method":"/ateapi.Control/ResumeActor","req":"sess-abc","resp":{"actor":{"name":"sess-abc"}}}`)
	f.out["logs -n agentsuite-demo deploy/agentsessionsd"] = at(60*time.Millisecond) + `msg="operation finished" component=session operation=resume session_uid=sess-abc phase=finish outcome=success`
	f.out["logs -n agentsuite-demo agentsuite-demo-microvm-0"] = at(20*time.Millisecond) + `{"msg":"Handle RPC","method":"/ateom.Ateom/RestoreWorkload","actor_name":"sess-abc"}`
	s := traceServer(f)
	s.lanes = []*lane{{Name: "stateless", Daemon: "agentsessionsd"}}
	tr := s.trace(context.Background(), &activity{ID: 5, Op: "resume", Lane: "stateless", Session: "sess-abc", OK: true, Started: t0, DurationMS: 60})
	if ev := tr.Nodes["ateapi"]; ev.State != "observed" || ev.Method != "ResumeActor" || strings.Contains(ev.Detail, "already RUNNING") {
		t.Errorf("ate-api: %+v", ev)
	}
	if w := tr.Nodes["worker"]; w.State != "observed" || w.Method != "RestoreWorkload" {
		t.Errorf("worker: %+v", w)
	}
}

// A suspend that ate-api saw only a GetActor for does not say the actor was attached: that is a
// turn's wording.
func TestTraceFallbackWordingIsPerOp(t *testing.T) {
	f := attachCluster(at(10*time.Millisecond) + `{"msg":"Handle RPC","method":"/ateapi.Control/GetActor","req":"sess-abc"}`)
	f.out["logs -n agentsuite-demo deploy/agentsessionsd"] = at(60*time.Millisecond) + `msg="operation finished" component=session operation=suspend session_uid=sess-abc phase=finish outcome=success`
	s := traceServer(f)
	s.lanes = []*lane{{Name: "stateless", Daemon: "agentsessionsd"}}
	tr := s.trace(context.Background(), &activity{ID: 6, Op: "suspend", Lane: "stateless", Session: "sess-abc", OK: true, Started: t0, DurationMS: 60})
	if ev := tr.Nodes["ateapi"]; ev.Method != "GetActor" || strings.Contains(ev.Detail, "attached") || strings.Contains(ev.Detail, "already RUNNING") {
		t.Errorf("ate-api: %+v", ev)
	}
	if w := tr.Nodes["worker"]; w.State != "skipped" || strings.Contains(w.Detail, "atunnel") {
		t.Errorf("worker: %+v", w)
	}
}

// A placing turn reports every lifecycle method it made, in the order logged across ate-api's
// replicas, and the restore.
func TestTracePlacementMethods(t *testing.T) {
	f := attachCluster(at(10*time.Millisecond) + `{"msg":"Handle RPC","method":"/ateapi.Control/GetActor","req":"sess-abc"}
` + at(20*time.Millisecond) + `{"msg":"Handle RPC","method":"/ateapi.Control/CreateActor","req":"sess-abc"}`)
	f.out["get pods -n ate-system"] = "pod/ate-api-server-1\npod/ate-api-server-0\npod/atenet-router-1\n"
	f.out["logs -n ate-system ate-api-server-1"] = at(200*time.Millisecond) + `{"msg":"Handle RPC","method":"/ateapi.Control/ResumeActor","req":"sess-abc","resp":{"actor":{"name":"sess-abc"},"resumed":true}}`
	f.out["logs -n agentsuite-demo agentsuite-demo-microvm-0"] = at(150*time.Millisecond) + `{"msg":"Handle RPC","method":"/ateom.Ateom/RestoreWorkload","actor_name":"sess-abc"}`
	s := traceServer(f)
	s.lanes = []*lane{{Name: "stateless", Daemon: "agentsessionsd"}}
	tr := s.trace(context.Background(), &activity{ID: 1, Op: "exec", Lane: "stateless", Session: "sess-abc", OK: true, Started: t0, DurationMS: 400})
	if got := tr.Nodes["ateapi"].Method; got != "CreateActor + ResumeActor" {
		t.Errorf("ate-api method %q", got)
	}
	if w := tr.Nodes["worker"]; w.State != "observed" || w.Method != "RestoreWorkload" {
		t.Errorf("worker: %+v", w)
	}
}

// The bridge's own actor reads (the page refreshing the actor after a call) put GetActor lines
// naming the session in ate-api's log; they are not the daemon's.
func TestTraceIgnoresTheBridgesOwnReads(t *testing.T) {
	f := attachCluster("")
	s := traceServer(f)
	s.lanes = []*lane{{Name: "stateless", Daemon: "agentsessionsd"}}
	start := time.Now().UTC()
	if _, err := s.kube.actor(context.Background(), "sess-abc"); err != nil {
		t.Fatal(err)
	}
	f.out["logs -n ate-system ate-api-server-0"] = start.Add(time.Millisecond).Format(time.RFC3339Nano) + ` {"msg":"Handle RPC","method":"/ateapi.Control/GetActor","req":"sess-abc"}`
	a := &activity{ID: 1, Op: "exec", Lane: "stateless", Session: "sess-abc", OK: true, Started: start.Add(-time.Second), DurationMS: 900}
	tr := s.trace(context.Background(), a)
	if tr.Nodes["ateapi"].State != "missing" {
		t.Errorf("ate-api: %+v, want missing: the only GetActor was the bridge's", tr.Nodes["ateapi"])
	}
	if tr.Nodes["worker"].State != "missing" {
		t.Errorf("worker: %+v, want missing", tr.Nodes["worker"])
	}
}

// A call that starts right after the previous call's trace read the actor: the read's GetActor and
// the daemon's land within readLag of each other. The read accounts for one line only.
func TestTraceKeepsTheDaemonsReadNextToTheBridges(t *testing.T) {
	f := attachCluster("")
	s := traceServer(f)
	s.lanes = []*lane{{Name: "stateless", Daemon: "agentsessionsd"}}
	if _, err := s.kube.actor(context.Background(), "sess-abc"); err != nil {
		t.Fatal(err)
	}
	r := s.kube.ownReads([]string{"sess-abc"})[0]
	started := r.to.Add(time.Millisecond)
	f.out["logs -n ate-system ate-api-server-0"] = r.to.UTC().Format(time.RFC3339Nano) + ` {"msg":"Handle RPC","method":"/ateapi.Control/GetActor","req":"sess-abc"}
` + started.Add(10*time.Millisecond).UTC().Format(time.RFC3339Nano) + ` {"msg":"Handle RPC","method":"/ateapi.Control/GetActor","req":"sess-abc","t":"daemon"}`
	a := &activity{ID: 2, Op: "exec", Lane: "stateless", Session: "sess-abc", OK: true, Started: started, DurationMS: 400}
	tr := s.trace(context.Background(), a)
	if ev := tr.Nodes["ateapi"]; ev.State != "observed" || ev.Method != "GetActor" || !strings.Contains(ev.Detail, "only GetActor") {
		t.Errorf("ate-api: %+v, want the daemon's GetActor", ev)
	}
	if tr.Nodes["worker"].State != "skipped" {
		t.Errorf("worker: %+v, want skipped", tr.Nodes["worker"])
	}
}

func TestDropOwnReads(t *testing.T) {
	ms := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Millisecond) }
	reads := []actorRead{{uid: "s", from: ms(0), to: ms(10)}, {uid: "s", from: ms(1000), to: ms(1010)}}
	for _, c := range []struct {
		times []int
		want  []int
	}{
		{nil, nil},
		{[]int{5}, nil},                  // the first read's own line
		{[]int{5, 20}, []int{1}},         // the read's line, then the daemon's within readLag
		{[]int{500}, []int{0}},           // outside every read
		{[]int{5, 1005, 1100}, []int{2}}, // one line per read
		{[]int{-5, 5}, []int{0}},         // before the read began: not its line
		{[]int{300}, []int{0}},           // past readLag
		{[]int{1005, 1006, 1007}, []int{1, 2}},
	} {
		var times []time.Time
		for _, n := range c.times {
			times = append(times, ms(n))
		}
		got := dropOwnReads(times, reads)
		if len(got) != len(c.want) {
			t.Errorf("%v: kept %v, want %v", c.times, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%v: kept %v, want %v", c.times, got, c.want)
				break
			}
		}
	}
}

func TestTraceCreateProvisionsNothing(t *testing.T) {
	f := &fakeCluster{out: map[string]string{
		"logs -n agentsuite-demo deploy/agentsessionsd-volumes": at(time.Millisecond) + `msg="operation finished" component=session operation=create error_kind="" session_uid=sess-other phase=finish outcome=success
` + at(2*time.Millisecond) + `msg="operation finished" component=session operation=create error_kind="" session_uid=sess-new phase=finish outcome=success`,
	}}
	tr := traceServer(f).trace(context.Background(), &activity{ID: 1, Op: "create", Lane: "volumes", Session: "sess-new", OK: true, Started: t0, DurationMS: 5})
	if tr.Nodes["daemon"].State != "observed" || !strings.Contains(tr.Nodes["daemon"].Detail, "sess-new") {
		t.Errorf("daemon: %+v", tr.Nodes["daemon"])
	}
	for _, n := range []string{"ateapi", "worker", "actor", "router"} {
		if tr.Nodes[n].State != "skipped" {
			t.Errorf("%s: %s, want skipped", n, tr.Nodes[n].State)
		}
	}
	if len(f.calls) != 1 {
		t.Errorf("create's trace read %d things, want only the daemon log: %q", len(f.calls), f.calls)
	}
}

func TestTraceWithoutCluster(t *testing.T) {
	s := &server{lanes: []*lane{{Name: "stateless", Daemon: "agentsessionsd"}}}
	tr := s.trace(context.Background(), &activity{ID: 1, Op: "exec", Lane: "stateless", Session: "s", OK: true, Started: time.Now(),
		Records: []recordView{{Kind: "INPUT"}, {Kind: "MODEL_CALL"}}})
	if tr.Nodes["daemon"].State != "unknown" || tr.Nodes["model"].State != "observed" {
		t.Errorf("trace without cluster reads: %+v", tr.Nodes)
	}
}

// memoryTier is a memory-tier ActorTemplate in the shape emit render writes, and its daemon flag.
func memoryTier(t *testing.T, mutate func(*atepb.ActorTemplate)) ([]byte, string) {
	t.Helper()
	tm := &atepb.ActorTemplate{
		Metadata:       &atepb.ResourceMetadata{Atespace: "agentsuite-demo", Name: "support-desk-triage-1a2b3c4d"},
		Containers:     []*atepb.Container{{Name: "harness", Image: "localhost:5001/h@sha256:" + strings.Repeat("0", 64)}},
		SandboxConfig:  &atepb.SandboxConfig{SandboxClass: atepb.SandboxClass_SANDBOX_CLASS_MICROVM},
		SnapshotConfig: &atepb.SnapshotConfig{PreferredFidelity: atepb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, StorageLocation: "gs://ate-snapshots/agentsuite-demo/support-desk-triage-1a2b3c4d/"},
	}
	if mutate != nil {
		mutate(tm)
	}
	b, err := protojson.Marshal(tm)
	if err != nil {
		t.Fatal(err)
	}
	return b, "-substrate-harness=triage-memory=agentsuite-demo/support-desk-triage-1a2b3c4d,memory\n"
}

const memoryManifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: agentsessionsd-memory
spec:
  template:
    spec:
      containers:
        - name: agentsessionsd
          args:
            - -addr=:8080
            - -substrate-harness=triage-memory=agentsuite-demo/support-desk-triage-1a2b3c4d,memory
            - -substrate-atespace=agentsuite-demo
---
apiVersion: v1
kind: Service
metadata:
  name: agentsessionsd-memory
`

func TestVolumesTemplate(t *testing.T) {
	b, flag := memoryTier(t, nil)
	d, err := volumesTemplate(b, flag)
	if err != nil {
		t.Fatal(err)
	}
	var got atepb.ActorTemplate
	if err := protojson.Unmarshal(d.template, &got); err != nil {
		t.Fatal(err)
	}
	if got.GetMetadata().GetName() != "support-desk-triage-1a2b3c4d-vol" || d.name != got.GetMetadata().GetName() {
		t.Errorf("name %q / %q", got.GetMetadata().GetName(), d.name)
	}
	if got.GetSnapshotConfig().GetPreferredFidelity() != atepb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES {
		t.Errorf("fidelity %s", got.GetSnapshotConfig().GetPreferredFidelity())
	}
	if loc := got.GetSnapshotConfig().GetStorageLocation(); loc != "gs://ate-snapshots/agentsuite-demo/support-desk-triage-1a2b3c4d-vol/" {
		t.Errorf("storage location %q", loc)
	}
	if len(got.GetVolumes()) != 1 || got.GetVolumes()[0].GetDurableDir() == nil || len(got.GetContainers()[0].GetVolumeMounts()) != 1 {
		t.Errorf("want one durableDir volume mounted: %v", got.GetVolumes())
	}
	if d.flag != "-substrate-harness=triage-memory=agentsuite-demo/support-desk-triage-1a2b3c4d-vol,memory" {
		t.Errorf("flag %q", d.flag)
	}

	m, err := volumesManifest(memoryManifest, flag, d.flag)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(m, "agentsessionsd-memory") || strings.Count(m, "name: agentsessionsd-volumes") != 2 || !strings.Contains(m, "- "+d.flag+"\n") {
		t.Errorf("manifest:\n%s", m)
	}
}

func TestVolumesTemplateRefusals(t *testing.T) {
	for name, mutate := range map[string]func(*atepb.ActorTemplate){
		"not MEMORY": func(tm *atepb.ActorTemplate) {
			tm.SnapshotConfig.PreferredFidelity = atepb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES
		},
		"gVisor": func(tm *atepb.ActorTemplate) { tm.SandboxConfig.SandboxClass = atepb.SandboxClass_SANDBOX_CLASS_GVISOR },
		"volumes already": func(tm *atepb.ActorTemplate) {
			tm.Volumes = []*atepb.Volume{{Name: "v", DurableDir: &atepb.DurableDirVolumeSource{}}}
		},
		"two containers": func(tm *atepb.ActorTemplate) { tm.Containers = append(tm.Containers, &atepb.Container{Name: "x"}) },
		"storage elsewhere": func(tm *atepb.ActorTemplate) {
			tm.SnapshotConfig.StorageLocation = "gs://ate-snapshots/other/"
		},
	} {
		b, flag := memoryTier(t, mutate)
		if _, err := volumesTemplate(b, flag); err == nil {
			t.Errorf("%s: derived", name)
		}
	}
	b, _ := memoryTier(t, nil)
	for _, flag := range []string{
		"-substrate-harness=triage-memory=agentsuite-demo/support-desk-triage-1a2b3c4d", // not ,memory
		"-substrate-harness=triage-memory=agentsuite-demo/other,memory",
		"-model=x",
	} {
		if _, err := volumesTemplate(b, flag); err == nil {
			t.Errorf("flag %q: derived", flag)
		}
	}
	if _, err := volumesManifest(strings.Replace(memoryManifest, "            - -substrate-harness", "            - -other", 1), "-substrate-harness=triage-memory=agentsuite-demo/support-desk-triage-1a2b3c4d,memory", "-x"); err == nil {
		t.Error("manifest without the memory flag: derived")
	}
	memFlag := "-substrate-harness=triage-memory=agentsuite-demo/support-desk-triage-1a2b3c4d,memory"
	if _, err := volumesManifest(strings.ReplaceAll(memoryManifest, "agentsessionsd-memory", "agentsessionsd"), memFlag, "-x"); err == nil {
		t.Error("manifest that does not name agentsessionsd-memory: derived")
	}
	if _, err := volumesManifest(strings.Replace(memoryManifest, "            - -substrate-atespace", "            - -substrate-harness=other=agentsuite-demo/t\n            - -substrate-atespace", 1), memFlag, "-substrate-harness=triage-memory=agentsuite-demo/t-vol,memory"); err == nil {
		t.Error("manifest serving a second harness: derived")
	}
	if got := suffixName(strings.Repeat("a", 62), "-vol"); len(got) != 63 || !strings.HasSuffix(got, "-vol") {
		t.Errorf("suffixName = %q", got)
	}
}

func TestRunInjectWritesFilesOnly(t *testing.T) {
	state := t.TempDir()
	b, flag := memoryTier(t, nil)
	mem := filepath.Join(state, "render-memory")
	if err := os.MkdirAll(mem, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		filepath.Join(mem, "actortemplate.json"):           string(b),
		filepath.Join(mem, "agentsessionsd.args"):          flag,
		filepath.Join(state, "agentsessionsd-memory.yaml"): memoryManifest,
	} {
		if err := os.WriteFile(name, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(t.TempDir(), "volumes")
	if err := runInject([]string{"-state", state, "-out", out}); err != nil {
		t.Fatal(err)
	}
	name, err := os.ReadFile(filepath.Join(out, "template-name"))
	if err != nil || strings.TrimSpace(string(name)) != "support-desk-triage-1a2b3c4d-vol" {
		t.Errorf("template-name %q, %v", name, err)
	}
	for _, f := range []string{"actortemplate.json", "agentsessionsd-volumes.yaml"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Error(err)
		}
	}
	if err := runInject([]string{"-state", t.TempDir(), "-out", out}); err == nil || !strings.Contains(err.Error(), "run up.sh first") {
		t.Errorf("missing render: err = %v", err)
	}
}

// A replay's router hop is searched only when controller.Replay ran; a replay that stopped before
// it routed no turn, so the hop is skipped, not missing.
func TestTraceReplayRoutesOnlyWhenTheControllerRan(t *testing.T) {
	for _, c := range []struct {
		name   string
		a      activity
		router string
	}{
		{"held", activity{OK: true, Replay: &replayResult{Turns: 1, ControllerCalls: 0}}, "observed"},
		{"controller failed", activity{FailedAt: "bridge", Message: "replay: unavailable", Replay: &replayResult{ControllerCalls: 0}}, "observed"},
		{"stopped before the controller", activity{FailedAt: "bridge", Message: "read fake model stats: refused", Replay: &replayResult{ControllerCalls: -1}}, "skipped"},
		{"nothing to re-run", activity{FailedAt: "bridge", Message: "session sess-abc has no completed turn: nothing to re-execute"}, "skipped"},
		{"Replay RPC failed", activity{Code: "Unavailable", Message: "daemon down"}, "skipped"},
	} {
		f := attachCluster("")
		f.out["logs -n agentsuite-demo deploy/agentsessionsd"] = at(5*time.Millisecond) + `msg="operation finished" component=session operation=replay session_uid=sess-abc phase=finish outcome=success`
		s := traceServer(f)
		s.lanes = []*lane{{Name: "stateless", Daemon: "agentsessionsd"}}
		a := c.a
		a.ID, a.Op, a.Lane, a.Session, a.Started, a.DurationMS = 1, "replay", "stateless", "sess-abc", t0, 400
		tr := s.trace(context.Background(), &a)
		if got := tr.Nodes["router"]; got.State != c.router {
			t.Errorf("%s: router %+v, want %s", c.name, got, c.router)
		}
		if c.router == "skipped" && !strings.Contains(tr.Nodes["router"].Detail, "controller.Replay did not run") {
			t.Errorf("%s: router detail %q", c.name, tr.Nodes["router"].Detail)
		}
	}
}

// Every Source is the read the bridge ran, kubeconfig, context and plugin path included, so a
// reader can paste it.
func TestTraceSourcesAreTheCommandsRun(t *testing.T) {
	f := attachCluster("")
	s := traceServer(f)
	s.lanes = []*lane{{Name: "stateless", Daemon: "agentsessionsd"}}
	s.kube.ate = "/work dir/bin/kubectl-ate"
	s.kube.kubeconfig = "/work dir/cluster/kubeconfig"
	s.kube.context = "kind-agentsuite-demo"
	tr := s.trace(context.Background(), &activity{ID: 1, Op: "exec", Lane: "stateless", Session: "sess-abc", OK: true, Started: t0, DurationMS: 400})
	want := map[string]string{
		"actor": "'/work dir/bin/kubectl-ate' get actor sess-abc -a agentsuite-demo -o json " +
			"--kubeconfig '/work dir/cluster/kubeconfig' --context kind-agentsuite-demo",
		"daemon": "kubectl logs -n agentsuite-demo deploy/agentsessionsd --timestamps --since-time=" + tr.Since +
			" --kubeconfig '/work dir/cluster/kubeconfig' --context kind-agentsuite-demo",
		"router": "kubectl logs -n ate-system atenet-router-1 --all-containers --timestamps --since-time=" + tr.Since +
			" --kubeconfig '/work dir/cluster/kubeconfig' --context kind-agentsuite-demo",
	}
	for n, w := range want {
		if got := tr.Nodes[n].Source; got != w {
			t.Errorf("%s source\n got %s\nwant %s", n, got, w)
		}
	}
	ran := map[string]bool{}
	for _, c := range f.calls {
		ran[strings.Join(c, " ")] = true
	}
	if !ran["/work dir/bin/kubectl-ate get actor sess-abc -a agentsuite-demo -o json --kubeconfig /work dir/cluster/kubeconfig --context kind-agentsuite-demo"] {
		t.Errorf("the actor read the source names did not run: %q", f.calls)
	}
	if got := shellQuote("it's"); got != `'it'\''s'` {
		t.Errorf("shellQuote = %s", got)
	}
}
