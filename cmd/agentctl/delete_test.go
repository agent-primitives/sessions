package main

import (
	"bytes"
	"context"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/agent-primitives/sessions/api/genpb"
)

// Build and run the actual command map: testing cmdDelete alone would miss an unregistered command.
func deleteCLI(t *testing.T) func(...string) (string, error) {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "agentctl")
	if out, err := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %s %v", out, err)
	}
	return func(args ...string) (string, error) {
		cmd := exec.CommandContext(t.Context(), binary, args...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil || (len(args) > 1 && args[1] == "--help") {
			return string(out) + stderr.String(), err
		}
		return string(out), nil
	}
}

// A missing command-map entry, wrong UID request or omission of the server timestamps would hide
// whether OK means an irreversible tombstone or actual resource release.
func TestDeleteCommandRequestAndPendingOutput(t *testing.T) {
	requests := make(chan *v1.DeleteSessionRequest, 2)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	v1.RegisterSessionsServer(srv, &deleteRecorder{requests: requests})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	run := deleteCLI(t)
	out, err := run("delete", "--server", lis.Addr().String(), "--session", "delete-uid", "--project", "project", "--journal", filepath.Join(t.TempDir(), "unused.db"))
	if err != nil {
		t.Fatalf("delete command: %s %v", out, err)
	}
	select {
	case req := <-requests:
		if req.GetUid() != "delete-uid" {
			t.Fatalf("Delete request UID=%q", req.GetUid())
		}
	default:
		t.Fatal("delete command sent no RPC")
	}
	for _, want := range []string{"session delete-uid", "deletion=PENDING", "deletion_requested=2026-01-02T03:04:05.123456789Z", "durable intent only", "compute and fork pins retained"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q: %s", want, out)
		}
	}
	out, err = run("delete", "--server", lis.Addr().String(), "--session", "unknown")
	if err == nil || !strings.Contains(out, "code = NotFound desc = session not found") {
		t.Fatalf("unknown Delete error: %s %v", out, err)
	}
}

type deleteRecorder struct {
	v1.UnimplementedSessionsServer
	requests chan *v1.DeleteSessionRequest
}

func (s *deleteRecorder) GetSession(_ context.Context, req *v1.GetSessionRequest) (*v1.Session, error) {
	sess := &v1.Session{Metadata: &v1.ResourceMetadata{Uid: req.GetUid()}, DeletionState: v1.DeletionState_DELETION_ACTIVE}
	if req.GetUid() == "pending" {
		sess.DeletionState = v1.DeletionState_DELETION_PENDING
		sess.DeletionRequestedTime = timestamppb.New(time.Unix(0, 0))
	}
	return sess, nil
}

func TestDeleteHelpAndGetTimestampSemantics(t *testing.T) {
	run := deleteCLI(t)
	if out, err := run(); err == nil || !strings.Contains(out, "|delete|") {
		t.Fatalf("usage lacks delete: %s %v", out, err)
	}
	out, err := run("delete", "--help")
	if err != nil {
		t.Fatalf("help: %s %v", out, err)
	}
	for _, want := range []string{"-session", "-server", "-journal", "-project", "no data erasure or compute cleanup"} {
		if !strings.Contains(out, want) {
			t.Fatalf("help missing %q: %s", want, out)
		}
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	v1.RegisterSessionsServer(srv, &deleteRecorder{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	for _, tc := range []struct{ uid, want string }{
		{"active", "deletion=ACTIVE deletion_requested=unset"},
		{"pending", "deletion=PENDING deletion_requested=1970-01-01T00:00:00Z"},
	} {
		out, err := run("get", "--server", lis.Addr().String(), "--session", tc.uid)
		if err != nil || !strings.Contains(out, tc.want) || strings.Contains(out, "deletion_completed") || strings.Contains(out, "deletion=COMPLETE") {
			t.Fatalf("Get %s times: %s %v", tc.uid, out, err)
		}
	}
}

func (s *deleteRecorder) DeleteSession(_ context.Context, req *v1.DeleteSessionRequest) (*v1.Session, error) {
	s.requests <- req
	if req.GetUid() == "unknown" {
		return nil, status.Error(codes.NotFound, "session not found")
	}
	return &v1.Session{Metadata: &v1.ResourceMetadata{Uid: req.GetUid()}, DeletionState: v1.DeletionState_DELETION_PENDING, DeletionRequestedTime: timestamppb.New(time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC))}, nil
}

// Separate embedded invocations reopen the real SQLite journal. Delete must hide/refuse only the
// named session while preserving audit reads and existing children; retries cannot move intent time.
func TestEmbeddedDeleteAuditAndDescendants(t *testing.T) {
	runBinary := deleteCLI(t)
	journal := filepath.Join(t.TempDir(), "journal.db")
	run := func(args ...string) (string, error) {
		return runBinary(append(args, "--journal", journal, "--project", "delete")...)
	}
	mustRun := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("%v: %s %v", args, out, err)
		}
		return strings.TrimSpace(out)
	}
	parent := mustRun("create", "--name", "parent")
	mustRun("exec", "--session", parent, "--input", "before")
	fork := mustRun("fork", "--session", parent)
	fields := strings.Fields(fork)
	if len(fields) < 2 || fields[0] != "child" {
		t.Fatalf("fork output: %s", fork)
	}
	child := fields[1]
	deleted := mustRun("delete", "--session", parent)
	get := mustRun("get", "--session", parent)
	if !strings.Contains(get, "deletion=PENDING") || !strings.Contains(get, "deletion_requested=") || strings.Contains(get, "deletion_completed") || strings.Contains(get, "deletion=COMPLETE") {
		t.Fatalf("Get hides intent fields: %s", get)
	}
	if retry := mustRun("delete", "--session", parent); retry != deleted {
		t.Fatalf("retry changed output/times:\n%s\n%s", deleted, retry)
	}
	if replay := mustRun("replay", "--session", parent); !strings.Contains(replay, "EVENT_OUTPUT") || !strings.Contains(replay, "EVENT_LIFECYCLE") {
		t.Fatalf("audit unavailable: %s", replay)
	}
	list := mustRun("list")
	if strings.Contains(list, parent+"\t") || !strings.Contains(list, child+"\t") {
		t.Fatalf("List hides wrong UID: %s", list)
	}
	for _, args := range [][]string{{"exec", "--session", parent, "--input", "after"}, {"fork", "--session", parent, "--at", "1"}, {"suspend", "--session", parent}, {"resume", "--session", parent}} {
		out, err := run(args...)
		if err == nil || !strings.Contains(out, "code = FailedPrecondition") {
			t.Fatalf("%v: want FailedPrecondition, got %s %v", args, out, err)
		}
	}
	if out := mustRun("exec", "--session", child, "--input", "survives"); !strings.Contains(out, "echo:survives") {
		t.Fatalf("child affected by parent tombstone: %s", out)
	}
	mustRun("fork", "--session", child)
	if got := mustRun("get", "--session", parent); got != get {
		t.Fatalf("refusal/child mutation changed tombstone:\n%s\n%s", get, got)
	}
}
