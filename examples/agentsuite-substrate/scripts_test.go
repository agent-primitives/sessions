package scripts_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These tests run up.sh and down.sh against stub tools, to pin how they decide whether the demo's
// kind cluster exists and whose it is. Nothing here touches Docker or a cluster: every tool the
// scripts call before the cluster phase ends is a stub that appends its arguments to a call log.

const cluster = "demo"

// stubs is one run's fake environment.
type stubs struct {
	dir  string // the stubs' state: the call log and the control-plane container ID
	work string // AGENTSUITE_DEMO_WORK
	env  []string
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
	// docker inspect reports the control-plane container ID kept in $STUB_DIR/node-id.
	stub(t, bin, "docker", log+`
case "$1" in
info) echo arm64 ;;
inspect) [ -f "$STUB_DIR/node-id" ] && cat "$STUB_DIR/node-id" || exit 1 ;;
esac
`)
	// go stands in for the first step after the cluster phase (building kubectl-ate) and stops the
	// run there with a status the tests recognize.
	stub(t, bin, "go", log+"exit 97\n")
	for _, name := range []string{"ko", "git", "python3", "kubectl", "curl"} {
		stub(t, bin, name, log)
	}
	// create-kind-cluster.sh creates the control-plane container, then exits STUB_CREATE_RC.
	stub(t, filepath.Join(substrate, "hack"), "create-kind-cluster.sh", `echo "create-kind-cluster.sh" >>"$STUB_DIR/calls"
echo "${STUB_NEW_NODE_ID:-node-1}" >"$STUB_DIR/node-id"
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
// calls the stubs logged.
func (s *stubs) run(t *testing.T, script string, env ...string) (int, string, string) {
	t.Helper()
	if err := os.Remove(filepath.Join(s.dir, "calls")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	cmd := exec.Command("./" + script)
	cmd.Env = append(append([]string{}, s.env...), env...)
	out, err := cmd.CombinedOutput()
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
