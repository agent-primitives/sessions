package scripts_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// These tests run up.sh and down.sh against stub tools, to pin how they decide whether the demo's
// kind cluster exists and whose it is, what up.sh validates before it changes anything, and what
// it renders. Nothing here touches Docker or a cluster: every tool the scripts call is a stub that
// appends its arguments to a call log, except python3, which is the host's (the scripts parse JSON
// with it).

const cluster = "demo"

// stubs is one run's fake environment.
type stubs struct {
	dir  string // the stubs' state: the call log, the control-plane container ID, kubectl's stdin
	work string // AGENTSUITE_DEMO_WORK
	env  []string
}

// digest is the image digest the ko and docker stubs report.
const digest = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// emit is cmd/emit, built once for the run, which the go stub copies into place for up.sh's `go
// build -o ... ./cmd/emit`, so up.sh's check-model runs the real check rather than a stand-in.
var emit struct {
	once sync.Once
	dir  string
	path string
	err  error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if emit.dir != "" {
		_ = os.RemoveAll(emit.dir)
	}
	os.Exit(code)
}

// emitBinary returns the path of the built cmd/emit.
func emitBinary(t *testing.T) string {
	t.Helper()
	emit.once.Do(func() {
		if emit.dir, emit.err = os.MkdirTemp("", "emit-"); emit.err != nil {
			return
		}
		emit.path = filepath.Join(emit.dir, "emit")
		out, err := exec.Command("go", "build", "-o", emit.path, "./cmd/emit").CombinedOutput()
		if err != nil {
			emit.err = fmt.Errorf("go build ./cmd/emit: %v\n%s", err, out)
		}
	})
	if emit.err != nil {
		t.Fatal(emit.err)
	}
	return emit.path
}

