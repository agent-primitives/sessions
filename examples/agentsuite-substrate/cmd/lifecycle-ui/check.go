package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// runCheck drives a running page through every lifecycle call over its own HTTP API, the calls the
// buttons make, and fails on the first result that does not hold. It is the page's end-to-end check
// against the example cluster (ui.sh check); it prints each step, the page URLs to open, and the
// trace of each call.
func runCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	base := fs.String("url", "http://127.0.0.1:8090", "the running page")
	fakeModel := fs.Bool("fake-model", true, "the daemons call the fake model, whose replies list the turns so far")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c := &checker{base: strings.TrimRight(*base, "/"), fake: *fakeModel, http: &http.Client{Timeout: 4 * time.Minute}}
	return c.run(context.Background())
}

type checker struct {
	base string
	fake bool
	http *http.Client
	// unseen lists the hops a call must take that its trace did not show. The check carries on
	// through every call, then fails if any are listed.
	unseen []string
}

func (c *checker) post(ctx context.Context, path string, body any) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%s: %s: %s", path, resp.Status, strings.TrimSpace(string(msg)))
	}
	return resp, nil
}

func (c *checker) call(ctx context.Context, path string, body any) (*activity, error) {
	resp, err := c.post(ctx, path, body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var a activity
	return &a, json.NewDecoder(resp.Body).Decode(&a)
}

func (c *checker) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// exec sends one turn and reads the NDJSON stream to its activity, counting the frames.
func (c *checker) exec(ctx context.Context, sid, input string) (*activity, int, error) {
	resp, err := c.post(ctx, "/api/exec", map[string]string{"session": sid, "input": input})
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	frames := 0
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var f struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			return nil, frames, err
		}
		frames++
		if f.Type == "activity" {
			var a activity
			return &a, frames, json.Unmarshal(f.Data, &a)
		}
	}
	return nil, frames, errors.New("exec stream ended without its activity")
}

func (c *checker) expect(ok bool, format string, args ...any) error {
	if !ok {
		return fmt.Errorf("CHECK FAILED: "+format, args...)
	}
	fmt.Printf("  ok   "+format+"\n", args...)
	return nil
}

func (c *checker) step(name string) { fmt.Printf("\n== %s\n", name) }

// trace prints the page's evidence for one call and fails when a hop the call must take was not
// observed.
func (c *checker) trace(ctx context.Context, a *activity, must ...string) error {
	want := map[string]string{}
	for _, n := range must {
		want[n] = "observed"
		if n == "daemon" && !a.OK {
			want[n] = "refused"
		}
	}
	return c.traceWant(ctx, a, want, nil)
}

// placed is a call that creates, resumes or suspends the actor: every hop is observed, the router
// too when the call is a turn.
func placed(turn bool) map[string]string {
	m := map[string]string{"daemon": "observed", "ateapi": "observed", "worker": "observed", "actor": "observed"}
	if turn {
		m["router"], m["model"] = "observed", "observed"
	}
	return m
}

// attached is a turn on an actor that is already RUNNING: ate-api sees only GetActor, nothing is
// restored, and the router carries the turn.
var attached = map[string]string{"daemon": "observed", "ateapi": "observed", "worker": "skipped", "actor": "observed", "router": "observed", "model": "observed"}

// traceWant prints the page's evidence for one call and fails when a hop is not in the state
// want gives it, or its method is not the one methods gives it.
func (c *checker) traceWant(ctx context.Context, a *activity, want, methods map[string]string) error {
	var t traceView
	if err := c.get(ctx, fmt.Sprintf("/api/trace?id=%d", a.ID), &t); err != nil {
		return err
	}
	nodes := []string{"browser", "bridge", "daemon", "ateapi", "worker", "actor", "router", "model"}
	for _, n := range nodes {
		ev := t.Nodes[n]
		m := ""
		if ev.Method != "" {
			m = "[" + ev.Method + "] "
		}
		fmt.Printf("       %-8s %-9s %s%s\n", n, ev.State, m, cut(ev.Detail, 150))
	}
	for _, n := range nodes {
		st, ok := want[n]
		if !ok {
			continue
		}
		got := t.Nodes[n]
		if got.State != st || (methods[n] != "" && got.Method != methods[n]) {
			w := st
			if methods[n] != "" {
				w += " [" + methods[n] + "]"
			}
			c.unseen = append(c.unseen, fmt.Sprintf("%s (activity %d): %s is %s [%s], want %s", a.Op, a.ID, n, got.State, got.Method, w))
			fmt.Printf("  MISS %s trace: %s is %s [%s], want %s\n", a.Op, n, got.State, got.Method, w)
			continue
		}
		fmt.Printf("  ok   %s trace: %s is %s\n", a.Op, n, st)
	}
	return nil
}

