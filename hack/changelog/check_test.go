package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicContract(t *testing.T) {
	for p, want := range map[string]bool{
		"spec.md":                             true,
		"api/session.proto":                   true,
		"api/genpb/session.pb.go":             true,
		"api/runtime.go":                      true,
		"api/runtime_test.go":                 true, // everything under api/
		"other/x.proto":                       true,
		"controller/controller.go":            true,
		"sqlitelog/store.go":                  true,
		"integrations/substrate/adapter.go":   true,
		"controller/controller_test.go":       false,
		"internal/version/version.go":         false,
		"session/internal/x/x.go":             false,
		"controller/testdata/v0.1.2/main.go":  false,
		"cmd/agentsessionsd/main.go":          false,
		"integrations/substrate/cmd/x/x.go":   false,
		"hack/doccheck/main.go":               false,
		"examples/agentsuite-substrate/x.go":  false,
		"docs/concepts.md":                    false,
		"README.md":                           false,
		"CHANGELOG.md":                        false,
		"go.mod":                              false,
		".github/workflows/ci.yml":            false,
		"changelog/unreleased/create-shim.md": false,
	} {
		if got := publicContract(p); got != want {
			t.Errorf("publicContract(%q) = %v, want %v", p, got, want)
		}
	}
}

// gitRepo is a throwaway repository whose base commit has CHANGELOG.md, a Go package, docs and the
// fragment directory.
type gitRepo struct {
	t   *testing.T
	dir string
}

func newGitRepo(t *testing.T) *gitRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	r := &gitRepo{t: t, dir: t.TempDir()}
	r.git("init", "-q")
	r.write("CHANGELOG.md", intro+history)
	r.write("controller/controller.go", "package controller\n")
	r.write("docs/concepts.md", "# Concepts\n")
	r.write(fragmentDir+"/"+keepFile, "")
	r.write(fragmentDir+"/old.md", frag("Added", "- Old."))
	r.commit("base")
	return r
}

