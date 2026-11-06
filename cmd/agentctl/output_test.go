package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/sqlitelog"
)

type outputServer struct {
	v1.UnimplementedSessionsServer
	session      *v1.Session
	records      []*v1.LogRecord
	pages        chan *v1.ListSessionsRequest
	listContexts chan context.Context
	replays      chan *v1.ReplayRequest
	deletes      chan *v1.DeleteSessionRequest
	err          error
	empty        bool
}

func (s *outputServer) CreateSession(context.Context, *v1.CreateSessionRequest) (*v1.Session, error) {
	return s.session, s.err
}
func (s *outputServer) GetSession(context.Context, *v1.GetSessionRequest) (*v1.Session, error) {
	return s.session, s.err
}
func (s *outputServer) DeleteSession(_ context.Context, req *v1.DeleteSessionRequest) (*v1.Session, error) {
	s.deletes <- req
	return s.session, s.err
}
func (s *outputServer) Suspend(context.Context, *v1.SuspendRequest) (*v1.Session, error) {
	return s.session, s.err
}
func (s *outputServer) Resume(context.Context, *v1.ResumeRequest) (*v1.Session, error) {
	return s.session, s.err
}
func (s *outputServer) Fork(context.Context, *v1.ForkRequest) (*v1.ForkResponse, error) {
	return &v1.ForkResponse{Children: []*v1.Session{s.session}}, s.err
}
func (s *outputServer) ListSessions(ctx context.Context, req *v1.ListSessionsRequest) (*v1.ListSessionsResponse, error) {
	s.pages <- req
	s.listContexts <- ctx
	if s.err != nil {
		return nil, s.err
	}
	if s.empty {
		return &v1.ListSessionsResponse{}, nil
	}
	page := &v1.ListSessionsResponse{Sessions: []*v1.Session{s.session}}
	if req.GetPageToken() == "" {
		page.NextPageToken = "next"
	}
	return page, nil
}
func (s *outputServer) Replay(req *v1.ReplayRequest, stream v1.Sessions_ReplayServer) error {
	s.replays <- req
	for _, record := range s.records {
		if err := stream.Send(record); err != nil {
			return err
		}
	}
	return s.err
}

func startOutputServer(t *testing.T, s *outputServer) string {
	t.Helper()
	s.pages = make(chan *v1.ListSessionsRequest, 10)
	s.listContexts = make(chan context.Context, 10)
	s.replays = make(chan *v1.ReplayRequest, 10)
	s.deletes = make(chan *v1.DeleteSessionRequest, 10)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	v1.RegisterSessionsServer(srv, s)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func detailValue(output, label string) string {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, label+"  ") {
			return strings.TrimSpace(strings.TrimPrefix(line, label))
		}
	}
	return ""
}

func captureCLIOutput(t *testing.T, run func() error) (string, string, error) {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = write
	defer func() { os.Stderr = old; read.Close() }()
	out, runErr := captureApprovalOutput(t, run)
	write.Close()
	diagnostics, err := io.ReadAll(read)
	if err != nil {
		t.Fatal(err)
	}
	return out, string(diagnostics), runErr
}

