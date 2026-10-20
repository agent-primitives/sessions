package main

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func scriptFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/bash\n"+body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestProductionSmokeBoundsStalledProbe(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "agentctl")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build agentctl: %v\n%s", err, out)
	}
	for _, tc := range []struct {
		name, ready, overall, want string
	}{
		{"readiness deadline", "5", "12", "readiness timed out"},
		{"whole smoke deadline", "30", "2", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lis, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = lis.Close() })
			var connections atomic.Int32
			go func() {
				for {
					conn, err := lis.Accept()
					if err != nil {
						return
					}
					connections.Add(1)
					go func() {
						defer func() { _ = conn.Close() }()
						_, _ = io.Copy(io.Discard, conn)
					}()
				}
			}()
			dir := scriptFixture(t, map[string]string{
				"go": `cp "$TEST_AGENTCTL" "$3"`,
				"kubectl": `echo 'Forwarding from 127.0.0.1:18080 -> 8080'
exec sleep 60`,
			})
			t.Setenv("TEST_FIXTURE", dir)
			t.Setenv("TEST_AGENTCTL", binary)
			t.Setenv("LOCAL_PORT", strings.Split(lis.Addr().String(), ":")[1])
			t.Setenv("READY_TIMEOUT_SECONDS", tc.ready)
			t.Setenv("PROBE_TIMEOUT_SECONDS", "1")
			t.Setenv("SMOKE_TIMEOUT_SECONDS", tc.overall)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "../../hack/smoke-production-kind.sh")
			cmd.WaitDelay = time.Second
			start := time.Now()
			out, err := cmd.CombinedOutput()
			if err == nil || ctx.Err() != nil || time.Since(start) > 9*time.Second {
				t.Fatalf("smoke did not terminate within its own deadline: %v\n%s", err, out)
			}
			if tc.want != "" && !strings.Contains(string(out), tc.want) {
				t.Fatalf("missing readiness diagnostic:\n%s", out)
			}
			if tc.want == "" {
				exit, ok := err.(*exec.ExitError)
				if !ok || exit.ExitCode() != 124 {
					t.Fatalf("error = %v, want overall timeout exit 124\n%s", err, out)
				}
			} else {
				if connections.Load() < 2 {
					t.Fatalf("per-probe timeout did not permit retries: %d connections", connections.Load())
				}
			}
			if strings.Contains(string(out), "execute the first chat turn") {
				t.Fatal("smoke continued after failed readiness")
			}
		})
	}
}

