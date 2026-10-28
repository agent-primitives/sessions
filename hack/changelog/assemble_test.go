package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const intro = "# Changelog\n\nIntro paragraph.\n\n"

const history = "## v0.3.0 - 2026-10-22\n\n### Added\n\n- Old entry.\n"

// newRepo writes CHANGELOG.md and the given fragments (name to contents) into a temporary root.
func newRepo(t *testing.T, changelog string, frags map[string]string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, fragmentDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, keepFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, data := range frags {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "CHANGELOG.md"), []byte(changelog), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func readChangelog(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func listFragmentDir(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, fragmentDir))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func frag(category, body string) string {
	return "---\ncategory: " + category + "\n---\n\n" + body + "\n"
}

func TestAssembleOrdersByCategoryThenName(t *testing.T) {
	root := newRepo(t, intro+history, map[string]string{
		"zeta.md":          frag("Added", "- Zeta, with a [link](../../docs/z.md)."),
		"alpha.md":         frag("Added", "- Alpha."),
		"fix-thing.md":     frag("Fixed", "- Fixed a thing."),
		"leak.md":          frag("Security", "- Closed a leak."),
		"schema-v7.md":     frag("Compatibility", "- Schema v7, see [rollout](../../CHANGELOG.md#v4).\n- Second item."),
		"behavior-swap.md": frag("Changed", "- Changed a default."),
	})
	changed, err := assemble(root, "v0.4.0", "2026-10-30")
	if err != nil || !changed {
		t.Fatalf("assemble = %v, %v; want true, nil", changed, err)
	}
	want := intro + `## v0.4.0 - 2026-10-30

### Compatibility

- Schema v7, see [rollout](#v4).
- Second item.

### Added

- Alpha.
- Zeta, with a [link](docs/z.md).

### Changed

- Changed a default.

### Fixed

- Fixed a thing.

### Security

- Closed a leak.

` + history
	if got := readChangelog(t, root); got != want {
		t.Errorf("CHANGELOG.md =\n%s\nwant\n%s", got, want)
	}
	if got := listFragmentDir(t, root); !slices.Equal(got, []string{keepFile}) {
		t.Errorf("fragment dir = %v, want only %s", got, keepFile)
	}
}

func TestAssembleIsIdempotent(t *testing.T) {
	root := newRepo(t, intro+history, map[string]string{"a.md": frag("Fixed", "- Fixed.")})
	if changed, err := assemble(root, "v0.4.0", "2026-10-30"); err != nil || !changed {
		t.Fatalf("first assemble = %v, %v; want true, nil", changed, err)
	}
	first := readChangelog(t, root)
	for range 2 {
		changed, err := assemble(root, "v0.4.0", "2026-10-30")
		if err != nil || changed {
			t.Fatalf("rerun = %v, %v; want false, nil", changed, err)
		}
		if got := readChangelog(t, root); got != first {
			t.Errorf("rerun changed CHANGELOG.md:\n%s", got)
		}
	}
	if err := verifyAssembled(root); err != nil {
		t.Errorf("verifyAssembled after assemble: %v", err)
	}
}

func TestAssembleSkipsFinderMetadata(t *testing.T) {
	root := newRepo(t, intro+history, map[string]string{"a.md": frag("Fixed", "- Fixed."), finderMetadata: "\x00\x01"})
	if changed, err := assemble(root, "v0.4.0", "2026-10-30"); err != nil || !changed {
		t.Fatalf("assemble = %v, %v; want true, nil", changed, err)
	}
	if got, want := listFragmentDir(t, root), []string{finderMetadata, keepFile}; !slices.Equal(got, want) {
		t.Errorf("fragment dir = %v, want %v", got, want)
	}
	if err := verifyAssembled(root); err != nil {
		t.Errorf("verifyAssembled = %v, want nil", err)
	}
}

func TestAssembleEmptySetChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frags  map[string]string
		noKeep bool
	}{
		{name: "only the keep file"},
		{name: "no fragment directory", noKeep: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newRepo(t, intro+history, tc.frags)
			if tc.noKeep {
				if err := os.RemoveAll(filepath.Join(root, "changelog")); err != nil {
					t.Fatal(err)
				}
			}
			changed, err := assemble(root, "v0.4.0", "2026-10-30")
			if err != nil || changed {
				t.Fatalf("assemble = %v, %v; want false, nil", changed, err)
			}
			if got := readChangelog(t, root); got != intro+history {
				t.Errorf("CHANGELOG.md changed:\n%s", got)
			}
		})
	}
}