func TestSessionJSON(t *testing.T) {
	sess := &v1.Session{
		Metadata: &v1.ResourceMetadata{Uid: "uid", Name: "named", Project: "project"},
		Harness:  "echo", ExecState: v1.ExecState_EXEC_AWAITING, ComputeState: v1.ComputeState_COMPUTE_COLD,
		DeletionState: v1.DeletionState_DELETION_ACTIVE, LastSeq: 9007199254740993,
		PendingApproval: &v1.ApprovalRef{ExecutionId: "e", ToolCallId: "c", RequestSeq: 9},
	}
	s := &outputServer{session: sess}
	server := startOutputServer(t, s)
	for _, tc := range []struct {
		name string
		run  func([]string) error
		want proto.Message
		got  proto.Message
	}{
		{"create", cmdCreate, sess, &v1.Session{}},
		{"get", cmdGet, sess, &v1.Session{}},
		{"delete", cmdDelete, sess, &v1.Session{}},
		{"suspend", cmdSuspend, sess, &v1.Session{}},
		{"resume", cmdResume, sess, &v1.Session{}},
		{"fork", cmdFork, &v1.ForkResponse{Children: []*v1.Session{sess}}, &v1.ForkResponse{}},
		{"list", cmdList, &v1.ListSessionsResponse{Sessions: []*v1.Session{sess, sess}}, &v1.ListSessionsResponse{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"--server", server, "--json"}
			if tc.name != "create" && tc.name != "list" {
				args = append(args, "--session", "uid")
			}
			if tc.name == "list" {
				args = append(args, "--page-size", "1")
			}
			out, err := captureApprovalOutput(t, func() error { return tc.run(args) })
			if err != nil {
				t.Fatal(err)
			}
			if err := protojson.Unmarshal([]byte(out), tc.got); err != nil || !proto.Equal(tc.got, tc.want) {
				t.Fatalf("JSON=%s err=%v want=%v", out, err, tc.want)
			}
			var body map[string]any
			if err := json.Unmarshal([]byte(out), &body); err != nil {
				t.Fatal(err)
			}
			fields := body
			switch tc.name {
			case "list":
				fields = body["sessions"].([]any)[0].(map[string]any)
			case "fork":
				fields = body["children"].([]any)[0].(map[string]any)
			}
			if fields["last_seq"] != "9007199254740993" || fields["exec_state"] != "EXEC_AWAITING" || fields["lastSeq"] != nil || fields["model"] != nil {
				t.Fatalf("wrong protobuf JSON field names or types: %v", fields)
			}
		})
	}
	for _, token := range []string{"", "next"} {
		req := <-s.pages
		if req.GetPageSize() != 1 || req.GetPageToken() != token {
			t.Fatalf("pagination request = %v", req)
		}
	}
	out, err := captureApprovalOutput(t, func() error { return cmdCreate([]string{"--server", server}) })
	if err != nil || out != "uid\n" {
		t.Fatalf("create UID contract: %q %v", out, err)
	}
}

func TestReplayJSONLines(t *testing.T) {
	records := []*v1.LogRecord{
		{Seq: 7, PrevHash: strings.Repeat("a", 64), ContentHash: strings.Repeat("b", 64), Fence: 9007199254740993,
			Event: &v1.Event{Kind: v1.EventKind_EVENT_APPROVAL_RESULT, Body: &v1.Event_ApprovalResult{ApprovalResult: &v1.ApprovalResult{}}}},
		{Seq: 8, PrevHash: strings.Repeat("b", 64), ContentHash: strings.Repeat("c", 64),
			Event: &v1.Event{Kind: v1.EventKind_EVENT_OUTPUT, Body: &v1.Event_Message{Message: &v1.Message{Role: "assistant", Parts: []*v1.Part{{Part: &v1.Part_Text{Text: &v1.TextPart{Text: "hello\n\u009b\u202e"}}}}}}}},
	}
	s := &outputServer{records: records}
	server := startOutputServer(t, s)
	args := []string{"--server", server, "--session", "uid", "--from", "7", "--json", "-v"}
	out, err := captureApprovalOutput(t, func() error { return cmdReplay(args) })
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != len(records) {
		t.Fatalf("not one record per line: %q", out)
	}
	for i, line := range lines {
		var got v1.LogRecord
		if err := protojson.Unmarshal([]byte(line), &got); err != nil || !proto.Equal(&got, records[i]) {
			t.Fatalf("record %d: %s err=%v", i, line, err)
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatal(err)
		}
		if fields["content_hash"] != records[i].GetContentHash() || fields["contentHash"] != nil {
			t.Fatalf("wrong replay field names: %v", fields)
		}
		assertOneSafeLine(t, line)
	}
	if req := <-s.replays; req.GetSession() != "uid" || req.GetFromSeq() != 7 {
		t.Fatalf("replay request = %v", req)
	}
	empty := startOutputServer(t, &outputServer{})
	out, err = captureApprovalOutput(t, func() error {
		return cmdReplay([]string{"--server", empty, "--session", "uid", "--json"})
	})
	if err != nil || out != "" {
		t.Fatalf("empty replay: %q %v", out, err)
	}
}

