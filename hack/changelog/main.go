// Command changelog manages changelog fragments: one Markdown file per pull request under
// changelog/unreleased/ that a release assembles into a section of CHANGELOG.md, so concurrent pull
// requests never edit the same lines. The fragment format and the rules are in changelog/README.md.
//
// Usage, from the repository root:
//
//	go run ./hack/changelog assemble [-date YYYY-MM-DD] <version>
//	    Move every fragment into a new "## <version> - <date>" section of CHANGELOG.md, ordered by
//	    category and then file name, and delete the fragments. Refuses unknown categories and
//	    malformed fragments without changing anything; with no fragments it is a no-op.
//	go run ./hack/changelog check -base <rev> -head <rev> [-skip]
//	    The pull request gate: fail when base..head changes a public contract path without adding
//	    or editing a fragment (-skip: the pull request carries the skip-changelog label). Whatever
//	    -skip says, also fail when a fragment at head is malformed, or when CHANGELOG.md at head is
//	    missing, is not a regular file or has an "## Unreleased" section.
//	go run ./hack/changelog verify-assembled
//	    Fail when fragments are still waiting for assembly; the release workflow runs it on the
//	    tagged commit.
//
// Exit status: 0 on success, 1 on a failed check or refused assembly, 2 on a usage error or, in
// check, a git failure.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// printf writes to a terminal or CI log; a failed write has nowhere better to be reported.
func printf(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}

func run(args []string, stdout, stderr io.Writer) int {
	usage := func() int {
		printf(stderr, "usage: changelog assemble [-date YYYY-MM-DD] <version>\n"+
			"       changelog check -base <rev> -head <rev> [-skip]\n"+
			"       changelog verify-assembled\n")
		return 2
	}
	if len(args) == 0 {
		return usage()
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "repository root")
	switch args[0] {
	case "assemble":
		date := fs.String("date", time.Now().UTC().Format(time.DateOnly), "release date, YYYY-MM-DD")
		if fs.Parse(args[1:]) != nil || fs.NArg() != 1 {
			return usage()
		}
		changed, err := assemble(*root, fs.Arg(0), *date)
		if err != nil {
			printf(stderr, "changelog assemble: %v\n", err)
			return 1
		}
		if !changed {
			printf(stdout, "No changelog fragments to assemble; CHANGELOG.md is unchanged.\n")
			return 0
		}
		printf(stdout, "Assembled the fragments into the %s section of CHANGELOG.md and deleted them.\n", fs.Arg(0))
		return 0
	case "check":
		base := fs.String("base", "", "base revision")
		head := fs.String("head", "", "head revision")
		skip := fs.Bool("skip", false, "the pull request carries the "+skipLabel+" label")
		if fs.Parse(args[1:]) != nil || fs.NArg() != 0 || *base == "" || *head == "" {
			return usage()
		}
		report, err := check(*root, *base, *head, *skip)
		printf(stdout, "%s", report)
		if errors.Is(err, errCheckFailed) {
			return 1
		}
		if err != nil {
			printf(stderr, "changelog check: %v\n", err)
			return 2
		}
		return 0
	case "verify-assembled":
		if fs.Parse(args[1:]) != nil || fs.NArg() != 0 {
			return usage()
		}
		if err := verifyAssembled(*root); err != nil {
			printf(stderr, "changelog verify-assembled: %v\n", err)
			return 1
		}
		printf(stdout, "No changelog fragments wait for assembly.\n")
		return 0
	default:
		return usage()
	}
}
