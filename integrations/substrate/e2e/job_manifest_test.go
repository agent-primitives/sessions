package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Render the real Job through the shell runner without needing a cluster. The approval
// placeholder contains the ordinary harness placeholder, so substitution order matters.
func TestConformanceJobKeepsHarnessImagesDistinct(t *testing.T) {
	dir := t.TempDir()
	capture := filepath.Join(dir, "manifest.yaml")
	kubectl := `#!/bin/sh
case "$*" in
  *" apply "*) cat > "$MANIFEST_CAPTURE" ;;
  *" get job "*) printf '1' ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(kubectl), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MANIFEST_CAPTURE", capture)
	ordinary := "registry/harness@sha256:" + strings.Repeat("a", 64)
	approval := "registry/approval@sha256:" + strings.Repeat("b", 64)
	t.Setenv("HARNESS_IMAGE", ordinary)
	for _, tc := range []struct{ name, image string }{{"approval fixture", approval}, {"no approval fixture", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("APPROVAL_HARNESS_IMAGE", tc.image)
			cmd := exec.CommandContext(t.Context(), "bash", filepath.Join("..", "..", "..", "hack", "run-e2e-job.sh"), "TestApproval", "registry/tests:fixture", "5")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("render Job: %v\n%s", err, out)
			}
			manifest, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			for name, want := range map[string]string{"HARNESS_IMAGE": ordinary, "APPROVAL_HARNESS_IMAGE": tc.image} {
				re := regexp.MustCompile(`(?m)^\s*- name: ` + name + `[^\n]*\n\s*value: "([^"]*)"$`)
				match := re.FindStringSubmatch(string(manifest))
				if len(match) != 2 || match[1] != want {
					t.Errorf("%s = %v, want %q", name, match, want)
				}
			}
		})
	}
}
