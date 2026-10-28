package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/model/openai"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/substrate"
)

// recordView is one journal record as the page lists it.
type recordView struct {
	Seq       int64  `json:"seq"`
	Kind      string `json:"kind"`
	Text      string `json:"text,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Hash      string `json:"hash"`
	Prev      string `json:"prev"`
	Execution string `json:"execution,omitempty"`
}

func viewRecord(r *v1.LogRecord) recordView {
	ev := r.GetEvent()
	v := recordView{
		Seq:       r.GetSeq(),
		Kind:      strings.TrimPrefix(ev.GetKind().String(), "EVENT_"),
		Hash:      shortHash(r.GetContentHash()),
		Prev:      shortHash(r.GetPrevHash()),
		Execution: ev.GetExecutionId(),
	}
	if m := ev.GetMessage(); m != nil {
		var parts []string
		for _, p := range m.GetParts() {
			if t := p.GetText(); t != nil {
				parts = append(parts, t.GetText())
			}
		}
		v.Text = strings.Join(parts, " ")
		v.Detail = m.GetRole()
	}
	switch {
	case ev.GetModel() != nil:
		v.Detail = "model " + ev.GetModel().GetModel()
	case ev.GetUsage() != nil:
		u := ev.GetUsage()
		v.Detail = fmt.Sprintf("%d in, %d out tokens", u.GetInputTokens(), u.GetOutputTokens())
	case ev.GetLifecycle() != nil:
		v.Detail = strings.TrimPrefix(ev.GetLifecycle().GetKind().String(), "LIFECYCLE_")
	case ev.GetExecutionStart() != nil:
		v.Detail = "harness " + ev.GetExecutionStart().GetHarness()
	case ev.GetEnd() != nil:
		v.Detail = ev.GetEnd().GetState()
	case ev.GetError() != nil:
		v.Detail = ev.GetError().GetDescription()
	}
	return v
}

// shortHash keeps a hash readable: its first 12 hex after any "sha256:" prefix, "-" when empty.
func shortHash(h string) string {
	h = strings.TrimPrefix(h, "sha256:")
	if h == "" {
		return "-"
	}
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

type journal struct {
	Session  string       `json:"session"`
	Records  []recordView `json:"records"`
	Verified bool         `json:"verified"`
	Error    string       `json:"error,omitempty"`
}

// journalView lists a session's records and checks the hash chain here, on the client, as
// cmd/replay does: the page does not take the daemon's word for it.
func journalView(uid string, protos []*v1.LogRecord) journal {
	j := journal{Session: uid, Records: make([]recordView, 0, len(protos))}
	recs := make([]eventlog.Record, 0, len(protos))
	for _, p := range protos {
		j.Records = append(j.Records, viewRecord(p))
		recs = append(recs, eventlog.RecordFromProto(p))
	}
	if len(recs) == 0 {
		return j
	}
	if err := eventlog.NewFrom(recs).Verify(); err != nil {
		j.Error = err.Error()
		return j
	}
	j.Verified = true
	return j
}

// replayResult is what a replay showed: the turns it re-ran, the outputs, and the model calls.
// It exists once the journal was read and verified; a replay that fails later keeps what it
// established so far, and the activity's ok, not this, says whether the replay held.
type replayResult struct {
	Records int      `json:"records"`
	Turns   int      `json:"turns"`
	Outputs []string `json:"outputs"`
	// ControllerCalls is the controller's model count; -1 until its replay ran.
	ControllerCalls int `json:"controllerCalls"`
	// FakeBefore and FakeAfter are the fake model's counter around the replay; -1 when there is
	// no fake (a real provider; the controller's count is the proof) or the counter was not read.
	// Every daemon calls the same fake model, so the counter also counts other sessions' turns.
	FakeBefore int64 `json:"fakeBefore"`
	FakeAfter  int64 `json:"fakeAfter"`
	// ModelCalled is set when the controller counted a model call. A move of the shared fake
	// counter alone fails the replay but is not attributed to it (counterMoved).
	ModelCalled bool   `json:"modelCalled,omitempty"`
	Head        string `json:"head"`
}

func (s *server) handleReplay(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Session string `json:"session"`
	}
	if !decode(w, r, &body) {
		return
	}
	if s.cfg.Router == "" {
		writeError(w, errBadRequest("replay is off: start the page with -router (ui.sh forwards atenet-router)"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	l, err := s.laneOf(ctx, body.Session)
	if err != nil {
		writeError(w, err)
		return
	}
	if l.Tier != "STATELESS_REPLAY" {
		a := s.begin("replay", l, body.Session)
		a.FailedAt, a.Declined = "bridge", true
		writeJSON(w, http.StatusOK, s.finish(a, fmt.Errorf("replay re-runs turns on the session's live actor, which only the stateless tier supports (a memory-tier harness keeps its conversation in RAM)")))
		return
	}
	// The replay's turns reach the actor through atenet-router, which resumes a suspended actor on
	// any routed request (substrate v0.6.0, cmd/atenet/internal/router/ingress). On a session that
	// is not LIVE that would wake the actor outside Sessions, which would still report it suspended
	// with no Resume journaled. This is a read before the call, not a guard: a Suspend from another
	// client between the read and the routed turns is not caught.
	sess, err := l.c.GetSession(ctx, body.Session)
	if err != nil {
		writeError(w, err)
		return
	}
	a := s.begin("replay", l, body.Session)
	if err := replayableCompute(sess.GetComputeState()); err != nil {
		a.FailedAt, a.Declined = "bridge", true
		a.After = viewSession(l, sess)
		writeJSON(w, http.StatusOK, s.finish(a, err))
		return
	}
	// The daemon's part is the Replay RPC; everything after it is the bridge's.
	protos, err := l.c.Replay(ctx, body.Session, 1, 0)
	if err == nil {
		a.Replay, err = s.replay(ctx, body.Session, protos)
		if err != nil {
			a.FailedAt = "bridge"
		}
	}
	a.After = s.current(ctx, l, body.Session)
	writeJSON(w, http.StatusOK, s.finish(a, err))
}

// replayableCompute declines a replay unless Sessions reports the session's compute LIVE: the
// replay drives the running actor, and only Sessions.Resume or a turn may bring it back.
func replayableCompute(cs v1.ComputeState) error {
	switch cs {
	case v1.ComputeState_COMPUTE_LIVE:
		return nil
	case v1.ComputeState_COMPUTE_COLD:
		return fmt.Errorf("the session is suspended (COMPUTE_COLD): replay re-runs turns on the running actor, " +
			"and routing them would wake the actor outside Sessions; Resume the session first")
	}
	return fmt.Errorf("the session's compute is %s, not COMPUTE_LIVE: replay re-runs turns on the running actor; "+
		"run a turn or Resume the session first", cs)
}

// replay is cmd/replay's run, in process, on the journal the Replay RPC returned: verify the chain,
// and run the controller's replay against the session's own actor through the atenet-router, with
// every model call served from the journal. It fails if the controller counted a model call or the
// shared fake model's counter moved; only the controller's count sets ModelCalled. Once the journal
// verifies it returns a result with any error, holding what was established before the failure.
func (s *server) replay(ctx context.Context, uid string, protos []*v1.LogRecord) (
	*replayResult, error,
) {
	log, harness, turns, err := loadJournal(uid, protos)
	if err != nil {
		return nil, err
	}
	res := &replayResult{Records: len(protos), Turns: turns, ControllerCalls: -1,
		FakeBefore: -1, FakeAfter: -1, Head: shortHash(protos[len(protos)-1].GetContentHash())}
	before, err := s.modelCalls(ctx)
	if err != nil {
		return res, err
	}
	res.FakeBefore = before
	model, err := openai.New(openai.WithModel("replay-must-not-call"), openai.WithBaseURL(s.cfg.ModelURL))
	if err != nil {
		return res, err
	}
	opts := []controller.Option{controller.WithSessionUID(uid)}
	if harness != "" {
		opts = append(opts, controller.WithHarness(harness))
	}
	ctl, err := controller.New(eventlog.AsStore(log), model.Model, opts...)
	if err != nil {
		return res, err
	}
	h, closeHarness, err := placement.DefaultDial(api.Incarnation{
		ID:           uid,
		Address:      s.cfg.Router,
		CallMetadata: map[string]string{substrate.TargetActorHeader: s.cfg.Atespace + "/" + uid},
	})
	if err != nil {
		return res, fmt.Errorf("dial the actor through the router: %w", err)
	}
	defer func() { _ = closeHarness() }()
	outputs, err := ctl.Replay(ctx, h)
	res.ControllerCalls = ctl.ModelInvocations()
	res.ModelCalled = res.ControllerCalls > 0
	if err != nil {
		return res, fmt.Errorf("replay: %w", err)
	}
	res.Outputs = outputs
	after, err := s.modelCalls(ctx)
	if err != nil {
		return res, err
	}
	res.FakeAfter = after
	if res.ModelCalled {
		return res, fmt.Errorf("replay called the model")
	}
	// The controller counted no call, but the counter is shared: a turn from another tab or
	// `ui.sh check` moves it too. Fail closed without blaming the replay.
	if res.counterMoved() {
		return res, fmt.Errorf("the fake model's counter moved %d -> %d during the replay while the controller counted "+
			"no call; the daemons share that counter, so the move cannot be attributed to the replay and zero model "+
			"calls is not established; rerun with no other turns in flight", res.FakeBefore, res.FakeAfter)
	}
	if err := log.Verify(); err != nil {
		return res, fmt.Errorf("journal does not verify after replay: %w", err)
	}
	return res, nil
}

// counterMoved reports that the fake model's counter was read on both sides of the replay and moved.
func (r *replayResult) counterMoved() bool {
	return r.FakeBefore >= 0 && r.FakeAfter >= 0 && r.FakeAfter != r.FakeBefore
}

// loadJournal is cmd/replay's: a verified log, the harness the last completed turn ran on, and how
// many completed turns the controller will re-run. Nothing to re-run is an error, so a replay is
// never reported for turns that did not run.
func loadJournal(uid string, protos []*v1.LogRecord) (*eventlog.Log, string, int, error) {
	if len(protos) == 0 {
		return nil, "", 0, fmt.Errorf("session %s has no journal records: nothing to replay", uid)
	}
	recs := make([]eventlog.Record, 0, len(protos))
	started := map[string]string{}
	harness, turns := "", 0
	for _, p := range protos {
		r := eventlog.RecordFromProto(p)
		switch {
		case r.Event.Kind == api.EventExecutionStart && r.Event.ExecutionStart != nil:
			started[r.Event.ExecutionID] = r.Event.ExecutionStart.Harness
		case r.Event.Kind == api.EventEnd:
			turns++
			if started[r.Event.ExecutionID] != "" {
				harness = started[r.Event.ExecutionID]
			}
		}
		recs = append(recs, r)
	}
	log := eventlog.NewFrom(recs)
	if err := log.Verify(); err != nil {
		return nil, "", 0, fmt.Errorf("journal does not verify: %w", err)
	}
	if turns == 0 {
		return nil, "", 0, fmt.Errorf("session %s has no completed turn: nothing to re-execute", uid)
	}
	return log, harness, turns, nil
}

// modelCalls reads the fake model's counter, or -1 without one.
func (s *server) modelCalls(ctx context.Context) (int64, error) {
	if s.cfg.ModelStats == "" {
		return -1, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.ModelStats, nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("read fake model stats: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("read fake model stats: %s", resp.Status)
	}
	var st struct {
		Calls int64 `json:"calls"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return 0, fmt.Errorf("decode fake model stats: %w", err)
	}
	return st.Calls, nil
}
