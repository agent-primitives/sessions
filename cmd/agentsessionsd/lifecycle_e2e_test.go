package main

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestSessionLifecycleRealBinaries is the smoke test for the quickstart path: agentsessionsd on the
// local runtime with its built-in echo harness and model (no substrate, no model endpoint), driven by
// agentctl over the network. It covers create, exec, suspend, resume, fork, delete, replay and list,
// checks what each command prints rather than its exit status, and restarts the daemon on the same
// journal to check that the state it reports survived.
func TestSessionLifecycleRealBinaries(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs agentsessionsd and agentctl")
	}
	bin := buildBinaries(t)
	addr := freeLoopbackAddr(t)
	journalPath := filepath.Join(t.TempDir(), "journal.db")
	daemon := startServer(t, bin, addr, "-journal", journalPath)

	run := func(args ...string) (string, error) {
		args = append(args[:1:1], append([]string{"--server", addr, "--project", "smoke"}, args[1:]...)...)
		out, err := exec.Command(filepath.Join(bin, "agentctl"), args...).CombinedOutput()
		return string(out), err
	}
	ctl := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("agentctl %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	expect := func(out, pattern string) []string {
		t.Helper()
		m := regexp.MustCompile(`(?m)` + pattern).FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("output does not match %q:\n%s", pattern, out)
		}
		return m
	}

	uid := strings.TrimSpace(ctl("create", "--name", "smoke-parent"))
	if !regexp.MustCompile(`^sess-[0-9a-f]+$`).MatchString(uid) {
		t.Fatalf("create printed %q, want a session UID", uid)
	}
	expect(ctl("get", "--session", uid), `^session `+uid+` project=smoke name=smoke-parent harness=echo last_seq=0 compute=NONE deletion=ACTIVE deletion_requested=unset$`)

	out := ctl("exec", "--session", uid, "--input", "hello")
	expect(out, `^seq=4 +EVENT_OUTPUT  assistant: echo:hello$`)
	expect(out, `^seq=5 +EVENT_END$`)

	expect(ctl("suspend", "--session", uid), `^session `+uid+` suspend compute=COMPUTE_COLD last_seq=6$`)
	expect(ctl("get", "--session", uid), `^session `+uid+` .* last_seq=6 compute=COLD deletion=ACTIVE deletion_requested=unset$`)
	expect(ctl("resume", "--session", uid), `^session `+uid+` resume compute=COMPUTE_LIVE last_seq=7$`)

	out = ctl("exec", "--session", uid, "--input", "again")
	expect(out, `^seq=11 +EVENT_OUTPUT  assistant: echo:again$`)

	child := expect(ctl("fork", "--session", uid, "--at", "5", "--names", "smoke-child"),
		`^child (sess-[0-9a-f]+) parent=`+uid+` fork_seq=5 name=smoke-child$`)[1]
	// The child shares the parent's history up to seq 5, so its turn sees "hello", not "again".
	out = ctl("exec", "--session", child, "--input", "from child")
	expect(out, `^seq=\d+ +EVENT_OUTPUT  assistant: echo:from child$`)
	replay := ctl("replay", "--session", child)
	if got := regexp.MustCompile(`(?m)^seq=\d+ +EVENT_OUTPUT  assistant: (.*)$`).FindAllStringSubmatch(replay, -1); len(got) != 2 ||
		got[0][1] != "echo:hello" || got[1][1] != "echo:from child" {
		t.Fatalf("child replay outputs = %v, want echo:hello then echo:from child:\n%s", got, replay)
	}

	list := ctl("list")
	expect(list, `^`+uid+`\tlast_seq=12\tcompute=LIVE\tharness=echo\tname=smoke-parent$`)
	expect(list, `^`+child+`\tlast_seq=\d+\tcompute=\w+\tharness=echo\tname=smoke-child$`)

	// Delete the child. It is a permanent tombstone: the reply carries PENDING and the committed
	// request time, a retry returns that same time, Get and Replay still serve the UID with Replay
	// ending in the DELETE lifecycle record, List hides it, and a new turn is refused.
	requested := expect(ctl("delete", "--session", child), `^session `+child+` deletion=PENDING deletion_requested=(\S+)$`)[1]
	if _, err := time.Parse(time.RFC3339Nano, requested); err != nil {
		t.Fatalf("delete printed deletion_requested=%q, want an RFC 3339 time: %v", requested, err)
	}
	tombstone := ` deletion=PENDING deletion_requested=` + regexp.QuoteMeta(requested) + `$`
	expect(ctl("delete", "--session", child), `^session `+child+tombstone)
	expect(ctl("get", "--session", child), `^session `+child+` project=smoke name=smoke-child harness=echo last_seq=\d+ compute=\w+`+tombstone)
	before := replay
	replay = ctl("replay", "--session", child)
	if !strings.HasPrefix(replay, before) {
		t.Fatalf("child replay lost records on delete:\nbefore\n%s\nafter\n%s", before, replay)
	}
	expect(strings.TrimPrefix(replay, before), `\Aseq=\d+ +EVENT_LIFECYCLE\n\z`)
	if strings.Contains(ctl("list"), child) {
		t.Fatalf("list shows deleted session %s", child)
	}
	if out, err := run("exec", "--session", child, "--input", "after delete"); err == nil || !strings.Contains(out, "code = FailedPrecondition") {
		t.Fatalf("exec on a deleted session: err=%v, want FailedPrecondition:\n%s", err, out)
	}

	// A restarted daemon reports the same sessions from the journal and continues them.
	daemon.stop(t)
	startServer(t, bin, addr, "-journal", journalPath)
	expect(ctl("get", "--session", uid), `^session `+uid+` project=smoke name=smoke-parent harness=echo last_seq=12 compute=\w+ deletion=ACTIVE deletion_requested=unset$`)
	expect(ctl("get", "--session", child), `^session `+child+` .*`+tombstone)
	if again := ctl("replay", "--session", child); again != replay {
		t.Fatalf("child replay changed across a restart:\nbefore\n%s\nafter\n%s", replay, again)
	}
	if strings.Contains(ctl("list"), child) {
		t.Fatalf("list shows deleted session %s after a restart", child)
	}
	expect(ctl("exec", "--session", uid, "--input", "after restart"), `^seq=16 +EVENT_OUTPUT  assistant: echo:after restart$`)
}
