package substratekind_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func bootstrapFixture(t *testing.T) (string, string) {
	t.Helper()
	prerequisites(t)
	dir, state := fixture(t, map[string]string{
		"docker": `case "$1" in
  info) echo x86_64 ;;
  ps) if [[ -f "$TEST_DIR/created" ]]; then echo node; fi ;;
  *) exit 1 ;;
esac`,
		"git": `case "$*" in
  "clone --quiet https://github.com/agent-substrate/substrate $TEST_DIR/substrate")
    echo clone >> "$TEST_DIR/bootstrap-calls"
    [[ "$TEST_BOOTSTRAP_FAILURE" != clone ]] || exit 128
    mkdir -p "$SUBSTRATE_DIR"
    cp -R "$TEST_DIR/upstream/." "$SUBSTRATE_DIR"
    [[ "$TEST_BOOTSTRAP_FAILURE" != partial-clone ]] || exit 128 ;;
  "-C $SUBSTRATE_DIR status --porcelain") echo status >> "$TEST_DIR/bootstrap-calls" ;;
  "-C $SUBSTRATE_DIR fetch --quiet origin $TEST_PIN")
    echo fetch >> "$TEST_DIR/bootstrap-calls"
    [[ "$TEST_BOOTSTRAP_FAILURE" != fetch ]] || exit 128 ;;
  "-C $SUBSTRATE_DIR checkout --quiet --detach $TEST_PIN")
    echo checkout >> "$TEST_DIR/bootstrap-calls"
    [[ "$TEST_BOOTSTRAP_FAILURE" != checkout ]] || exit 128
    touch "$SUBSTRATE_DIR/.pinned" ;;
  *) echo unexpected-git-invocation >&2; exit 1 ;;
esac`,
		"go": `[[ "$PWD" == "$SUBSTRATE_DIR" ]]
case "$*" in
  "run ./cmd/ate-setup "*)
    [[ " $* " == *" --kubeconfig=$KUBECONFIG "* ]]
    [[ " $* " == *" --atenet-dataplane=agentgateway deploy ate-system "* ]]
    echo install >> "$TEST_DIR/bootstrap-calls" ;;
  "build -o "*)
    echo client-build >> "$TEST_DIR/bootstrap-calls"
    echo bootstrap-complete >&2; exit 42 ;;
  *) echo unexpected-go-invocation >&2; exit 1 ;;
esac`,
	})
	t.Setenv("SUBSTRATE_DIR", filepath.Join(dir, "substrate"))
	t.Setenv("TEST_PIN", substratePin(t))
	t.Setenv("SANDBOX_CLASS", "gvisor")
	t.Setenv("MODEL_SOURCE", "stub")
	t.Setenv("TEST_BOOTSTRAP_FAILURE", "")

	for name, body := range map[string]string{
		"kind.sh": `[[ "$PWD" == "$SUBSTRATE_DIR" ]]
touch "$TEST_DIR/wrapper-invoked"
[[ -f "$SUBSTRATE_DIR/.pinned" ]] || { echo unpinned-kind-wrapper-refused >&2; exit 1; }
case "$*" in
  "get clusters")
    echo kind-list >> "$TEST_DIR/bootstrap-calls"
    if [[ -f "$TEST_DIR/created" ]]; then echo "$CLUSTER"; fi ;;
  "export kubeconfig --name $CLUSTER --kubeconfig $KUBECONFIG")
    echo kind-export >> "$TEST_DIR/bootstrap-calls" ;;
  *) echo unexpected-kind-invocation >&2; exit 1 ;;
esac`,
		"create-kind-cluster.sh": `[[ "$PWD" == "$SUBSTRATE_DIR" ]]
echo create >> "$TEST_DIR/bootstrap-calls"
touch "$TEST_DIR/created"`,
	} {
		path := filepath.Join(dir, "upstream", "hack", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/usr/bin/env bash\nset -euo pipefail\n"+body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "upstream", ".git"), 0o700); err != nil {
		t.Fatal(err)
	}

	// Allow only shell utilities and the declared prerequisite, never host cluster tools.
	tools := []string{"bash", "env", "dirname", "mkdir", "rmdir", "sed", "grep", "cp", "touch", "jq"}
	if _, err := exec.LookPath("timeout"); err == nil {
		tools = append(tools, "timeout")
	} else {
		tools = append(tools, "gtimeout")
	}
	for _, tool := range tools {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(dir, tool)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	if path, err := exec.LookPath("kind"); err == nil {
		t.Fatalf("bootstrap test can find ambient kind: %s", path)
	}
	if _, err := os.Stat(os.Getenv("SUBSTRATE_DIR")); !os.IsNotExist(err) {
		t.Fatalf("managed checkout is not initially absent: %v", err)
	}
	return dir, state
}

func TestSetupBootstrapsManagedCheckoutWithoutSystemKind(t *testing.T) {
	dir, state := bootstrapFixture(t)
	out, err := exec.CommandContext(t.Context(), "bash", "up.sh").CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 42 || !strings.Contains(string(out), "bootstrap-complete") {
		t.Fatalf("setup did not reach client building after bootstrap: %v\n%s", err, out)
	}
	calls, err := os.ReadFile(filepath.Join(dir, "bootstrap-calls"))
	want := "clone\nstatus\nfetch\ncheckout\nkind-list\ncreate\nkind-export\ninstall\nkind-list\nclient-build\n"
	if err != nil || string(calls) != want {
		t.Fatalf("unexpected bootstrap ordering: %v\n%s\n%s", err, calls, out)
	}
	owner, err := os.ReadFile(filepath.Join(state, "owner"))
	if err != nil || !strings.HasPrefix(string(owner), "node installed ") {
		t.Fatalf("bootstrap did not record installation: %v\n%s", err, owner)
	}
	if _, err := os.Stat(filepath.Join(dir, "substrate-bootstrap-incomplete")); !os.IsNotExist(err) {
		t.Fatalf("successful bootstrap left an incomplete marker: %v", err)
	}
}

func TestSetupRefusesIncompleteBootstrapOnRetry(t *testing.T) {
	for _, tc := range []struct {
		step  string
		alias string
	}{
		{step: "clone"},
		{step: "partial-clone"},
		{step: "fetch"},
		{step: "checkout"},
		{step: "fetch", alias: "trailing-slash"},
		{step: "fetch", alias: "dot-component"},
		{step: "fetch", alias: "symlink"},
	} {
		t.Run(tc.step+"/"+tc.alias, func(t *testing.T) {
			step := tc.step
			dir, state := bootstrapFixture(t)
			t.Setenv("TEST_BOOTSTRAP_FAILURE", step)
			out, err := exec.CommandContext(t.Context(), "bash", "up.sh").CombinedOutput()
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ExitCode() != 128 {
				t.Fatalf("bootstrap did not stop at failed %s: %v\n%s", step, err, out)
			}
			marker := filepath.Join(dir, "substrate-bootstrap-incomplete")
			if info, err := os.Stat(marker); err != nil || !info.IsDir() {
				t.Fatalf("failed bootstrap lost its incomplete marker: %v", err)
			}
			before, err := os.ReadFile(filepath.Join(dir, "bootstrap-calls"))
			if err != nil {
				t.Fatal(err)
			}
			checkout := filepath.Join(dir, "substrate")
			switch tc.alias {
			case "trailing-slash":
				t.Setenv("SUBSTRATE_DIR", checkout+"/")
			case "dot-component":
				t.Setenv("SUBSTRATE_DIR", dir+"/./substrate")
			case "symlink":
				alias := filepath.Join(dir, "checkout-alias")
				if err := os.Symlink(checkout, alias); err != nil {
					t.Fatal(err)
				}
				t.Setenv("SUBSTRATE_DIR", alias)
			}
			t.Setenv("TEST_BOOTSTRAP_FAILURE", "")
			for _, script := range []string{"up.sh", "down.sh"} {
				out, err := exec.CommandContext(t.Context(), "bash", script).CombinedOutput()
				if err == nil || !strings.Contains(string(out), "Incomplete managed Substrate bootstrap") {
					t.Fatalf("%s did not refuse incomplete bootstrap: %v\n%s", script, err, out)
				}
				after, err := os.ReadFile(filepath.Join(dir, "bootstrap-calls"))
				if err != nil || string(after) != string(before) {
					t.Fatalf("%s changed the incomplete checkout: %v\n%s", script, err, after)
				}
				for _, path := range []string{"wrapper-invoked", "created", "recipe-test/owner"} {
					if _, err := os.Stat(filepath.Join(dir, path)); !os.IsNotExist(err) {
						t.Fatalf("%s reached %s on failed bootstrap: %v", script, path, err)
					}
				}
				if _, err := os.Stat(marker); err != nil {
					t.Fatalf("%s removed the incomplete marker: %v", script, err)
				}
			}
			if _, err := os.Stat(filepath.Join(state, "lock")); !os.IsNotExist(err) {
				t.Fatalf("failed bootstrap left the cluster lock held: %v", err)
			}
		})
	}
}

func TestRelativeKubeconfigAcrossWorkingDirectories(t *testing.T) {
	dir, _ := fixture(t, map[string]string{
		"kubectl": `printf '%s\n' "$PWD" "$KUBECONFIG" "$@" > "$TEST_DIR/kubectl-args"`,
	})
	t.Setenv("SUBSTRATE_KIND_KUBECONFIG", "relative directory/kubeconfig")
	checkout := os.Getenv("SUBSTRATE_DIR")
	if err := os.MkdirAll(filepath.Join(checkout, "hack"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "hack", "kind.sh"),
		[]byte("#!/usr/bin/env bash\nset -euo pipefail\n"+
			`printf '%s\n' "$PWD" "$KUBECONFIG" "$@" > "$TEST_DIR/kind-args"`), 0o700); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(t.Context(), "bash", "-c", `source ./common.sh
kind_command export kubeconfig --name "$CLUSTER" --kubeconfig "$KUBECONFIG"
"${K[@]}" get pods`).CombinedOutput()
	if err != nil {
		t.Fatalf("kubeconfig commands failed: %v\n%s", err, out)
	}
	config := filepath.Join(cwd, "relative directory", "kubeconfig")
	for file, args := range map[string][]string{
		"kind-args":    {checkout, config, "export", "kubeconfig", "--name", "recipe-test", "--kubeconfig", config},
		"kubectl-args": {cwd, config, "--context", "kind-recipe-test", "--kubeconfig", config, "get", "pods"},
	} {
		got, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil || string(got) != strings.Join(args, "\n")+"\n" {
			t.Fatalf("%s used a different kubeconfig: %v\n%s", file, err, got)
		}
	}
}