func TestHumanSessionOutput(t *testing.T) {
	sess := &v1.Session{
		Metadata: &v1.ResourceMetadata{Uid: "uid", Name: "name\n\x1b[8m", Project: "project"},
		Harness:  "echo", ExecState: v1.ExecState_EXEC_COMPLETED, ComputeState: v1.ComputeState_COMPUTE_LIVE,
		DeletionState: v1.DeletionState_DELETION_ACTIVE, LastSeq: 5,
	}
	details := sessionDetails(sess)
	for label, want := range map[string]string{
		"Session": "uid", "Name": `name\n\x1b[8m`, "Project": "project", "Harness": "echo",
		"Execution": "COMPLETED", "Compute": "LIVE", "Last seq": "5", "Deletion": "ACTIVE", "Deletion requested": "unset",
	} {
		if got := detailValue(details, label); got != want {
			t.Errorf("%s = %q, want %q", label, got, want)
		}
	}
	table := sessionTable([]*v1.Session{sess})
	lines := strings.Split(strings.TrimSuffix(table, "\n"), "\n")
	if len(lines) != 2 || strings.Contains(table, "\x1b") {
		t.Fatalf("unsafe table: %q", table)
	}
	for header, value := range map[string]string{
		"SESSION": "uid", "EXECUTION": "COMPLETED", "COMPUTE": "LIVE", "HARNESS": "echo", "LAST SEQ": "5",
	} {
		if strings.Index(lines[0], header) != strings.Index(lines[1], value) {
			t.Errorf("unaligned %s:\n%s", header, table)
		}
	}
	if !strings.Contains(table, `name\n\x1b[8m`) {
		t.Fatalf("missing escaped name: %q", table)
	}
	for _, state := range []string{"EXEC_STATE_UNSPECIFIED", "COMPUTE_STATE_UNSPECIFIED", "DELETION_STATE_UNSPECIFIED"} {
		prefix := strings.SplitN(state, "_", 2)[0] + "_"
		if got := stateName(state, prefix); got != "UNSPECIFIED" {
			t.Fatalf("unspecified state = %s", got)
		}
	}
	if got := stateName("99", "EXEC_"); got != "99" {
		t.Fatalf("unknown enum value = %s", got)
	}
}

func TestListEmptyAndPageSize(t *testing.T) {
	s := &outputServer{empty: true}
	server := startOutputServer(t, s)
	for _, jsonOutput := range []bool{false, true} {
		args := []string{"--server", server, "--page-size", strconv.Itoa(int(^uint(0) >> 1))}
		if jsonOutput {
			args = append(args, "--json")
		}
		out, err := captureApprovalOutput(t, func() error { return cmdList(args) })
		if err != nil {
			t.Fatal(err)
		}
		if jsonOutput {
			var page v1.ListSessionsResponse
			if err := protojson.Unmarshal([]byte(out), &page); err != nil || len(page.GetSessions()) != 0 || page.GetNextPageToken() != "" {
				t.Fatalf("empty JSON list: %q %v", out, err)
			}
			var fields map[string]any
			if err := json.Unmarshal([]byte(out), &fields); err != nil || len(fields) != 0 {
				t.Fatalf("empty list should omit default fields: %q %v", out, err)
			}
		} else if !strings.HasPrefix(out, "SESSION  NAME") || strings.Count(out, "\n") != 1 {
			t.Fatalf("empty human list: %q", out)
		}
		if req := <-s.pages; req.GetPageSize() != sqlitelog.MaxPageSize {
			t.Fatalf("page size not clamped: %v", req)
		}
	}
	if err := cmdList([]string{"--server", server, "--page-size", "-1"}); err == nil {
		t.Fatal("negative page size accepted")
	}
}

