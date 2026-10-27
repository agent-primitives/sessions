package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// version matches a root-module release tag, as hack/release-tags.sh selects them.
var version = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

// finderMetadata is the file macOS Finder writes into a directory it opens. It is never committed
// (check refuses it at a pull request's head), so a local assemble skips it.
const finderMetadata = ".DS_Store"

// readFragmentDir reads every entry of root/fragmentDir except finderMetadata. A missing directory
// has no fragments.
func readFragmentDir(root string) ([]file, error) {
	dir := filepath.Join(root, fragmentDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var files []file
	for _, e := range entries {
		if e.Name() == finderMetadata && !e.IsDir() {
			continue
		}
		f := file{name: e.Name(), dir: e.IsDir()}
		if !f.dir && f.name != keepFile {
			if f.data, err = os.ReadFile(filepath.Join(dir, f.name)); err != nil {
				return nil, err
			}
		}
		files = append(files, f)
	}
	return files, nil
}

// assemble moves every fragment under root/fragmentDir into a new "## <tag> - <date>" section at
// the top of root/CHANGELOG.md and deletes the fragments. It reports whether it changed anything.
//
// It validates everything before writing: an unknown category, a malformed fragment, an
// "## Unreleased" section or an existing section for tag refuses with nothing changed. With no
// fragments it changes nothing, so a second run after a successful one is a no-op.
func assemble(root, tag, date string) (bool, error) {
	if !version.MatchString(tag) {
		return false, fmt.Errorf("version %q is not a release tag such as v0.4.0", tag)
	}
	if _, err := time.Parse(time.DateOnly, date); err != nil {
		return false, fmt.Errorf("date %q is not YYYY-MM-DD", date)
	}
	files, err := readFragmentDir(root)
	if err != nil {
		return false, err
	}
	frags, err := parseFragments(files)
	if err != nil {
		return false, err
	}
	path := filepath.Join(root, "CHANGELOG.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	changelog := string(data)
	if unreleasedHeading(changelog) {
		return false, errors.New("CHANGELOG.md has an \"## Unreleased\" section; move its entries into fragments first")
	}
	if len(frags) == 0 {
		return false, nil
	}
	lines := strings.SplitAfter(changelog, "\n")
	insert := len(lines)
	for i, line := range lines {
		trimmed := strings.TrimRight(line, "\n")
		if trimmed == "## "+tag || strings.HasPrefix(trimmed, "## "+tag+" ") {
			return false, fmt.Errorf("CHANGELOG.md already has a %s section; add these fragments to it by hand or choose another version", tag)
		}
		if strings.HasPrefix(line, "## ") && insert == len(lines) {
			insert = i
		}
	}
	head := strings.Join(lines[:insert], "")
	if insert == len(lines) {
		// No release section yet: append after the introduction.
		head = strings.TrimRight(head, "\n") + "\n\n"
	}
	out := head + renderSection(tag, date, frags) + "\n" + strings.Join(lines[insert:], "")
	out = strings.TrimRight(out, "\n") + "\n"

	// Write the new CHANGELOG.md before deleting any fragment, and through a rename, so a failure
	// leaves either the old file or the new one. A failure after the rename leaves fragments a
	// rerun refuses (their section exists): delete them by hand.
	tmp, err := os.CreateTemp(root, ".CHANGELOG.md.*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(out); err != nil {
		return false, errors.Join(err, tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return false, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return false, err
	}
	for _, f := range frags {
		if err := os.Remove(filepath.Join(root, fragmentDir, f.name)); err != nil {
			return true, fmt.Errorf("CHANGELOG.md is assembled, but deleting %s failed: %w", f.name, err)
		}
	}
	return true, nil
}

// verifyAssembled fails when fragments are still waiting under root/fragmentDir or CHANGELOG.md
// has an "## Unreleased" section: the state a release tag must not point at.
func verifyAssembled(root string) error {
	files, err := readFragmentDir(root)
	if err != nil {
		return err
	}
	var pending []string
	for _, f := range files {
		if f.name != keepFile {
			pending = append(pending, filepath.ToSlash(filepath.Join(fragmentDir, f.name)))
		}
	}
	data, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		return err
	}
	var errs []error
	if len(pending) > 0 {
		errs = append(errs, fmt.Errorf("unassembled changelog fragments: %s; run `go run ./hack/changelog assemble <version>` in a release pull request before tagging",
			strings.Join(pending, ", ")))
	}
	if unreleasedHeading(string(data)) {
		errs = append(errs, errors.New("CHANGELOG.md has an \"## Unreleased\" section"))
	}
	return errors.Join(errs...)
}
