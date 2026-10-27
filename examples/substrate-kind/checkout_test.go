package substratekind_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func substratePin(t *testing.T) string {
	t.Helper()
	workflow, err := os.ReadFile("../../.github/workflows/substrate-e2e.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(workflow), "\n") {
		if pin, ok := strings.CutPrefix(line, "  SUBSTRATE_REF: "); ok {
			return pin
		}
	}
	t.Fatal("workflow has no Substrate revision")
	return ""
}

func TestSetupRefusesCheckoutInspectionFailure(t *testing.T) {
	for _, managed := range []bool{false, true} {
		t.Run(map[bool]string{false: "custom", true: "managed"}[managed], func(t *testing.T) {
			dir, _ := fixture(t, map[string]string{
				"docker": `case "$1" in info) echo x86_64 ;; ps) exit 0 ;; *) exit 1 ;; esac`,
				"git":    `case "$*" in *"rev-parse HEAD") echo "$TEST_PIN" ;; *) exit 1 ;; esac`,
				"kind":   `[[ "$*" == "get clusters" ]] || { echo unexpected-kind-invocation >&2; exit 1; }`,
			})
			prerequisites(t)
			t.Setenv("TEST_PIN", substratePin(t))
			if managed {
				checkout := filepath.Join(dir, "substrate")
				if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("SUBSTRATE_DIR", checkout)
			}
			t.Setenv("SANDBOX_CLASS", "gvisor")
			t.Setenv("MODEL_SOURCE", "stub")
			out, err := exec.CommandContext(t.Context(), "bash", "up.sh").CombinedOutput()
			if err == nil || !strings.Contains(string(out), "Cannot inspect substrate checkout.") ||
				strings.Contains(string(out), "unexpected-kind-invocation") {
				t.Fatalf("checkout inspection did not fail closed: %v\n%s", err, out)
			}
		})
	}
}

func TestSetupRefusesStaleInstallBeforeChangingCheckout(t *testing.T) {
	for _, stage := range []string{"created", "installed"} {
		for _, revision := range []string{"", strings.Repeat("0", 40), "current"} {
			for _, checkout := range []string{"custom", "managed"} {
				t.Run(stage+"/"+revision+"/"+checkout, func(t *testing.T) {
					prerequisites(t)
					dir, state := fixture(t, map[string]string{
						"docker":  `case "$1" in info) echo x86_64 ;; ps) echo node ;; *) exit 1 ;; esac`,
						"kind":    `[[ "$*" == "get clusters" ]]; echo recipe-test`,
						"git":     `touch "$TEST_DIR/checkout-inspected"; exit 1`,
						"ko":      `exit 1`,
						"kubectl": `exit 1`,
					})
					if checkout == "managed" {
						path := filepath.Join(dir, "substrate")
						if err := os.MkdirAll(filepath.Join(path, ".git"), 0o700); err != nil {
							t.Fatal(err)
						}
						t.Setenv("SUBSTRATE_DIR", path)
					}
					recordedRevision := revision
					if recordedRevision == "current" {
						recordedRevision = substratePin(t)
					}
					owner := "node " + stage + " " + recordedRevision + "\n"
					if err := os.WriteFile(filepath.Join(state, "owner"), []byte(owner), 0o600); err != nil {
						t.Fatal(err)
					}
					t.Setenv("SANDBOX_CLASS", "gvisor")
					t.Setenv("MODEL_SOURCE", "stub")
					out, err := exec.CommandContext(t.Context(), "bash", "up.sh").CombinedOutput()
					if err == nil {
						t.Fatalf("setup unexpectedly succeeded:\n%s", out)
					}
					_, inspected := os.Stat(filepath.Join(dir, "checkout-inspected"))
					if revision == "" || revision == strings.Repeat("0", 40) {
						if !strings.Contains(string(out), "Substrate cannot be upgraded in place") || !os.IsNotExist(inspected) {
							t.Fatalf("stale install reached checkout inspection: %v\n%s", inspected, out)
						}
					} else if inspected != nil || !strings.Contains(string(out), "Cannot inspect substrate checkout") {
						t.Fatalf("matching revision did not reach checkout inspection: %v\n%s", inspected, out)
					} else {
						managed := strings.Contains(string(out), "Preparing the managed Substrate checkout")
						custom := strings.Contains(string(out), "Checking the supplied Substrate checkout")
						if managed != (checkout == "managed") || custom != (checkout == "custom") {
							t.Fatalf("%s case inspected the wrong checkout path:\n%s", checkout, out)
						}
					}
					got, err := os.ReadFile(filepath.Join(state, "owner"))
					if err != nil || string(got) != owner {
						t.Fatalf("setup changed ownership on refusal: %v\n%s", err, got)
					}
				})
			}
		}
	}
}
