package substratekind_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupRefusesCheckoutInspectionFailure(t *testing.T) {
	for _, managed := range []bool{false, true} {
		t.Run(map[bool]string{false: "custom", true: "managed"}[managed], func(t *testing.T) {
			dir, _ := fixture(t, map[string]string{
				"docker": `[[ "$1" == info ]]; echo x86_64`,
				"git":    `case "$*" in *"rev-parse HEAD") echo "$TEST_PIN" ;; *) exit 1 ;; esac`,
				"kind":   `echo unexpected-kind-invocation >&2; exit 1`,
			})
			prerequisites(t)
			workflow, err := os.ReadFile("../../.github/workflows/substrate-e2e.yml")
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(string(workflow), "\n") {
				if pin, ok := strings.CutPrefix(line, "  SUBSTRATE_REF: "); ok {
					t.Setenv("TEST_PIN", pin)
				}
			}
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