// stub writes an executable shell script named name into bin.
func stub(t *testing.T, bin, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func newStubs(t *testing.T) *stubs {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the demo scripts need a POSIX shell")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found")
	}
	python3, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	s := &stubs{dir: t.TempDir(), work: t.TempDir()}
	bin := filepath.Join(s.dir, "bin")
	substrate := filepath.Join(s.dir, "substrate")
	for _, d := range []string{bin, filepath.Join(substrate, ".git"), filepath.Join(substrate, "hack")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The scripts start with `#!/usr/bin/env bash`; PATH below holds only the stubs and the system
	// directories, so put the test's bash first.
	if err := os.Symlink(bash, filepath.Join(bin, "bash")); err != nil {
		t.Fatal(err)
	}
	log := `echo "$(basename "$0") $*" >>"$STUB_DIR/calls"` + "\n"
	// kind lists STUB_CLUSTERS, or fails when STUB_KIND_LIST_FAIL is set, as a broken kind binary
	// or an unreachable Docker daemon does.
	stub(t, bin, "kind", log+`
if [ "$1 $2" = "get clusters" ]; then
	if [ -n "${STUB_KIND_LIST_FAIL:-}" ]; then
		echo "ERROR: failed to list clusters" >&2
		exit 1
	fi
	[ -z "${STUB_CLUSTERS:-}" ] || echo "$STUB_CLUSTERS"
fi
`)
	// docker inspect reports the control-plane container ID kept in $STUB_DIR/node-id, or the
	// harness image's digest.
	stub(t, bin, "docker", log+`
case "$1" in
info) echo arm64 ;;
inspect)
	case "$3" in
	*RepoDigests*) echo "localhost:5001/agentsuite-harness@`+digest+`" ;;
	*) [ -f "$STUB_DIR/node-id" ] && cat "$STUB_DIR/node-id" || exit 1 ;;
	esac
	;;
esac
`)
	// go build of cmd/emit installs the real emit, or fails as a broken build does when
	// STUB_EMIT_BUILD_FAIL is set. Otherwise go stands in for the first step after the cluster
	// phase (building kubectl-ate) and stops the run there with a status the tests recognize, unless
	// STUB_FULL is set: then it succeeds, and emit render writes the files up.sh reads.
	stub(t, bin, "go", log+`
if [ "$1 $2 $4" = "build -o ./cmd/emit" ]; then
	if [ -n "${STUB_EMIT_BUILD_FAIL:-}" ]; then
		echo "cmd/emit/main.go:1:1: expected 'package', found broken" >&2
		exit 1
	fi
	exec /bin/cp "`+emitBinary(t)+`" "$3"
fi
[ -n "${STUB_FULL:-}" ] || exit 97
if [ "$1 $2 $3" = "run ./cmd/emit render" ]; then
	prev=
	for a; do
		[ "$prev" = -out ] && out=$a
		prev=$a
	done
	mkdir -p "$out"
	echo tmpl-1 >"$out/template-name"
	echo '{}' >"$out/harness-registration.json"
	echo '-substrate-harness=triage=agentsuite-demo/tmpl-1' >"$out/agentsessionsd.args"
fi
`)
	stub(t, bin, "ko", log+`echo "localhost:5001/${3##*/}@`+digest+`"`+"\n")
	// kubectl keeps what it is given on stdin, reports the agentsessionsd-model Secret's
	// resourceVersion as STUB_SECRET_VERSION, and one stable worker pod.
	stub(t, bin, "kubectl", log+`
case " $* " in
*" -f - "*) cat >>"$STUB_DIR/kubectl-stdin" ;;
*" get secret "*) echo "${STUB_SECRET_VERSION:-}" ;;
*" get pods "*) echo "worker-0=10.0.0.1" ;;
esac
`)
	// python3 is the host's. STUB_BINDING stands in for the binding.json up.sh reads.
	stub(t, bin, "python3", log+`
if [ -n "${STUB_BINDING:-}" ] && [ "$#" -eq 3 ] && [ "$1" = -c ] && [ "${3##*/}" = binding.json ]; then
	set -- -c "$2" "$STUB_BINDING"
fi
exec "`+python3+`" "$@"
`)
	// The waits in up.sh and hack/wait-worker-pool.sh need not wait for stubs.
	stub(t, bin, "sleep", "")
	for _, name := range []string{"git", "curl"} {
		stub(t, bin, name, log)
	}
	// kubectl-ate is built into the work directory by up.sh's go build; the go stub builds nothing.
	if err := os.MkdirAll(filepath.Join(s.work, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	stub(t, filepath.Join(s.work, "bin"), "kubectl-ate", log+`
case " $* " in
*" -o json "*) echo '{"status": {"goldenSnapshotStatus": {"goldenTag": {"name": "golden-1"}}}}' ;;
esac
`)
	// create-kind-cluster.sh creates the control-plane container, then exits STUB_CREATE_RC. With
	// STUB_CREATE_SIGNAL it first interrupts up.sh, as a Ctrl-C (INT, which also reaches this
	// script) or a kill of up.sh (TERM) would while kind is still running.
	stub(t, filepath.Join(substrate, "hack"), "create-kind-cluster.sh", `echo "create-kind-cluster.sh" >>"$STUB_DIR/calls"
echo "${STUB_NEW_NODE_ID:-node-1}" >"$STUB_DIR/node-id"
if [ -n "${STUB_CREATE_SIGNAL:-}" ]; then
	i=0
	while [ ! -s "$STUB_DIR/script-pid" ] && [ "$i" -lt 500 ]; do
		/bin/sleep 0.01
		i=$((i + 1))
	done
	kill -s "$STUB_CREATE_SIGNAL" "$(cat "$STUB_DIR/script-pid")"
	[ "$STUB_CREATE_SIGNAL" != INT ] || kill -s INT $$
fi
exit "${STUB_CREATE_RC:-0}"
`)
	stub(t, filepath.Join(substrate, "hack"), "install-ate-kind.sh", `echo "install-ate-kind.sh $*" >>"$STUB_DIR/calls"
exit "${STUB_INSTALL_RC:-0}"
`)
	s.env = []string{
		"PATH=" + bin + ":/usr/bin:/bin",
		"HOME=" + s.dir,
		"TMPDIR=" + s.dir,
		"STUB_DIR=" + s.dir,
		"AGENTSUITE_DEMO_WORK=" + s.work,
		"AGENTSUITE_DEMO_CLUSTER=" + cluster,
		"SUBSTRATE_DIR=" + substrate,
	}
	return s
}

// node sets the control-plane container ID docker reports; "" removes the container.
func (s *stubs) node(t *testing.T, id string) {
	t.Helper()
	path := filepath.Join(s.dir, "node-id")
	if id == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		return
	}
	if err := os.WriteFile(path, []byte(id+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// record writes one of the scripts' ownership records (created-cluster-* or setup-cluster-*).
func (s *stubs) record(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.work, name+"-"+cluster), []byte(content+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// read returns a record's content, or "<absent>".
func (s *stubs) read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.work, name+"-"+cluster))
	if errors.Is(err, os.ErrNotExist) {
		return "<absent>"
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// run runs script with extra environment and returns its exit status, combined output and the
// calls the stubs logged. The script's PID is in $STUB_DIR/script-pid while it runs.
func (s *stubs) run(t *testing.T, script string, env ...string) (int, string, string) {
	t.Helper()
	for _, name := range []string{"calls", "kubectl-stdin", "script-pid"} {
		if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("./" + script)
	cmd.Env = append(append([]string{}, s.env...), env...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Renamed into place, so a stub that waits for the file reads a whole PID.
	pid := filepath.Join(s.dir, "script-pid")
	if err := os.WriteFile(pid+".tmp", []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(pid+".tmp", pid); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	out := buf.Bytes()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(filepath.Join(s.dir, "calls"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return code, string(out), string(calls)
}

// pastClusterPhase is the status the go stub exits with, so up.sh reached the step after the cluster.
const pastClusterPhase = 97

func TestUpClusterPhase(t *testing.T) {
	for _, tc := range []struct {
		name      string
		env       []string
		node      string // control-plane container before the run
		owned     string // created-cluster record before the run
		setup     string // setup-cluster record before the run
		wantCode  int
		wantOut   string
		wantCalls []string // in order
		noCalls   []string
		wantOwned string
		wantSetup string
	}{
		{
			// The listing failing is not proof of absence: create-kind-cluster.sh would delete a
			// cluster of this name.
			name: "a failed kind listing changes nothing", env: []string{"STUB_KIND_LIST_FAIL=1", "STUB_CLUSTERS=" + cluster},
			node: "node-0", owned: "node-0", setup: "node-0 installed",
			wantCode: 1, wantOut: "kind get clusters failed",
			noCalls:   []string{"create-kind-cluster.sh", "install-ate-kind.sh", "kind delete", "kind export", "kubectl", "go "},
			wantOwned: "node-0", wantSetup: "node-0 installed",
		},
		{
			name: "an unlisted cluster whose container exists is refused", node: "node-0",
			wantCode: 1, wantOut: "kind does not list it, but container demo-control-plane exists",
			noCalls:   []string{"create-kind-cluster.sh", "install-ate-kind.sh"},
			wantOwned: "<absent>", wantSetup: "<absent>",
		},
		{
			name:      "an absent cluster is created and installed",
			wantCode:  pastClusterPhase,
			wantCalls: []string{"create-kind-cluster.sh", "kind export kubeconfig", "install-ate-kind.sh --deploy-ate-system"},
			wantOwned: "node-1", wantSetup: "node-1 installed",
		},
		{
			// Ownership is recorded as soon as the container exists, so the rerun below can finish it.
			name: "a create that fails after the node exists records ownership", env: []string{"STUB_CREATE_RC=3"},
			wantCode: 3, wantOut: "rerun up.sh to recreate the cluster",
			wantCalls: []string{"create-kind-cluster.sh"},
			noCalls:   []string{"install-ate-kind.sh"},
			wantOwned: "node-1", wantSetup: "<absent>",
		},
		{
			// A Ctrl-C reaches kind and up.sh; up.sh still records the node kind created.
			name: "an interrupt after the node exists records ownership", env: []string{"STUB_CREATE_SIGNAL=INT"},
			wantCode:  130,
			wantCalls: []string{"create-kind-cluster.sh"},
			noCalls:   []string{"kind export", "install-ate-kind.sh"},
			wantOwned: "node-1", wantSetup: "<absent>",
		},
		{
			name: "a TERM after the node exists records ownership", env: []string{"STUB_CREATE_SIGNAL=TERM"},
			wantCode:  143,
			wantCalls: []string{"create-kind-cluster.sh"},
			noCalls:   []string{"kind export", "install-ate-kind.sh"},
			wantOwned: "node-1", wantSetup: "<absent>",
		},
		{
			// The rerun after the interrupt recreates the cluster up.sh recorded.
			name: "an interrupted rerun records the new node", env: []string{"STUB_CLUSTERS=" + cluster, "STUB_NEW_NODE_ID=node-2", "STUB_CREATE_SIGNAL=TERM"},
			node: "node-1", owned: "node-1",
			wantCode: 143, wantOut: "its setup did not finish; recreating it",
			wantCalls: []string{"create-kind-cluster.sh"},
			noCalls:   []string{"install-ate-kind.sh"},
			wantOwned: "node-2", wantSetup: "<absent>",
		},
		{
			name: "an owned cluster whose create did not finish is recreated", env: []string{"STUB_CLUSTERS=" + cluster, "STUB_NEW_NODE_ID=node-2"},
			node: "node-1", owned: "node-1",
			wantCode: pastClusterPhase, wantOut: "its setup did not finish; recreating it",
			wantCalls: []string{"create-kind-cluster.sh", "install-ate-kind.sh"},
			wantOwned: "node-2", wantSetup: "node-2 installed",
		},
		{
			name: "an owned cluster whose install did not finish is installed", env: []string{"STUB_CLUSTERS=" + cluster},
			node: "node-1", owned: "node-1", setup: "node-1 created",
			wantCode:  pastClusterPhase,
			wantCalls: []string{"kind export kubeconfig", "install-ate-kind.sh --deploy-ate-system"},
			noCalls:   []string{"create-kind-cluster.sh"},
			wantOwned: "node-1", wantSetup: "node-1 installed",
		},
		{
			name: "an installed owned cluster is reused", env: []string{"STUB_CLUSTERS=" + cluster},
			node: "node-1", owned: "node-1", setup: "node-1 installed",
			wantCode:  pastClusterPhase,
			wantCalls: []string{"kind export kubeconfig", "kubectl --context kind-demo"},
			noCalls:   []string{"create-kind-cluster.sh", "install-ate-kind.sh"},
			wantOwned: "node-1", wantSetup: "node-1 installed",
		},
		{
			// A same-named cluster recreated by someone else has another container ID, so the old
			// records do not make it up.sh's.
			name: "a cluster up.sh did not create is reused without installing", env: []string{"STUB_CLUSTERS=other\n" + cluster},
			node: "node-9", owned: "node-1", setup: "node-1 created",
			wantCode: pastClusterPhase, wantOut: "up.sh did not create kind cluster demo",
			wantCalls: []string{"kubectl --context kind-demo"},
			noCalls:   []string{"create-kind-cluster.sh", "install-ate-kind.sh"},
			wantOwned: "node-1", wantSetup: "node-1 created",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStubs(t)
			s.node(t, tc.node)
			if tc.owned != "" {
				s.record(t, "created-cluster", tc.owned)
			}
			if tc.setup != "" {
				s.record(t, "setup-cluster", tc.setup)
			}
			code, out, calls := s.run(t, "up.sh", tc.env...)
			if code != tc.wantCode {
				t.Fatalf("up.sh exit %d, want %d\noutput:\n%s\ncalls:\n%s", code, tc.wantCode, out, calls)
			}
			if !strings.Contains(out, tc.wantOut) {
				t.Errorf("output does not contain %q:\n%s", tc.wantOut, out)
			}
			rest := calls
			for _, c := range tc.wantCalls {
				i := strings.Index(rest, c)
				if i < 0 {
					t.Fatalf("calls do not contain %q in order:\n%s", c, calls)
				}
				rest = rest[i+len(c):]
			}
			for _, c := range tc.noCalls {
				if strings.Contains(calls, c) {
					t.Errorf("calls contain %q:\n%s", c, calls)
				}
			}
			if got := s.read(t, "created-cluster"); got != tc.wantOwned {
				t.Errorf("created-cluster record = %q, want %q", got, tc.wantOwned)
			}
			if got := s.read(t, "setup-cluster"); got != tc.wantSetup {
				t.Errorf("setup-cluster record = %q, want %q", got, tc.wantSetup)
			}
		})
	}
}

func TestDown(t *testing.T) {
	for _, tc := range []struct {
		name       string
		env        []string
		node       string
		wantCode   int
		wantOut    string
		wantDelete bool
		wantKept   bool // the records survive
	}{
		{
			// Only a listing that succeeds and lacks the cluster shows it is gone.
			name: "a failed kind listing keeps the records", env: []string{"STUB_KIND_LIST_FAIL=1"},
			node: "node-1", wantCode: 1, wantOut: "kind get clusters failed", wantKept: true,
		},
		{name: "an absent cluster drops the records", wantOut: "nothing to delete"},
		{
			// kind may not list a cluster whose container still exists (mid-create, lost labels); the
			// record is all that lets a later down.sh delete it.
			name: "an unlisted cluster whose container exists keeps the records", node: "node-1",
			wantCode: 1, wantOut: "container demo-control-plane exists", wantKept: true,
		},
		{name: "an owned cluster is deleted", env: []string{"STUB_CLUSTERS=" + cluster}, node: "node-1", wantDelete: true},
		{
			name: "a cluster up.sh did not create is refused", env: []string{"STUB_CLUSTERS=" + cluster},
			node: "node-9", wantCode: 1, wantOut: "up.sh did not create it", wantKept: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStubs(t)
			s.node(t, tc.node)
			s.record(t, "created-cluster", "node-1")
			s.record(t, "setup-cluster", "node-1 installed")
			code, out, calls := s.run(t, "down.sh", tc.env...)
			if code != tc.wantCode {
				t.Fatalf("down.sh exit %d, want %d\noutput:\n%s\ncalls:\n%s", code, tc.wantCode, out, calls)
			}
			if !strings.Contains(out, tc.wantOut) {
				t.Errorf("output does not contain %q:\n%s", tc.wantOut, out)
			}
			if deleted := strings.Contains(calls, "kind delete cluster --name demo"); deleted != tc.wantDelete {
				t.Errorf("kind delete called = %v, want %v:\n%s", deleted, tc.wantDelete, calls)
			}
			owned, setup := s.read(t, "created-cluster"), s.read(t, "setup-cluster")
			if tc.wantKept && (owned != "node-1" || setup != "node-1 installed") {
				t.Errorf("records = %q, %q, want them kept", owned, setup)
			}
			if !tc.wantKept && (owned != "<absent>" || setup != "<absent>") {
				t.Errorf("records = %q, %q, want them removed", owned, setup)
			}
		})
	}
}

// A kind cluster name may contain '.', which grep -x would read as any character.
func TestClusterNameIsMatchedLiterally(t *testing.T) {
	s := newStubs(t)
	code, out, calls := s.run(t, "down.sh", "AGENTSUITE_DEMO_CLUSTER=demo.x", "STUB_CLUSTERS=demo-x")
	if code != 0 || !strings.Contains(out, "no kind cluster demo.x; nothing to delete") {
		t.Fatalf("down.sh exit %d, want 0 and demo.x not listed\noutput:\n%s\ncalls:\n%s", code, out, calls)
	}
	code, out, _ = s.run(t, "down.sh", "AGENTSUITE_DEMO_CLUSTER=../x")
	if code != 1 || !strings.Contains(out, "AGENTSUITE_DEMO_CLUSTER=../x: want lowercase letters") {
		t.Fatalf("down.sh exit %d, want 1 and the name refused\noutput:\n%s", code, out)
	}
}

// up.sh refuses inputs it cannot deploy before it runs anything that touches the cluster (or even
// lists it), so a misconfigured rerun leaves the cluster, the deployment and the
// agentsessionsd-model Secret as they were.
func TestUpRefusesInputsBeforeTheCluster(t *testing.T) {
	binding := func(t *testing.T, atespace string) string {
		t.Helper()
		b, err := os.ReadFile("binding.json")
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "binding.json")
		b = bytes.Replace(b, []byte(`"atespace": "agentsuite-demo"`), []byte(`"atespace": "`+atespace+`"`), 1)
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	provider := []string{"AGENTSUITE_MODEL_BASE_URL=https://llm.example/v1", "AGENTSUITE_MODEL=m", "MODEL_API_KEY=k"}
	for _, tc := range []struct {
		name    string
		env     func(t *testing.T) []string
		wantOut string
	}{
		{
			name: "a base URL with whitespace",
			env: func(*testing.T) []string {
				return append(provider, "AGENTSUITE_MODEL_BASE_URL=https://llm.example/v1 x")
			},
			wantOut: "AGENTSUITE_MODEL_BASE_URL contains whitespace or a control character; nothing was changed",
		},
		{
			name:    "a model with a newline",
			env:     func(*testing.T) []string { return append(provider, "AGENTSUITE_MODEL=m\n- -evil") },
			wantOut: "AGENTSUITE_MODEL contains whitespace or a control character; nothing was changed",
		},
		{
			name: "a base URL naming a placeholder",
			env: func(*testing.T) []string {
				return append(provider, "AGENTSUITE_MODEL_BASE_URL=https://llm.example/DAEMON_IMAGE")
			},
			wantOut: "AGENTSUITE_MODEL_BASE_URL contains DAEMON_IMAGE, a placeholder of deploy/agentsessionsd.yaml",
		},
		{
			name: "a base URL with no scheme",
			env: func(*testing.T) []string {
				return append(provider, "AGENTSUITE_MODEL_BASE_URL=api.example.com/v1")
			},
			wantOut: "AGENTSUITE_MODEL_BASE_URL 'api.example.com/v1' with AGENTSUITE_MODEL 'm' would not start agentsessionsd: emit: openai: base URL \"api.example.com/v1\" is not absolute (want scheme://host[/path]); nothing was changed",
		},
		{
			// emit is built and run as a binary, so go run's own "exit status 1" line is not
			// appended to the reason.
			name: "a base URL with a port that is not a number, reported alone",
			env: func(*testing.T) []string {
				return append(provider, "AGENTSUITE_MODEL_BASE_URL=http://host:abc/v1")
			},
			wantOut: "would not start agentsessionsd: emit: openai: base URL: parse \"http://host:abc/v1\": invalid port \":abc\" after host; nothing was changed",
		},
		{
			name:    "an emit that does not build",
			env:     func(*testing.T) []string { return append(provider, "STUB_EMIT_BUILD_FAIL=1") },
			wantOut: "cannot build cmd/emit to check AGENTSUITE_MODEL_BASE_URL and AGENTSUITE_MODEL: cmd/emit/main.go:1:1: expected 'package', found broken; nothing was changed",
		},
		{
			name:    "a base URL without a model",
			env:     func(*testing.T) []string { return []string{"AGENTSUITE_MODEL_BASE_URL=https://llm.example/v1"} },
			wantOut: "AGENTSUITE_MODEL_BASE_URL is set without AGENTSUITE_MODEL; set the provider's model; nothing was changed",
		},
		{
			// Without a URL the run would pick the fake model, delete the stored key and replace
			// the daemon's pod, dropping its journal.
			name:    "a model without a base URL",
			env:     func(*testing.T) []string { return []string{"AGENTSUITE_MODEL=m"} },
			wantOut: "AGENTSUITE_MODEL is set without AGENTSUITE_MODEL_BASE_URL; set both for a real provider, or unset AGENTSUITE_MODEL for the fake model; nothing was changed",
		},
		{
			name:    "a key without a base URL",
			env:     func(*testing.T) []string { return []string{"MODEL_API_KEY=k"} },
			wantOut: "MODEL_API_KEY is set without AGENTSUITE_MODEL_BASE_URL; set both for a real provider, or unset MODEL_API_KEY for the fake model; nothing was changed",
		},
		{
			name: "a base URL with no host",
			env: func(*testing.T) []string {
				return append(provider, "AGENTSUITE_MODEL_BASE_URL=https:///v1")
			},
			wantOut: "is not absolute",
		},
		{
			name: "a base URL with only userinfo for a host",
			env: func(*testing.T) []string {
				return append(provider, "AGENTSUITE_MODEL_BASE_URL=https://user@/v1")
			},
			wantOut: "is not absolute",
		},
		{
			name: "a base URL with a port that is not a number",
			env: func(*testing.T) []string {
				return append(provider, "AGENTSUITE_MODEL_BASE_URL=http://host:abc/v1")
			},
			wantOut: "would not start agentsessionsd: emit: openai: base URL: parse \"http://host:abc/v1\": invalid port",
		},
		{
			name: "a base URL with a bad escape",
			env: func(*testing.T) []string {
				return append(provider, "AGENTSUITE_MODEL_BASE_URL=http://h%zz/v1")
			},
			wantOut: "invalid URL escape",
		},
		{
			name: "a base URL with another scheme",
			env: func(*testing.T) []string {
				return append(provider, "AGENTSUITE_MODEL_BASE_URL=ftp://llm.example/v1")
			},
			wantOut: "base URL \"ftp://llm.example/v1\" is not http or https; nothing was changed",
		},
		{
			name:    "a binding for another atespace",
			env:     func(t *testing.T) []string { return []string{"STUB_BINDING=" + binding(t, "other")} },
			wantOut: "binding.json names atespace 'other', but the demo deploys into atespace agentsuite-demo; nothing was changed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStubs(t)
			s.node(t, "node-1")
			s.record(t, "created-cluster", "node-1")
			s.record(t, "setup-cluster", "node-1 installed")
			code, out, calls := s.run(t, "up.sh", append(tc.env(t), "STUB_CLUSTERS="+cluster, "STUB_FULL=1")...)
			if code != 1 || !strings.Contains(out, tc.wantOut) {
				t.Fatalf("up.sh exit %d, want 1 with %q\noutput:\n%s", code, tc.wantOut, out)
			}
			if strings.Contains(out, "exit status") {
				t.Errorf("output carries a go run exit status:\n%s", out)
			}
			for _, c := range []string{"git ", "kind ", "kubectl", "create-kind-cluster.sh", "ko "} {
				if strings.Contains(calls, c) {
					t.Errorf("calls contain %q:\n%s", c, calls)
				}
			}
		})
	}
}

// stdin returns what the scripts piped into kubectl -f - during the last run.
func stdin(t *testing.T, s *stubs) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.dir, "kubectl-stdin"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(b)
}

// The model base URL is copied into the Deployment as written, query and fragment included.
// model/openai joins the completions path to the URL's path and keeps its query, so such a URL
// reaches the provider's completions endpoint (TestBaseURLWithAQueryOrFragmentKeepsTheCompletionsPath).
func TestUpRendersTheDaemon(t *testing.T) {
	const (
		url   = "https://llm.example/v1?api-version=2&tenant=a#b"
		model = `gpt&x#1\1`
	)
	for _, tc := range []struct {
		name        string
		env         []string
		wantLines   []string
		wantSecret  string // the Secret call that precedes the apply
		wantVersion string
	}{
		{
			name:        "fake provider",
			wantLines:   []string{"- -model=triage-model", "- -model-base-url=http://fakemodel.agentsuite-demo.svc:8080/v1"},
			wantSecret:  "delete secret agentsessionsd-model --ignore-not-found",
			wantVersion: "none",
		},
		{
			name:        "real provider",
			env:         []string{"AGENTSUITE_MODEL_BASE_URL=" + url, "AGENTSUITE_MODEL=" + model, "MODEL_API_KEY=k", "STUB_SECRET_VERSION=7"},
			wantLines:   []string{"- -model=" + model, "- -model-base-url=" + url},
			wantSecret:  "apply --server-side",
			wantVersion: "7",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStubs(t)
			s.node(t, "node-1")
			s.record(t, "created-cluster", "node-1")
			s.record(t, "setup-cluster", "node-1 installed")
			code, out, calls := s.run(t, "up.sh", append(tc.env, "STUB_CLUSTERS="+cluster, "STUB_FULL=1")...)
			if code != 0 {
				t.Fatalf("up.sh exit %d, want 0\noutput:\n%s\ncalls:\n%s", code, out, calls)
			}
			path := filepath.Join(s.work, "cluster-"+cluster, "agentsessionsd.yaml")
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			lines := map[string]bool{}
			for _, l := range strings.Split(string(b), "\n") {
				lines[strings.TrimSpace(l)] = true
			}
			want := append([]string{
				"image: localhost:5001/agentsessionsd@" + digest,
				"- -substrate-harness=triage=agentsuite-demo/tmpl-1",
				`agentsuite-demo/model-secret-version: "` + tc.wantVersion + `"`,
			}, tc.wantLines...)
			for _, l := range want {
				if !lines[l] {
					t.Errorf("rendered manifest has no line %q:\n%s", l, b)
				}
			}
			for _, p := range []string{"DAEMON_IMAGE", "MODEL_ID", "MODEL_BASE_URL", "SUBSTRATE_HARNESS_FLAG", "MODEL_SECRET_VERSION"} {
				if strings.Contains(string(b), p) {
					t.Errorf("rendered manifest still contains %s", p)
				}
			}
			secret := strings.Index(calls, tc.wantSecret)
			apply := strings.Index(calls, "apply -f "+path)
			if secret < 0 || apply < secret {
				t.Errorf("want %q, then apply -f %s:\n%s", tc.wantSecret, path, calls)
			}
			// The fake model is built, deployed and waited for only when it serves the run.
			fake := tc.env == nil
			for _, call := range []string{"ko build --base-import-paths ./cmd/fakemodel", "rollout status deploy/fakemodel"} {
				if got := strings.Contains(calls, call); got != fake {
					t.Errorf("%q called = %v, want %v:\n%s", call, got, fake, calls)
				}
			}
			if got := strings.Contains(stdin(t, s), "name: fakemodel"); got != fake {
				t.Errorf("fakemodel manifest applied = %v, want %v", got, fake)
			}
			// The kubeconfig and the composition are this cluster's.
			if !strings.Contains(calls, "kind export kubeconfig --name demo --kubeconfig "+filepath.Join(s.work, "cluster-demo", "kubeconfig")) ||
				!strings.Contains(calls, "-out "+filepath.Join(s.work, "cluster-demo", "render")) {
				t.Errorf("want the kubeconfig and render output under %s:\n%s", filepath.Join(s.work, "cluster-demo"), calls)
			}
		})
	}
}
