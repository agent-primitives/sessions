package main

import (
	"strings"
	"testing"
)

func TestParseFragment(t *testing.T) {
	const fm = "---\ncategory: Added\n---\n\n"
	cases := []struct {
		name, file, data string
		wantBody         string // checked when wantErr is ""
		wantErr          string // a substring of the error; "" means it parses
	}{
		{name: "valid", file: "x.md", data: fm + "- An entry\n  wrapped.\n", wantBody: "- An entry\n  wrapped."},
		{name: "crlf", file: "x.md", data: "---\r\ncategory: Fixed\r\n---\r\n- Fixed it.\r\n", wantBody: "- Fixed it."},
		{name: "several items", file: "x.md", data: fm + "- One.\n- Two.\n", wantBody: "- One.\n- Two."},
		{name: "unknown category", file: "x.md", data: "---\ncategory: Removed\n---\n- x\n", wantErr: `unknown category "Removed"; use one of Compatibility, Added, Changed, Fixed, Security`},
		{name: "category is case sensitive", file: "x.md", data: "---\ncategory: added\n---\n- x\n", wantErr: `unknown category "added"`},
		{name: "no front matter", file: "x.md", data: "- x\n", wantErr: "must start with front matter"},
		{name: "other key", file: "x.md", data: "---\npr: 224\n---\n- x\n", wantErr: `the only front matter line must be "category: <Category>"`},
		{name: "extra key", file: "x.md", data: "---\ncategory: Added\npr: 224\n---\n- x\n", wantErr: "must start with front matter"},
		{name: "empty body", file: "x.md", data: fm + "\n\n", wantErr: "no entry"},
		{name: "not a list item", file: "x.md", data: fm + "Prose.\n", wantErr: "must be a Markdown list item"},
		{name: "heading", file: "x.md", data: fm + "- x\n\n#### Sub\n", wantErr: "headings are not allowed"},
		{name: "heading indented one space", file: "x.md", data: fm + "- x\n\n ### Removed\n", wantErr: "headings are not allowed"},
		{name: "heading indented three spaces", file: "x.md", data: fm + "- x\n\n   ### Removed\n", wantErr: "headings are not allowed"},
		{name: "heading in a list item", file: "x.md", data: fm + "- x\n\n  ### Removed\n", wantErr: "headings are not allowed"},
		{name: "heading after a tab", file: "x.md", data: fm + "- x\n\n\t# Removed\n", wantErr: "headings are not allowed"},
		{name: "heading as list item", file: "x.md", data: fm + "- x\n- ## Removed\n", wantErr: "headings are not allowed"},
		{name: "heading in ordered list item", file: "x.md", data: fm + "- x\n  1. # Removed\n", wantErr: "headings are not allowed"},
		{name: "heading in block quote", file: "x.md", data: fm + "- x\n\n> ## Removed\n", wantErr: "headings are not allowed"},
		{name: "empty heading", file: "x.md", data: fm + "- x\n\n##\n", wantErr: "headings are not allowed"},
		{name: "setext equals", file: "x.md", data: fm + "- x\n\nRemoved\n=======\n", wantErr: "a line of only = or - (a heading underline or a rule) is not allowed"},
		{name: "setext dashes in a list item", file: "x.md", data: fm + "- x\n  Removed\n  ---\n", wantErr: "a line of only = or - (a heading underline or a rule) is not allowed"},
		{name: "hash in text is not a heading", file: "x.md", data: fm + "- Fixes #224 and C# 8 #tags.\n  #224 too.\n", wantBody: "- Fixes #224 and C# 8 #tags.\n  #224 too."},
		{name: "heading in fence is code", file: "x.md", data: fm + "- x\n\n  ```\n  # comment\n  ```\n", wantBody: "- x\n\n  ```\n  # comment\n  ```"},
		{name: "rule after a blank line", file: "x.md", data: fm + "- x\n\n---\n", wantErr: "a line of only = or - (a heading underline or a rule) is not allowed"},
		{name: "fence body less indented than its fence", file: "x.md", data: fm + "- x\n\n  ```\n# comment\n  ```\n", wantErr: `line "# comment": indented less than the opening fence`},
		{name: "no-break-space line less indented than its fence", file: "x.md", data: fm + "- x\n\n  ```\n\u00a0\n  ```\n", wantErr: "indented less than the opening fence"},
		{name: "closing fence less indented than its fence", file: "x.md", data: fm + "- x\n\n  ```\n  code\n```\n", wantErr: "line \"```\": indented less than the opening fence"},
		{name: "fence indented four spaces hides no heading", file: "x.md", data: fm + "- x\n\n    ```\n### Removed\n    ```\n", wantErr: `line "### Removed": indented less than the opening fence`},
		{name: "tab indent counts to four columns", file: "x.md", data: fm + "- x\n\n    ```\n\tcode\n    ```\n", wantBody: "- x\n\n    ```\n\tcode\n    ```"},
		{name: "blank line in fence", file: "x.md", data: fm + "- x\n\n  ```\n  a\n\n  b\n  ```\n", wantBody: "- x\n\n  ```\n  a\n\n  b\n  ```"},
		{name: "unterminated fence", file: "x.md", data: fm + "- x\n  ```\n", wantErr: "unterminated code fence"},
		{name: "shorter fence does not close a longer one", file: "x.md", data: fm + "- x\n\n  ````\n  code\n  ```\n", wantErr: "unterminated code fence"},
		{name: "fence with trailing text does not close", file: "x.md", data: fm + "- x\n\n  ```\n  code\n  ``` go\n", wantErr: "unterminated code fence"},
		{name: "tilde fence does not close a backtick fence", file: "x.md", data: fm + "- x\n\n  ```\n  code\n  ~~~\n", wantErr: "unterminated code fence"},
		{name: "unterminated tilde fence", file: "x.md", data: fm + "- x\n\n  ~~~\n  code\n", wantErr: "unterminated code fence"},
		{
			name: "longer fence holds a shorter one", file: "x.md",
			data:     fm + "- x\n\n  ````markdown\n  ```\n  # comment\n  [a](../../docs/a.md)\n  ```\n  ````\n",
			wantBody: "- x\n\n  ````markdown\n  ```\n  # comment\n  [a](../../docs/a.md)\n  ```\n  ````",
		},
		{
			name: "tilde fence holds code", file: "x.md",
			data:     fm + "- x\n\n  ~~~sh\n  ```\n  # comment\n  ~~~~\n  [a](../../docs/a.md)\n",
			wantBody: "- x\n\n  ~~~sh\n  ```\n  # comment\n  ~~~~\n  [a](docs/a.md)",
		},
		{name: "closing fence with trailing whitespace closes", file: "x.md", data: fm + "- x\n\n  ```\n  code\n  ```  \t\n", wantBody: "- x\n\n  ```\n  code\n  ```"},
		{name: "backtick in a backtick fence's info string is not a fence", file: "x.md", data: fm + "- x\n\n  ```a`b\n  # comment\n  ```\n", wantErr: `line "  # comment": headings are not allowed`},
		{
			name: "fence line indented four past the opening fence is code", file: "x.md",
			data:     fm + "- x\n\n  ```\n      ```\n  # comment\n  ```\n",
			wantBody: "- x\n\n  ```\n      ```\n  # comment\n  ```",
		},
		{name: "closing fence indented past the opening fence", file: "x.md", data: fm + "- x\n\n  ```\n  code\n    ```\n", wantErr: "line \"    ```\": closing fence indented more than its opening fence"},
		{name: "uppercase name", file: "Create-Harness.md", data: fm + "- x\n", wantErr: "lowercase slug"},
		{name: "not markdown", file: "x.txt", data: fm + "- x\n", wantErr: "lowercase slug"},
		{
			name: "links rewritten to the root", file: "x.md",
			data: fm + "- See [a](../../docs/a.md), [b](../../docs/a.md#sec \"t\"), [c](../../CHANGELOG.md#v4),\n" +
				"  [d](https://example.com/x), [e](#here) and ![i](../../site/i.png).\n",
			wantBody: "- See [a](docs/a.md), [b](docs/a.md#sec \"t\"), [c](#v4),\n" +
				"  [d](https://example.com/x), [e](#here) and ![i](site/i.png).",
		},
		{name: "link outside repository", file: "x.md", data: fm + "- [a](../../../x.md)\n", wantErr: "points outside the repository"},
		{name: "link to another fragment", file: "x.md", data: fm + "- See [related](other.md).\n", wantErr: `link "other.md": points into changelog/unreleased/, which a release deletes`},
		{name: "link to another fragment with an anchor", file: "x.md", data: fm + "- [r](./other.md#x \"t\")\n", wantErr: "points into changelog/unreleased/"},
		{name: "link to another fragment through its directory", file: "x.md", data: fm + "- [r](../unreleased/other.md)\n", wantErr: "points into changelog/unreleased/"},
		{name: "link to another fragment from the root", file: "x.md", data: fm + "- [r](../../changelog/unreleased/other.md)\n", wantErr: "points into changelog/unreleased/"},
		{name: "link to the fragment directory", file: "x.md", data: fm + "- [r](.) and [s](../unreleased/)\n", wantErr: "points into changelog/unreleased/"},
		{name: "link to a query in the fragment directory", file: "x.md", data: fm + "- [r](?plain=1)\n", wantErr: "points into changelog/unreleased/"},
		{name: "percent-encoded link to another fragment", file: "x.md", data: fm + "- [r](../%75nreleased/other.md)\n", wantErr: "points into changelog/unreleased/"},
		{name: "entity-encoded link to another fragment", file: "x.md", data: fm + "- [r](../unr&#101;leased/other.md)\n", wantErr: "a relative target may not contain"},
		{name: "entity-encoded hash in the path", file: "x.md", data: fm + "- [r](x&#35;/../../../docs/a.md)\n", wantErr: "a relative target may not contain"},
		{name: "percent-encoded hash does not end the path", file: "x.md", data: fm + "- [r](x%23/../../../docs/a.md)\n", wantBody: "- [r](docs/a.md)"},
		{name: "link to the changelog directory", file: "x.md", data: fm + "- [r](../README.md) and [s](../)\n", wantBody: "- [r](changelog/README.md) and [s](changelog)"},
		{name: "query with parent steps keeps its path", file: "x.md", data: fm + "- [related](../../docs/a.md?next=/../../changelog/unreleased/other.md)\n", wantBody: "- [related](docs/a.md?next=/../../changelog/unreleased/other.md)"},
		{name: "anchor with parent steps keeps its path", file: "x.md", data: fm + "- [r](../../docs/a.md#x/../../changelog/unreleased/other.md)\n", wantBody: "- [r](docs/a.md#x/../../changelog/unreleased/other.md)"},
		{name: "query into the fragment directory", file: "x.md", data: fm + "- [r](../../docs/a.md?x=1/../../../changelog/unreleased/other.md#y)\n", wantBody: "- [r](docs/a.md?x=1/../../../changelog/unreleased/other.md#y)"},
		{name: "query after a fragment path", file: "x.md", data: fm + "- [r](../../docs/../changelog/unreleased/other.md?x=/../../../docs/a.md)\n", wantErr: "points into changelog/unreleased/"},
		{name: "escaped slash in a link target", file: "x.md", data: fm + "- [related](../unreleased\\/other.md)\n", wantErr: `link "../unreleased\\/other.md": a relative target may not contain \ or &`},
		{name: "backslash in a link target", file: "x.md", data: fm + "- [r](../unreleased\\other.md)\n", wantErr: "a relative target may not contain"},
		{name: "backslash in a link anchor", file: "x.md", data: fm + "- [r](../../docs/a.md#x\\_y)\n", wantErr: "a relative target may not contain"},
		{name: "entity-encoded slash in a link target", file: "x.md", data: fm + "- [r](../unreleased&#47;other.md)\n", wantErr: "a relative target may not contain"},
		{name: "entity-encoded query in a link target", file: "x.md", data: fm + "- [r](../../docs/a.md&#63;x=/../../changelog/unreleased/other.md)\n", wantErr: "a relative target may not contain"},
		{name: "percent-encoded slash to another fragment", file: "x.md", data: fm + "- [r](../unreleased%2Fother.md)\n", wantErr: "points into changelog/unreleased/"},
		{name: "percent-encoded slash out of the fragment directory", file: "x.md", data: fm + "- [r](../../changelog/unreleased%2F..%2F..%2Fdocs/a.md)\n", wantErr: `write each parent directory step as ../, not "..%2F"`},
		{name: "percent-encoded parent step", file: "x.md", data: fm + "- [r](../../docs/%2e%2e/changelog/unreleased/other.md)\n", wantErr: `write each parent directory step as ../, not "%2e%2e/"`},
		{name: "percent-encoded parent step at the end", file: "x.md", data: fm + "- [r](../../changelog/unreleased/x/%2E%2E)\n", wantErr: "points into changelog/unreleased/"},
		{name: "percent-encoded current directory step", file: "x.md", data: fm + "- [r](../../changelog/%2e/unreleased/other.md)\n", wantErr: "points into changelog/unreleased/"},
		{name: "mailto link is a URL", file: "x.md", data: fm + "- [m](mailto:a@example.com)\n", wantBody: "- [m](mailto:a@example.com)"},
		{name: "scheme-like text in a query is not a URL", file: "x.md", data: fm + "- [r](changelog/unreleased/other.md?u=https://example.com)\n", wantErr: "points into changelog/unreleased/"},
		{name: "relative link with a URL in its query", file: "x.md", data: fm + "- [r](../../docs/a.md?u=https://example.com)\n", wantBody: "- [r](docs/a.md?u=https://example.com)"},
		{name: "fragment link in a link title", file: "x.md", data: fm + "- [a](../../docs/a.md \"x](../unreleased/o.md)\")\n", wantErr: "../ is allowed only in an inline link target"},
		{name: "fragment link in angle brackets", file: "x.md", data: fm + "- [a](<../unreleased/o.md>)\n", wantErr: "write each link on one line"},
		{name: "parentheses around a fragment directory step", file: "x.md", data: fm + "- [a](../../docs/a(b)/../../../changelog/unreleased/o.md)\n", wantErr: `link "../../docs/a(b": a relative target may not contain ( or )`},
		{name: "parentheses before escaped dot steps", file: "x.md", data: fm + "- See [n](../../docs/a()/.\\./.\\./changelog/unreleased/o.md).\n", wantErr: "a relative target may not contain ( or )"},
		{name: "parentheses before fully escaped dot steps", file: "x.md", data: fm + "- [n](../../docs/a(x)/\\.\\./\\.\\./changelog/unreleased/o.md)\n", wantErr: "a relative target may not contain ( or )"},
		{name: "parenthesis in a link query", file: "x.md", data: fm + "- [n](../../docs/a.md?x=(y)\n", wantErr: "a relative target may not contain ( or )"},
		{name: "percent-encoded parentheses in a link target", file: "x.md", data: fm + "- [n](../../docs/a%28b%29.md)\n", wantBody: "- [n](docs/a%28b%29.md)"},
		{name: "fragment link in a code span", file: "x.md", data: fm + "- Write `[r](other.md)`.\n", wantBody: "- Write `[r](other.md)`."},
		{name: "absolute link", file: "x.md", data: fm + "- [a](/docs/a.md)\n", wantErr: "relative to the fragment"},
		{
			name: "links in a title and around parentheses", file: "x.md",
			data:     fm + "- [a](../../docs/a.md \"x](y\") and [w](https://example.com/A_(b)).\n",
			wantBody: "- [a](docs/a.md \"x](y\") and [w](https://example.com/A_(b)).",
		},
		{name: "autolink is absolute", file: "x.md", data: fm + "- See <https://example.com/x>.\n", wantBody: "- See <https://example.com/x>."},
		{name: "image in a link", file: "x.md", data: fm + "- [![i](../../site/i.png)](../../docs/a.md)\n", wantBody: "- [![i](site/i.png)](docs/a.md)"},
		{name: "html anchor", file: "x.md", data: fm + "- <a name=\"v4\"></a> Moved.\n", wantBody: "- <a name=\"v4\"></a> Moved."},
		{name: "footnote", file: "x.md", data: fm + "- x[^1]\n\n[^1]: See below.\n", wantErr: `line "- x[^1]": footnotes ([^label] and [^label]: text) are not allowed`},
		{name: "footnote definition", file: "x.md", data: fm + "- x\n\n[^1]: See below.\n", wantErr: `line "[^1]: See below.": footnotes`},
		{name: "footnote definition in a list item", file: "x.md", data: fm + "- x\n\n  [^note]: See below.\n", wantErr: "footnotes ([^label] and [^label]: text) are not allowed"},
		{name: "footnote reference on a continuation line", file: "x.md", data: fm + "- x\n  y[^note].\n", wantErr: "footnotes ([^label] and [^label]: text) are not allowed"},
		{name: "footnote-shaped text in a code span", file: "x.md", data: fm + "- Matches `[^a-z]` and `[^1]: x`.\n", wantBody: "- Matches `[^a-z]` and `[^1]: x`."},
		{name: "footnote-shaped text in a code span after an html anchor", file: "x.md", data: fm + "- <a name=\"x\"></a> Show `[^n]`.\n\n- Matches `[^a-z]`.\n", wantBody: "- <a name=\"x\"></a> Show `[^n]`.\n\n- Matches `[^a-z]`."},
		{name: "footnote after an html anchor", file: "x.md", data: fm + "- <a name=\"x\"></a> Show x[^n].\n", wantErr: "footnotes ([^label] and [^label]: text) are not allowed"},
		{name: "footnote-shaped text in a fenced block", file: "x.md", data: fm + "- x\n\n  ```\n  [^1]: x\n  ```\n", wantBody: "- x\n\n  ```\n  [^1]: x\n  ```"},
		{name: "label followed by text is not a link definition", file: "x.md", data: fm + "- [Breaking]: the API changed.\n", wantBody: "- [Breaking]: the API changed."},
		{name: "link definition", file: "x.md", data: fm + "- See [guide].\n\n[guide]: ../../docs/allocation-ownership.md\n", wantErr: `line "[guide]: ../../docs/allocation-ownership.md": reference-style link definitions are not allowed; write the link inline`},
		{name: "link definition in a list item", file: "x.md", data: fm + "- See [guide].\n\n  [guide]: ../../docs/a.md \"Guide\"\n", wantErr: "reference-style link definitions are not allowed"},
		{name: "link definition as a list item", file: "x.md", data: fm + "- x\n- [guide]: ../../docs/a.md\n", wantErr: "reference-style link definitions are not allowed"},
		{name: "link definition in a block quote", file: "x.md", data: fm + "- x\n\n> [guide]: ../../docs/a.md\n", wantErr: "reference-style link definitions are not allowed"},
		{name: "link definition with target on the next line", file: "x.md", data: fm + "- See [guide].\n\n[guide]:\n  ../../docs/a.md\n", wantErr: "reference-style link definitions are not allowed"},
		{name: "link definition with title on the next line", file: "x.md", data: fm + "- See [guide].\n\n[guide]: ../../docs/a.md\n  \"Guide\"\n", wantErr: "reference-style link definitions are not allowed"},
		{name: "image definition", file: "x.md", data: fm + "- ![logo][l]\n\n[l]: ../../site/i.png\n", wantErr: "reference-style link definitions are not allowed"},
		{name: "absolute link definition", file: "x.md", data: fm + "- See [RFC 8785].\n\n[RFC 8785]: https://www.rfc-editor.org/rfc/rfc8785\n", wantErr: "reference-style link definitions are not allowed"},
		{name: "html link", file: "x.md", data: fm + "- See <a href=\"../../docs/a.md\">a</a>.\n", wantErr: `HTML links and images are not allowed; write a Markdown link`},
		{name: "html image", file: "x.md", data: fm + "- <IMG alt=\"i\" SRC='../../site/i.png'>\n", wantErr: "HTML links and images are not allowed"},
		{name: "link target in angle brackets", file: "x.md", data: fm + "- [a](<../../docs/a.md>)\n", wantErr: "write each link on one line as [text](target)"},
		{name: "link target with spaces in angle brackets", file: "x.md", data: fm + "- [a](<../../docs/a b.md>)\n", wantErr: "write each link on one line"},
		{name: "link with a single-quoted title", file: "x.md", data: fm + "- [a](../../docs/a.md 'A')\n", wantErr: "write each link on one line"},
		{name: "link with a space before the target", file: "x.md", data: fm + "- [a]( ../../docs/a.md)\n", wantErr: "write each link on one line"},
		{name: "link target on the next line", file: "x.md", data: fm + "- [a](\n  ../../docs/a.md)\n", wantErr: "write each link on one line"},
		{name: "unparsed link after a rewritten one", file: "x.md", data: fm + "- [a](../../docs/a.md) [b](../../docs/b.md 'B')\n", wantErr: "write each link on one line"},
		{name: "link definition with an escaped bracket in its label", file: "x.md", data: fm + "- See [a\\]b].\n\n[a\\]b]: ../../docs/a.md\n", wantErr: "reference-style link definitions are not allowed"},
		{name: "link definition with its label on two lines", file: "x.md", data: fm + "- See [my guide].\n\n[my\nguide]: ../../docs/a.md\n", wantErr: `line "guide]: ../../docs/a.md": reference-style link definitions are not allowed`},
		{name: "link definition with a target in angle brackets", file: "x.md", data: fm + "- See [guide].\n\n[guide]: <../../docs/a b.md>\n", wantErr: "reference-style link definitions are not allowed"},
		{name: "link definition in a block quote in a list item", file: "x.md", data: fm + "- > [guide]: ../../docs/a.md\n", wantErr: "reference-style link definitions are not allowed"},
		{name: "footnote definition with its label on two lines", file: "x.md", data: fm + "- x\n\n[^my\nnote]: See below.\n", wantErr: `line "[^my": footnotes`},
		{name: "html link wrapped before its attribute", file: "x.md", data: fm + "- See <a\n  href=\"../../docs/a.md\">a</a>.\n", wantErr: `line "  href=\"../../docs/a.md\">a</a>.": HTML links and images are not allowed`},
		{name: "html image wrapped before its attribute", file: "x.md", data: fm + "- <img\n  src=\"../../site/i.png\">\n", wantErr: "HTML links and images are not allowed"},
		{name: "html attribute wrapped before its value", file: "x.md", data: fm + "- See <a href\n  =\"../../docs/a.md\">a</a>.\n", wantErr: `line "  =\"../../docs/a.md\">a</a>.": ../ is allowed only in an inline link target`},
		{name: "other html attribute", file: "x.md", data: fm + "- <video poster=\"../../site/v.png\"></video>\n", wantErr: "../ is allowed only in an inline link target"},
		{name: "percent-encoded parent directory", file: "x.md", data: fm + "- <object data=\"%2E%2e/%2e%2E/site/v.svg\"></object>\n", wantErr: "../ is allowed only in an inline link target"},
		{name: "parent directory in text", file: "x.md", data: fm + "- Replaces the root module with ../..\n", wantErr: "../ is allowed only in an inline link target"},
		{
			name: "code spans are not checked or rewritten", file: "x.md",
			data:     fm + "- Use `<a href=\"x\">`, ``a `](b` c``, `[a](../../docs/a.md)` and `../..`.\n",
			wantBody: "- Use `<a href=\"x\">`, ``a `](b` c``, `[a](../../docs/a.md)` and `../..`.",
		},
		{name: "escaped backticks are not a code span", file: "x.md", data: fm + "- \\`[a](../../docs/a.md) ../\\`\n", wantErr: "../ is allowed only in an inline link target"},
		{name: "escaped backslash before a code span", file: "x.md", data: fm + "- \\\\`../..`\n", wantBody: "- \\\\`../..`"},
		{name: "backtick in an html tag", file: "x.md", data: fm + "- See <a title=\"`\" href=\"../../docs/a.md\">a</a> `x`\n", wantErr: "HTML links and images are not allowed"},
		{name: "backtick in an autolink", file: "x.md", data: fm + "- See <https://example.com/`> ../ `x`\n", wantErr: "../ is allowed only in an inline link target"},
		{name: "html tag open at the end of a line", file: "x.md", data: fm + "- See <a title=\"x\n  `\" href=\"../../docs/a.md\">a</a> `x`\n", wantErr: "HTML links and images are not allowed"},
		{name: "backticks in an html block", file: "x.md", data: fm + "- x\n\n<p>` <a href=\"../../docs/a.md\">a</a> `</p>\n", wantErr: "HTML links and images are not allowed"},
		{
			name: "code span across lines", file: "x.md",
			data:     fm + "- `a\n  b` [c](https://example.com/c) `d\n  e`\n\n  [f](../../docs/f.md) `../g`\n",
			wantBody: "- `a\n  b` [c](https://example.com/c) `d\n  e`\n\n  [f](docs/f.md) `../g`",
		},
		{name: "link in a code span across lines", file: "x.md", data: fm + "- Write `foo\n  [x](../../docs/a.md)` here.\n", wantErr: `line "  [x](../../docs/a.md)` + "`" + ` here.": cannot tell whether the link to "../../docs/a.md" is in inline code`},
		{name: "link after a stray backtick", file: "x.md", data: fm + "- A stray ` backtick\n  then [x](../../docs/a.md) link.\n", wantErr: "cannot tell whether the link"},
		{name: "link after an html tag open at the end of a line", file: "x.md", data: fm + "- See <a\n  title=\"x\">a</a> [x](../../docs/a.md)\n", wantErr: "cannot tell whether the link"},
		{name: "unchanged link after a stray backtick", file: "x.md", data: fm + "- A stray ` backtick\n  then [x](https://example.com/x) and [y](#y).\n", wantBody: "- A stray ` backtick\n  then [x](https://example.com/x) and [y](#y)."},
		{
			name: "link and code span after an html anchor", file: "x.md",
			data:     fm + "- <a name=\"x\"></a>\n  Use `[x](../../docs/a.md)`, `y` and [z](../../CHANGELOG.md#z).\n",
			wantBody: "- <a name=\"x\"></a>\n  Use `[x](../../docs/a.md)`, `y` and [z](#z).",
		},
		{name: "html anchor before a code span and a link", file: "x.md", data: fm + "- <a name=\"x\"></a> Use `y` and [z](../../docs/z.md).\n", wantBody: "- <a name=\"x\"></a> Use `y` and [z](docs/z.md)."},
		{name: "quoted > in an html attribute", file: "x.md", data: fm + "- See <a title=\">`\" href=\"../../docs/a.md\" x=\"`\">a</a>.\n", wantErr: "HTML links and images are not allowed"},
		{name: "quoted > in an html attribute open at the end of a line", file: "x.md", data: fm + "- See <a title=\">\n  `\" href=\"../../docs/a.md\" x=\"`\">a</a>.\n", wantErr: "HTML links and images are not allowed"},
		{name: "backtick in an html comment", file: "x.md", data: fm + "- x <!-- a > ` --> ../ `y`\n", wantErr: "../ is allowed only in an inline link target"},
		{name: "unterminated html comment", file: "x.md", data: fm + "- Entry.\n\n<!-- draft\n", wantErr: `line "<!-- draft": HTML comments, <!...> and <?...> markup and pre, script, style and textarea elements are not allowed outside code`},
		{name: "closed html comment", file: "x.md", data: fm + "- Entry. <!-- note -->\n", wantErr: "HTML comments, <!...> and <?...> markup"},
		{name: "html comment in a list item", file: "x.md", data: fm + "- Entry.\n\n  <!--\n  draft -->\n", wantErr: `line "  <!--": HTML comments`},
		{name: "html declaration", file: "x.md", data: fm + "- x\n\n<!DOCTYPE html\n", wantErr: "HTML comments, <!...> and <?...> markup"},
		{name: "cdata section", file: "x.md", data: fm + "- x\n\n<![CDATA[ draft\n", wantErr: "HTML comments, <!...> and <?...> markup"},
		{name: "processing instruction", file: "x.md", data: fm + "- x\n\n<?php draft\n", wantErr: "HTML comments, <!...> and <?...> markup"},
		{name: "pre element", file: "x.md", data: fm + "- x\n\n<pre>\n", wantErr: "HTML comments, <!...> and <?...> markup"},
		{name: "script element in capitals", file: "x.md", data: fm + "- x\n\n<SCRIPT type=\"x\">\n", wantErr: "HTML comments, <!...> and <?...> markup"},
		{name: "style element alone on its line", file: "x.md", data: fm + "- x\n\n<style\n", wantErr: "HTML comments, <!...> and <?...> markup"},
		{name: "textarea element", file: "x.md", data: fm + "- x <textarea>y</textarea>\n", wantErr: "HTML comments, <!...> and <?...> markup"},
		{name: "other html element", file: "x.md", data: fm + "- x <preview>y</preview> <b>z</b>\n", wantBody: "- x <preview>y</preview> <b>z</b>"},
		{
			name: "html comment in code", file: "x.md",
			data:     fm + "- Write `<!-- x` or `<pre>`.\n\n  ```html\n  <!-- draft\n  ```\n",
			wantBody: "- Write `<!-- x` or `<pre>`.\n\n  ```html\n  <!-- draft\n  ```",
		},
		{name: "html comment in a code span after an html anchor", file: "x.md", data: fm + "- <a name=\"x\"></a> Show `<!--note-->`.\n\n- Write `<!-- x`.\n", wantBody: "- <a name=\"x\"></a> Show `<!--note-->`.\n\n- Write `<!-- x`."},
		{name: "html comment in a code span after an html anchor alone on its line", file: "x.md", data: fm + "- <a name=\"x\"></a>\n  Write `<!-- x`.\n", wantBody: "- <a name=\"x\"></a>\n  Write `<!-- x`."},
		{name: "html comment after an html anchor", file: "x.md", data: fm + "- <a name=\"x\"></a> Show <!--note-->.\n", wantErr: "HTML comments, <!...> and <?...> markup"},
		{name: "html comment in backticks in an html block", file: "x.md", data: fm + "- <span>\n  Text `<!--` code.\n", wantErr: `line "  Text ` + "`<!--`" + ` code.": HTML comments, <!...> and <?...> markup and pre, script, style and textarea elements are not allowed outside code, and this line may be in an HTML block`},
		{name: "html comment in backticks in a block-level html block", file: "x.md", data: fm + "- <div class=\"x\"> hi\n  `<!--`\n", wantErr: "this line may be in an HTML block"},
		{name: "fence in an html block", file: "x.md", data: fm + "- <div>\n  ```\n  <!--\n  ```\n", wantErr: "line \"  ```\": cannot tell whether this fence opens a code block"},
		// <span> cannot interrupt a paragraph, so the fence opens a code block, whose link must not
		// be rewritten; the tool cannot tell, so it refuses the fence.
		{name: "fence after an inline tag alone on its line", file: "x.md", data: fm + "- text\n  <span>\n  ~~~\n  [x](../../docs/a.md)\n  ~~~\n", wantErr: `line "  ~~~": cannot tell whether this fence opens a code block`},
		{name: "html comment in backticks on the line opening an html block", file: "x.md", data: fm + "- <div>`<!--`\n", wantErr: "which this or an earlier line starting with an HTML tag opens"},
		// cmark-gfm, which GitHub renders with, takes source as a block-level tag.
		{name: "html comment in backticks after a source tag", file: "x.md", data: fm + "- <source> Show `<!-- x`.\n", wantErr: "this line may be in an HTML block"},
		{name: "entry ending in a top-level html block", file: "x.md", data: fm + "- x\n\n<details>\n<summary>Example</summary>\n\nOutput.\n\n</details>\n", wantErr: "x.md: the entry may end in an HTML block"},
		// A block in the list item ends with the item, but the tool does not track containers.
		{name: "entry ending in an html block in its list item", file: "x.md", data: fm + "- x\n\n  <details>\n  </details>\n", wantErr: "the entry may end in an HTML block"},
		// Only spaces and tabs make a blank line, which ends an HTML block; a no-break space does not.
		{name: "html comment in backticks after a no-break-space line in an html block", file: "x.md", data: fm + "- Self-contained.\n\n<div>\n\u00a0\nUse `<!--` here.\n\n</div>\n\nEnd.\n", wantErr: "Use `<!--` here.\": HTML comments, <!...> and <?...> markup and pre, script, style and textarea elements are not allowed outside code, and this line may be in an HTML block"},
		{name: "entry ending in an html block after a no-break-space line", file: "x.md", data: fm + "- x\n\n<details>\n\u00a0\nExample output.\n", wantErr: "the entry may end in an HTML block"},
		// An autolink is text, not a tag: it opens no HTML block, so inline code after it is code.
		{name: "entry ending in a uri autolink", file: "x.md", data: fm + "- x\n- <https://example.com/x>\n", wantBody: "- x\n- <https://example.com/x>"},
		{name: "entry ending in a mailto autolink", file: "x.md", data: fm + "- <mailto:dev@example.com>\n", wantBody: "- <mailto:dev@example.com>"},
		{name: "entry ending in an email autolink", file: "x.md", data: fm + "- <dev@example.com>\n", wantBody: "- <dev@example.com>"},
		{name: "entry ending in an email autolink starting with a slash", file: "x.md", data: fm + "- </x@example.com>\n", wantBody: "- </x@example.com>"},
		{name: "fence after an autolink alone on its line", file: "x.md", data: fm + "- <https://example.com/x>\n  ```\n  <!-- x\n  ```\n", wantBody: "- <https://example.com/x>\n  ```\n  <!-- x\n  ```"},
		{name: "html comment in a code span after an autolink alone on its line", file: "x.md", data: fm + "- <https://example.com/x>\n  Write `<!-- x`.\n", wantBody: "- <https://example.com/x>\n  Write `<!-- x`."},
		{name: "entry ending in a tag with a digit in its name", file: "x.md", data: fm + "- <div2 x=\"y\">\n", wantErr: "the entry may end in an HTML block"},
		{name: "entry ending in a custom element", file: "x.md", data: fm + "- <custom-tag>\n", wantErr: "the entry may end in an HTML block"},
		{name: "entry ending in a closing custom element", file: "x.md", data: fm + "- x\n\n</custom-tag>\n", wantErr: "the entry may end in an HTML block"},
		{name: "entry ending in text after an html block", file: "x.md", data: fm + "- x\n\n<details>\n</details>\n\nMore.\n", wantBody: "- x\n\n<details>\n</details>\n\nMore."},
		{name: "html comment in a code span after an html block", file: "x.md", data: fm + "- <div>\n  x\n  </div>\n\n  Write `<!-- x`.\n", wantBody: "- <div>\n  x\n  </div>\n\n  Write `<!-- x`."},
		{name: "less-than sign in text", file: "x.md", data: fm + "- When a < b, use `../x`;\n  see [y](../../docs/y.md).\n", wantBody: "- When a < b, use `../x`;\n  see [y](docs/y.md)."},
		{name: "link definition with its label's last line a bare ]:", file: "x.md", data: fm + "- See [g].\n\n[g\n]: &#46;&#46;/&#46;&#46;/docs/a.md\n", wantErr: `line "]: &#46;&#46;/&#46;&#46;/docs/a.md": reference-style link definitions are not allowed`},
		{name: "link definition with an empty last label line in a block quote", file: "x.md", data: fm + "- x\n\n> [g\n> ]:\n> ../../docs/a.md\n", wantErr: "reference-style link definitions are not allowed"},
		{name: "entity-encoded parent directory", file: "x.md", data: fm + "- <video poster=\"&#46;&#x2E;/&period;.&sol;site/v.png\"></video>\n", wantErr: "../ is allowed only in an inline link target"},
		{name: "percent-encoded parent directory in a link target", file: "x.md", data: fm + "- [a](%2e%2e/%2e%2e/%2e%2e/x.md)\n", wantErr: `link "%2e%2e/%2e%2e/%2e%2e/x.md": write each parent directory step as ../, not "%2e%2e/"`},
		{name: "entity-encoded parent directory in a link target", file: "x.md", data: fm + "- [a](&#46;&#46;/../../x.md)\n", wantErr: "write each parent directory step as ../"},
		{name: "backslash parent directory in a link target", file: "x.md", data: fm + "- [a](..\\..\\..\\x.md)\n", wantErr: "write each parent directory step as ../"},
		{name: "unmatched backtick run before a code span", file: "x.md", data: fm + "- ``a `b` ../c\n", wantErr: "../ is allowed only in an inline link target"},
		{name: "backtick in a link target", file: "x.md", data: fm + "- [x](a`b) [y](../../docs/c.md) `z) w`\n", wantErr: "write each link on one line"},
		{name: "backtick in a link title", file: "x.md", data: fm + "- [x](a \"t`\") [y](../../docs/c.md) `z`\n", wantErr: "write each link on one line"},
		{name: "code span in a link title", file: "x.md", data: fm + "- [x](../../docs/a.md \"`t`\")\n", wantBody: "- [x](docs/a.md \"`t`\")"},
		{name: "parent directory in a link title", file: "x.md", data: fm + "- [x](../../docs/a.md \"../t\")\n", wantErr: "../ is allowed only in an inline link target"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseFragment(tc.file, []byte(tc.data))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.body != tc.wantBody {
				t.Errorf("body =\n%s\nwant\n%s", got.body, tc.wantBody)
			}
		})
	}
}