func (r *gitRepo) git(args ...string) {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{
		"-C", r.dir, "-c", "user.name=test", "-c", "user.email=test@example.invalid",
		"-c", "commit.gpgsign=false",
	}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func (r *gitRepo) write(p, data string) {
	r.t.Helper()
	full := filepath.Join(r.dir, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *gitRepo) remove(p string) {
	r.t.Helper()
	if err := os.Remove(filepath.Join(r.dir, p)); err != nil {
		r.t.Fatal(err)
	}
}

func (r *gitRepo) commit(msg string) {
	r.t.Helper()
	// --force: a global excludes file (core.excludesFile, or ~/.config/git/ignore) commonly ignores
	// .DS_Store, which the Finder metadata case commits.
	r.git("add", "-A", "--force")
	r.git("commit", "-q", "--allow-empty", "-m", msg)
}

// TestCheck drives the pull request gate through run, as the changelog workflow calls it, over a
// one-commit change per case.
func TestCheck(t *testing.T) {
	const goEdit = "package controller\n\nfunc New() {}\n"
	cases := []struct {
		name   string
		skip   bool
		change func(r *gitRepo)
		want   int    // exit status
		output string // a substring of stdout
	}{
		{
			name:   "contract change without fragment fails",
			change: func(r *gitRepo) { r.write("controller/controller.go", goEdit) },
			want:   1, output: "adds no fragment under changelog/unreleased/:\n  controller/controller.go\n",
		},
		{
			name:   "proto and spec change without fragment fails",
			change: func(r *gitRepo) { r.write("api/session.proto", "syntax"); r.write("spec.md", "# spec") },
			want:   1, output: "  api/session.proto\n  spec.md\n",
		},
		{
			name: "contract change with fragment passes",
			change: func(r *gitRepo) {
				r.write("controller/controller.go", goEdit)
				r.write(fragmentDir+"/new-func.md", frag("Added", "- `controller.New`."))
			},
			want: 0, output: "fragments: changelog/unreleased/new-func.md",
		},
		{
			name: "contract change editing an existing fragment passes",
			change: func(r *gitRepo) {
				r.write("controller/controller.go", goEdit)
				r.write(fragmentDir+"/old.md", frag("Added", "- Old, refined."))
			},
			want: 0, output: "fragments: changelog/unreleased/old.md",
		},
		{
			name:   "contract change with skip label passes",
			skip:   true,
			change: func(r *gitRepo) { r.write("controller/controller.go", goEdit) },
			want:   0, output: "the skip-changelog label waives it",
		},
		{
			name: "deleting a fragment does not count",
			change: func(r *gitRepo) {
				r.write("controller/controller.go", goEdit)
				r.remove(fragmentDir + "/old.md")
			},
			want: 1, output: "adds no fragment",
		},
		{
			name:   "non-contract change passes",
			change: func(r *gitRepo) { r.write("docs/concepts.md", "# Concepts\n\nMore.\n") },
			want:   0, output: "No public contract paths changed",
		},
		{
			name:   "test-only change passes",
			change: func(r *gitRepo) { r.write("controller/controller_test.go", "package controller\n") },
			want:   0, output: "No public contract paths changed",
		},
		{
			name: "invalid fragment fails even with the label",
			skip: true,
			change: func(r *gitRepo) {
				r.write("controller/controller.go", goEdit)
				r.write(fragmentDir+"/new-func.md", frag("Misc", "- x"))
			},
			want: 1, output: `new-func.md: unknown category "Misc"`,
		},
		{
			name: "fragment with an indented heading fails",
			change: func(r *gitRepo) {
				r.write("api/session.proto", "syntax")
				r.write(fragmentDir+"/new-rpc.md", frag("Added", "- Valid item.\n\n ### Removed\n\n- Gone."))
			},
			want: 1, output: "new-rpc.md: line \" ### Removed\": headings are not allowed; a release section supplies them\n" +
				"Public contract paths changed; fragments: changelog/unreleased/new-rpc.md, which must be valid (see above).\n",
		},
		{
			name:   "stray file in the fragment directory fails",
			change: func(r *gitRepo) { r.write(fragmentDir+"/sub/x.md", frag("Added", "- x")) },
			want:   1, output: "sub/: directories are not allowed",
		},
		{
			name:   "committed Finder metadata fails",
			change: func(r *gitRepo) { r.write(fragmentDir+"/"+finderMetadata, "\x00") },
			want:   1, output: ".DS_Store: name must be a lowercase slug",
		},
		{
			name: "reintroducing Unreleased fails",
			change: func(r *gitRepo) {
				r.write("CHANGELOG.md", intro+"## Unreleased\n\n### Added\n\n- x\n\n"+history)
			},
			want: 1, output: `CHANGELOG.md has an "## Unreleased" section`,
		},
		{
			name:   "deleting CHANGELOG.md fails",
			change: func(r *gitRepo) { r.remove("CHANGELOG.md") },
			want:   1, output: "CHANGELOG.md is missing or not a file",
		},
		{
			name:   "renaming CHANGELOG.md fails",
			change: func(r *gitRepo) { r.git("mv", "CHANGELOG.md", "HISTORY.md") },
			want:   1, output: "CHANGELOG.md is missing or not a file",
		},
		{
			name: "replacing CHANGELOG.md with a directory fails",
			change: func(r *gitRepo) {
				r.remove("CHANGELOG.md")
				r.write("CHANGELOG.md/README.md", "# Changelog\n")
			},
			want: 1, output: "CHANGELOG.md is missing or not a file",
		},
		{
			name: "replacing CHANGELOG.md with a symlink fails",
			change: func(r *gitRepo) {
				r.remove("CHANGELOG.md")
				if err := os.Symlink("HISTORY.md", filepath.Join(r.dir, "CHANGELOG.md")); err != nil {
					r.t.Fatal(err)
				}
			},
			want: 1, output: "CHANGELOG.md is missing or not a file",
		},
		{
			name: "executable CHANGELOG.md passes",
			change: func(r *gitRepo) {
				if err := os.Chmod(filepath.Join(r.dir, "CHANGELOG.md"), 0o755); err != nil {
					r.t.Fatal(err)
				}
			},
			want: 0, output: "No public contract paths changed",
		},
		{
			name: "release pull request passes",
			change: func(r *gitRepo) {
				r.write("CHANGELOG.md", intro+"## v0.4.0 - 2026-10-30\n\n### Added\n\n- Old.\n\n"+history)
				r.remove(fragmentDir + "/old.md")
			},
			want: 0, output: "No public contract paths changed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newGitRepo(t)
			tc.change(r)
			r.commit("change")
			args := []string{"check", "-root", r.dir, "-base", "HEAD^1", "-head", "HEAD"}
			if tc.skip {
				args = append(args, "-skip=true")
			}
			var stdout, stderr bytes.Buffer
			got := run(args, &stdout, &stderr)
			if got != tc.want || !strings.Contains(stdout.String(), tc.output) {
				t.Errorf("exit %d, want %d; stdout:\n%s\nwant it to contain %q; stderr:\n%s",
					got, tc.want, stdout.String(), tc.output, stderr.String())
			}
		})
	}
}

func TestCheckGitError(t *testing.T) {
	r := newGitRepo(t)
	var stdout, stderr bytes.Buffer
	if got := run([]string{"check", "-root", r.dir, "-base", "no-such-ref", "-head", "HEAD"}, &stdout, &stderr); got != 2 {
		t.Fatalf("exit %d, want 2; stderr: %s", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "no-such-ref") {
		t.Errorf("stderr = %q, want it to name the bad revision", stderr.String())
	}
}

// TestCheckChangelogReadError checks that a failure to read CHANGELOG.md at head is a git error
// (exit 2), not an absent "## Unreleased" section.
func TestCheckChangelogReadError(t *testing.T) {
	r := newGitRepo(t)
	blob, err := exec.Command("git", "-C", r.dir, "rev-parse", "HEAD:CHANGELOG.md").Output()
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(string(blob))
	if err := os.Remove(filepath.Join(r.dir, ".git", "objects", id[:2], id[2:])); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if got := run([]string{"check", "-root", r.dir, "-base", "HEAD", "-head", "HEAD"}, &stdout, &stderr); got != 2 {
		t.Fatalf("exit %d, want 2; stdout:\n%s\nstderr: %s", got, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "CHANGELOG.md") {
		t.Errorf("stderr = %q, want it to name CHANGELOG.md", stderr.String())
	}
}

func TestRunUsage(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"nope"},
		{"assemble"},
		{"assemble", "v1.0.0", "extra"},
		{"check", "-base", "HEAD^1"},
		{"verify-assembled", "extra"},
	} {
		var stdout, stderr bytes.Buffer
		if got := run(args, &stdout, &stderr); got != 2 {
			t.Errorf("run(%q) = %d, want 2", args, got)
		}
	}
}
