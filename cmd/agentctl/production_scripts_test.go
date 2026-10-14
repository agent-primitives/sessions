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
