// Command doccheck type-checks the Go code blocks in the repository's Markdown against the current
// module, so a doc example cannot drift from the API it shows.
//
// Every ```go block must be preceded by a marker comment that says how it is checked:
//
//	<!-- doccheck: decls -->            top-level declarations; common imports are supplied
//	<!-- doccheck: file -->             a whole file with its own imports (package clause optional)
//	<!-- doccheck: body(PARAMS) -->     statements; checked as func _(PARAMS) error { ... }
//	<!-- doccheck: skip REASON -->      not checked, for the reason given (an excerpt, say)
//
// decls and body accept a trailing "with DECLS", Go declarations the block assumes exist, for
// example "with type myHarness = echoagent.Harness". Unused variables, imports and labels are not
// errors, since a block is a fragment of a program; every other type error is.
//
// Usage: go run ./hack/doccheck [file.md ...]   (default: every tracked *.md file)
package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// commonImports are in scope for decls and body blocks. Docs use these names unqualified by an
// import statement; an unused one is not an error.
var commonImports = map[string]string{
	"context":       "context",
	"errors":        "errors",
	"fmt":           "fmt",
	"io":            "io",
	"net":           "net",
	"os":            "os",
	"slog":          "log/slog",
	"strconv":       "strconv",
	"strings":       "strings",
	"sync":          "sync",
	"time":          "time",
	"grpc":          "google.golang.org/grpc",
	"api":           "github.com/agent-primitives/sessions/api",
	"v1":            "github.com/agent-primitives/sessions/api/genpb",
	"client":        "github.com/agent-primitives/sessions/client",
	"controller":    "github.com/agent-primitives/sessions/controller",
	"echoagent":     "github.com/agent-primitives/sessions/harness/echoagent",
	"harnesswire":   "github.com/agent-primitives/sessions/harnesswire",
	"local":         "github.com/agent-primitives/sessions/runtime/local",
	"observability": "github.com/agent-primitives/sessions/observability",
	"placement":     "github.com/agent-primitives/sessions/placement",
	"session":       "github.com/agent-primitives/sessions/session",
	"sqlitelog":     "github.com/agent-primitives/sessions/sqlitelog",
}

var (
	markerRE = regexp.MustCompile(`^<!--\s*doccheck:\s*(.*?)\s*-->\s*$`)
	bodyRE   = regexp.MustCompile(`^body\((.*)\)$`)
)

type block struct {
	file   string
	line   int // line of the first code line
	mode   string
	params string // body only
	with   string
	code   string
}

func main() {
	files := os.Args[1:]
	if len(files) == 0 {
		out, err := exec.Command("git", "ls-files", "*.md").Output()
		if err != nil {
			fail("list Markdown files: %v", err)
		}
		files = strings.Fields(string(out))
	}
	checked, skipped, problems, err := check(files)
	if err != nil {
		fail("%v", err)
	}
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, p)
		}
		fail("%d problem(s) in Go blocks in Markdown", len(problems))
	}
	fmt.Printf("doccheck: %d Go block(s) type-checked, %d skipped by marker\n", checked, skipped)
}

// check extracts and type-checks the Go blocks in files. problems lists what is wrong with the
// blocks, sorted; err is a failure to run the check at all.
func check(files []string) (checked, skipped int, problems []string, err error) {
	var blocks []block
	for _, f := range files {
		bs, errs := extract(f)
		blocks = append(blocks, bs...)
		problems = append(problems, errs...)
	}

	paths := map[string]bool{}
	for _, p := range commonImports {
		paths[p] = true
	}
	fset := token.NewFileSet()
	parsed := make([]*ast.File, len(blocks))
	for i, b := range blocks {
		if b.mode == "skip" {
			continue
		}
		f, err := parser.ParseFile(fset, "doccheck.go", source(b), parser.AllErrors)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		parsed[i] = f
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			paths[p] = true
		}
	}
	exports, err := exportData(paths)
	if err != nil {
		return 0, 0, nil, err
	}
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file, ok := exports[path]
		if !ok || file == "" {
			return nil, fmt.Errorf("no export data for %q", path)
		}
		return os.Open(file)
	})

	for i, f := range parsed {
		if f == nil {
			continue
		}
		checked++
		conf := types.Config{Importer: imp, Error: func(err error) {
			if unusedError(err) {
				return
			}
			problems = append(problems, err.Error())
		}}
		_, _ = conf.Check(fmt.Sprintf("doccheck%d", i), fset, []*ast.File{f}, nil)
	}
	sort.Strings(problems)
	return checked, len(blocks) - checked, problems, nil
}

// unusedRE matches go/types' messages for an unused variable, import or label (go/types
// stmt.go, resolver.go and labels.go). go/types marks these soft, but it marks some real compile
// errors soft too ("no new variables on left side of :=", "cannot range over"), so Soft alone is
// not the test. If a Go release rewords one of these, the gate gets stricter, not weaker.
var unusedRE = regexp.MustCompile(`^(` + strings.Join([]string{
	`declared and not used: \w+`,
	`\w+ declared and not used`,
	`label \w+ declared and not used`,
	`"[^"]+" imported and not used`,
	`"[^"]+" imported as \w+ and not used`,
}, "|") + `)$`)

