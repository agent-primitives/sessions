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
	// or an unreachable Docker daemon does. With STUB_CLUSTERS_FROM_NODE set it lists the demo's
	// cluster while the control-plane container exists, so a run can create it.
	stub(t, bin, "kind", log+`
if [ "$1 $2" = "get clusters" ]; then
	if [ -n "${STUB_KIND_LIST_FAIL:-}" ]; then
		echo "ERROR: failed to list clusters" >&2
		exit 1
	fi
	[ -z "${STUB_CLUSTERS:-}" ] || echo "$STUB_CLUSTERS"
	[ -z "${STUB_CLUSTERS_FROM_NODE:-}" ] || [ ! -f "$STUB_DIR/node-id" ] || echo "$AGENTSUITE_DEMO_CLUSTER"
fi
`)
	// docker inspect reports the control-plane container ID kept in $STUB_DIR/node-id, or the
	// harness image's digest.
	stub(t, bin, "docker", log+`
case "$1" in
info) echo "${STUB_DOCKER_ARCH:-arm64}" ;;
run) [ -z "${STUB_NO_KVM:-}" ] || exit 125 ;;
inspect)
	case "$3" in
	*RepoDigests*) echo "localhost:5001/agentsuite-harness@`+digest+`" ;;
	*)
		# STUB_NODE_HIDDEN_ONCE: the first lookup fails, as one during a Docker hiccup would.
		if [ -n "${STUB_NODE_HIDDEN_ONCE:-}" ] && [ ! -f "$STUB_DIR/node-hidden" ]; then
			: >"$STUB_DIR/node-hidden"
			exit 1
		fi
		[ -f "$STUB_DIR/node-id" ] && cat "$STUB_DIR/node-id" || exit 1
		;;
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
# go run ./cmd/ate-setup is the ate-system install, which exits STUB_INSTALL_RC.
[ "$1 $2" != "run ./cmd/ate-setup" ] || exit "${STUB_INSTALL_RC:-0}"
[ -n "${STUB_FULL:-}" ] || exit 97
if [ "$1 $2 $3" = "run ./cmd/emit render" ]; then
	prev= model=unset tier=
	for a; do
		[ "$prev" = -out ] && out=$a
		[ "$prev" = -model ] && model=$a
		[ "$prev" = -tier ] && tier=$a
		prev=$a
	done
	# The -model up.sh forwards to the harness, kept verbatim (the call log's echo is not).
	printf '%s' "$model" >"$STUB_DIR/render-model"
	mkdir -p "$out"
	echo '{}' >"$out/harness-registration.json"
	if [ "$tier" = memory ]; then
		echo tmpl-2 >"$out/template-name"
		echo '-substrate-harness=triage-memory=agentsuite-demo/tmpl-2,memory' >"$out/agentsessionsd.args"
	else
		echo tmpl-1 >"$out/template-name"
		echo '-substrate-harness=triage=agentsuite-demo/tmpl-1' >"$out/agentsessionsd.args"
	fi
fi
`)
	stub(t, bin, "ko", log+`echo "localhost:5001/${3##*/}@`+digest+`"`+"\n")
	// kubectl keeps what it is given on stdin, reports the agentsessionsd-model Secret's
	// resourceVersion as STUB_SECRET_VERSION, the agentsessionsd Deployment's arguments with
	// STUB_DEPLOYED_URL as its -model-base-url (none when it is "none", and no Deployment when it is
	// "missing"), and one stable worker pod.
	stub(t, bin, "kubectl", log+`
case " $* " in
*" -f - "*) cat >>"$STUB_DIR/kubectl-stdin" ;;
*" get secret "*) echo "${STUB_SECRET_VERSION:-}" ;;
*" get deploy agentsessionsd "*)
	[ "${STUB_DEPLOYED_URL:-}" != missing ] || exit 1
	echo -addr=:8080
	[ "${STUB_DEPLOYED_URL:-}" = none ] || echo "-model-base-url=${STUB_DEPLOYED_URL:-}"
	;;
*" get pods "*) echo "worker-0=10.0.0.1" ;;
esac
`)
	// python3 is the host's. STUB_BINDING stands in for the binding.json up.sh reads.
	stub(t, bin, "python3", log+`
if [ -n "${STUB_BINDING:-}" ] && [ "$#" -eq 3 ] && [ "$1" = -c ] && case "${3##*/}" in binding*.json) true ;; *) false ;; esac; then
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
	// The micro-VM class's assets and SandboxConfig; exits STUB_MICROVM_RC.
	stub(t, filepath.Join(substrate, "hack"), "install-microvm-deps.sh", `echo "install-microvm-deps.sh $* ARCH=$ARCH" >>"$STUB_DIR/calls"
exit "${STUB_MICROVM_RC:-0}"
`)
	// up.sh runs ate-setup itself; this wrapper would build for the host's architecture.
	stub(t, filepath.Join(substrate, "hack"), "install-ate-kind.sh", `echo "install-ate-kind.sh $*" >>"$STUB_DIR/calls"
exit 1
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
	return s.runArgs(t, script, nil, env...)
}

