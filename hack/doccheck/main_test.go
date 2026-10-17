package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheck runs the gate on Markdown it writes and pins which blocks pass. go/types marks some
// real compile errors soft, alongside the unused-variable and unused-import errors a fragment may
// have; only the latter may pass.
func TestCheck(t *testing.T) {
	const client = "<!-- doccheck: body(ctx context.Context, c *client.Client) -->\n"
	cases := []struct {
		name, md string
		want     string // a substring of the problem reported for the file; "" means it passes
	}{
		{"unused-var", client + "```go\nx := 1\n```\n", ""},
		{"unused-import", "<!-- doccheck: file -->\n```go\npackage p\n\nimport \"fmt\"\nimport str \"strings\"\n```\n", ""},
		{"unused-label", client + "```go\nL:\n\tfor {\n\t}\n```\n", ""},
		{"no-new-var", client + "```go\nx := 1\nx := 2\n_ = x\n```\n", "no new variables on left side of :="},
		{"no-new-var-params", client + "```go\nc, ctx := c, ctx\n_, _ = c, ctx\n```\n", "no new variables on left side of :="},
		{"range", client + "```go\nfor range c {\n}\n```\n", "cannot range over c"},
		{"type-arg", "<!-- doccheck: decls -->\n```go\nfunc f[T int]() {}\n\nvar _ = f[string]\n```\n", "string does not satisfy int"},
		{"undefined", client + "```go\n_ = c.NoSuchMethod\n```\n", "c.NoSuchMethod undefined"},
		{"no-marker", "```go\nx := 1\n```\n", "has no <!-- doccheck: ... --> marker"},
		{"skip", "<!-- doccheck: skip an excerpt -->\n```go\nnot Go\n```\n", ""},
		{"skip-tab", "<!-- doccheck: skip\tan excerpt -->\n```go\nnot Go\n```\n", ""},
		{"skip-no-reason", "<!-- doccheck: skip -->\n```go\nnot Go\n```\n", "skip needs a reason"},
		{"skip-no-delimiter", "<!-- doccheck: skipgarbage -->\n```go\nnot Go\n```\n", `unknown marker "skipgarbage"`},
	}
	dir := t.TempDir()
	var files []string
	for _, tc := range cases {
		path := filepath.Join(dir, tc.name+".md")
		if err := os.WriteFile(path, []byte("# "+tc.name+"\n\n"+tc.md), 0o600); err != nil {
			t.Fatal(err)
		}
		files = append(files, path)
	}
	_, _, problems, err := check(files)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		prefix := filepath.Join(dir, tc.name+".md") + ":"
		var got []string
		for _, p := range problems {
			if strings.HasPrefix(p, prefix) {
				got = append(got, p)
			}
		}
		switch {
		case tc.want == "" && len(got) > 0:
			t.Errorf("%s: want no problem, got %q", tc.name, got)
		case tc.want != "" && (len(got) == 0 || !strings.Contains(strings.Join(got, "\n"), tc.want)):
			t.Errorf("%s: want a problem containing %q, got %q", tc.name, tc.want, got)
		}
	}
	if t.Failed() {
		t.Logf("all problems:\n%s", strings.Join(problems, "\n"))
	}
}

// TestCheckAfterALongLine checks that a line longer than any scanner buffer does not end extraction:
// the block after it is still checked and its error reported at the right line.
func TestCheckAfterALongLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.md")
	md := "# doc\n\n" + strings.Repeat("x", 2<<20) + "\n\n<!-- doccheck: body() -->\n```go\n_ = noSuchName\n```\n"
	if err := os.WriteFile(path, []byte(md), 0o600); err != nil {
		t.Fatal(err)
	}
	checked, _, problems, err := check([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	if checked != 1 || len(problems) != 1 || !strings.HasPrefix(problems[0], path+":7:") ||
		!strings.Contains(problems[0], "undefined: noSuchName") {
		t.Fatalf("want 1 block checked and one problem at %s:7 naming noSuchName, got %d checked, %q", path, checked, problems)
	}
}

// TestCheckReportsTheDocLine checks that a problem points at the Markdown line to fix.
func TestCheckReportsTheDocLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.md")
	md := "# doc\n\n<!-- doccheck: body() -->\n```go\nx := 1\nx := 2\n_ = x\n```\n"
	if err := os.WriteFile(path, []byte(md), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, problems, err := check([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 1 || !strings.HasPrefix(problems[0], path+":6:") {
		t.Fatalf("want one problem at %s:6, got %q", path, problems)
	}
}

func TestUnusedRE(t *testing.T) {
	for msg, want := range map[string]bool{
		"declared and not used: x":                     true,
		"x declared and not used":                      true,
		"label L declared and not used":                true,
		`"fmt" imported and not used`:                  true,
		`"strings" imported as str and not used`:       true,
		"no new variables on left side of :=":          false,
		"no new variable on left side of :=":           false,
		"cannot range over c (variable of type int)":   false,
		"cannot declare in post statement":             false,
		"declared and not used: x; and something else": false,
	} {
		if got := unusedRE.MatchString(msg); got != want {
			t.Errorf("unusedRE.MatchString(%q) = %v, want %v", msg, got, want)
		}
	}
}