// unusedError reports whether err is an unused variable, import or label, which is not an error in
// a doc block: a block is a fragment of a program, not the whole of one.
func unusedError(err error) bool {
	var te types.Error
	return errors.As(err, &te) && te.Soft && unusedRE.MatchString(te.Msg)
}

// extract returns the ```go blocks in a Markdown file with their markers. A block without a marker
// is a problem: whether a doc example is checked must be a decision, not an accident.
func extract(path string) ([]block, []string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, []string{err.Error()}
	}
	var (
		blocks   []block
		problems []string
		marker   string
		markerAt int
		cur      *block
		buf      bytes.Buffer
		fence    string
	)
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if cur != nil {
			if strings.HasPrefix(trimmed, fence) && strings.Trim(trimmed, "`~") == "" {
				cur.code = buf.String()
				blocks = append(blocks, *cur)
				cur, fence = nil, ""
				continue
			}
			buf.WriteString(line)
			buf.WriteByte('\n')
			continue
		}
		if fence != "" { // inside a non-Go fence
			if strings.HasPrefix(trimmed, fence) && strings.Trim(trimmed, "`~") == "" {
				fence = ""
			}
			continue
		}
		if m := markerRE.FindStringSubmatch(trimmed); m != nil {
			marker, markerAt = m[1], n
			continue
		}
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			open := trimmed[:3]
			for strings.HasPrefix(trimmed[len(open):], open[:1]) {
				open += open[:1]
			}
			rest := trimmed[len(open):]
			if open[0] == '`' && strings.Contains(rest, "`") {
				marker = "" // a code span, not a fence (CommonMark: no backtick in a backtick info string)
				continue
			}
			fence = open
			info := strings.Fields(rest)
			if len(info) == 0 || info[0] != "go" {
				marker = ""
				continue
			}
			b := block{file: path, line: n + 1}
			if marker == "" || markerAt != n-1 {
				problems = append(problems, fmt.Sprintf("%s:%d: ```go block has no <!-- doccheck: ... --> marker on the line before it", path, n))
				b.mode = "skip"
			} else if err := parseMarker(marker, &b); err != nil {
				problems = append(problems, fmt.Sprintf("%s:%d: %v", path, markerAt, err))
				b.mode = "skip"
			}
			marker = ""
			cur = &b
			buf.Reset()
			continue
		}
		if trimmed != "" {
			marker = ""
		}
	}
	if cur != nil || fence != "" {
		problems = append(problems, fmt.Sprintf("%s: unterminated code fence", path))
	}
	return blocks, problems
}

func parseMarker(m string, b *block) error {
	if m, ok := strings.CutPrefix(m, "skip"); ok {
		if strings.TrimSpace(m) == "" {
			return errors.New("doccheck: skip needs a reason")
		}
		b.mode = "skip"
		return nil
	}
	spec, with, _ := strings.Cut(m, " with ")
	b.with = with
	switch spec = strings.TrimSpace(spec); {
	case spec == "decls" || spec == "file":
		b.mode = spec
	case bodyRE.MatchString(spec):
		b.mode, b.params = "body", bodyRE.FindStringSubmatch(spec)[1]
	default:
		return fmt.Errorf("doccheck: unknown marker %q (want decls, file, body(PARAMS) or skip REASON)", spec)
	}
	if b.mode == "file" && with != "" {
		return errors.New("doccheck: a file block declares what it needs itself; drop the with clause")
	}
	return nil
}

// source wraps a block into a file. //line directives keep every position in terms of the Markdown
// file, so an error points at the doc line to fix.
func source(b block) string {
	var s strings.Builder
	if b.mode == "file" {
		if !regexp.MustCompile(`(?m)^package\s`).MatchString(b.code) {
			s.WriteString("package doccheck\n")
		}
		fmt.Fprintf(&s, "//line %s:%d\n%s", b.file, b.line, b.code)
		return s.String()
	}
	s.WriteString("package doccheck\n\nimport (\n")
	names := make([]string, 0, len(commonImports))
	for name := range commonImports {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&s, "\t%s %q\n", name, commonImports[name])
	}
	s.WriteString(")\n\n")
	if b.with != "" {
		fmt.Fprintf(&s, "//line %s:%d\n%s\n\n", b.file, b.line-2, b.with)
	}
	if b.mode == "body" {
		fmt.Fprintf(&s, "func _(%s) error {\n//line %s:%d\n%s\n\treturn nil\n}\n", b.params, b.file, b.line, b.code)
	} else {
		fmt.Fprintf(&s, "//line %s:%d\n%s", b.file, b.line, b.code)
	}
	return s.String()
}

// exportData builds the packages and returns each one's compiler export data file, which is what
// the gc importer reads; go list does the module resolution.
func exportData(paths map[string]bool) (map[string]string, error) {
	args := []string{"list", "-export", "-deps", "-f", "{{.ImportPath}}\t{{.Export}}"}
	for p := range paths {
		args = append(args, p)
	}
	cmd := exec.Command("go", args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list -export: %v", err)
	}
	exports := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if p, file, ok := strings.Cut(line, "\t"); ok {
			exports[p] = file
		}
	}
	return exports, nil
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "doccheck: "+format+"\n", args...)
	os.Exit(1)
}