// runArgs is run with arguments for the script.
func (s *stubs) runArgs(t *testing.T, script string, args []string, env ...string) (int, string, string) {
	t.Helper()
	for _, name := range []string{"calls", "kubectl-stdin", "script-pid"} {
		if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("./"+script, args...)
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

// installCall is how the go stub logs up.sh's ate-system install.
const installCall = "go run ./cmd/ate-setup --kind"

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
			noCalls:   []string{"create-kind-cluster.sh", installCall, "kind delete", "kind export", "kubectl", "go "},
			wantOwned: "node-0", wantSetup: "node-0 installed",
		},
		{
			name: "an unlisted cluster whose container exists is refused", node: "node-0",
			wantCode: 1, wantOut: "kind does not list it, but container demo-control-plane exists",
			noCalls:   []string{"create-kind-cluster.sh", installCall},
			wantOwned: "<absent>", wantSetup: "<absent>",
		},
		{
			name:      "an absent cluster is created and installed",
			wantCode:  pastClusterPhase,
			wantCalls: []string{"create-kind-cluster.sh", "kind export kubeconfig", installCall + " --no-dev-env --kind-cluster-name=demo --context=kind-demo"},
			noCalls:   []string{"install-ate-kind.sh"},
			wantOwned: "node-1", wantSetup: "node-1 installed",
		},
		{
			// ate-setup on Kind builds for its own GOARCH unless told otherwise, so up.sh passes the
			// platform Docker reports: a Mac driving a remote amd64 Docker gets amd64 images.
			name: "the install builds for the platform Docker reports", env: []string{"STUB_DOCKER_ARCH=x86_64"},
			wantCode:  pastClusterPhase,
			wantCalls: []string{installCall, "--ko-default-platforms=linux/amd64", "deploy ate-system --setup-csi=none"},
			wantOwned: "node-1", wantSetup: "node-1 installed",
		},
		{
			name:      "an arm64 Docker gets arm64 control-plane images",
			wantCode:  pastClusterPhase,
			wantCalls: []string{installCall, "--ko-default-platforms=linux/arm64", "deploy ate-system"},
			wantOwned: "node-1", wantSetup: "node-1 installed",
		},
		{
			// The record stays at created, so the rerun installs again instead of reusing it.
			name: "a failed install keeps the cluster at created", env: []string{"STUB_INSTALL_RC=5"},
			wantCode:  5,
			wantCalls: []string{"create-kind-cluster.sh", installCall},
			wantOwned: "node-1", wantSetup: "node-1 created",
		},
		{
			// Ownership is recorded as soon as the container exists, so the rerun below can finish it.
			name: "a create that fails after the node exists records ownership", env: []string{"STUB_CREATE_RC=3"},
			wantCode: 3, wantOut: "rerun up.sh to recreate the cluster",
			wantCalls: []string{"create-kind-cluster.sh"},
			noCalls:   []string{installCall},
			wantOwned: "node-1", wantSetup: "node-1 creating",
		},
		{
			// A Ctrl-C reaches kind and up.sh; up.sh still records the node kind created.
			name: "an interrupt after the node exists records ownership", env: []string{"STUB_CREATE_SIGNAL=INT"},
			wantCode:  130,
			wantCalls: []string{"create-kind-cluster.sh"},
			noCalls:   []string{"kind export", installCall},
			wantOwned: "node-1", wantSetup: "node-1 creating",
		},
		{
			name: "a TERM after the node exists records ownership", env: []string{"STUB_CREATE_SIGNAL=TERM"},
			wantCode:  143,
			wantCalls: []string{"create-kind-cluster.sh"},
			noCalls:   []string{"kind export", installCall},
			wantOwned: "node-1", wantSetup: "node-1 creating",
		},
		{
			// The rerun after the interrupt recreates the cluster up.sh recorded.
			name: "an interrupted rerun records the new node", env: []string{"STUB_CLUSTERS=" + cluster, "STUB_NEW_NODE_ID=node-2", "STUB_CREATE_SIGNAL=TERM"},
			node: "node-1", owned: "node-1", setup: "node-1 creating",
			wantCode: 143, wantOut: "its setup did not finish; recreating it",
			wantCalls: []string{"create-kind-cluster.sh"},
			noCalls:   []string{installCall},
			wantOwned: "node-2", wantSetup: "node-2 creating",
		},
		{
			name: "an owned cluster whose create did not finish is recreated", env: []string{"STUB_CLUSTERS=" + cluster, "STUB_NEW_NODE_ID=node-2"},
			node: "node-1", owned: "node-1", setup: "node-1 creating",
			wantCode: pastClusterPhase, wantOut: "its setup did not finish; recreating it",
			wantCalls: []string{"create-kind-cluster.sh", installCall},
			wantOwned: "node-2", wantSetup: "node-2 installed",
		},
		{
			// An installed cluster whose setup record was lost looks the same as one whose create
			// was cut before the record: recreating it would delete the daemon's journal, so it is
			// refused and both records are kept for down.sh.
			name: "an owned cluster with no setup record is refused", env: []string{"STUB_CLUSTERS=" + cluster},
			node: "node-1", owned: "node-1",
			wantCode: 1, wantOut: "does not record how far its setup got",
			noCalls:   []string{"create-kind-cluster.sh", installCall, "kind delete", "kind export", "kubectl"},
			wantOwned: "node-1", wantSetup: "<absent>",
		},
		{
			name: "an owned cluster whose setup record names another node is refused", env: []string{"STUB_CLUSTERS=" + cluster},
			node: "node-1", owned: "node-1", setup: "node-0 installed",
			wantCode: 1, wantOut: "Run ./down.sh to delete it",
			noCalls:   []string{"create-kind-cluster.sh", installCall, "kind delete", "kind export", "kubectl"},
			wantOwned: "node-1", wantSetup: "node-0 installed",
		},
		{
			name: "an owned cluster whose setup record holds an unknown stage is refused", env: []string{"STUB_CLUSTERS=" + cluster},
			node: "node-1", owned: "node-1", setup: "node-1 bogus",
			wantCode: 1, wantOut: "does not record how far its setup got",
			noCalls:   []string{"create-kind-cluster.sh", installCall, "kind delete", "kind export", "kubectl"},
			wantOwned: "node-1", wantSetup: "node-1 bogus",
		},
		{
			name: "an owned cluster whose install did not finish is installed", env: []string{"STUB_CLUSTERS=" + cluster},
			node: "node-1", owned: "node-1", setup: "node-1 created",
			wantCode:  pastClusterPhase,
			wantCalls: []string{"kind export kubeconfig", installCall},
			noCalls:   []string{"create-kind-cluster.sh"},
			wantOwned: "node-1", wantSetup: "node-1 installed",
		},
		{
			name: "an installed owned cluster is reused", env: []string{"STUB_CLUSTERS=" + cluster},
			node: "node-1", owned: "node-1", setup: "node-1 installed",
			wantCode:  pastClusterPhase,
			wantCalls: []string{"kind export kubeconfig", "kubectl --context kind-demo"},
			noCalls:   []string{"create-kind-cluster.sh", installCall},
			wantOwned: "node-1", wantSetup: "node-1 installed",
		},
		{
			// A same-named cluster recreated by someone else has another container ID, so the old
			// records do not make it up.sh's.
			name: "a cluster up.sh did not create is reused without installing", env: []string{"STUB_CLUSTERS=other\n" + cluster},
			node: "node-9", owned: "node-1", setup: "node-1 created",
			wantCode: pastClusterPhase, wantOut: "up.sh did not create kind cluster demo",
			wantCalls: []string{"kubectl --context kind-demo"},
			noCalls:   []string{"create-kind-cluster.sh", installCall},
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

// AGENTSUITE_DEMO_SKIP_TEMPLATE=stateless renders the stateless tier's template but neither looks it
// up nor creates it, creates the memory tier's, and still deploys agentsessionsd with the stateless
// tier's -substrate-harness flag (the recorded demo creates that template on camera).
func TestUpSkipsATemplate(t *testing.T) {
	s := newStubs(t)
	s.node(t, "node-1")
	s.record(t, "created-cluster", "node-1")
	s.record(t, "setup-cluster", "node-1 installed")
	code, out, calls := s.run(t, "up.sh", "STUB_CLUSTERS="+cluster, "STUB_FULL=1", "AGENTSUITE_DEMO_SKIP_TEMPLATE=stateless")
	if code != 0 {
		t.Fatalf("up.sh exit %d, want 0\noutput:\n%s\ncalls:\n%s", code, out, calls)
	}
	if strings.Contains(calls, "actor-template tmpl-1") || !strings.Contains(calls, "get actor-template tmpl-2") {
		t.Errorf("want template tmpl-2 looked up and tmpl-1 left alone; calls:\n%s", calls)
	}
	if !strings.Contains(out, "agentsuite-demo/tmpl-1 (stateless tier) rendered, not created: AGENTSUITE_DEMO_SKIP_TEMPLATE=stateless") {
		t.Errorf("output does not say tmpl-1 was skipped:\n%s", out)
	}
	b, err := os.ReadFile(filepath.Join(s.work, "cluster-"+cluster, "agentsessionsd.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "- -substrate-harness=triage=agentsuite-demo/tmpl-1\n") {
		t.Errorf("agentsessionsd.yaml lacks the stateless tier's flag:\n%s", b)
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

// A failed driver.sh setup runs down.sh only when it created the cluster itself. A rerun of setup
// on an installed cluster (driver.sh asks for one when the model token is near expiry) that fails,
// before up.sh or after it reused the cluster, keeps the cluster and agentsessionsd's session
// journals in it, as up.sh does.
func TestDriverSetupDeletesOnlyTheClusterItCreated(t *testing.T) {
	model := []string{"AGENTSUITE_MODEL_BASE_URL=https://example.openai.azure.com/openai/v1", "AGENTSUITE_MODEL=m",
		"AGENTSUITE_MODEL_PATH=/chat/completions", "AZURE_MI_CLIENT_ID=00000000-0000-0000-0000-000000000000"}
	installed := func(s *stubs, t *testing.T) {
		s.node(t, "node-1")
		s.record(t, "created-cluster", "node-1")
		s.record(t, "setup-cluster", "node-1 installed")
	}
	for _, tc := range []struct {
		name       string
		before     func(*stubs, *testing.T)
		env        []string
		wantOut    string
		wantCreate bool
		wantDelete bool
	}{
		{
			name: "a rerun on an installed cluster missing a model variable keeps it", before: installed,
			env:     append([]string{"STUB_CLUSTERS=" + cluster}, model[:3]...),
			wantOut: "AZURE_MI_CLIENT_ID is not set",
		},
		{
			// up.sh reuses the cluster, then stops at the go stub (building kubectl-ate).
			name: "a rerun on an installed cluster that fails in up.sh keeps it", before: installed,
			env:     append([]string{"STUB_CLUSTERS=" + cluster}, model...),
			wantOut: "DEMO FAILED: up.sh --substrate-only",
		},
		{
			// kind lists the cluster, but setup's container lookup fails and up.sh's then finds it:
			// setup cannot tell that container from one it created, so it keeps the cluster.
			name: "a listed cluster whose container setup could not see is kept", before: installed,
			env:     append([]string{"STUB_CLUSTERS=" + cluster, "STUB_NODE_HIDDEN_ONCE=1"}, model...),
			wantOut: "DEMO FAILED: up.sh --substrate-only",
		},
		{
			name: "a setup that created the cluster deletes it", before: func(*stubs, *testing.T) {},
			env:     append([]string{"STUB_CLUSTERS_FROM_NODE=1"}, model...),
			wantOut: "DEMO FAILED: up.sh --substrate-only", wantCreate: true, wantDelete: true,
		},
		{
			// up.sh recreates an owned cluster whose first setup did not finish: a new container.
			name: "a setup that recreated an unfinished cluster deletes it",
			before: func(s *stubs, t *testing.T) {
				s.node(t, "node-0")
				s.record(t, "created-cluster", "node-0")
				s.record(t, "setup-cluster", "node-0 creating")
			},
			env:     append([]string{"STUB_CLUSTERS_FROM_NODE=1"}, model...),
			wantOut: "DEMO FAILED: up.sh --substrate-only", wantCreate: true, wantDelete: true,
		},
		{
			name: "DEMO_KEEP_ON_FAILURE keeps a cluster setup created", before: func(*stubs, *testing.T) {},
			env:     append([]string{"STUB_CLUSTERS_FROM_NODE=1", "DEMO_KEEP_ON_FAILURE=1"}, model...),
			wantOut: "DEMO FAILED: up.sh --substrate-only", wantCreate: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStubs(t)
			tc.before(s, t)
			owned, setup := s.read(t, "created-cluster"), s.read(t, "setup-cluster")
			code, out, calls := s.runArgs(t, "demo/driver.sh", []string{"setup"}, append([]string{"STUB_NEW_NODE_ID=node-1"}, tc.env...)...)
			if code == 0 {
				t.Fatalf("driver.sh setup exit 0, want a failure\noutput:\n%s\ncalls:\n%s", out, calls)
			}
			if !strings.Contains(out, tc.wantOut) {
				t.Errorf("output does not contain %q:\n%s", tc.wantOut, out)
			}
			if created := strings.Contains(calls, "create-kind-cluster.sh"); created != tc.wantCreate {
				t.Errorf("create-kind-cluster.sh called = %v, want %v:\n%s", created, tc.wantCreate, calls)
			}
			if deleted := strings.Contains(calls, "kind delete cluster --name demo"); deleted != tc.wantDelete {
				t.Fatalf("kind delete called = %v, want %v\noutput:\n%s\ncalls:\n%s", deleted, tc.wantDelete, out, calls)
			}
			if !tc.wantCreate {
				if o, st := s.read(t, "created-cluster"), s.read(t, "setup-cluster"); o != owned || st != setup {
					t.Errorf("records = %q, %q, want %q, %q kept", o, st, owned, setup)
				}
				if !strings.Contains(out, "this setup created no kind cluster, so down.sh is not run") {
					t.Errorf("output does not say down.sh is not run:\n%s", out)
				}
			}
		})
	}
}

// driver.sh setup removes a reset.log from an earlier run before anything else, even a setup that
// fails at once: with DEMO_REMOTE=hq the logs stay on the VM between runs and record.sh fetches the
// whole directory, so a stale one would be read as a reset after this setup.
func TestDriverSetupRemovesAnEarlierResetLog(t *testing.T) {
	s := newStubs(t)
	logs := t.TempDir()
	reset := filepath.Join(logs, "reset.log")
	if err := os.WriteFile(reset, []byte("RESET OK: no session actors\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, calls := s.runArgs(t, "demo/driver.sh", []string{"setup"}, "DEMO_LOGS="+logs)
	if code == 0 || !strings.Contains(out, "AGENTSUITE_MODEL_BASE_URL is not set") {
		t.Fatalf("driver.sh setup exit %d, want a failure on the unset model\noutput:\n%s\ncalls:\n%s", code, out, calls)
	}
	if _, err := os.Stat(reset); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("reset.log after driver.sh setup: %v, want it removed", err)
	}
}

// driver.sh reset deletes the session actors (sess-…) in the demo's atespace, leaves the golden
// actors, deletes the stateless tier's ActorTemplate, and prints RESET OK only once none of them is
// left; a session actor or the template still there after its wait fails the reset. Only a NotFound
// answer counts as the template being gone: any other error reading it fails the reset.
func TestDriverResetChecksTheSessionActorsAreGone(t *testing.T) {
	for _, tc := range []struct {
		name        string
		env         []string
		wantCode    int
		wantOut     string
		wantTmplDel bool // reset deletes ActorTemplate tmpl-1
	}{
		{name: "the actors go", wantOut: "RESET OK: no session actors, no ActorTemplate tmpl-1"},
		{name: "an actor stays", env: []string{"STUB_ACTORS_STAY=1"}, wantCode: 1, wantOut: "DEMO FAILED: session actors are still there: sess-a sess-b"},
		{name: "the template goes", env: []string{"STUB_TEMPLATE=present"}, wantOut: "RESET OK: no session actors, no ActorTemplate tmpl-1", wantTmplDel: true},
		{name: "the template stays", env: []string{"STUB_TEMPLATE=stays"}, wantCode: 1, wantOut: "DEMO FAILED: ActorTemplate tmpl-1 is still there", wantTmplDel: true},
		{name: "the template cannot be read", env: []string{"STUB_TEMPLATE=error"}, wantCode: 1, wantOut: "DEMO FAILED: cannot tell whether ActorTemplate tmpl-1 exists"},
		{name: "the template cannot be read after its delete", env: []string{"STUB_TEMPLATE=error-after-delete"}, wantCode: 1, wantOut: "DEMO FAILED: cannot tell whether ActorTemplate tmpl-1 exists", wantTmplDel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStubs(t)
			render := filepath.Join(s.work, "cluster-"+cluster, "render")
			if err := os.MkdirAll(render, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(render, "template-name"), []byte("tmpl-1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			// get actors lists two session actors and a golden one until a delete is logged (or
			// always, with STUB_ACTORS_STAY). get actor-template answers NotFound, as kubectl-ate
			// does at substrate 96dffb2, unless STUB_TEMPLATE says the template is there (until its
			// delete is logged, or always) or cannot be read.
			stub(t, filepath.Join(s.work, "bin"), "kubectl-ate", `echo "kubectl-ate $*" >>"$STUB_DIR/calls"
case " $* " in
*" get actors "*)
	if [ -n "${STUB_ACTORS_STAY:-}" ] || ! grep -q "delete actor" "$STUB_DIR/calls"; then
		echo '{"actors": [{"metadata": {"name": "sess-a"}}, {"metadata": {"name": "golden-1"}}, {"metadata": {"name": "sess-b"}}]}'
	else
		echo '{"actors": [{"metadata": {"name": "golden-1"}}]}'
	fi
	;;
*" get actor-template "*)
	deleted=
	if grep -q "delete actor-template" "$STUB_DIR/calls"; then deleted=1; fi
	case "${STUB_TEMPLATE:-}" in
	stays) echo tmpl-1; exit 0 ;;
	present | error-after-delete) [ -n "$deleted" ] || { echo tmpl-1; exit 0; } ;;
	esac
	case "${STUB_TEMPLATE:-}" in error | error-after-delete)
		echo 'Error: failed to get actor template "tmpl-1": rpc error: code = Unavailable desc = connection refused' >&2
		exit 1
		;;
	esac
	echo 'Error: failed to get actor template "tmpl-1": rpc error: code = NotFound desc = ActorTemplate agentsuite-demo/tmpl-1 not found' >&2
	exit 1
	;;
esac
`)
			code, out, calls := s.runArgs(t, "demo/driver.sh", []string{"reset"}, tc.env...)
			if code != tc.wantCode {
				t.Fatalf("driver.sh reset exit %d, want %d\noutput:\n%s\ncalls:\n%s", code, tc.wantCode, out, calls)
			}
			if !strings.Contains(out, tc.wantOut) {
				t.Errorf("output does not contain %q:\n%s", tc.wantOut, out)
			}
			if strings.Contains(tc.wantOut, "cannot tell") && !strings.Contains(out, "code = Unavailable desc = connection refused") {
				t.Errorf("output does not show the error reading the template:\n%s", out)
			}
			if tc.wantCode != 0 && strings.Contains(out, "RESET OK") {
				t.Errorf("a failed reset printed RESET OK:\n%s", out)
			}
			for _, n := range []string{"sess-a", "sess-b"} {
				if !strings.Contains(calls, "delete actor "+n+" ") {
					t.Errorf("reset did not delete %s:\n%s", n, calls)
				}
			}
			if strings.Contains(calls, "delete actor golden-1") {
				t.Errorf("reset deleted the golden actor:\n%s", calls)
			}
			if got := strings.Contains(calls, "delete actor-template tmpl-1 "); got != tc.wantTmplDel {
				t.Errorf("reset deleted ActorTemplate tmpl-1: %v, want %v\ncalls:\n%s", got, tc.wantTmplDel, calls)
			}
		})
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
	azure := []string{
		"AGENTSUITE_MODEL_BASE_URL=https://res.openai.azure.com", "AGENTSUITE_MODEL=dep", "MODEL_API_KEY=k",
		"AGENTSUITE_MODEL_PATH=/openai/deployments/dep/chat/completions?api-version=2024-10-21",
		"AGENTSUITE_MODEL_AUTH_HEADER=api-key",
	}
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
			wantOut: "the model settings (AGENTSUITE_MODEL_BASE_URL, AGENTSUITE_MODEL 'm', AGENTSUITE_MODEL_PATH, AGENTSUITE_MODEL_AUTH_HEADER) would not start agentsessionsd: emit: check-model: the base URL is not http or https (the value is not shown, since it may hold a credential); nothing was changed",
		},
		{
			// emit is built and run as a binary, so go run's own "exit status 1" line is not
			// appended to the reason.
			name: "a base URL with a port that is not a number, reported alone",
			env: func(*testing.T) []string {
				return append(provider, "AGENTSUITE_MODEL_BASE_URL=http://host:abc/v1")
			},
			wantOut: "would not start agentsessionsd: emit: check-model: the base URL is not a valid URL (the value is not shown, since it may hold a credential); nothing was changed",
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
			wantOut: "has no host",
		},
		{
			// A credential in the URL would be printed and rendered into the Deployment, outside the
			// Secret that holds MODEL_API_KEY; the refusal does not repeat it either.
			name: "a base URL with userinfo",
			env: func(*testing.T) []string {
				return append(provider, "AGENTSUITE_MODEL_BASE_URL=https://user:secret@llm.example/v1")
			},
			wantOut: "carries userinfo",
		},
		{
			name: "a base URL with only userinfo for a host",
			env: func(*testing.T) []string {
				return append(provider, "AGENTSUITE_MODEL_BASE_URL=https://secret@/v1")
			},
			wantOut: "carries userinfo",
		},
		{
			name: "a base URL with userinfo and no scheme",
			env: func(*testing.T) []string {
				return append(provider, "AGENTSUITE_MODEL_BASE_URL=user:secret@llm.example/v1")
			},
			wantOut: "is not http or https",
		},
		{
			name: "a base URL whose userinfo reads as a port",
			env: func(*testing.T) []string {
				return append(provider, "AGENTSUITE_MODEL_BASE_URL=https://user:secret/x@llm.example/v1")
			},
			wantOut: "is not a valid URL",
		},
		{
			name: "a base URL with a query",
			env: func(*testing.T) []string {
				return append(provider, "AGENTSUITE_MODEL_BASE_URL=https://llm.example/v1?api-key=secret")
			},
			wantOut: "carries a query",
		},
		{
			name: "a base URL with a fragment",
			env: func(*testing.T) []string {
				return append(provider, "AGENTSUITE_MODEL_BASE_URL=https://llm.example/v1#secret")
			},
			wantOut: "carries a fragment",
		},
		{
			name: "a base URL with a port that is not a number",
			env: func(*testing.T) []string {
				return append(provider, "AGENTSUITE_MODEL_BASE_URL=http://host:abc/v1")
			},
			wantOut: "is not a valid URL",
		},
		{
			name: "a base URL with a bad escape",
			env: func(*testing.T) []string {
				return append(provider, "AGENTSUITE_MODEL_BASE_URL=http://h%zz/v1")
			},
			wantOut: "is not a valid URL",
		},
		{
			name: "a base URL with another scheme",
			env: func(*testing.T) []string {
				return append(provider, "AGENTSUITE_MODEL_BASE_URL=ftp://llm.example/v1")
			},
			wantOut: "the base URL is not http or https (the value is not shown, since it may hold a credential); nothing was changed",
		},
		{
			// Azure OpenAI's api-key belongs in MODEL_API_KEY, which up.sh stores in a Secret; in the
			// path it would be rendered into the Deployment and logged by the daemon.
			name: "an Azure path with a key in its query",
			env: func(*testing.T) []string {
				return append(azure, "AGENTSUITE_MODEL_PATH=/openai/deployments/dep/chat/completions?api-version=2024-10-21&api-key=secret")
			},
			wantOut: "the model path carries a query parameter other than api-version; pass the provider's key in MODEL_API_KEY (the value is not shown, since it may hold a credential); nothing was changed",
		},
		{
			name: "an Azure path whose api-version is a key",
			env: func(*testing.T) []string {
				return append(azure, "AGENTSUITE_MODEL_PATH=/openai/deployments/dep/chat/completions?api-version=secret")
			},
			wantOut: "the model path's api-version is not one date",
		},
		{
			name: "an Azure path with userinfo",
			env: func(*testing.T) []string {
				return append(azure, "AGENTSUITE_MODEL_PATH=//user:secret@res.openai.azure.com/chat/completions")
			},
			wantOut: "does not start with a single '/'",
		},
		{
			// The api-version belongs in the path: a query on the base URL could hold a credential.
			name: "an Azure base URL with the api-version in its query",
			env: func(*testing.T) []string {
				return append(azure, "AGENTSUITE_MODEL_BASE_URL=https://res.openai.azure.com?api-version=2024-10-21&api-key=secret")
			},
			wantOut: "the base URL carries a query",
		},
		{
			name: "a path with whitespace",
			env: func(*testing.T) []string {
				return append(azure, "AGENTSUITE_MODEL_PATH=/chat/completions - -evil")
			},
			wantOut: "AGENTSUITE_MODEL_PATH contains whitespace or a control character; nothing was changed",
		},
		{
			name: "a path naming a placeholder",
			env: func(*testing.T) []string {
				return append(azure, "AGENTSUITE_MODEL_PATH=/DAEMON_IMAGE")
			},
			wantOut: "AGENTSUITE_MODEL_PATH contains DAEMON_IMAGE, a placeholder of deploy/agentsessionsd.yaml",
		},
		{
			name: "an auth header that is not a header name",
			env: func(*testing.T) []string {
				return append(azure, "AGENTSUITE_MODEL_AUTH_HEADER=api_key")
			},
			wantOut: "the auth header is not an HTTP header name",
		},
		{
			// "- -model=gpt:" is a YAML map, not a string; the Deployment would fail to apply
			// only after the Secret had changed.
			name:    "a model ending with ':'",
			env:     func(*testing.T) []string { return append(azure, "AGENTSUITE_MODEL=gpt:") },
			wantOut: "AGENTSUITE_MODEL ends with ':', which deploy/agentsessionsd.yaml would read as a map key; nothing was changed",
		},
		{
			name:    "a base URL ending with ':'",
			env:     func(*testing.T) []string { return append(provider, "AGENTSUITE_MODEL_BASE_URL=https://llm.example:") },
			wantOut: "AGENTSUITE_MODEL_BASE_URL ends with ':', which deploy/agentsessionsd.yaml would read as a map key; nothing was changed",
		},
		{
			name:    "a path ending with ':'",
			env:     func(*testing.T) []string { return append(azure, "AGENTSUITE_MODEL_PATH=/openai/deployments/dep:") },
			wantOut: "AGENTSUITE_MODEL_PATH ends with ':', which deploy/agentsessionsd.yaml would read as a map key; nothing was changed",
		},
		{
			name:    "an auth header ending with ':'",
			env:     func(*testing.T) []string { return append(azure, "AGENTSUITE_MODEL_AUTH_HEADER=x:") },
			wantOut: "AGENTSUITE_MODEL_AUTH_HEADER ends with ':', which deploy/agentsessionsd.yaml would read as a map key; nothing was changed",
		},
		{
			name: "a path without a base URL",
			env: func(*testing.T) []string {
				return []string{"AGENTSUITE_MODEL_PATH=/openai/deployments/dep/chat/completions?api-version=2024-10-21"}
			},
			wantOut: "AGENTSUITE_MODEL_PATH is set without AGENTSUITE_MODEL_BASE_URL; set both for a real provider, or unset AGENTSUITE_MODEL_PATH for the fake model; nothing was changed",
		},
		{
			name:    "an auth header without a base URL",
			env:     func(*testing.T) []string { return []string{"AGENTSUITE_MODEL_AUTH_HEADER=api-key"} },
			wantOut: "AGENTSUITE_MODEL_AUTH_HEADER is set without AGENTSUITE_MODEL_BASE_URL; set both for a real provider, or unset AGENTSUITE_MODEL_AUTH_HEADER for the fake model; nothing was changed",
		},
		{
			// The key and prompts would cross the network unencrypted. The refusal names the host
			// and never the key ("secret" is checked below).
			name: "a key over plain http to a remote host",
			env: func(*testing.T) []string {
				return []string{"AGENTSUITE_MODEL_BASE_URL=http://llm.example:8000/v1", "AGENTSUITE_MODEL=m", "MODEL_API_KEY=secret-key"}
			},
			wantOut: `would not start agentsessionsd: emit: modelconfig: the model base URL uses plain http to host "llm.example" while a model API key is set`,
		},
		{
			name:    "a binding for another atespace",
			env:     func(t *testing.T) []string { return []string{"STUB_BINDING=" + binding(t, "other")} },
			wantOut: "binding-microvm.json names atespace 'other', but the demo deploys into atespace agentsuite-demo; nothing was changed",
		},
		{
			name: "a gVisor binding for another atespace",
			env: func(t *testing.T) []string {
				return []string{"AGENTSUITE_DEMO_SANDBOX=gvisor", "STUB_BINDING=" + binding(t, "other")}
			},
			wantOut: "binding.json names atespace 'other', but the demo deploys into atespace agentsuite-demo; nothing was changed",
		},
		{
			// The micro-VM class is the default; without KVM it is refused, never swapped for gVisor.
			name:    "micro-VM on a Docker host without KVM",
			env:     func(*testing.T) []string { return []string{"STUB_NO_KVM=1"} },
			wantOut: "AGENTSUITE_DEMO_SANDBOX=microvm needs /dev/kvm in the Docker host, and 'docker run --device /dev/kvm' failed; set AGENTSUITE_DEMO_SANDBOX=gvisor on a host without KVM (an arm64 Mac); nothing was changed",
		},
		{
			name:    "a template tier to skip that is not a tier",
			env:     func(*testing.T) []string { return []string{"AGENTSUITE_DEMO_SKIP_TEMPLATE=both"} },
			wantOut: "AGENTSUITE_DEMO_SKIP_TEMPLATE=both: want stateless or memory; nothing was changed",
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
			if strings.Contains(out, "secret") {
				t.Errorf("output repeats a credential from the base URL:\n%s", out)
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

// The model base URL and model are copied into the Deployment as written: '&', '#' and '\' are not
// special to the substitution. up.sh refuses a URL with a query or fragment (it could carry a
// credential), so the URL's '&' is in its path.
func TestUpRendersTheDaemon(t *testing.T) {
	const (
		url   = "https://llm.example/v1&tenant=a/b"
		model = `gpt&x#1\1`
	)
	for _, tc := range []struct {
		name        string
		env         []string
		wantLines   []string
		wantSecret  string // the Secret call that precedes the apply
		wantVersion string
		wantModel   string // the -model emit render gives the harness; "" keeps the fixture's
	}{
		{
			name: "fake provider",
			wantLines: []string{"- -model=triage-model", "- -model-base-url=http://fakemodel.agentsuite-demo.svc:8080/v1",
				"- -model-path=/chat/completions", "- -model-auth-header=Authorization"},
			wantSecret:  "delete secret agentsessionsd-model --ignore-not-found",
			wantVersion: "none",
		},
		{
			name: "real provider",
			env:  []string{"AGENTSUITE_MODEL_BASE_URL=" + url, "AGENTSUITE_MODEL=" + model, "MODEL_API_KEY=k", "STUB_SECRET_VERSION=7"},
			wantLines: []string{"- -model=" + model, "- -model-base-url=" + url,
				"- -model-path=/chat/completions", "- -model-auth-header=Authorization"},
			wantSecret:  "apply --server-side",
			wantVersion: "7",
			wantModel:   model,
		},
		{
			// A *.svc provider may take a key over plain http (a trusted-network exception).
			name: "service-name provider over http with a key",
			env: []string{"AGENTSUITE_MODEL_BASE_URL=http://vllm.models.svc.cluster.local:8000/v1", "AGENTSUITE_MODEL=m",
				"MODEL_API_KEY=k", "STUB_SECRET_VERSION=9"},
			wantLines: []string{"- -model=m", "- -model-base-url=http://vllm.models.svc.cluster.local:8000/v1",
				"- -model-path=/chat/completions", "- -model-auth-header=Authorization"},
			wantSecret:  "apply --server-side",
			wantVersion: "9",
			wantModel:   "m",
		},
		{
			// An Azure OpenAI deployment: the api-version rides in the path, the key in the api-key
			// header from the Secret.
			name: "Azure OpenAI",
			env: []string{
				"AGENTSUITE_MODEL_BASE_URL=https://res.openai.azure.com", "AGENTSUITE_MODEL=dep", "MODEL_API_KEY=azure-key",
				"AGENTSUITE_MODEL_PATH=/openai/deployments/dep/chat/completions?api-version=2024-10-21",
				"AGENTSUITE_MODEL_AUTH_HEADER=api-key", "STUB_SECRET_VERSION=8",
			},
			wantLines: []string{"- -model=dep", "- -model-base-url=https://res.openai.azure.com",
				"- -model-path=/openai/deployments/dep/chat/completions?api-version=2024-10-21", "- -model-auth-header=api-key"},
			wantSecret:  "apply --server-side",
			wantVersion: "8",
			wantModel:   "dep",
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
			// The harness requests the provider's model (an Azure deployment name), or the
			// fixture's own with the fake model.
			if got, err := os.ReadFile(filepath.Join(s.dir, "render-model")); err != nil || string(got) != tc.wantModel {
				t.Errorf("emit render -model = %q (err %v), want %q", got, err, tc.wantModel)
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
			for _, p := range []string{"DAEMON_NAME", "DAEMON_IMAGE", "MODEL_ID", "MODEL_BASE_URL", "MODEL_PATH", "MODEL_AUTH_HEADER", "SUBSTRATE_HARNESS_FLAG", "MODEL_SECRET_VERSION"} {
				if strings.Contains(string(b), p) {
					t.Errorf("rendered manifest still contains %s", p)
				}
			}
			// The memory tier gets its own Deployment and Service, with the same model settings and
			// Secret version and the memory-tier harness flag.
			memPath := filepath.Join(s.work, "cluster-"+cluster, "agentsessionsd-memory.yaml")
			mb, err := os.ReadFile(memPath)
			if err != nil {
				t.Fatal(err)
			}
			wantMem := strings.ReplaceAll(string(b), "- -substrate-harness=triage=agentsuite-demo/tmpl-1", "- -substrate-harness=triage-memory=agentsuite-demo/tmpl-2,memory")
			wantMem = strings.ReplaceAll(wantMem, "name: agentsessionsd\n  namespace:", "name: agentsessionsd-memory\n  namespace:")
			wantMem = strings.ReplaceAll(wantMem, "app: agentsessionsd}", "app: agentsessionsd-memory}")
			if string(mb) != wantMem {
				t.Errorf("memory-tier manifest:\n%s\nwant:\n%s", mb, wantMem)
			}
			if !strings.Contains(calls, "apply -f "+memPath) || !strings.Contains(calls, "rollout status deploy/agentsessionsd-memory") {
				t.Errorf("the memory-tier daemon was not applied and waited for:\n%s", calls)
			}
			// The key reaches kubectl over stdin only: never the manifest, argv or the output.
			if strings.Contains(string(b), "azure-key") || strings.Contains(calls, "azure-key") || strings.Contains(out, "azure-key") {
				t.Errorf("the API key leaked into the manifest, a call or the output")
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

// demoStubs prepares s for demo.sh in real-provider mode: an agentctl that keeps each session's
// journal in $STUB_DIR/j-<session> and prints records the way agentctl does, a kubectl-ate that
// reports the actor states agentctl left, a replay that counts the completed turns in the journal,
// and the template name up.sh would have rendered. STUB_FAULT makes agentctl or replay misbehave in
// one way, so a test can show demo.sh's journal checks catch it.
func demoStubs(t *testing.T, s *stubs) {
	t.Helper()
	bin := filepath.Join(s.work, "bin")
	log := `echo "$(basename "$0") $*" >>"$STUB_DIR/calls"` + "\n"
	stub(t, bin, "agentctl", log+`
cmd=$1
shift
sess= input= at=
while [ $# -gt 0 ]; do
	case "$1" in
	--session) sess=$2; shift ;;
	--input) input=$2; shift ;;
	--at) at=$2; shift ;;
	esac
	shift
done
j="$STUB_DIR/j-$sess"
rec() {
	n=$(($(wc -l <"$j") + 1))
	printf 'seq=%-3d %s\n' "$n" "$*" >>"$j"
	printf 'seq=%-3d %s\n' "$n" "$*"
}
case "$cmd" in
list) ;;
create)
	: >"$STUB_DIR/j-sess-1"
	echo sess-1
	;;
get) echo "session $sess" ;;
exec)
	turn=$(($(grep -c EVENT_END "$j") + 1))
	[ "$STUB_FAULT $turn" = "lost-input 2" ] || rec "EVENT_INPUT  user: $input"
	[ "$STUB_FAULT $turn" = "no-model-call 2" ] || [ "$STUB_FAULT $turn" = "no-model-call-4 4" ] ||
		[ "$STUB_FAULT $sess" = "no-child-model-call child-1" ] || rec "EVENT_MODEL_CALL"
	rec "EVENT_OUTPUT  assistant: a real reply $turn"
	rec "EVENT_END"
	echo ACTOR_STATE_RUNNING >"$STUB_DIR/state-$sess"
	;;
suspend)
	[ "$STUB_FAULT" = no-lifecycle ] || rec "EVENT_LIFECYCLE"
	echo ACTOR_STATE_SUSPENDED >"$STUB_DIR/state-$sess"
	: >"$STUB_DIR/snap-$sess"
	;;
resume)
	rec "EVENT_LIFECYCLE"
	echo ACTOR_STATE_RUNNING >"$STUB_DIR/state-$sess"
	;;
replay) cat "$j" ;;
fork)
	if [ "$STUB_FAULT" = whole-parent-fork ]; then
		cp "$j" "$STUB_DIR/j-child-1"
	else
		awk -v at="$at" '{ s = $1; sub("seq=", "", s); if (s + 0 <= at + 0) print }' "$j" >"$STUB_DIR/j-child-1"
	fi
	if [ "$STUB_FAULT" = fork-writes-parent ]; then
		printf 'seq=%-3d %s\n' 99 "EVENT_INPUT  user: stray" >>"$j"
	fi
	echo "child child-1 parent=$sess fork_seq=$at name=after-turn-1"
	;;
*) exit 2 ;;
esac
`)
	stub(t, bin, "kubectl-ate", log+`
prev= name= json=
for a; do
	[ "$prev" = actor ] && name=$a
	[ "$a" = json ] && json=1
	prev=$a
done
st=$(cat "$STUB_DIR/state-$name" 2>/dev/null || echo ACTOR_STATE_SUSPENDED)
if [ -z "$json" ]; then
	echo "$name $st"
	exit 0
fi
snaps='[]'
if [ -f "$STUB_DIR/snap-$name" ]; then
	snaps='[{"generation": 1, "fidelity": "FULL", "durableSnapshot": {"status": "SNAPSHOT_STORAGE_STATUS_COMPLETED", "object": {"snapshotUri": "gs://snap/1"}}}]'
fi
echo "{\"status\": {\"state\": \"$st\", \"snapshots\": $snaps}}"
`)
	stub(t, bin, "replay", log+`
prev= sess=
for a; do
	[ "$prev" = -session ] && sess=$a
	prev=$a
done
n=$(grep -c EVENT_END "$STUB_DIR/j-$sess")
[ "$STUB_FAULT" != replay-skips-a-turn ] || n=$((n - 1))
echo "journal: hash chain verifies locally"
echo "re-executed $n completed turns"
`)
	render := filepath.Join(s.work, "cluster-"+cluster, "render")
	if err := os.MkdirAll(render, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(render, "template-name"), []byte("tmpl-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// With a real provider demo.sh cannot check reply text, so its claims rest on the journal: the
// inputs in order, one model call per turn, the suspend and resume records, the fork child's prefix,
// an untouched parent, and the turns the replay re-ran. Each check must fail the demo when the
// journal does not hold what it claims, and the fake model must be neither port-forwarded nor asked.
func TestDemoRealProviderJournalChecks(t *testing.T) {
	for _, tc := range []struct {
		fault   string
		wantOut string // "" passes
	}{
		{fault: ""},
		{fault: "lost-input", wantOut: "DEMO FAILED: journal inputs are not the 3 turns: my laptop will not boot|I was billed twice this month"},
		{fault: "no-model-call", wantOut: "DEMO FAILED: journal does not record 3 model calls"},
		{fault: "no-lifecycle", wantOut: "DEMO FAILED: journal does not record the suspend and the resume"},
		{fault: "no-model-call-4", wantOut: "DEMO FAILED: journal does not record a model call for the 4th turn"},
		{fault: "no-child-model-call", wantOut: "DEMO FAILED: child journal does not record turn 1's model call plus its own"},
		{fault: "replay-skips-a-turn", wantOut: "DEMO FAILED: replay did not re-execute the 4 turns"},
		{fault: "whole-parent-fork", wantOut: "DEMO FAILED: child journal inputs are not turn 1 plus its own"},
		{fault: "fork-writes-parent", wantOut: "DEMO FAILED: parent journal changed"},
	} {
		name := tc.fault
		if name == "" {
			name = "a journal that holds every claim"
		}
		t.Run(name, func(t *testing.T) {
			s := newStubs(t)
			demoStubs(t, s)
			code, out, calls := s.run(t, "demo.sh", "STUB_FULL=1", "STUB_FAULT="+tc.fault,
				"AGENTSUITE_MODEL_BASE_URL=https://res.openai.azure.com", "STUB_DEPLOYED_URL=https://res.openai.azure.com")
			if tc.wantOut == "" {
				if code != 0 {
					t.Fatalf("demo.sh exit %d, want 0\noutput:\n%s\ncalls:\n%s", code, out, calls)
				}
				for _, want := range []string{
					"model: a real provider",
					"the journal records the 3 inputs in order and 3 model calls the host made",
					"child child-1 forked at seq 4",
					"Done in",
					"5 turns called the provider (each journaled a model call); the replay called it 0 times (controller count)",
				} {
					if !strings.Contains(out, want) {
						t.Errorf("output has no %q:\n%s", want, out)
					}
				}
				// The replay relies on the controller's count, not the fake's counter.
				if !strings.Contains(calls, "-stats-url \n") && !strings.Contains(calls, "-stats-url\n") {
					t.Errorf("replay was not run with an empty -stats-url:\n%s", calls)
				}
			} else if code != 1 || !strings.Contains(out, tc.wantOut) {
				t.Fatalf("demo.sh exit %d, want 1 with %q\noutput:\n%s", code, tc.wantOut, out)
			}
			for _, c := range []string{"svc/fakemodel", "curl "} {
				if strings.Contains(calls, c) {
					t.Errorf("real-provider demo called %q:\n%s", c, calls)
				}
			}
		})
	}
}

// demo.sh takes its provider mode from the deployed daemon: a run whose AGENTSUITE_MODEL_BASE_URL
// (unset for the fake model) is not the daemon's -model-base-url fails before it forwards a port or
// runs a turn, so a fake-backed run is never reported as a real provider's, or the reverse. The
// refusal names the fake model or a real provider and quotes neither URL, since the caller's may
// hold a credential.
func TestDemoRefusesAProviderModeMismatch(t *testing.T) {
	const (
		fake   = "http://fakemodel.agentsuite-demo.svc:8080/v1"
		advice = " (neither URL is shown, since it may hold a credential): give demo.sh the AGENTSUITE_MODEL_BASE_URL up.sh was given, unset for the fake model"
	)
	for _, tc := range []struct {
		name     string
		deployed string // STUB_DEPLOYED_URL
		env      []string
		wantOut  string
	}{{
		name:     "fake model deployed, real provider asked for",
		deployed: fake,
		env:      []string{"AGENTSUITE_MODEL_BASE_URL=https://res.openai.azure.com"},
		wantOut:  "DEMO FAILED: agentsessionsd was deployed for the fake model, but this run expects a real provider" + advice,
	}, {
		name:     "real provider deployed, fake model asked for",
		deployed: "https://res.openai.azure.com",
		wantOut:  "DEMO FAILED: agentsessionsd was deployed for a real provider, but this run expects the fake model" + advice,
	}, {
		name:     "another real provider deployed",
		deployed: "https://other.openai.azure.com",
		env:      []string{"AGENTSUITE_MODEL_BASE_URL=https://res.openai.azure.com"},
		wantOut:  "DEMO FAILED: agentsessionsd was deployed for a real provider at another URL than this run's AGENTSUITE_MODEL_BASE_URL" + advice,
	}, {
		name:     "the fake model's URL asked for as a real provider",
		deployed: "https://res.openai.azure.com",
		env:      []string{"AGENTSUITE_MODEL_BASE_URL=" + fake},
		wantOut:  "DEMO FAILED: agentsessionsd was deployed for a real provider, but this run expects the fake model" + advice,
	}, {
		name:     "a credential in the URL asked for",
		deployed: "https://res.openai.azure.com",
		env:      []string{"AGENTSUITE_MODEL_BASE_URL=https://user:secret@res.openai.azure.com/v1?api-key=secret#secret"},
		wantOut:  "DEMO FAILED: agentsessionsd was deployed for a real provider at another URL than this run's AGENTSUITE_MODEL_BASE_URL" + advice,
	}, {
		name:     "no model base URL deployed",
		deployed: "none",
		wantOut:  "DEMO FAILED: deployment agentsuite-demo/agentsessionsd has no -model-base-url; run up.sh first",
	}, {
		name:     "no daemon deployed",
		deployed: "missing",
		wantOut:  "DEMO FAILED: cannot read deployment agentsuite-demo/agentsessionsd; run up.sh first",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStubs(t)
			demoStubs(t, s)
			code, out, calls := s.run(t, "demo.sh", append([]string{"STUB_DEPLOYED_URL=" + tc.deployed}, tc.env...)...)
			if code != 1 || !strings.Contains(out, tc.wantOut) {
				t.Fatalf("demo.sh exit %d, want 1 with %q\noutput:\n%s", code, tc.wantOut, out)
			}
			for _, u := range []string{"secret", "res.openai.azure.com", "other.openai.azure.com", "fakemodel"} {
				if strings.Contains(out, u) {
					t.Errorf("demo.sh output quotes %q:\n%s", u, out)
				}
			}
			for _, c := range []string{"port-forward", "agentctl ", "curl "} {
				if strings.Contains(calls, c) {
					t.Errorf("demo.sh called %q before refusing the mismatch:\n%s", c, calls)
				}
			}
		})
	}
}

// demo.tape hides the recording around each wait driver.sh show runs out of frame, so it must list
// one hidden pair (the "⠿" start and the "✓" end) per `run -w` call: a call the tape does not know
// shifts every later cut onto the wrong wait. driver.sh keeps `run -w` out of loops, so the count of
// calls is the count of waits.
func TestDemoTapeMatchesDriverWaits(t *testing.T) {
	driver, err := os.ReadFile("demo/driver.sh")
	if err != nil {
		t.Fatal(err)
	}
	tape, err := os.ReadFile("demo/demo.tape")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	for _, line := range strings.Split(string(driver), "\n") {
		f := strings.Fields(line)
		if len(f) > 1 && f[0] == "run" && (f[1] == "-w" || (f[1] == "-x" && len(f) > 2 && f[2] == "-w")) {
			calls++
		}
	}
	starts := strings.Count(string(tape), "\nWait+Line@5m /⠿")
	ends := strings.Count(string(tape), "\nWait+Line@5m /✓")
	if calls == 0 || starts != calls || ends != calls {
		t.Fatalf("driver.sh makes %d `run -w` calls; demo.tape waits for %d starts and %d ends", calls, starts, ends)
	}
}

// present.awk holds each line's newline until the next line, so vhs's Wait+Line sees the newest
// line, and a line starting with "@@R " overwrites the line before it.
func TestDemoPresent(t *testing.T) {
	cmd := exec.Command("awk", "-f", "demo/present.awk")
	cmd.Stdin = strings.NewReader("title\n  ⠿ waiting …\n@@R   ✓ done in 3 s\nnext\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if want := "title\n  ⠿ waiting …\r\x1b[K  ✓ done in 3 s\nnext\n"; string(out) != want {
		t.Fatalf("present.awk wrote %q, want %q", out, want)
	}
}

// trace-hop.py shows, for one hop of the traced turn, the earliest matching line across the pods'
// logs (not the first file's), the keys it is asked for under their short names, and a container
// line's message without the Go log timestamp that repeats the line's own time; it fails, naming
// the hop, when nothing matches.
func TestDemoTraceHop(t *testing.T) {
	python3, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// The first pod's log has the later call; the earliest is in the second's, with a longer fraction.
	podA := write("a.log", `[pod/api-a/server] {"time":"2026-10-15T05:23:31.1Z","msg":"Handle RPC","method":"/ateapi.Control/ResumeActor","req":{"actor":{"name":"sess-1"}}}`+"\n")
	podB := write("b.log", `[pod/api-b/server] {"time":"2026-10-15T05:23:29.50412Z","msg":"Handle RPC","method":"/ateapi.Control/GetActor","req":{"name":"sess-1"}}`+"\n"+
		`[pod/api-b/server] {"time":"2026-10-15T05:23:29.510999Z","msg":"Handle RPC","method":"/ateapi.Control/CreateActor","req":{"actor":{"metadata":{"name":"sess-1"}}}}`+"\n")
	harness := write("harness.log", `{"time":"2026-10-15T05:23:32.622Z","message":"2026/10/15 05:23:32 harness: model m used 41 input and 37 output tokens"}`+"\n")
	for _, tc := range []struct {
		name    string
		args    []string
		want    string
		wantErr string
	}{
		{
			name: "the earliest match across files, keys under their short names",
			args: []string{"ate-api", `"method":"/ateapi\.Control/(CreateActor|ResumeActor)".*"sess-1"`, "method,actor=req.actor.metadata.name,actor=req.actor.name", podA, podB},
			want: "ate-api         05:23:29.510 Handle RPC method=/ateapi.Control/CreateActor actor=sess-1\n",
		},
		{
			name: "a message's Go log timestamp is dropped",
			args: []string{"harness", `harness: model .* used [0-9]+ input`, "", harness},
			want: "harness         05:23:32.622 harness: model m used 41 input and 37 output tokens\n",
		},
		{
			name:    "no match",
			args:    []string{"worker", `RestoreWorkload`, "method", podA, podB},
			wantErr: "trace-hop.py: no worker line matches RestoreWorkload in a.log, b.log",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(python3, append([]string{"-I", "demo/trace-hop.py"}, tc.args...)...)
			var stdout, stderr strings.Builder
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(stderr.String(), tc.wantErr) {
					t.Fatalf("err %v, stderr %q; want a failure saying %q", err, stderr.String(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("%v: %s", err, stderr.String())
			}
			if stdout.String() != tc.want {
				t.Fatalf("got %q, want %q", stdout.String(), tc.want)
			}
		})
	}
}