func cut(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func (c *checker) actor(ctx context.Context, sid string) (*actorView, error) {
	var out struct {
		Actor *actorView `json:"actor"`
		Error string     `json:"error"`
	}
	if err := c.get(ctx, "/api/actor?session="+sid, &out); err != nil {
		return nil, err
	}
	if out.Actor == nil {
		return nil, errors.New("actor: " + out.Error)
	}
	return out.Actor, nil
}

func (c *checker) run(ctx context.Context) error {
	var cfg struct {
		Lanes []struct {
			Name string `json:"name"`
			Up   bool   `json:"up"`
		} `json:"lanes"`
		Atespace string `json:"atespace"`
		Replay   bool   `json:"replay"`
		Cluster  bool   `json:"cluster"`
	}
	if err := c.get(ctx, "/api/config", &cfg); err != nil {
		return err
	}
	up := map[string]bool{}
	for _, l := range cfg.Lanes {
		up[l.Name] = l.Up
	}
	for _, l := range []string{"stateless", "volumes"} {
		if err := c.expect(up[l], "lane %s answers", l); err != nil {
			return err
		}
	}
	if err := c.expect(cfg.Replay && cfg.Cluster, "replay and cluster reads are on"); err != nil {
		return err
	}

	c.step("create, on the stateless tier")
	a, err := c.call(ctx, "/api/create", map[string]string{"lane": "stateless", "name": "lifecycle-check"})
	if err != nil {
		return err
	}
	if err := c.expect(a.OK && a.Session != "", "CreateSession: %s", a.Session); err != nil {
		return err
	}
	sid := a.Session
	if err := c.trace(ctx, a, "daemon"); err != nil {
		return err
	}

	turns := []string{"my laptop will not boot", "the VPN drops every hour"}
	for i, in := range turns {
		c.step(fmt.Sprintf("exec, turn %d", i+1))
		a, frames, err := c.exec(ctx, sid, in)
		if err != nil {
			return err
		}
		if err := c.expect(a.OK && len(a.Records) > 0 && frames >= len(a.Records)+2, "Exec streamed %d records in %d frames: %q", len(a.Records), frames, cut(a.Output, 120)); err != nil {
			return err
		}
		if c.fake {
			if err := c.expect(strings.Contains(a.Output, fmt.Sprintf("%d) %s", i+1, in)), "the reply lists turn %d", i+1); err != nil {
				return err
			}
		}
		if i == 0 {
			if err := c.traceWant(ctx, a, placed(true), map[string]string{"ateapi": "CreateActor + ResumeActor", "worker": "RestoreWorkload"}); err != nil {
				return err
			}
		} else if err := c.traceWant(ctx, a, attached, map[string]string{"ateapi": "GetActor"}); err != nil {
			return err
		}
	}
	av, err := c.actor(ctx, sid)
	if err != nil {
		return err
	}
	actorUID := av.UID
	if err := c.expect(av.State == "RUNNING", "actor %s/%s is RUNNING", cfg.Atespace, sid); err != nil {
		return err
	}

	c.step("suspend")
	if a, err = c.call(ctx, "/api/suspend", map[string]string{"session": sid}); err != nil {
		return err
	}
	if err := c.expect(a.OK, "Suspend OK%s", a.Message); err != nil {
		return err
	}
	if av, err = c.actor(ctx, sid); err != nil {
		return err
	}
	if err := c.expect(av.State == "SUSPENDED" && len(av.Snapshots) > 0 && av.Snapshots[0].Storage == "COMPLETED",
		"actor SUSPENDED with a completed snapshot (%+v)", av.Snapshots); err != nil {
		return err
	}
	if err := c.traceWant(ctx, a, placed(false), map[string]string{"ateapi": "SuspendActor", "worker": "CheckpointWorkload"}); err != nil {
		return err
	}

	c.step("resume, then a third turn")
	if a, err = c.call(ctx, "/api/resume", map[string]string{"session": sid}); err != nil {
		return err
	}
	if err := c.expect(a.OK, "Resume OK%s", a.Message); err != nil {
		return err
	}
	if err := c.traceWant(ctx, a, placed(false), map[string]string{"ateapi": "ResumeActor", "worker": "RestoreWorkload"}); err != nil {
		return err
	}
	ex, _, err := c.exec(ctx, sid, "also, my password expired")
	if err != nil {
		return err
	}
	if err := c.expect(ex.OK, "Exec after resume"); err != nil {
		return err
	}
	if c.fake {
		if err := c.expect(strings.Contains(ex.Output, "1) my laptop will not boot; 2) the VPN drops every hour; 3) also, my password expired"), "the third reply carries both earlier turns"); err != nil {
			return err
		}
	}
	if av, err = c.actor(ctx, sid); err != nil {
		return err
	}
	if err := c.expect(av.State == "RUNNING" && av.UID == actorUID, "the same actor (UID %s) is RUNNING again", actorUID); err != nil {
		return err
	}

	c.step("replay: re-run every turn against the journal")
	if a, err = c.call(ctx, "/api/replay", map[string]string{"session": sid}); err != nil {
		return err
	}
	if err := c.expect(a.OK && a.Replay != nil && a.Replay.Turns == 3 && a.Replay.ControllerCalls == 0 && a.Replay.FakeAfter == a.Replay.FakeBefore,
		"Replay re-ran 3 turns with 0 model calls (%+v) %s", a.Replay, a.Message); err != nil {
		return err
	}
	if err := c.trace(ctx, a, "daemon", "router", "actor"); err != nil {
		return err
	}

	c.step("fork at the end of turn 1, and a turn on the child")
	var j journal
	if err := c.get(ctx, "/api/journal?session="+sid, &j); err != nil {
		return err
	}
	if err := c.expect(j.Verified, "the parent's journal (%d records) verifies on the client", len(j.Records)); err != nil {
		return err
	}
	var endSeq int64
	for _, r := range j.Records {
		if r.Kind == "END" {
			endSeq = r.Seq
			break
		}
	}
	if a, err = c.call(ctx, "/api/fork", map[string]any{"session": sid, "atSeq": endSeq}); err != nil {
		return err
	}
	if err := c.expect(a.OK && a.Child != "", "Fork at seq %d: child %s", endSeq, a.Child); err != nil {
		return err
	}
	child := a.Child
	if err := c.traceWant(ctx, a, placed(false), map[string]string{"ateapi": "CreateActor + ResumeActor", "worker": "RestoreWorkload"}); err != nil {
		return err
	}
	if ex, _, err = c.exec(ctx, child, "actually, the printer is on fire"); err != nil {
		return err
	}
	if err := c.expect(ex.OK, "Exec on the child"); err != nil {
		return err
	}
	// Fork resumed the child's actor before handing it out, so its first turn attaches too.
	if err := c.traceWant(ctx, ex, attached, map[string]string{"ateapi": "GetActor"}); err != nil {
		return err
	}
	if c.fake {
		if err := c.expect(strings.Contains(ex.Output, "1) my laptop will not boot; 2) actually, the printer is on fire"), "the child holds turn 1 and its own turn only"); err != nil {
			return err
		}
	}

	c.step("the fidelity refusal: the memory-tier harness on a VOLUMES template")
	if a, err = c.call(ctx, "/api/create", map[string]string{"lane": "volumes", "name": "fidelity-check"}); err != nil {
		return err
	}
	if err := c.expect(a.OK, "CreateSession on agentsessionsd-volumes: %s", a.Session); err != nil {
		return err
	}
	vid := a.Session
	if ex, _, err = c.exec(ctx, vid, "remember the codename heron"); err != nil {
		return err
	}
	if err := c.expect(ex.OK, "a first turn places the actor (its golden snapshot is MEMORY)"); err != nil {
		return err
	}
	start := time.Now()
	if a, err = c.call(ctx, "/api/suspend", map[string]string{"session": vid}); err != nil {
		return err
	}
	took := time.Since(start)
	r := a.Refusal
	if err := c.expect(!a.OK && a.Code == "FailedPrecondition" && r != nil, "Suspend refused with FailedPrecondition in %s", took.Round(time.Millisecond)); err != nil {
		return err
	}
	if err := c.expect(r.Session == vid && r.Actor == cfg.Atespace+"/"+vid && r.Required == "MEMORY" && r.Observed == "VOLUMES" && r.Suspended,
		"the refusal names session %s, actor %s, required %s, observed %s, actor left suspended", r.Session, r.Actor, r.Required, r.Observed); err != nil {
		return err
	}
	if err := c.trace(ctx, a, "daemon", "ateapi", "worker", "actor"); err != nil {
		return err
	}
	if av, err = c.actor(ctx, vid); err != nil {
		return err
	}
	if err := c.expect(av.State == "SUSPENDED" && len(av.Snapshots) > 0 && av.Snapshots[0].Fidelity == "VOLUMES", "ate-api: the actor is SUSPENDED holding a VOLUMES snapshot"); err != nil {
		return err
	}

	fmt.Printf("\npage URLs:\nstateless session: %s/?session=%s\nfork child:        %s/?session=%s\nrefused session:   %s/?session=%s\n",
		c.base, sid, c.base, child, c.base, vid)
	if len(c.unseen) > 0 {
		return fmt.Errorf("CHECK FAILED: every call worked, but %d hops did not show what the call did:\n  %s", len(c.unseen), strings.Join(c.unseen, "\n  "))
	}
	fmt.Println("CHECK OK")
	return nil
}