func TestListPreservesCommandContextAcrossPages(t *testing.T) {
	s := &outputServer{session: &v1.Session{}}
	server := startOutputServer(t, s)
	original := cmdCtx
	cmdCtx = metadata.AppendToOutgoingContext(t.Context(), "x-cli-test", "command")
	t.Cleanup(func() { cmdCtx = original })
	_, err := captureApprovalOutput(t, func() error {
		return cmdList([]string{"--server", server, "--json", "--page-size", "1"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.listContexts) != 2 {
		t.Fatalf("got %d pages, want 2", len(s.listContexts))
	}
	for range 2 {
		values := metadata.ValueFromIncomingContext(<-s.listContexts, "x-cli-test")
		if len(values) != 1 || values[0] != "command" {
			t.Fatalf("page lost command context: %v", values)
		}
	}
}

func TestServerDefaultAndOverride(t *testing.T) {
	s := &outputServer{session: &v1.Session{Metadata: &v1.ResourceMetadata{Uid: "remote"}}}
	server := startOutputServer(t, s)
	t.Setenv("AGENTSESSIONS_SERVER", server)
	out, diagnostics, err := captureCLIOutput(t, func() error { return cmdCreate(nil) })
	if err != nil || out != "remote\n" || !strings.Contains(diagnostics, server) || !strings.Contains(diagnostics, "AGENTSESSIONS_SERVER") {
		t.Fatalf("server environment default: stdout=%q stderr=%q err=%v", out, diagnostics, err)
	}
	out, diagnostics, err = captureCLIOutput(t, func() error { return cmdGet([]string{"--session", "uid", "--json"}) })
	var sess v1.Session
	if err != nil || protojson.Unmarshal([]byte(out), &sess) != nil || sess.GetMetadata().GetUid() != "remote" || !strings.Contains(diagnostics, server) {
		t.Fatalf("environment target corrupted JSON: stdout=%q stderr=%q err=%v", out, diagnostics, err)
	}
	other := &outputServer{session: &v1.Session{Metadata: &v1.ResourceMetadata{Uid: "override"}}}
	override := startOutputServer(t, other)
	out, diagnostics, err = captureCLIOutput(t, func() error { return cmdCreate([]string{"--server", override}) })
	if err != nil || out != "override\n" || diagnostics != "" {
		t.Fatalf("explicit server: stdout=%q stderr=%q err=%v", out, diagnostics, err)
	}
	journal := filepath.Join(t.TempDir(), "local.db")
	out, diagnostics, err = captureCLIOutput(t, func() error { return cmdCreate([]string{"--server=", "--journal", journal}) })
	if err != nil || !strings.HasPrefix(out, "sess-") || diagnostics != "" {
		t.Fatalf("explicit embedded mode: stdout=%q stderr=%q err=%v", out, diagnostics, err)
	}
	if err := cmdHarness([]string{"list"}); err == nil || !strings.Contains(err.Error(), "--server") {
		t.Fatalf("harness inherited session server: %v", err)
	}
}

func TestRemoteJournalRefusedBeforeDelete(t *testing.T) {
	s := &outputServer{session: &v1.Session{}}
	server := startOutputServer(t, s)
	t.Setenv("AGENTSESSIONS_SERVER", server)
	journal := filepath.Join(t.TempDir(), "unused.db")
	for _, args := range [][]string{
		{"--session", "uid", "--journal", journal},
		{"--session", "uid", "--journal", "agentsessions.db", "--server", server},
	} {
		out, diagnostics, err := captureCLIOutput(t, func() error { return cmdDelete(args) })
		if err == nil || !strings.Contains(err.Error(), "--journal") || out != "" || diagnostics != "" || len(s.deletes) != 0 {
			t.Fatalf("remote journal not refused: stdout=%q stderr=%q err=%v", out, diagnostics, err)
		}
	}
	if _, err := os.Stat(journal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refusal touched local journal: %v", err)
	}
}

func TestUnwritableDiagnostics(t *testing.T) {
	s := &outputServer{session: &v1.Session{
		Metadata: &v1.ResourceMetadata{Uid: "uid"}, DeletionState: v1.DeletionState_DELETION_PENDING,
	}}
	server := startOutputServer(t, s)
	t.Setenv("AGENTSESSIONS_SERVER", server)
	closed, err := os.CreateTemp(t.TempDir(), "closed")
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	old := os.Stderr
	os.Stderr = closed
	defer func() { os.Stderr = old }()
	err = cmdDelete([]string{"--session", "uid"})
	if !errors.Is(err, os.ErrClosed) || len(s.deletes) != 0 {
		t.Fatalf("delete proceeded without target notice: %v", err)
	}
	out, err := captureApprovalOutput(t, func() error {
		return cmdDelete([]string{"--server", server, "--session", "uid", "--json"})
	})
	if !errors.Is(err, os.ErrClosed) || !strings.Contains(err.Error(), "intent committed") || len(s.deletes) != 1 || out != "" {
		t.Fatalf("lost committed delete after warning failure: stdout=%q err=%v", out, err)
	}
}

func TestJSONFailureDoesNotPrintSuccess(t *testing.T) {
	s := &outputServer{err: status.Error(codes.Aborted, "conflict")}
	server := startOutputServer(t, s)
	for _, run := range []func([]string) error{cmdCreate, cmdGet, cmdList, cmdDelete, cmdSuspend, cmdResume, cmdFork, cmdReplay} {
		out, err := captureApprovalOutput(t, func() error { return run([]string{"--server", server, "--json"}) })
		if status.Code(err) != codes.Aborted || out != "" {
			t.Fatalf("failure produced success output: %q %v", out, err)
		}
	}
	closed, err := os.CreateTemp(t.TempDir(), "closed")
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	old := os.Stdout
	os.Stdout = closed
	defer func() { os.Stdout = old }()
	if err := writeJSON(&v1.Session{}); err == nil || !errors.Is(err, os.ErrClosed) || !strings.Contains(err.Error(), "remain committed") {
		t.Fatalf("stdout failure not reported: %v", err)
	}
}
