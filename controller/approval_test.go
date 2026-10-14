package controller

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/aramase/agentsessions/api"
	"github.com/aramase/agentsessions/eventlog"
	"github.com/aramase/agentsessions/sqlitelog"
)

func gateEvents() []api.Event {
	n := int64(0)
	return []api.Event{
		{ExecutionID: "e", Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{InputCount: &n}},
		{ExecutionID: "e", Kind: api.EventToolCall, ToolCall: &api.ToolCall{ID: "c", Tool: "write", IdempotencyKey: "k", Mediation: api.MediationRequiresApproval}},
		{ExecutionID: "e", Kind: api.EventApprovalRequest, Approval: &api.ApprovalRequest{ToolCallID: "c"}},
		{ExecutionID: "e", Kind: api.EventApprovalResult, ApprovalResult: &api.ApprovalResult{ToolCallID: "c", RequestSeq: 3, Approved: true}},
		{ExecutionID: "e", Kind: api.EventToolResult, Result: &api.ToolResult{ID: "c", ApprovalRequestSeq: 3, ApprovalDecisionSeq: 4}},
		{ExecutionID: "e", Kind: api.EventEnd, End: &api.HarnessEnd{State: "COMPLETED"}},
	}
}
func gateAppend(t *testing.T, log eventlog.Store, events ...api.Event) {
	t.Helper()
	for _, ev := range events {
		head, err := log.Head()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := log.Append(head, 0, ev); err != nil {
			t.Fatal(err)
		}
	}
}
func gateLog(t *testing.T, events ...api.Event) eventlog.Store {
	t.Helper()
	log := eventlog.AsStore(eventlog.New())
	gateAppend(t, log, events...)
	return log
}

type approvalHarness struct {
	runs, describes int
	fn              func(context.Context, *api.Start, api.EventSink) error
}

