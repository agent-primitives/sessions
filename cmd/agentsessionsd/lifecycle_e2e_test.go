package main

import (
	"maps"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	v1 "github.com/agent-primitives/sessions/api/genpb"
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
	expectGet := func(uid, name, seq, compute, execution, deletion, requested string) {
		t.Helper()
		out := ctl("get", "--session", uid)
		for label, value := range map[string]string{
			"Session": uid, "Name": name, "Project": "smoke", "Harness": "echo",
			"Last seq": seq, "Compute": compute, "Execution": execution, "Deletion": deletion, "Deletion requested": requested,
		} {
			expect(out, `^`+label+` +`+value+`$`)
		}
	}

	// exec, fork and replay are read through --json: their human views can change. Lines that are not
	// JSON are diagnostics on stderr, which ctl merges into its output.
	jsonLines := func(out string, decode func([]byte) error) {
		t.Helper()
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "{") {
				if err := decode([]byte(line)); err != nil {
					t.Fatalf("decode %s: %v", line, err)
				}
			}
		}
	}
	execRecords := func(args ...string) []*v1.LogRecord {
		t.Helper()
		var recs []*v1.LogRecord
		jsonLines(ctl(append([]string{"exec", "--json"}, args...)...), func(b []byte) error {
			var u v1.ExecUpdate
			if err := protojson.Unmarshal(b, &u); err != nil {
				return err
			}
			if r := u.GetRecord(); r != nil {
				recs = append(recs, r)
			}
			return nil
		})
		return recs
	}
	replayRecords := func(sess string) []*v1.LogRecord {
		t.Helper()
		var recs []*v1.LogRecord
		jsonLines(ctl("replay", "--json", "--session", sess), func(b []byte) error {
			var r v1.LogRecord
			if err := protojson.Unmarshal(b, &r); err != nil {
				return err
			}
			recs = append(recs, &r)
			return nil
		})
		return recs
	}
	// outputs is the text of each OUTPUT record, by seq.
	outputs := func(recs []*v1.LogRecord) map[int64]string {
		texts := map[int64]string{}
		for _, r := range recs {
			if r.GetEvent().GetKind() == v1.EventKind_EVENT_OUTPUT {
				var text strings.Builder
				for _, p := range r.GetEvent().GetMessage().GetParts() {
					text.WriteString(p.GetText().GetText())
				}
				texts[r.GetSeq()] = text.String()
			}
		}
		return texts
	}
	expectOutput := func(recs []*v1.LogRecord, seq int64, text string) {
		t.Helper()
		if got, ok := outputs(recs)[seq]; !ok || got != text {
			t.Fatalf("OUTPUT at seq %d = %q (present %v), want %q; records:\n%v", seq, got, ok, text, recs)
		}
	}

	uid := strings.TrimSpace(ctl("create", "--name", "smoke-parent"))
	if !regexp.MustCompile(`^sess-[0-9a-f]+$`).MatchString(uid) {
		t.Fatalf("create printed %q, want a session UID", uid)
	}
	expectGet(uid, "smoke-parent", "0", "NONE", "PENDING", "ACTIVE", "unset")

	recs := execRecords("--session", uid, "--input", "hello")
	expectOutput(recs, 4, "echo:hello")
	if last := recs[len(recs)-1]; last.GetSeq() != 5 || last.GetEvent().GetEnd().GetState() != "COMPLETED" {
		t.Fatalf("turn ended with %v, want END COMPLETED at seq 5", last)
	}

	expect(ctl("suspend", "--session", uid), `^session `+uid+` suspend execution=COMPLETED compute=COLD last_seq=6$`)
	expectGet(uid, "smoke-parent", "6", "COLD", "COMPLETED", "ACTIVE", "unset")
	expect(ctl("resume", "--session", uid), `^session `+uid+` resume execution=COMPLETED compute=LIVE last_seq=7$`)

	expectOutput(execRecords("--session", uid, "--input", "again"), 11, "echo:again")

	var forked v1.ForkResponse
	jsonLines(ctl("fork", "--json", "--session", uid, "--at", "5", "--names", "smoke-child"), func(b []byte) error {
		return protojson.Unmarshal(b, &forked)
	})
	if len(forked.GetChildren()) != 1 || forked.GetChildren()[0].GetParentUid() != uid || forked.GetChildren()[0].GetForkSeq() != 5 ||
		forked.GetChildren()[0].GetMetadata().GetName() != "smoke-child" || !regexp.MustCompile(`^sess-[0-9a-f]+$`).MatchString(forked.GetChildren()[0].GetMetadata().GetUid()) {
		t.Fatalf("fork = %v, want one child smoke-child of %s at seq 5", &forked, uid)
	}
	child := forked.GetChildren()[0].GetMetadata().GetUid()
	// The child shares the parent's history up to seq 5, so its turn sees "hello", not "again".
	recs = execRecords("--session", child, "--input", "from child")
	if got := outputs(recs); len(got) != 1 || !slices.Contains(slices.Collect(maps.Values(got)), "echo:from child") {
		t.Fatalf("child turn outputs = %v, want echo:from child", got)
	}
	replay := replayRecords(child)
	var got []string
	for _, r := range replay {
		if text, ok := outputs([]*v1.LogRecord{r})[r.GetSeq()]; ok {
			got = append(got, text)
		}
	}
	if len(got) != 2 || got[0] != "echo:hello" || got[1] != "echo:from child" {
		t.Fatalf("child replay outputs = %q, want echo:hello then echo:from child", got)
	}

	list := ctl("list")
	expect(list, `^SESSION +NAME +EXECUTION +COMPUTE +HARNESS +LAST SEQ$`)
	expect(list, `^`+uid+` +smoke-parent +COMPLETED +LIVE +echo +12$`)
	expect(list, `^`+child+` +smoke-child +COMPLETED +\w+ +echo +\d+$`)

	// Delete the child. It is a permanent tombstone: the reply carries PENDING and the committed
	// request time, a retry returns that same time, Get and Replay still serve the UID with Replay
	// ending in the DELETE lifecycle record, List hides it, and a new turn is refused.
	requested := expect(ctl("delete", "--session", child), `^session `+child+` deletion=PENDING deletion_requested=(\S+)$`)[1]
	if _, err := time.Parse(time.RFC3339Nano, requested); err != nil {
		t.Fatalf("delete printed deletion_requested=%q, want an RFC 3339 time: %v", requested, err)
	}
	tombstone := ` deletion=PENDING deletion_requested=` + regexp.QuoteMeta(requested) + `$`
	expect(ctl("delete", "--session", child), `^session `+child+tombstone)
	expectGet(child, "smoke-child", `\d+`, `\w+`, "COMPLETED", "PENDING", regexp.QuoteMeta(requested))
	before := replay
	replay = replayRecords(child)
	if len(replay) != len(before)+1 {
		t.Fatalf("child replay has %d records after delete, want the %d before plus one", len(replay), len(before))
	}
	if !slices.EqualFunc(before, replay[:len(before)], func(a, b *v1.LogRecord) bool { return proto.Equal(a, b) }) {
		t.Fatalf("child replay changed on delete:\nbefore\n%v\nafter\n%v", before, replay)
	}
	if last := replay[len(replay)-1]; last.GetEvent().GetLifecycle().GetKind() != v1.Lifecycle_LIFECYCLE_DELETE {
		t.Fatalf("child replay ends with %v, want the DELETE lifecycle record", last)
	}
	if strings.Contains(ctl("list"), child) {
		t.Fatalf("list shows deleted session %s", child)
	}
	if out, err := run("exec", "--session", child, "--input", "after delete"); err == nil || !strings.Contains(out, "code = FailedPrecondition") {
		t.Fatalf("exec on a deleted session: err=%v, want FailedPrecondition:\n%s", err, out)
	}

	// A restarted daemon reports the same sessions from the journal and continues them.
	daemon.stop(t)
	startServer(t, bin, addr, "-journal", journalPath)
	expectGet(uid, "smoke-parent", "12", `\w+`, "COMPLETED", "ACTIVE", "unset")
	expectGet(child, "smoke-child", `\d+`, `\w+`, "COMPLETED", "PENDING", regexp.QuoteMeta(requested))
	if again := replayRecords(child); !slices.EqualFunc(again, replay, func(a, b *v1.LogRecord) bool { return proto.Equal(a, b) }) {
		t.Fatalf("child replay changed across a restart:\nbefore\n%v\nafter\n%v", replay, again)
	}
	if strings.Contains(ctl("list"), child) {
		t.Fatalf("list shows deleted session %s after a restart", child)
	}
	expectOutput(execRecords("--session", uid, "--input", "after restart"), 16, "echo:after restart")
}
