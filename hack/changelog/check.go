package main

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"slices"
	"strings"
)

// skipLabel is the pull request label that waives the fragment requirement.
const skipLabel = "skip-changelog"

// errCheckFailed is a check failure already explained on the output.
var errCheckFailed = errors.New("changelog check failed")

// publicContract reports whether a changed path is part of the public contract that
// .github/copilot-instructions.md rule 6 asks a changelog entry for: spec.md, everything under
// api/ (protos, generated stubs, the Go Runtime SPI), any .proto file, and the non-test Go source
// of an importable package. Importable means outside internal/, testdata/ and cmd/ directories
// (cmd/ holds main packages) and outside hack/ and examples/. integrations/substrate is a
// published module, so its packages count too.
func publicContract(p string) bool {
	switch {
	case p == "spec.md", strings.HasPrefix(p, "api/"), strings.HasSuffix(p, ".proto"):
		return true
	case !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go"):
		return false
	}
	segs := strings.Split(path.Dir(p), "/")
	if segs[0] == "hack" || segs[0] == "examples" {
		return false
	}
	for _, s := range segs {
		if s == "internal" || s == "testdata" || s == "cmd" {
			return false
		}
	}
	return true
}

// isFragment reports whether p is an entry directly under fragmentDir other than keepFile.
func isFragment(p string) bool {
	return path.Dir(p) == fragmentDir && path.Base(p) != keepFile
}

func git(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// fragmentsAt reads every entry under fragmentDir at the head revision of the repository in dir.
func fragmentsAt(dir, head string) ([]file, error) {
	out, err := git(dir, "ls-tree", "-z", head, fragmentDir+"/")
	if err != nil {
		return nil, err
	}
	var files []file
	for entry := range strings.SplitSeq(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if entry == "" {
			continue
		}
		// <mode> SP <type> SP <object> TAB <path>
		meta, p, ok := strings.Cut(entry, "\t")
		if !ok {
			return nil, fmt.Errorf("unexpected git ls-tree output %q", entry)
		}
		f := file{name: path.Base(p), dir: strings.Contains(meta, " tree ")}
		if !f.dir && f.name != keepFile {
			if f.data, err = git(dir, "show", head+":"+p); err != nil {
				return nil, err
			}
		}
		files = append(files, f)
	}
	return files, nil
}

// changelogAt reads CHANGELOG.md at the head revision of the repository in dir. It returns nil
// and no error when head has no regular CHANGELOG.md file (it was deleted or renamed, or is a
// directory or symlink), and an error when git fails.
func changelogAt(dir, head string) ([]byte, error) {
	out, err := git(dir, "ls-tree", "-z", head, "--", "CHANGELOG.md")
	if err != nil {
		return nil, err
	}
	// <mode> SP <type> SP <object> TAB <path>; a regular file has mode 100644 or 100755.
	meta, _, _ := strings.Cut(string(out), "\t")
	if !strings.HasPrefix(meta, "100644 blob ") && !strings.HasPrefix(meta, "100755 blob ") {
		return nil, nil
	}
	return git(dir, "show", head+":CHANGELOG.md")
}

// check is the pull request gate. It fails when the change from base to head touches a public
// contract path without adding or editing a fragment, unless skip (the pull request carries
// skipLabel). Independently of skip, every fragment at head must be valid, and CHANGELOG.md must
// exist at head (a release assembles the fragments into it) and have no "## Unreleased" section.
// It returns its report, and errCheckFailed on a failed check or another error when git fails.
func check(dir, base, head string, skip bool) (string, error) {
	diff, err := git(dir, "diff", "--name-status", "--no-renames", "-z", base, head, "--")
	if err != nil {
		return "", err
	}
	var contract, added []string
	fields := strings.Split(strings.TrimSuffix(string(diff), "\x00"), "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		status, p := fields[i], fields[i+1]
		if publicContract(p) {
			contract = append(contract, p)
		}
		// A deleted fragment does not describe this change; an added or edited one does.
		if isFragment(p) && status != "D" {
			added = append(added, p)
		}
	}

	var out strings.Builder
	failed := false
	files, err := fragmentsAt(dir, head)
	if err != nil {
		return "", err
	}
	invalid := false
	if _, err := parseFragments(files); err != nil {
		fmt.Fprintf(&out, "invalid changelog fragments (format: changelog/README.md):\n%v\n", err)
		invalid, failed = true, true
	}
	changelog, err := changelogAt(dir, head)
	switch {
	case err != nil:
		return "", err
	case changelog == nil:
		fmt.Fprintln(&out, "CHANGELOG.md is missing or not a file: a release assembles the fragments into it; restore it.")
		failed = true
	case unreleasedHeading(string(changelog)):
		fmt.Fprintln(&out, "CHANGELOG.md has an \"## Unreleased\" section: move its entries into fragments under changelog/unreleased/ (changelog/README.md).")
		failed = true
	}

	switch {
	case len(contract) == 0:
		fmt.Fprintln(&out, "No public contract paths changed; no fragment required.")
	case len(added) > 0 && invalid:
		fmt.Fprintf(&out, "Public contract paths changed; fragments: %s, which must be valid (see above).\n", strings.Join(added, ", "))
	case len(added) > 0:
		fmt.Fprintf(&out, "Public contract paths changed; fragments: %s\n", strings.Join(added, ", "))
	case skip:
		fmt.Fprintf(&out, "Public contract paths changed without a fragment; the %s label waives it.\n", skipLabel)
	default:
		slices.Sort(contract)
		fmt.Fprintf(&out, "This pull request changes public contract paths but adds no fragment under %s/:\n", fragmentDir)
		for _, p := range contract {
			fmt.Fprintf(&out, "  %s\n", p)
		}
		fmt.Fprintf(&out, "Add one (format: changelog/README.md). If the change needs no release note, a maintainer can add the %s label.\n", skipLabel)
		failed = true
	}
	if failed {
		return out.String(), errCheckFailed
	}
	return out.String(), nil
}
