package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// kube reads the demo cluster: kubectl get and logs, and the kubectl-ate plugin's get and logs.
// run refuses any other verb, so the page can never change the cluster.
type kube struct {
	kubectl, ate        string
	kubeconfig, context string
	namespace, atespace string
	// exec runs a binary; tests replace it.
	exec func(ctx context.Context, bin string, args ...string) ([]byte, error)

	// reads are the bridge's own recent actor reads. Each is a GetActor in ate-api's log naming the
	// session, which the trace must not take for the daemon's.
	mu    sync.Mutex
	reads []actorRead
}

type actorRead struct {
	uid      string
	from, to time.Time
}

// maxReads bounds the bridge's record of its own reads; a trace looks back seconds, not minutes.
const maxReads = 256

var readVerbs = map[string]bool{"get": true, "logs": true}

func (k *kube) run(ctx context.Context, plugin bool, args ...string) ([]byte, error) {
	if len(args) == 0 || !readVerbs[args[0]] {
		return nil, fmt.Errorf("cluster read refused: %q is not get or logs", strings.Join(args, " "))
	}
	bin := k.kubectl
	if plugin {
		bin = k.ate
	}
	if bin == "" {
		return nil, errors.New("cluster reads are off")
	}
	full := append([]string{}, args...)
	if k.kubeconfig != "" {
		full = append(full, "--kubeconfig", k.kubeconfig)
	}
	if k.context != "" {
		full = append(full, "--context", k.context)
	}
	run := k.exec
	if run == nil {
		run = func(ctx context.Context, bin string, args ...string) ([]byte, error) {
			var stderr bytes.Buffer
			cmd := exec.CommandContext(ctx, bin, args...)
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil {
				msg := strings.TrimSpace(stderr.String())
				if msg == "" {
					msg = err.Error()
				}
				return out, errors.New(firstLine(msg))
			}
			return out, nil
		}
	}
	return run(ctx, bin, full...)
}

// actorView is the actor as substrate reports it.
type actorView struct {
	Name      string         `json:"name"`
	Atespace  string         `json:"atespace"`
	UID       string         `json:"uid,omitempty"`
	State     string         `json:"state"`
	Template  string         `json:"template,omitempty"`
	Worker    string         `json:"worker,omitempty"`
	Snapshots []snapshotView `json:"snapshots"`
}

type snapshotView struct {
	Generation int64  `json:"generation"`
	Fidelity   string `json:"fidelity"`
	Storage    string `json:"storage"`
}

func (k *kube) actor(ctx context.Context, uid string) (*actorView, error) {
	from := time.Now()
	out, err := k.run(ctx, true, "get", "actor", uid, "-a", k.atespace, "-o", "json")
	k.mu.Lock()
	k.reads = append(k.reads, actorRead{uid: uid, from: from, to: time.Now()})
	if len(k.reads) > maxReads {
		k.reads = append([]actorRead(nil), k.reads[len(k.reads)-maxReads:]...)
	}
	k.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return parseActor(out)
}

// ownReads returns the bridge's own reads of the actors in ids.
func (k *kube) ownReads(ids []string) []actorRead {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []actorRead
	for _, r := range k.reads {
		if containsString(ids, r.uid) {
			out = append(out, r)
		}
	}
	return out
}

// readLag is how long after a read's answer its ate-api line can be stamped. ate-api logs an RPC
// when its handler returns, before kubectl-ate has the answer; the container runtime stamps the
// line as it reads it, a little later.
const readLag = 250 * time.Millisecond

// dropOwnReads removes, for each of the bridge's own reads, the earliest line at times[i] inside
// that read's span: the line the read itself caused. Pairing one line per read, rather than
// dropping every line in the span, keeps the daemon's own call when it lands next to a read, as
// it does when a call follows the previous call's trace at once. times is sorted; the result holds
// the indexes kept.
func dropOwnReads(times []time.Time, reads []actorRead) []int {
	gone := make([]bool, len(times))
	for _, r := range reads {
		for i, t := range times {
			if !gone[i] && !t.Before(r.from) && !t.After(r.to.Add(readLag)) {
				gone[i] = true
				break
			}
		}
	}
	var keep []int
	for i := range times {
		if !gone[i] {
			keep = append(keep, i)
		}
	}
	return keep
}