// TestAssembleRefusals checks that each refused input leaves CHANGELOG.md and every fragment as
// they were.
func TestAssembleRefusals(t *testing.T) {
	good := frag("Added", "- Good.")
	cases := []struct {
		name, changelog, version, date string
		frags                          map[string]string
		wantErr                        string
	}{
		{
			name: "unknown category", changelog: intro + history, version: "v0.4.0", date: "2026-10-30",
			frags:   map[string]string{"good.md": good, "bad.md": frag("Removed", "- Gone.")},
			wantErr: `bad.md: unknown category "Removed"`,
		},
		{
			name: "indented heading", changelog: intro + history, version: "v0.4.0", date: "2026-10-30",
			frags:   map[string]string{"good.md": good, "bad.md": frag("Added", "- Valid item.\n\n ### Removed\n\n- Gone.")},
			wantErr: "bad.md: line \" ### Removed\": headings are not allowed",
		},
		{
			// Assembled, the comment would hide good.md's entry and every older release section.
			name: "unterminated html comment", changelog: intro + history, version: "v0.4.0", date: "2026-10-30",
			frags:   map[string]string{"good.md": good, "a-draft.md": frag("Added", "- Entry.\n\n<!-- draft")},
			wantErr: "a-draft.md: line \"<!-- draft\": HTML comments",
		},
		{
			// Assembled, aa-details.md's HTML block would take in ab-quote.md's entry, where the
			// inline code would be a raw comment hiding every later entry and older release.
			name: "fragment ending in an html block", changelog: intro + history, version: "v0.4.0", date: "2026-10-30",
			frags: map[string]string{
				"aa-details.md": frag("Added", "- Example output is now collapsible.\n\n<details>\n<summary>Example</summary>\n\nOutput.\n\n</details>"),
				"ab-quote.md":   frag("Added", "- Fragments may now quote `<!--` in inline code."),
			},
			wantErr: "aa-details.md: the entry may end in an HTML block",
		},
		{
			name: "malformed fragment", changelog: intro + history, version: "v0.4.0", date: "2026-10-30",
			frags:   map[string]string{"good.md": good, "bad.md": "- no front matter\n"},
			wantErr: "bad.md: must start with front matter",
		},
		{
			name: "stray file", changelog: intro + history, version: "v0.4.0", date: "2026-10-30",
			frags:   map[string]string{"good.md": good, "Notes.md": good},
			wantErr: "Notes.md: name must be a lowercase slug",
		},
		{
			name: "section exists", changelog: intro + "## v0.4.0 - 2026-10-29\n\n- Curated.\n\n" + history,
			version: "v0.4.0", date: "2026-10-30", frags: map[string]string{"good.md": good},
			wantErr: "already has a v0.4.0 section",
		},
		{
			name: "unreleased section", changelog: intro + "## Unreleased\n\n### Added\n\n- Old style.\n\n" + history,
			version: "v0.4.0", date: "2026-10-30", frags: map[string]string{"good.md": good},
			wantErr: `"## Unreleased" section`,
		},
		{
			name: "unreleased section with closing hashes", changelog: intro + "## Unreleased ##\n\n- Old style.\n\n" + history,
			version: "v0.4.0", date: "2026-10-30", frags: map[string]string{"good.md": good},
			wantErr: `"## Unreleased" section`,
		},
		{
			name: "unreleased section after a tab", changelog: intro + "##\tUnreleased\n\n- Old style.\n\n" + history,
			version: "v0.4.0", date: "2026-10-30", frags: map[string]string{"good.md": good},
			wantErr: `"## Unreleased" section`,
		},
		{
			name: "bad version", changelog: intro + history, version: "0.4.0", date: "2026-10-30",
			frags: map[string]string{"good.md": good}, wantErr: "not a release tag",
		},
		{
			name: "bad date", changelog: intro + history, version: "v0.4.0", date: "30-10-2026",
			frags: map[string]string{"good.md": good}, wantErr: "not YYYY-MM-DD",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := newRepo(t, tc.changelog, tc.frags)
			before := listFragmentDir(t, root)
			changed, err := assemble(root, tc.version, tc.date)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || changed {
				t.Fatalf("assemble = %v, %v; want false and an error containing %q", changed, err, tc.wantErr)
			}
			if got := readChangelog(t, root); got != tc.changelog {
				t.Errorf("CHANGELOG.md changed:\n%s", got)
			}
			if got := listFragmentDir(t, root); !slices.Equal(got, before) {
				t.Errorf("fragment dir = %v, want %v", got, before)
			}
		})
	}
}

func TestAssembleFirstRelease(t *testing.T) {
	root := newRepo(t, intro, map[string]string{"a.md": frag("Added", "- First.")})
	if _, err := assemble(root, "v0.1.0", "2026-10-30"); err != nil {
		t.Fatal(err)
	}
	want := intro + "## v0.1.0 - 2026-10-30\n\n### Added\n\n- First.\n"
	if got := readChangelog(t, root); got != want {
		t.Errorf("CHANGELOG.md =\n%q\nwant\n%q", got, want)
	}
}

func TestVerifyAssembled(t *testing.T) {
	root := newRepo(t, intro+history, map[string]string{"a.md": frag("Added", "- Pending.")})
	err := verifyAssembled(root)
	if err == nil || !strings.Contains(err.Error(), "changelog/unreleased/a.md") {
		t.Fatalf("verifyAssembled = %v, want it to name the pending fragment", err)
	}

	for _, heading := range []string{"## Unreleased\n", "## Unreleased ##\n", "##\tUnreleased\n", "Unreleased\n----------\n"} {
		root = newRepo(t, intro+heading+"\n"+history, nil)
		if err := verifyAssembled(root); err == nil || !strings.Contains(err.Error(), "Unreleased") {
			t.Fatalf("verifyAssembled with %q = %v, want an Unreleased refusal", heading, err)
		}
	}

	root = newRepo(t, intro+history, nil)
	if err := verifyAssembled(root); err != nil {
		t.Fatalf("verifyAssembled = %v, want nil", err)
	}
}
