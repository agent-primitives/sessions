package e2e

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestProvisionScriptListsEveryFixedAtespace keeps provisionScript in step with the suite: the Job's
// editor principal cannot create an atespace, so a test whose default atespace the script does not
// list fails at fixture setup on the live cluster. It runs offline, from source, in the per-PR
// integrations job.
func TestProvisionScriptListsEveryFixedAtespace(t *testing.T) {
	if os.Getenv("AGENTSESSIONS_E2E") == "1" {
		t.Skip("reads the source tree, which the in-cluster test binary does not have")
	}
	provisioned := provisionedAtespaces(t, filepath.Join("..", "..", "..", provisionScript))

	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var used []string
	for _, name := range files {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			switch fn.Name {
			case "env":
				// env("<X>_ATESPACE", "<default>"): the default is the atespace the Job uses.
				if len(call.Args) != 2 || !strings.HasSuffix(stringLit(call.Args[0]), "_ATESPACE") {
					return true
				}
				def := stringLit(call.Args[1])
				if def == "" {
					t.Errorf("%s: an atespace default must be a string literal so this test can check it", fset.Position(call.Pos()))
					return true
				}
				used = append(used, def)
			case "newFixture":
				// newFixture(t, <atespace>): the atespace is an env(...) default, checked above, or a literal.
				if len(call.Args) != 2 {
					return true
				}
				if inner, ok := call.Args[1].(*ast.CallExpr); ok {
					if id, ok := inner.Fun.(*ast.Ident); ok && id.Name == "env" {
						return true
					}
				}
				lit := stringLit(call.Args[1])
				if lit == "" {
					t.Errorf("%s: pass newFixture a string literal or env(\"SUBSTRATE_ATESPACE\", \"<literal>\") so this test can check it", fset.Position(call.Pos()))
					return true
				}
				used = append(used, lit)
			}
			return true
		})
	}
	if len(used) == 0 {
		t.Fatal("found no atespace in the suite's tests; the parser no longer matches how they name one")
	}
	for _, space := range used {
		if !slices.Contains(provisioned, space) {
			t.Errorf("atespace %q is used by the suite but missing from ATESPACES in %s", space, provisionScript)
		}
	}
}

// stringLit returns the value of a string literal expression, or "" for anything else.
func stringLit(e ast.Expr) string {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	v, err := strconv.Unquote(lit.Value)
	if err != nil {
		return ""
	}
	return v
}

// provisionedAtespaces reads the ATESPACES=( ... ) array of the provisioning script: one name per
// line, with an optional trailing comment.
func provisionedAtespaces(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var spaces []string
	in, closed := false, false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case !in && line == "ATESPACES=(":
			in = true
		case in && line == ")":
			in, closed = false, true
		case in:
			if name, _, _ := strings.Cut(line, "#"); strings.TrimSpace(name) != "" {
				spaces = append(spaces, strings.TrimSpace(name))
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if !closed || len(spaces) == 0 {
		t.Fatalf("%s: no ATESPACES=( ... ) array found", path)
	}
	return spaces
}