func containsString(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// parseActor reads the fields the page shows from the plugin's JSON for one actor. A snapshot's
// URI is not shown: the generation, fidelity and storage status are what the refusal is about.
func parseActor(b []byte) (*actorView, error) {
	var raw struct {
		Metadata struct {
			Name     string `json:"name"`
			Atespace string `json:"atespace"`
			UID      string `json:"uid"`
		} `json:"metadata"`
		Template struct {
			Name string `json:"name"`
		} `json:"actorTemplate"`
		Status struct {
			State      string `json:"state"`
			Assignment struct {
				WorkerPod string `json:"workerPod"`
				NodeName  string `json:"nodeName"`
			} `json:"workerAssignment"`
			Snapshots []struct {
				Generation json.Number `json:"generation"`
				Fidelity   string      `json:"fidelity"`
				Durable    struct {
					Status string `json:"status"`
				} `json:"durableSnapshot"`
			} `json:"snapshots"`
		} `json:"status"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("decode actor: %w", err)
	}
	v := &actorView{
		Name: raw.Metadata.Name, Atespace: raw.Metadata.Atespace, UID: raw.Metadata.UID,
		State:    strings.TrimPrefix(raw.Status.State, "ACTOR_STATE_"),
		Template: raw.Template.Name,
		Worker:   raw.Status.Assignment.WorkerPod,
	}
	for _, s := range raw.Status.Snapshots {
		gen, _ := s.Generation.Int64()
		v.Snapshots = append(v.Snapshots, snapshotView{
			Generation: gen,
			Fidelity:   strings.TrimPrefix(s.Fidelity, "SNAPSHOT_FIDELITY_"),
			Storage:    strings.TrimPrefix(s.Durable.Status, "SNAPSHOT_STORAGE_STATUS_"),
		})
	}
	sort.Slice(v.Snapshots, func(i, j int) bool { return v.Snapshots[i].Generation > v.Snapshots[j].Generation })
	return v, nil
}

// nodeEvidence is what one node of the flow showed for an activity.
type nodeEvidence struct {
	// State is observed (a log line or state read shows the hop), missing (none found in the
	// window), refused (the call failed here), skipped (the hop is not on this call's path, or it
	// was asked and not taken, such as replay's model) or unknown (the cluster could not be read).
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
	// Method names the RPCs the evidence shows for this call, in the order they were logged
	// ("CreateActor + ResumeActor", "GetActor"). The page labels the edge into the hop with it.
	Method string `json:"method,omitempty"`
	// Source names where the evidence came from, as the command a reader can run themselves.
	Source string `json:"source,omitempty"`
}

type traceView struct {
	Activity int                     `json:"activity"`
	Op       string                  `json:"op"`
	Since    string                  `json:"since"`
	Nodes    map[string]nodeEvidence `json:"nodes"`
}

// Log patterns per hop, from the components' own log lines at the pinned substrate commit; demo/
// trace-turn.sh matches the same ones for a turn. The first group of each is the method the page
// shows. ate-api logs every RPC, its request included, as "Handle RPC" (internal/ateinterceptors).
var (
	ateapiPlace   = regexp.MustCompile(`/ateapi\.Control/(CreateActor|ResumeActor)\b`)
	ateapiSuspend = regexp.MustCompile(`/ateapi\.Control/(SuspendActor)\b`)
	ateapiGet     = regexp.MustCompile(`/ateapi\.Control/(GetActor)\b`)
	ateomRestore  = regexp.MustCompile(`/ateom\.Ateom/(RestoreWorkload|RunWorkload)\b`)
	ateomSuspend  = regexp.MustCompile(`/ateom\.Ateom/(CheckpointWorkload)\b`)
	routeOK       = regexp.MustCompile(`(Route ok)`)
	// resumedTrue is ResumeActorResponse.resumed as ate-api's JSON log writes it (omitempty).
	resumedTrue = regexp.MustCompile(`"resumed":true\b`)
)

// trace collects, for one activity, the evidence of each hop from the components' logs since the
// call started, and the actor's state now. A hop with no line is "missing", never assumed; one the
// evidence shows the call did not take (the restore of an actor that was attached) is "skipped".
func (s *server) trace(ctx context.Context, a *activity) traceView {
	since := a.Started.Add(-2 * time.Second).UTC()
	t := traceView{Activity: a.ID, Op: a.Op, Since: since.Format(time.RFC3339), Nodes: map[string]nodeEvidence{}}
	t.Nodes["browser"] = nodeEvidence{State: "observed", Detail: "POST /api/" + a.Op}
	t.Nodes["bridge"] = nodeEvidence{State: "observed", Detail: bridgeDetail(a)}
	l, _ := s.laneNamed(a.Lane)
	daemon := "agentsessionsd"
	if l != nil {
		daemon = l.Daemon
	}
	ids := []string{a.Session}
	if a.Child != "" {
		ids = append(ids, a.Child)
	}
	if s.kube == nil {
		for _, n := range []string{"daemon", "ateapi", "worker", "actor", "router"} {
			t.Nodes[n] = nodeEvidence{State: "unknown", Detail: "cluster reads are off"}
		}
	} else {
		s.traceCluster(ctx, a, daemon, ids, since, &t)
	}
	switch a.Op {
	case "exec":
		calls := 0
		for _, r := range a.Records {
			if r.Kind == "MODEL_CALL" {
				calls++
			}
		}
		st := "observed"
		if calls == 0 {
			st = "missing"
		}
		t.Nodes["model"] = nodeEvidence{State: st, Detail: fmt.Sprintf("%d model call(s) journaled by this turn", calls), Source: "the Exec stream"}
	case "replay":
		if a.Replay != nil {
			d := fmt.Sprintf("controller counted %d model calls", a.Replay.ControllerCalls)
			if a.Replay.FakeBefore >= 0 {
				d += fmt.Sprintf("; fake model counter %d -> %d", a.Replay.FakeBefore, a.Replay.FakeAfter)
			}
			t.Nodes["model"] = nodeEvidence{State: "skipped", Detail: d}
		}
	default:
		t.Nodes["model"] = nodeEvidence{State: "skipped"}
	}
	if !a.OK {
		// The bridge saw the failure from the daemon; a refusal is the daemon's (its substrate
		// adapter runs in process), anything else is reported there too, with the code.
		d := t.Nodes["daemon"]
		d.State = "refused"
		d.Detail = a.Code + ": " + a.Message
		t.Nodes["daemon"] = d
	}
	return t
}

func bridgeDetail(a *activity) string {
	switch a.Op {
	case "create":
		return "Sessions.CreateSession"
	case "exec":
		return "Sessions.Exec (streamed)"
	case "suspend":
		return "Sessions.Suspend"
	case "resume":
		return "Sessions.Resume"
	case "fork":
		return fmt.Sprintf("Sessions.Fork at seq %d", a.AtSeq)
	case "replay":
		return "Sessions.Replay (journal), then controller.Replay through atenet-router"
	}
	return a.Op
}

func (s *server) traceCluster(ctx context.Context, a *activity, daemon string, ids []string, since time.Time, t *traceView) {
	k := s.kube
	sinceArg := "--since-time=" + since.Format(time.RFC3339)
	// --since-time has whole-second precision, so it also returns the previous call's lines. Every
	// line is read with its timestamp and kept only inside the call's own window.
	w := window{from: a.Started.Add(-250 * time.Millisecond), to: a.Started.Add(time.Duration(a.DurationMS)*time.Millisecond + 2*time.Second)}
	// agentsessionsd: its own "operation finished" line for this call.
	src := fmt.Sprintf("kubectl -n %s logs deploy/%s %s", k.namespace, daemon, sinceArg)
	if out, err := k.run(ctx, false, "logs", "-n", k.namespace, "deploy/"+daemon, "--timestamps", sinceArg); err != nil {
		t.Nodes["daemon"] = nodeEvidence{State: "unknown", Detail: err.Error(), Source: src}
	} else {
		t.Nodes["daemon"] = match(out, w, daemonPattern(a), src)
	}
	if a.Op == "create" {
		// CreateSession writes the session record only; no actor exists until the first turn.
		for _, n := range []string{"ateapi", "worker", "actor", "router"} {
			t.Nodes[n] = nodeEvidence{State: "skipped", Detail: "CreateSession provisions no compute"}
		}
		return
	}
	pods, err := k.pods(ctx)
	if err != nil {
		for _, n := range []string{"ateapi", "worker", "router"} {
			t.Nodes[n] = nodeEvidence{State: "unknown", Detail: err.Error()}
		}
	} else {
		idRe := anyOf(ids)
		if a.Op != "replay" {
			api, om := ateapiPlace, ateomRestore
			if a.Op == "suspend" {
				api, om = ateapiSuspend, ateomSuspend
			}
			// Resume on a RUNNING actor: Backend.RestoreInstance reads the actor (GetActor), finds it
			// RUNNING and still calls ResumeActor, a no-op ate-api answers at once; that is the call's
			// own line. No turn is routed during a Resume, so the router's no-op resumes are not in
			// its window.
			ah := s.podMatch(ctx, pods.ateapi, "ate-system", sinceArg, w, api, ateapiGet, ids, idRe, a.Op == "resume")
			t.Nodes["ateapi"] = ah.evidence
			t.Nodes["worker"] = s.podMatch(ctx, pods.workers, k.namespace, sinceArg, w, om, nil, nil, idRe, false).evidence
			switch {
			case ah.noopResume:
				ev := t.Nodes["ateapi"]
				ev.Detail = "the actor was already RUNNING, so ate-api resumed nothing: " + ev.Detail
				t.Nodes["ateapi"] = ev
			case ah.fallbackOnly && a.Op == "exec":
				ev := t.Nodes["ateapi"]
				ev.Detail = "the actor was attached as it was: " + ev.Detail
				t.Nodes["ateapi"] = ev
			}
			if ah.fallbackOnly && t.Nodes["worker"].State == "missing" {
				// The substrate runtime reads the actor first on every placement and attaches to a
				// RUNNING one without another call (runtime/substrate Backend.Create): the second and
				// later turns of a session, and a fork child's first. A Resume of a RUNNING actor gets
				// resumed=false back. Either way nothing was restored.
				d := "no workload restored or checkpointed: ate-api saw only " + ah.evidence.Method + " for this session during the call"
				if a.Op == "exec" {
					d += "; the turn reaches the actor through atunnel in this pod, which logs nothing per request"
				}
				t.Nodes["worker"] = nodeEvidence{State: "skipped", Detail: d, Source: t.Nodes["worker"].Source}
			}
		} else {
			t.Nodes["ateapi"] = nodeEvidence{State: "skipped", Detail: "replay drives the running actor; no lifecycle call"}
			t.Nodes["worker"] = nodeEvidence{State: "skipped", Detail: "atunnel in the worker pod forwards the stream (not logged per request)"}
		}
		if a.Op == "exec" || a.Op == "replay" {
			t.Nodes["router"] = s.podMatch(ctx, pods.router, "ate-system", sinceArg, w, routeOK, nil, nil, idRe, false).evidence
		} else {
			t.Nodes["router"] = nodeEvidence{State: "skipped", Detail: "no turn is routed"}
		}
	}
	// The actor: its state as ate-api reports it now, the fork's child when there is one.
	target := ids[len(ids)-1]
	src = fmt.Sprintf("kubectl ate get actor %s -a %s -o json", target, k.atespace)
	if av, err := k.actor(ctx, target); err != nil {
		t.Nodes["actor"] = nodeEvidence{State: "missing", Detail: err.Error(), Source: src}
	} else {
		d := "state " + av.State
		if len(av.Snapshots) > 0 {
			sn := av.Snapshots[0]
			d += fmt.Sprintf("; snapshot generation %d %s (%s)", sn.Generation, sn.Fidelity, sn.Storage)
		}
		t.Nodes["actor"] = nodeEvidence{State: "observed", Detail: d, Source: src}
	}
}

// daemonPattern matches agentsessionsd's "operation finished" line for the call (observability's
// Start: component, operation, then the call's own attributes, ending with phase=finish).
func daemonPattern(a *activity) *regexp.Regexp {
	switch a.Op {
	case "create":
		// CreateSession names the session only in its finish line's result attributes.
		return regexp.MustCompile(`operation=create\b.*session_uid=` + regexp.QuoteMeta(a.Session) + `\b.*phase=finish`)
	case "fork":
		return regexp.MustCompile(`operation=fork parent_session_uid=` + regexp.QuoteMeta(a.Session) + `\b.*phase=finish`)
	}
	return regexp.MustCompile(`operation=` + a.Op + ` session_uid=` + regexp.QuoteMeta(a.Session) + `\b.*phase=finish`)
}

func anyOf(ids []string) *regexp.Regexp {
	q := make([]string, len(ids))
	for i, id := range ids {
		q[i] = regexp.QuoteMeta(id)
	}
	return regexp.MustCompile(strings.Join(q, "|"))
}

type podSet struct{ ateapi, router, workers []string }

// pods finds the ate-api and atenet-router pods in ate-system and the worker pods in the demo's
// namespace (everything there but the daemons and the fake model).
func (k *kube) pods(ctx context.Context) (podSet, error) {
	var p podSet
	sys, err := k.run(ctx, false, "get", "pods", "-n", "ate-system", "-o", "name")
	if err != nil {
		return p, err
	}
	for _, n := range podNames(sys) {
		switch {
		case strings.HasPrefix(n, "ate-api"):
			p.ateapi = append(p.ateapi, n)
		case strings.HasPrefix(n, "atenet-router"):
			p.router = append(p.router, n)
		}
	}
	demo, err := k.run(ctx, false, "get", "pods", "-n", k.namespace, "-o", "name")
	if err != nil {
		return p, err
	}
	for _, n := range podNames(demo) {
		if !strings.HasPrefix(n, "agentsessionsd") && !strings.HasPrefix(n, "fakemodel") {
			p.workers = append(p.workers, n)
		}
	}
	return p, nil
}

func podNames(out []byte) []string {
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if n := strings.TrimPrefix(strings.TrimSpace(line), "pod/"); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// podHits is what podMatch found: the evidence, whether it rests on the fallback alone (no
// primary line that did the work), and whether that includes a ResumeActor that resumed nothing.
type podHits struct {
	evidence     nodeEvidence
	fallbackOnly bool
	noopResume   bool
}

// noopResumeMethod labels a ResumeActor that ate-api answered with the actor already RUNNING.
const noopResumeMethod = "ResumeActor (already RUNNING)"

// podMatch searches the pods' logs since the call for lines with the id and primary, else with the
// id and fallback. The evidence is the earliest line, labelled with every method its pattern
// matched, in the order they were logged across the pods. A fallback line the bridge caused
// itself, reading an actor in ownIDs, is not evidence (dropOwnReads).
//
// A ResumeActor that resumed nothing is not a primary line. atenet-router calls ResumeActor on
// every routed request to make sure the actor runs, and ate-api answers at once, with resumed
// unset, when it already does (substrate 96dffb2, cmd/ateapi/internal/controlapi/
// workflow_resume.go). ate-api logs the response with the call, so the daemon's resume of a
// suspended or new actor is the line that says "resumed":true. With keepNoop (a Resume, which
// routes no turn) a no-op ResumeActor is the daemon's own and counts as a fallback line, labelled
// noopResumeMethod; otherwise it is the router's and is dropped.
func (s *server) podMatch(ctx context.Context, pods []string, ns, sinceArg string, w window, primary, fallback *regexp.Regexp, ownIDs []string, id *regexp.Regexp, keepNoop bool) podHits {
	if len(pods) == 0 {
		return podHits{evidence: nodeEvidence{State: "unknown", Detail: "no pod found in " + ns}}
	}
	type hit struct {
		at                time.Time
		line, src, method string
	}
	var first, second, noop []hit
	for _, pod := range pods {
		src := fmt.Sprintf("kubectl -n %s logs %s --all-containers %s", ns, pod, sinceArg)
		out, err := s.kube.run(ctx, false, "logs", "-n", ns, pod, "--all-containers", "--timestamps", sinceArg)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(bytes.NewReader(out))
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			ts, line, ok := w.line(sc.Text())
			if !ok || !id.MatchString(line) {
				continue
			}
			if m := primary.FindStringSubmatch(line); m != nil {
				switch {
				case m[1] != "ResumeActor" || resumedTrue.MatchString(line):
					first = append(first, hit{ts, line, src, m[1]})
				case keepNoop:
					noop = append(noop, hit{ts, line, src, noopResumeMethod})
				}
				continue
			}
			if fallback == nil {
				continue
			}
			if m := fallback.FindStringSubmatch(line); m != nil {
				second = append(second, hit{ts, line, src, m[1]})
			}
		}
	}
	if len(second) > 0 {
		sort.SliceStable(second, func(i, j int) bool { return second[i].at.Before(second[j].at) })
		times := make([]time.Time, len(second))
		for i, h := range second {
			times[i] = h.at
		}
		var kept []hit
		for _, i := range dropOwnReads(times, s.kube.ownReads(ownIDs)) {
			kept = append(kept, second[i])
		}
		second = kept
	}
	methods := func(hs []hit) string {
		sort.SliceStable(hs, func(i, j int) bool { return hs[i].at.Before(hs[j].at) })
		var ms []string
		for _, h := range hs {
			if !containsString(ms, h.method) {
				ms = append(ms, h.method)
			}
		}
		return strings.Join(ms, " + ")
	}
	switch {
	case len(first) > 0:
		m := methods(first)
		return podHits{evidence: nodeEvidence{State: "observed", Detail: summarize(first[0].line), Method: m, Source: first[0].src}}
	case len(second) > 0 || len(noop) > 0:
		// The bridge's reads are paired with GetActor lines only, so the no-op resumes join after.
		all := append(second, noop...)
		m := methods(all)
		d := "only " + m + " for this session during the call; " + summarize(all[0].line)
		return podHits{evidence: nodeEvidence{State: "observed", Detail: d, Method: m, Source: all[0].src}, fallbackOnly: true, noopResume: len(noop) > 0}
	}
	return podHits{evidence: nodeEvidence{State: "missing", Detail: "no line for this session during the call", Source: "kubectl -n " + ns + " logs <pod> " + sinceArg}}
}

// window is the span of one call; a log line outside it belongs to another call.
type window struct{ from, to time.Time }

// line splits the timestamp kubectl logs --timestamps puts first, and reports whether the line was
// written inside the window. A line without a readable timestamp is outside.
func (w window) line(s string) (time.Time, string, bool) {
	ts, rest, ok := strings.Cut(s, " ")
	if !ok {
		return time.Time{}, "", false
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil || t.Before(w.from) || t.After(w.to) {
		return time.Time{}, "", false
	}
	return t, rest, true
}

func match(out []byte, w window, re *regexp.Regexp, src string) nodeEvidence {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		if _, line, ok := w.line(sc.Text()); ok && re.MatchString(line) {
			return nodeEvidence{State: "observed", Detail: summarize(line), Source: src}
		}
	}
	return nodeEvidence{State: "missing", Detail: "no line for this call during it", Source: src}
}

// summarize shortens a log line for the page: the JSON fields that name the hop when it is a JSON
// log, else the line cut to 240 characters.
func summarize(line string) string {
	var m map[string]any
	if json.Unmarshal([]byte(line), &m) == nil {
		var parts []string
		for _, k := range []string{"time", "ts", "msg", "method", "code", "grpc.code", "targetAddr"} {
			if v, ok := m[k]; ok {
				parts = append(parts, fmt.Sprintf("%s=%v", k, v))
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, " ")
		}
	}
	if len(line) > 240 {
		return line[:240] + "…"
	}
	return line
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