func (h *approvalHarness) Describe(context.Context) (api.Descriptor, error) {
	h.describes++
	return api.Descriptor{ID: "gate", Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}}, nil
}
func (h *approvalHarness) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	h.runs++
	if h.fn != nil {
		return h.fn(ctx, start, sink)
	}
	return nil
}
func TestApprovalPublicStaging(t *testing.T) {
	for cut := 2; cut <= 6; cut++ {
		for _, entry := range []string{"resume", "replay"} {
			t.Run(entry+string(rune('0'+cut)), func(t *testing.T) {
				log := gateLog(t, gateEvents()[:cut]...)
				c, err := New(log, nil, WithSessionUID("s"))
				if err != nil {
					t.Fatal(err)
				}
				before, _ := log.Read(1)
				h := &approvalHarness{}
				if entry == "resume" {
					_, err = c.Resume(t.Context(), h)
				} else {
					_, err = c.Replay(t.Context(), h)
				}
				if !errors.Is(err, ErrApprovalUnavailable) {
					t.Fatalf("recorded gate must remain unavailable: %v", err)
				}
				after, _ := log.Read(1)
				if h.runs != 0 || h.describes != 0 || !reflect.DeepEqual(before, after) {
					t.Fatal("disabled recovery performed IO/repair")
				}
				if controllerErrorKind(err) != "approval_unavailable" {
					t.Fatalf("unhelpful code: %s", controllerErrorKind(err))
				}
			})
		}
	}
	for _, tc := range []struct {
		name   string
		events []api.Event
		exec   bool
		want   error
	}{
		{"owned pending blocks new input", gateEvents()[:3], true, ErrApprovalBlocked},
		{"ordinary marked receipt blocks recovery", []api.Event{{ExecutionID: "e", Kind: api.EventToolResult, Result: &api.ToolResult{ID: "ordinary", ApprovalRequestSeq: 9}}}, false, ErrApprovalUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := gateLog(t, tc.events...)
			c, err := New(log, nil, WithSessionUID("s"))
			if err != nil {
				t.Fatal(err)
			}
			before, err := log.Read(1)
			if err != nil {
				t.Fatal(err)
			}
			h := &approvalHarness{}
			if tc.exec {
				err = c.Exec(t.Context(), h, []api.Message{*api.TextMessage("user", "new")}, int64(len(before)))
			} else {
				_, err = c.Resume(t.Context(), h)
			}
			after, readErr := log.Read(1)
			if !errors.Is(err, tc.want) || readErr != nil || h.runs != 0 || h.describes != 0 || !reflect.DeepEqual(before, after) {
				t.Fatalf("staging admitted effects: err=%v read=%v describe=%d run=%d", err, readErr, h.describes, h.runs)
			}
			if tc.exec && controllerErrorKind(err) != "approval_blocked" {
				t.Fatalf("blocked kind=%s", controllerErrorKind(err))
			}
		})
	}
	t.Run("live pre-intent fallback", func(t *testing.T) {
		log := gateLog(t)
		effects := 0
		c, err := New(log, nil, WithSessionUID("s"), WithToolExecutor(func(context.Context, ToolCallContext, api.ToolCall) (api.ToolResult, error) {
			effects++
			return api.ToolResult{}, nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		h := &approvalHarness{fn: func(ctx context.Context, _ *api.Start, s api.EventSink) error {
			_, err := s.ToolCall(ctx, *gateEvents()[1].ToolCall)
			if !errors.Is(err, ErrApprovalUnavailable) {
				t.Fatalf("live gate: %v", err)
			}
			call := *gateEvents()[1].ToolCall
			call.ID = "fallback"
			call.Mediation = api.MediationControllerMediated
			_, err = s.ToolCall(ctx, call)
			return err
		}}
		if err := c.Exec(t.Context(), h, nil, 0); err != nil {
			t.Fatal(err)
		}
		if effects != 1 {
			t.Fatalf("effects %d", effects)
		}
		recs, _ := log.Read(1)
		for _, r := range recs {
			if r.Event.Kind == api.EventApprovalRequest || r.Event.ToolCall != nil && r.Event.ToolCall.Mediation == api.MediationRequiresApproval {
				t.Fatal("gate intent committed")
			}
		}
		if _, err := c.Replay(t.Context(), h); err != nil {
			t.Fatal(err)
		}
	})
}

func TestApprovalScannerAndSQLOracle(t *testing.T) {
	base := gateEvents()
	for _, tc := range []struct {
		name    string
		events  []api.Event
		invalid bool
	}{
		{"call", base[:2], false}, {"request", base[:3], false}, {"decision", base[:4], false}, {"receipt", base[:5], false}, {"complete", base, false},
		{"second request", append(append([]api.Event{}, base[:5]...), api.Event{ExecutionID: "e", Kind: api.EventToolCall, ToolCall: &api.ToolCall{ID: "second", IdempotencyKey: "k2", Mediation: api.MediationRequiresApproval}}, api.Event{ExecutionID: "e", Kind: api.EventApprovalRequest, Approval: &api.ApprovalRequest{ToolCallID: "second"}}), false},
		{"duplicate request", append(append([]api.Event{}, base[:3]...), base[2]), true},
		{"continue open", append(append([]api.Event{}, base[:3]...), base[5]), true},
		{"unbound decision", []api.Event{base[0], base[3]}, true},
		{"unbound receipt", []api.Event{base[0], base[4]}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := sqlitelog.Open(filepath.Join(t.TempDir(), "journal.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			log := store.Session("s")
			gateAppend(t, log, tc.events...)
			state, strictErr := InspectApproval(log, 0)
			if (strictErr != nil) != tc.invalid {
				t.Fatalf("strict oracle: state=%+v err=%v", state, strictErr)
			}
			info, err := store.SessionInfo("s")
			if err != nil {
				t.Fatal(err)
			}
			want := api.ApprovalRef{}
			if strictErr == nil && state != nil && !state.Inherited && !state.Completed && state.Request != nil && state.Decision == nil {
				want = api.ApprovalRef{ExecutionID: state.ExecutionID, ToolCallID: state.Call.ID, RequestSeq: state.Request.Seq}
			}
			if info.PendingApproval != want {
				t.Fatalf("projection=%+v strict=%+v", info.PendingApproval, want)
			}
		})
	}
	for _, status := range []api.ToolResultCode{api.ToolResultCodeApprovalDenied, api.ToolResultCodeExecutorError} {
		t.Run(string(status), func(t *testing.T) {
			b := gateEvents()
			b[3].ApprovalResult.Approved = status != api.ToolResultCodeApprovalDenied
			b[4].Result.Code = status
			b[4].Result.IsError = true
			b = append(b[:5], api.Event{ExecutionID: "e", Kind: api.EventToolCall, ToolCall: &api.ToolCall{ID: "second", IdempotencyKey: "k2", Mediation: api.MediationRequiresApproval}}, api.Event{ExecutionID: "e", Kind: api.EventApprovalRequest, Approval: &api.ApprovalRequest{ToolCallID: "second"}})
			s, err := sqlitelog.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			gateAppend(t, s.Session("s"), b...)
			state, err := InspectApproval(s.Session("s"), 0)
			if err != nil || state.Call.ID != "second" {
				t.Fatalf("strict=%+v %v", state, err)
			}
			info, err := s.SessionInfo("s")
			if err != nil || info.PendingApproval != (api.ApprovalRef{ExecutionID: "e", ToolCallID: "second", RequestSeq: 7}) {
				t.Fatalf("projection=%+v %v", info, err)
			}
		})
	}
	for _, change := range []struct {
		name   string
		mutate func([]api.Event)
	}{
		{"empty key", func(b []api.Event) { b[1].ToolCall.IdempotencyKey = "" }},
		{"empty id", func(b []api.Event) { b[1].ToolCall.ID = "" }},
		{"wrong request", func(b []api.Event) { b[2].Approval.ToolCallID = "wrong" }},
		{"wrong decision", func(b []api.Event) { b[3].ApprovalResult.RequestSeq = 2 }},
		{"wrong receipt id", func(b []api.Event) { b[4].Result.ID = "wrong" }},
		{"wrong receipt seq", func(b []api.Event) { b[4].Result.ApprovalDecisionSeq = 2 }},
		{"wrong status", func(b []api.Event) { b[4].Result.IsError = true }},
	} {
		t.Run(change.name, func(t *testing.T) {
			b := gateEvents()
			change.mutate(b)
			if _, err := InspectApproval(gateLog(t, b...), 0); err == nil {
				t.Fatal("bad evidence accepted")
			}
		})
	}
}

func TestApprovalSelectionDecisionAndOwnership(t *testing.T) {
	log := gateLog(t, gateEvents()[:3]...)
	dec := api.ApprovalDecision{ExecutionID: "e", ToolCallID: "c", RequestSeq: 3, Approved: true, Reason: "reviewed", Identity: api.IdentityRef{}}
	r, err := Approve(log, dec)
	if err != nil {
		t.Fatal(err)
	}
	// A retained writer is not evicted by an exact retry, even after receipt/newer execution.
	c, err := New(log, nil)
	if err != nil {
		t.Fatal(err)
	}
	receipt := gateEvents()[4]
	receipt.Result.ApprovalDecisionSeq = r.Seq
	if _, err := log.Append(r.Seq, c.fence, receipt); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(r.Seq+1, c.fence, api.Event{ExecutionID: "new", Kind: api.EventOutput, Message: api.TextMessage("assistant", "new")}); err != nil {
		t.Fatal(err)
	}
	retry, err := Approve(log, dec)
	if err != nil || retry.Seq != r.Seq {
		t.Fatalf("retry=%+v %v", retry, err)
	}
	dec.Reason = "changed"
	if _, err := Approve(log, dec); !errors.Is(err, ErrInvalidApprovalDecision) {
		t.Fatal(err)
	}
	if _, err := log.Append(r.Seq+2, c.fence, api.Event{ExecutionID: "e", Kind: api.EventError}); err != nil {
		t.Fatalf("retry/invalid fenced writer: %v", err)
	}
	if state, err := InspectApproval(log, 3); err != nil || state.Request.Seq != 3 || state.Head != 3 || state.Decision != nil {
		t.Fatalf("captured state=%+v %v", state, err)
	}
	if state, err := InspectApproval(log, 0); err != nil || state != nil {
		t.Fatalf("late old record won selection: %+v %v", state, err)
	}
	for _, cut := range []int{2, 3, 4, 5} {
		t.Run(string(rune('0'+cut)), func(t *testing.T) {
			parent := gateLog(t, gateEvents()[:cut]...)
			child := gateLog(t)
			if err := Fork(parent, child, int64(cut)); err != nil {
				t.Fatal(err)
			}
			state, err := InspectApproval(child, 0)
			if err != nil || !state.Inherited {
				t.Fatalf("ownership=%+v %v", state, err)
			}
			if _, err := Approve(child, api.ApprovalDecision{ExecutionID: "e", ToolCallID: "c", RequestSeq: 3}); !errors.Is(err, ErrInvalidApprovalDecision) {
				t.Fatalf("inherited decision=%v", err)
			}
			if cut == 4 {
				late := gateEvents()[4]
				head, _ := child.Head()
				if _, err := child.Append(head, 1, late); err != nil {
					t.Fatal(err)
				}
				state, err = InspectApproval(child, 0)
				if err != nil || state.Receipt == nil || !state.Inherited {
					t.Fatalf("late inherited receipt=%+v %v", state, err)
				}
			}
		})
	}
}

func gateLive(c *Controller) *liveSink {
	return &liveSink{c: c, executionID: "e", guard: &sinkGuard{}, desc: &api.Descriptor{Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}}, calls: make(map[string]bool)}
}
func allGateMethods(ctx context.Context, s api.EventSink) []error {
	_, m := s.Model(ctx, api.ModelRequest{})
	_, c := s.ToolCall(ctx, *gateEvents()[1].ToolCall)
	return []error{m, c, s.Output(ctx, "late"), s.Report(ctx, api.ToolResult{ID: "c"}), s.Usage(ctx, api.Usage{})}
}
func TestApprovalPrivateParkAndRecovery(t *testing.T) {
	log := gateLog(t, gateEvents()[:1]...)
	c, err := New(log, nil, WithSessionUID("s"))
	if err != nil {
		t.Fatal(err)
	}
	s := gateLive(c)
	_, err = s.parkTool(t.Context(), *gateEvents()[1].ToolCall)
	if !errors.Is(err, api.ErrApprovalParked) {
		t.Fatal(err)
	}
	for _, late := range allGateMethods(t.Context(), s) {
		if !errors.Is(late, api.ErrApprovalParked) {
			t.Fatalf("park not sticky: %v", late)
		}
	}
	s.guard.close()
	for _, late := range allGateMethods(t.Context(), s) {
		if !errors.Is(late, api.ErrApprovalParked) {
			t.Fatal(late)
		}
	}
	if head, _ := log.Head(); head != 3 {
		t.Fatalf("late effect appended %d", head)
	}
	for _, approved := range []bool{true, false} {
		t.Run(map[bool]string{true: "executor error", false: "denied"}[approved], func(t *testing.T) {
			log := gateLog(t, gateEvents()[:3]...)
			_, err := Approve(log, api.ApprovalDecision{ExecutionID: "e", ToolCallID: "c", RequestSeq: 3, Approved: approved})
			if err != nil {
				t.Fatal(err)
			}
			effects := 0
			c, err := New(log, func(context.Context, api.ModelRequest) (api.ModelResponse, error) {
				return api.ModelResponse{Message: *api.TextMessage("assistant", "continued")}, nil
			}, WithSessionUID("s"), WithToolExecutor(func(context.Context, ToolCallContext, api.ToolCall) (api.ToolResult, error) {
				effects++
				return api.ToolResult{}, errors.New("offline")
			}))
			if err != nil {
				t.Fatal(err)
			}
			scan, err := scanApprovals(log, 0)
			if err != nil {
				t.Fatal(err)
			}
			events := []api.Event{}
			recs, _ := log.Read(1)
			for _, r := range recs {
				events = append(events, r.Event)
			}
			execs, err := recordedExecutions(events)
			if err != nil {
				t.Fatal(err)
			}
			sink := &resumeSink{live: *gateLive(c), stream: execs[0].stream, approvals: approvalEvidence(scan, "e")}
			result, err := sink.ToolCall(t.Context(), *gateEvents()[1].ToolCall)
			if err != nil {
				t.Fatal(err)
			}
			want := api.ToolResultCodeApprovalDenied
			if approved {
				want = api.ToolResultCodeExecutorError
			}
			if result.Code != want || !result.IsError || result.Approval == nil || result.ApprovalDecisionSeq != 4 {
				t.Fatalf("receipt=%+v", result)
			}
			if effects != map[bool]int{true: 1, false: 0}[approved] {
				t.Fatal("wrong effect count")
			}
			if _, err := sink.Model(t.Context(), api.ModelRequest{}); err != nil {
				t.Fatalf("receipt could not continue: %v", err)
			}
			recs, _ = log.Read(1)
			if recs[4].Event.Result.Approval != nil {
				t.Fatal("transient authority persisted")
			}
			state, err := InspectApproval(log, 0)
			if err != nil {
				t.Fatal(err)
			}
			served, err := serveApproval(state, state.Receipt.Event.Result)
			if err != nil || served.Code != want {
				t.Fatalf("serve=%+v %v", served, err)
			}
		})
	}
}

func TestApprovalMismatchAndClosureSeal(t *testing.T) {
	for _, method := range []string{"tool", "model", "output", "report", "usage"} {
		t.Run(method, func(t *testing.T) {
			log := gateLog(t, gateEvents()...)
			scan, err := scanApprovals(log, 0)
			if err != nil {
				t.Fatal(err)
			}
			b := gateEvents()[1:5]
			c, err := New(log, nil)
			if err != nil {
				t.Fatal(err)
			}
			rs := &resumeSink{live: *gateLive(c), stream: b, approvals: approvalEvidence(scan, "e")}
			ps := &replaySink{stream: b, guard: &sinkGuard{}, approvals: approvalEvidence(scan, "e"), calls: make(map[string]bool)}
			for _, s := range []api.EventSink{rs, ps} {
				switch method {
				case "tool":
					call := *b[0].ToolCall
					call.IdempotencyKey = "changed"
					_, err = s.ToolCall(t.Context(), call)
				case "model":
					_, err = s.Model(t.Context(), api.ModelRequest{})
				case "output":
					err = s.Output(t.Context(), "wrong")
				case "report":
					err = s.Report(t.Context(), api.ToolResult{})
				case "usage":
					err = s.Usage(t.Context(), api.Usage{})
				}
				if !errors.Is(err, ErrReplayDiverged) {
					t.Fatalf("mismatch=%v", err)
				}
				for _, late := range allGateMethods(t.Context(), s) {
					if !errors.Is(late, ErrReplayDiverged) {
						t.Fatalf("mismatch unlocked method: %v", late)
					}
				}
			}
		})
	}
	c, err := New(gateLog(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	s := gateLive(c)
	s.guard.close()
	for _, err := range allGateMethods(t.Context(), s) {
		if !errors.Is(err, ErrSinkClosed) {
			t.Fatal(err)
		}
	}
}

type receiptFault struct {
	eventlog.Store
	fail bool
}

func (s *receiptFault) Append(head, fence int64, ev api.Event) (eventlog.Record, error) {
	if s.fail && ev.Kind == api.EventToolResult {
		return eventlog.Record{}, errors.New("receipt lost")
	}
	return s.Store.Append(head, fence, ev)
}
func TestApprovalDurableEffectBeforeReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "effects.db")
	open := func() *sql.DB {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		return db
	}
	db := open()
	defer func() { db.Close() }()
	if _, err := db.Exec("CREATE TABLE effects(session TEXT,key TEXT,PRIMARY KEY(session,key))"); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	executor := func(_ context.Context, scope ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
		attempts++
		if (scope.SessionUID != "a" && scope.SessionUID != "b") || call.ID != "c" || call.IdempotencyKey != "k" || call.Mediation != api.MediationRequiresApproval {
			t.Fatalf("lost original scope/call: %+v %+v", scope, call)
		}
		_, err := db.Exec("INSERT INTO effects VALUES(?,?) ON CONFLICT DO NOTHING", scope.SessionUID, call.IdempotencyKey)
		return api.ToolResult{}, err
	}
	for _, session := range []string{"a", "b"} {
		journalPath := filepath.Join(t.TempDir(), "journal.db")
		journal, err := sqlitelog.Open(journalPath)
		if err != nil {
			t.Fatal(err)
		}
		log := journal.Session(session)
		gateAppend(t, log, gateEvents()[:4]...)
		fault := &receiptFault{Store: log, fail: true}
		c, err := New(fault, nil, WithSessionUID(session), WithToolExecutor(executor))
		if err != nil {
			t.Fatal(err)
		}
		state, err := InspectApproval(log, 0)
		if err != nil {
			t.Fatal(err)
		}
		s := gateLive(c)
		if _, err := s.completeApproval(t.Context(), state); err == nil {
			t.Fatal("receipt fault missed")
		}
		for _, err := range allGateMethods(t.Context(), s) {
			if err == nil {
				t.Fatal("uncertain effect unlocked sink")
			}
		}
		db.Close()
		db = open()
		journal.Close()
		journal, err = sqlitelog.Open(journalPath)
		if err != nil {
			t.Fatal(err)
		}
		fault.Store = journal.Session(session)
		fault.fail = false
		c, err = New(fault, nil, WithSessionUID(session), WithToolExecutor(executor))
		if err != nil {
			t.Fatal(err)
		}
		state, err = InspectApproval(fault.Store, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := gateLive(c).completeApproval(t.Context(), state); err != nil {
			t.Fatal(err)
		}
		journal.Close()
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM effects").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 || attempts != 4 {
		t.Fatalf("durable scoped effects=%d attempts=%d", count, attempts)
	}
}

func TestApprovalPrivateRepairCapturedHeadFenceAndObserver(t *testing.T) {
	for _, mode := range []string{"repair", "CAS", "fenced", "inherited", "observer"} {
		t.Run(mode, func(t *testing.T) {
			log := gateLog(t, gateEvents()[:2]...)
			observed := 0
			c, err := New(log, nil, WithObserver(Observer{OnRecord: func(r eventlog.Record) {
				observed++
				if r.Seq != 3 || r.Event.Kind != api.EventApprovalRequest {
					t.Fatal(r)
				}
			}}))
			if err != nil {
				t.Fatal(err)
			}
			scan, err := scanApprovals(log, 0)
			if err != nil {
				t.Fatal(err)
			}
			want := api.ErrApprovalParked
			switch mode {
			case "CAS":
				if _, err := log.Append(2, c.fence, api.Event{ExecutionID: "e", Kind: api.EventError}); err != nil {
					t.Fatal(err)
				}
				want = eventlog.ErrConflict
			case "fenced":
				if _, err := log.NewFence(); err != nil {
					t.Fatal(err)
				}
				want = eventlog.ErrFenced
			case "inherited":
				scan.selected.Inherited = true
				want = ErrInheritedToolIntent
			case "observer":
				c.observer.OnRecord = func(eventlog.Record) { observed++ }
			}
			_, err = c.pendingApproval(scan, "e")
			if !errors.Is(err, want) {
				t.Fatalf("repair=%v want=%v", err, want)
			}
			wantObserved := 0
			if mode == "repair" || mode == "observer" {
				wantObserved = 1
			}
			if observed != wantObserved {
				t.Fatalf("observer=%d", observed)
			}
			if wantObserved == 1 {
				scan, err = scanApprovals(log, 0)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = c.pendingApproval(scan, "e"); !errors.Is(err, api.ErrApprovalParked) {
					t.Fatal(err)
				}
				if observed != 1 {
					t.Fatal("request repaired twice")
				}
			}
		})
	}
}
func TestApprovalInheritedReceiptAndReportAuthority(t *testing.T) {
	parent := gateLog(t, gateEvents()[:4]...)
	child := gateLog(t)
	if err := Fork(parent, child, 4); err != nil {
		t.Fatal(err)
	}
	head, _ := child.Head()
	if _, err := child.Append(head, 1, gateEvents()[4]); err != nil {
		t.Fatal(err)
	}
	state, err := InspectApproval(child, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := serveApproval(state, state.Receipt.Event.Result); err != nil || result.ID != "c" {
		t.Fatalf("inherited receipt=%+v %v", result, err)
	}
	for _, r := range []api.ToolResult{{Approval: &api.ApprovalResult{}}, {ApprovalRequestSeq: 3}, {ApprovalDecisionSeq: 4}, {Code: api.ToolResultCodeApprovalDenied}} {
		if err := approvalReportError(r); !errors.Is(err, ErrApprovalReceiptReport) {
			t.Fatal(err)
		}
	}
}