func TestParseFragmentsOrder(t *testing.T) {
	entry := func(name, category string) file {
		return file{name: name, data: []byte("---\ncategory: " + category + "\n---\n- " + name + "\n")}
	}
	got, err := parseFragments([]file{
		entry("b.md", "Added"),
		entry("z.md", "Security"),
		entry("a.md", "Fixed"),
		{name: keepFile},
		entry("a.md", "Compatibility"),
		entry("c.md", "Changed"),
		entry("a-b.md", "Added"),
	})
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, f := range got {
		order = append(order, f.category+"/"+f.name)
	}
	want := "Compatibility/a.md Added/a-b.md Added/b.md Changed/c.md Fixed/a.md Security/z.md"
	if s := strings.Join(order, " "); s != want {
		t.Errorf("order = %s, want %s", s, want)
	}
}

func TestParseFragmentsRefusesStrays(t *testing.T) {
	_, err := parseFragments([]file{
		{name: "ok.md", data: []byte("---\ncategory: Added\n---\n- ok\n")},
		{name: "sub", dir: true},
		{name: "note.markdown", data: []byte("---\ncategory: Added\n---\n- x\n")},
		{name: "bad.md", data: []byte("---\ncategory: Misc\n---\n- x\n")},
	})
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"sub/: directories are not allowed", "note.markdown: name must be", `bad.md: unknown category "Misc"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to contain %q", err, want)
		}
	}
}

func TestUnreleasedHeading(t *testing.T) {
	for _, tc := range []struct {
		changelog string
		want      bool
	}{
		{"## Unreleased\n", true},
		{"## unreleased\n", true},
		{"  ## Unreleased  \n", true},
		{"## Unreleased ##\n", true},
		{"## Unreleased #\t\n", true},
		{"##\tUnreleased\n", true},
		{"##\tUnreleased\t##\n", true},
		{"Unreleased\n---\n", true},
		{"Unreleased\n  -\n", true},
		{"# Changelog\r\n\r\n## Unreleased\r\n\r\n## v0.1.0\r\n", true},
		{"## Unreleased ##\r\n", true},
		{"Unreleased\r\n---\r\n", true},
		{"## Unreleased changes\r\n", false},
		{"## Unreleased changes\n", false},
		{"## Unreleased##\n", false},
		{"### Unreleased\n", false},
		{"# Unreleased\n", false},
		{"##Unreleased\n", false},
		{"Unreleased\n", false},
		{"Unreleased\n===\n", false},
		{"- Unreleased\n---\n", false},
		{"## v0.3.0 - 2026-10-22\n\n- Unreleased builds.\n", false},
	} {
		if got := unreleasedHeading(tc.changelog); got != tc.want {
			t.Errorf("unreleasedHeading(%q) = %v, want %v", tc.changelog, got, tc.want)
		}
	}
}