func TestProductionTeardownPreservesSharedRegistry(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		t.Run(map[bool]string{false: "fallback", true: "pinned"}[pinned], func(t *testing.T) {
			dir := scriptFixture(t, map[string]string{
				"kind":   `echo "$*" >> "$TEST_FIXTURE/kind-calls"`,
				"docker": `echo "$*" >> "$TEST_FIXTURE/docker-calls"; exit 1`,
			})
			substrate := t.TempDir()
			if pinned {
				if err := os.Mkdir(filepath.Join(substrate, "hack"), 0o700); err != nil {
					t.Fatal(err)
				}

				if err := os.WriteFile(filepath.Join(substrate, "hack", "kind.sh"),
					[]byte("#!/bin/bash\n[[ \"$PWD\" == \"$SUBSTRATE_DIR\" ]] || exit 1\nexec kind \"$@\"\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("TEST_FIXTURE", dir)
			t.Setenv("SUBSTRATE_DIR", substrate)
			t.Setenv("CLUSTER", "owned-test-cluster")
			cmd := exec.CommandContext(t.Context(), "bash", "../../hack/kind-production.sh", "teardown")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("teardown: %v\n%s", err, out)
			}
			calls, err := os.ReadFile(filepath.Join(dir, "kind-calls"))
			if err != nil || string(calls) != "delete cluster --name owned-test-cluster\n" {
				t.Fatalf("kind calls: %q %v", calls, err)
			}
			if _, err := os.Stat(filepath.Join(dir, "docker-calls")); !os.IsNotExist(err) {
				t.Fatal("teardown accessed the shared registry")
			}
		})
	}
}

func TestProductionSetupSelectionAndCheckoutSafety(t *testing.T) {
	for _, tc := range []struct {
		name, class, kvm, revision, dirty, want, credentials string
		fail                                                 bool
	}{
		{name: "automatic microvm", kvm: "yes", want: "microvm"},
		{name: "automatic fallback", kvm: "no", want: "gvisor"},
		{name: "explicit microvm", class: "microvm", kvm: "yes", want: "microvm"},
		{name: "explicit gvisor", class: "gvisor", kvm: "yes", want: "gvisor"},
		{name: "explicit gvisor without KVM", class: "gvisor", kvm: "no", want: "gvisor"},
		{name: "credential provider override", kvm: "yes", want: "microvm", credentials: `{"name":"custom-provider","config":{"audience":"test"}}`},
		{name: "explicit microvm without KVM", class: "microvm", kvm: "no", fail: true},
		{name: "invalid class", class: "typo", kvm: "yes", fail: true},
		{name: "wrong custom revision", kvm: "yes", revision: strings.Repeat("b", 40), fail: true},
		{name: "dirty custom checkout", kvm: "yes", dirty: " M owned-file", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := scriptFixture(t, map[string]string{
				"python3": `if [[ "$1" == "-" ]]; then cat >/dev/null; [[ "$TEST_KVM" == yes ]]; else echo "$SUBSTRATE_DIR"; fi`,
				"git": `case "$*" in
				*"rev-parse HEAD") echo "${TEST_REVISION:-$TEST_PIN}" ;;
				*"status --porcelain") echo "$TEST_DIRTY" ;;
				*) echo "unsafe git command: $*" >&2; exit 1 ;;
				esac`,
				"go":     `[[ "$*" == "env GOARCH" ]] && echo arm64`,
				"docker": "exit 0",
				"ko":     `echo "$KO_DEFAULTPLATFORMS $*" >> "$TEST_FIXTURE/builds"; echo example/image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`,
				"kubectl": `echo "$*" >> "$TEST_FIXTURE/kubectl-calls"
				if [[ "$*" == *"apply -f -"* ]]; then cat >> "$TEST_FIXTURE/manifests"; elif [[ "$*" == *"create"* ]]; then echo "{}"; elif [[ "$*" == *"jsonpath"* ]]; then echo worker=10.0.0.1; fi`,
				"sleep": "exit 0",
			})
			substrate := t.TempDir()
			if err := os.Mkdir(filepath.Join(substrate, "hack"), 0o700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"create-kind-cluster.sh", "install-ate-kind.sh", "install-microvm-deps.sh"} {
				if err := os.WriteFile(filepath.Join(substrate, "hack", name),
					[]byte("#!/bin/bash\nprintf '%s\\n' \"$ATE_CREDENTIAL_PROVIDER\" > \"$TEST_FIXTURE/credentials\"\necho \"$0 $*\" >> \"$TEST_FIXTURE/installs\"\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			workflow, err := os.ReadFile("../../.github/workflows/substrate-e2e.yml")
			if err != nil {
				t.Fatal(err)
			}
			var pin string
			for _, line := range strings.Split(string(workflow), "\n") {
				if strings.HasPrefix(line, "  SUBSTRATE_REF: ") {
					pin = strings.TrimPrefix(line, "  SUBSTRATE_REF: ")
				}
			}
			t.Setenv("TEST_FIXTURE", dir)
			t.Setenv("TEST_PIN", pin)
			t.Setenv("TEST_REVISION", tc.revision)
			t.Setenv("TEST_DIRTY", tc.dirty)
			t.Setenv("TEST_KVM", tc.kvm)
			t.Setenv("ATE_CREDENTIAL_PROVIDER", tc.credentials)
			t.Setenv("SUBSTRATE_DIR", substrate)
			t.Setenv("SUBSTRATE_REF", "")
			t.Setenv("SANDBOX_CLASS", tc.class)
			t.Setenv("SANDBOX_CONFIG", "")
			t.Setenv("KO_DEFAULTPLATFORMS", "")
			t.Setenv("USE_MODELSTUB", "1")
			t.Setenv("USE_OLLAMA", "0")
			t.Setenv("MODEL_API_KEY", "")
			t.Setenv("CLUSTER", "owned-selection-test")
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, "bash", "../../hack/kind-production.sh", "setup").CombinedOutput()
			if (err != nil) != tc.fail {
				t.Fatalf("setup: %v\n%s", err, out)
			}
			if tc.fail {
				if _, err := os.Stat(filepath.Join(dir, "installs")); !os.IsNotExist(err) {
					t.Fatal("invalid setup changed a cluster")
				}
				return
			}
			credentials, err := os.ReadFile(filepath.Join(dir, "credentials"))
			wantCredentials := tc.credentials
			if wantCredentials == "" {
				wantCredentials = `{"name":"k8s.io"}`
			}
			if err != nil || strings.TrimSpace(string(credentials)) != wantCredentials {
				t.Fatalf("credential provider = %q, %v; want %q", credentials, err, wantCredentials)
			}
			manifests, err := os.ReadFile(filepath.Join(dir, "manifests"))
			if err != nil {
				t.Fatal(err)
			}
			wantConfig := "microvm"
			if tc.want == "gvisor" {
				wantConfig = "gvisor-default"
			}
			for _, want := range []string{
				"sandboxClasses:\n    - name: " + tc.want, "workload: agentsessions-chat-" + tc.want,
				"--sandbox-class=" + tc.want, "--sandbox-config=" + wantConfig,
			} {
				if !strings.Contains(string(manifests), want) {
					t.Fatalf("missing %q in rendered manifests", want)
				}
			}
			builds, err := os.ReadFile(filepath.Join(dir, "builds"))
			if err != nil || !strings.Contains(string(builds), "linux/arm64 build ./cmd/ateom-"+tc.want) {
				t.Fatalf("wrong build platform or worker: %s %v", builds, err)
			}
			installs, err := os.ReadFile(filepath.Join(dir, "installs"))
			if err != nil || strings.Contains(string(installs), "install-microvm-deps.sh") != (tc.want == "microvm") {
				t.Fatalf("wrong microVM asset installation: %s %v", installs, err)
			}
			if tc.kvm == "no" && tc.class == "" && !strings.Contains(string(out), "falling back to gvisor") {
				t.Fatal("fallback not announced")
			}
		})
	}
}
