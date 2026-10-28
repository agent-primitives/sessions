package main

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

// fragmentDir holds the fragments, relative to the repository root.
const fragmentDir = "changelog/unreleased"

// keepFile keeps fragmentDir in git once a release has assembled every fragment.
const keepFile = ".gitkeep"

// categories are the CHANGELOG.md section headings a fragment may name, in the order a release
// section lists them (the order v0.2.0 and v0.3.0 use).
var categories = []string{"Compatibility", "Added", "Changed", "Fixed", "Security"}

var (
	fragmentName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*\.md$`)
	categoryLine = regexp.MustCompile(`^category: (\S.*)$`)
	// atxHeading matches an ATX heading (# Title) at any indentation and inside block quotes and
	// list items, where Markdown still renders it as a heading. It also refuses a "# " line in an
	// indented code block; a fenced block holds code with such lines.
	atxHeading = regexp.MustCompile(`^[ \t]*(?:>[ \t]*)*(?:(?:[-*+]|[0-9]{1,9}[.)])[ \t]+)*#{1,6}(?:[ \t]|$)`)
	// setextUnderline matches a line of only = or -: a heading underline, or after a blank line a
	// thematic break (rule).
	setextUnderline = regexp.MustCompile(`^[ \t]*(?:>[ \t]*)*(?:=+|-+)[ \t]*$`)
	// linkTarget matches the target of an inline link or image, with an optional "title": the one
	// form rewriteLinks rewrites, and the one hack/check-doc-links.py checks. A target with a
	// backtick is not matched, so a code span cannot start inside one.
	linkTarget = regexp.MustCompile(`\]\(([^)\s` + "`" + `]+)((?:\s+"[^"]*")?\))`)
	// containers matches the block quote markers and list markers a line may start with.
	containers = `^[ \t]*(?:(?:>|[-*+][ \t]|[0-9]{1,9}[.)][ \t])[ \t]*)*`
	// linkDefinition matches the line of a link reference definition ([label]: target) with the
	// end of its label, at any indentation and inside block quotes and list items: a label that
	// starts on the line, or the last line of one that started on an earlier line (label]: or a bare
	// ]:), with escaped characters (\]) in it, then a target on the same line, bare or in angle
	// brackets, or on the next. A footnote ([^1]: text) is not one, nor is a label followed by more
	// than a target and a title, which Markdown renders as text.
	linkDefinition = regexp.MustCompile(containers +
		`(?:\[(?:[^\]\\^]|\\.)(?:[^\]\\]|\\.)*|(?:[^\[\]\\]|\\.)*)\]:[ \t]*` +
		`(?:$|(?:<(?:[^>\\]|\\.)*>|\S+)[ \t]*(?:$|["'(]))`)
	// htmlLink matches an HTML link or image target attribute, also when its tag opened on an
	// earlier line. An anchor without one (<a name="x"></a>), which CHANGELOG.md uses, is allowed.
	htmlLink = regexp.MustCompile(`(?i)\b(?:href|src|srcset)\s*=`)
	// htmlStart matches a line that starts with an HTML tag or comment, which can open an HTML block,
	// where backticks do not start code spans.
	htmlStart = regexp.MustCompile(containers + `<`)
	// parentDir matches a parent directory step (../), also with dots or slash percent-encoded or
	// written as HTML character references, or with a backslash, which browsers treat as a slash.
	parentDir = regexp.MustCompile(`(?i)(?:\.|%2e|&#0*46;?|&#x0*2e;?|&period;){2}` +
		`(?:/|\\|%2f|%5c|&#0*47;?|&#x0*2f;?|&sol;|&#0*92;?|&#x0*5c;?|&bsol;)`)
)

// fragment is one parsed changelog/unreleased/<name>.md.
type fragment struct {
	name     string // file name, such as create-harness.md
	category string
	// body is the entry Markdown, trimmed, with relative links rewritten to resolve from the
	// repository root, where CHANGELOG.md lives.
	body string
}

// parseFragment parses and validates one fragment. The format is documented in
// changelog/README.md.
func parseFragment(name string, data []byte) (fragment, error) {
	if !fragmentName.MatchString(name) {
		return fragment{}, fmt.Errorf("%s: name must be a lowercase slug ending in .md, such as create-harness.md", name)
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) < 3 || lines[0] != "---" || lines[2] != "---" {
		return fragment{}, fmt.Errorf("%s: must start with front matter: ---, category: <Category>, ---", name)
	}
	m := categoryLine.FindStringSubmatch(lines[1])
	if m == nil {
		return fragment{}, fmt.Errorf("%s: the only front matter line must be \"category: <Category>\", got %q", name, lines[1])
	}
	category := strings.TrimSpace(m[1])
	if !slices.Contains(categories, category) {
		return fragment{}, fmt.Errorf("%s: unknown category %q; use one of %s", name, category, strings.Join(categories, ", "))
	}
	body := strings.TrimSpace(strings.Join(lines[3:], "\n"))
	if body == "" {
		return fragment{}, fmt.Errorf("%s: no entry after the front matter", name)
	}
	if !strings.HasPrefix(body, "- ") {
		return fragment{}, fmt.Errorf("%s: the entry must be a Markdown list item starting with \"- \"", name)
	}
	body, err := rewriteLinks(body)
	if err != nil {
		return fragment{}, fmt.Errorf("%s: %w", name, err)
	}
	return fragment{name: name, category: category, body: body}, nil
}

// rewriteLinks rewrites each relative link target in body from fragmentDir-relative to
// root-relative, and refuses headings, which belong to the release section. It refuses the link
// forms it cannot rewrite: an inline link in another form than linkTarget, a link reference
// definition (whose label would also apply to every fragment and release section) and an HTML
// link or image attribute. As a backstop for any other form, it refuses a parent directory step
// (../) left outside an inline link target and a code span: every in-repository target written
// relative to the fragment needs one, and a rewritten target has none. A relative target without
// one could only name a file in fragmentDir, which a release deletes.
//
// Fenced code blocks are left alone. A block closes as in CommonMark: on a fence of the opening
// fence's character, at least as long, with nothing but spaces and tabs after it. A non-blank line
// in a fenced block indented less than the opening fence is refused: in a list item it ends the
// item, and so the block, and Markdown renders it as text (a "# " line as a heading) and the later
// closing fence as the start of a block that never closes. A closing fence indented 1 to 3 columns
// past the opening fence is refused too: whether it closes the block depends on the opening fence's
// indentation within its list item. Indented 4 or more past it, it is code.
//
// Code spans are skipped by the link checks only where maskCodeSpans finds them for certain; see
// there. Where it cannot tell whether a line starts in a code span, the line is checked unmasked,
// and a link whose target assembly would change is refused, since it may be code that assembly
// must not rewrite. A link-shaped text in an HTML block, where Markdown renders no links, is
// rewritten like a link: that changes text only.
func rewriteLinks(body string) (string, error) {
	lines := strings.Split(body, "\n")
	var open codeFence // the opening fence of the current block; open.length == 0 outside one
	fenceIndent := 0
	// spansKnown is false from a line that may leave a code span, HTML tag or autolink open until
	// the paragraph ends; htmlSeen is true from a line that may open an HTML block to the end.
	spansKnown, htmlSeen := true, false
	for i, line := range lines {
		if open.length > 0 {
			indent := indentWidth(line)
			if strings.TrimSpace(line) != "" && indent < fenceIndent {
				return "", fmt.Errorf("line %q: indented less than the opening fence of its code block, which ends the block; indent every line of the block at least as far as the fence", line)
			}
			f, ok := parseFence(line)
			if !ok || f.char != open.char || f.length < open.length || strings.Trim(f.info, " \t") != "" ||
				indent >= fenceIndent+4 {
				continue
			}
			if indent > fenceIndent {
				return "", fmt.Errorf("line %q: closing fence indented more than its opening fence; indent it as far as the opening fence", line)
			}
			open, spansKnown = codeFence{}, true
			continue
		}
		if f, ok := parseFence(line); ok && (f.char != '`' || !strings.Contains(f.info, "`")) {
			open, fenceIndent, spansKnown = f, indentWidth(line), true
			continue
		}
		if strings.TrimSpace(line) == "" {
			spansKnown = true
			continue
		}
		if atxHeading.MatchString(line) {
			return "", fmt.Errorf("line %q: headings are not allowed; a release section supplies them", line)
		}
		if setextUnderline.MatchString(line) {
			return "", fmt.Errorf("line %q: a line of only = or - (a heading underline or a rule) is not allowed", line)
		}
		if linkDefinition.MatchString(line) {
			return "", fmt.Errorf("line %q: reference-style link definitions are not allowed; write the link inline, as [text](target)", line)
		}
		if htmlStart.MatchString(line) {
			htmlSeen = true
		}
		// spans is line with its code span contents blanked, at the same byte offsets, when
		// startKnown; masked is spans unless the line may be in an HTML block, where backticks do
		// not start code spans, so the checks see the whole line.
		startKnown := spansKnown
		spans := line
		if startKnown {
			spans, spansKnown = maskCodeSpans(line)
		}
		masked := spans
		if htmlSeen {
			masked = line
		}
		if htmlLink.MatchString(masked) {
			return "", fmt.Errorf("line %q: HTML links and images are not allowed; write a Markdown link, [text](target), or image, ![alt](target)", line)
		}
		var b, rest strings.Builder // rest is masked without the inline link targets
		last := 0
		for _, loc := range linkTarget.FindAllStringSubmatchIndex(masked, -1) {
			if strings.Contains(masked[last:loc[0]], "](") || masked[loc[2]] == '<' {
				return "", unparsedLink(line)
			}
			target := line[loc[2]:loc[3]]
			// A code span cannot start or end between the '(' and the target, which has no backtick:
			// the '(' is blanked in spans exactly when the target is in a code span, which is left
			// as written.
			if !startKnown || spans[loc[2]-1] == '(' {
				rewritten, err := rootRelative(target)
				if err != nil {
					return "", err
				}
				if !startKnown && rewritten != target {
					return "", fmt.Errorf("line %q: cannot tell whether the link to %q is in inline code, because an earlier line of the paragraph leaves a backtick, HTML tag or autolink open; write each piece of inline code on one line and escape a literal backtick as \\`", line, target)
				}
				target = rewritten
			}
			b.WriteString(line[last:loc[2]])
			b.WriteString(target)
			b.WriteString(line[loc[3]:loc[1]])
			rest.WriteString(masked[last:loc[2]])
			rest.WriteString(masked[loc[3]:loc[1]])
			last = loc[1]
		}
		if strings.Contains(masked[last:], "](") {
			return "", unparsedLink(line)
		}
		rest.WriteString(masked[last:])
		if parentDir.MatchString(rest.String()) {
			return "", fmt.Errorf("line %q: ../ is allowed only in an inline link target, [text](target), or in inline code; assembly rewrites only those targets, and any other relative path would resolve from CHANGELOG.md instead of the fragment", line)
		}
		b.WriteString(line[last:])
		lines[i] = b.String()
	}
	if open.length > 0 {
		return "", errors.New("unterminated code fence")
	}
	return strings.Join(lines, "\n"), nil
}

// maskCodeSpans returns line with the contents of each code span on it replaced by '_', and
// whether the next line starts outside any code span, HTML tag and autolink. Pairing follows
// CommonMark: a run of backticks opens a span closed by the next run of the same length, an
// unmatched run is text, and a backslash escapes a backtick. It assumes line starts outside a code
// span, HTML tag and autolink; the caller does not call it after a line where that may not hold. A
// run of backticks inside an HTML tag or autolink that may still be open (see openTag) does not
// open a span: masking stops there. It returns false after an unmatched run, which a later line may
// close, after masking stopped, and when line ends inside a construct openTag reports open.
func maskCodeSpans(line string) (string, bool) {
	b := []byte(line)
	clean := true
	for i := 0; i < len(b); {
		switch {
		case b[i] == '\\' && i+1 < len(b) && (b[i+1] == '`' || b[i+1] == '\\'):
			i += 2
			continue
		case b[i] != '`':
			i++
			continue
		}
		if openTag(b[:i]) {
			return string(b), false
		}
		n := backtickRun(b, i)
		closer := -1
		for j := i + n; j < len(b); {
			if b[j] != '`' {
				j++
				continue
			}
			m := backtickRun(b, j)
			if m == n {
				closer = j
				break
			}
			j += m
		}
		if closer < 0 {
			clean = false
			i += n
			continue
		}
		for k := i + n; k < closer; k++ {
			b[k] = '_'
		}
		i = closer + n
	}
	return string(b), clean && !openTag(b)
}

// backtickRun returns the length of the run of backticks starting at b[i].
func backtickRun(b []byte, i int) int {
	n := 0
	for i+n < len(b) && b[i+n] == '`' {
		n++
	}
	return n
}

// openTag reports whether b ends inside an HTML tag, comment, processing instruction, declaration,
// CDATA section or autolink that starts in it. A quoted attribute value is skipped, so a '>' in one
// does not end its tag. It errs towards open: a '<' before a letter, '/', '!' or '?' counts as a
// start even if the text after it is not a valid tag, which only makes the checks stricter.
func openTag(b []byte) bool {
	for i := 0; i < len(b); i++ {
		if b[i] != '<' {
			continue
		}
		end := tagEnd(b, i)
		if end < 0 {
			return true
		}
		i = end
	}
	return false
}

// tagEnd returns the index of the last byte of the construct that the '<' at b[i] may start, i if
// it cannot start one, or -1 if b ends inside it.
func tagEnd(b []byte, i int) int {
	s := b[i:]
	find := func(from int, end string) int {
		if from > len(s) {
			return -1
		}
		k := bytes.Index(s[from:], []byte(end))
		if k < 0 {
			return -1
		}
		return i + from + k + len(end) - 1
	}
	switch {
	case bytes.HasPrefix(s, []byte("<!--")):
		return find(4, "-->")
	case bytes.HasPrefix(s, []byte("<![CDATA[")):
		return find(9, "]]>")
	case bytes.HasPrefix(s, []byte("<?")):
		return find(2, "?>")
	case len(s) > 1 && (s[1] == '!' || s[1] == '/'):
		return find(2, ">")
	case len(s) > 1 && ('a' <= s[1]|0x20 && s[1]|0x20 <= 'z'):
		for j := 2; j < len(s); j++ {
			switch s[j] {
			case '"', '\'':
				k := bytes.IndexByte(s[j+1:], s[j])
				if k < 0 {
					return -1
				}
				j += k + 1
			case '>':
				return i + j
			}
		}
		return -1
	}
	return i
}

// unparsedLink is the error for a "](" that does not start a linkTarget, such as a target in angle
// brackets, after a space, with a backtick or a 'title' or split across lines.
func unparsedLink(line string) error {
	return fmt.Errorf("line %q: write each link on one line as [text](target) or [text](target \"title\"), with no spaces, angle brackets or backticks around or in the target", line)
}

// codeFence is a run of three or more backticks or tildes that opens or closes a fenced code
// block, and the text after it on its line.
type codeFence struct {
	char   byte
	length int
	info   string
}

// parseFence parses line, after its indentation, as a code fence. A backtick fence opens a block
// only when its info string has no backtick; the caller checks that.
func parseFence(line string) (codeFence, bool) {
	s := strings.TrimLeft(line, " \t")
	if s == "" || (s[0] != '`' && s[0] != '~') {
		return codeFence{}, false
	}
	rest := strings.TrimLeft(s, s[:1])
	f := codeFence{char: s[0], length: len(s) - len(rest), info: rest}
	return f, f.length >= 3
}

// indentWidth returns the column of line's first non-blank character, with a tab advancing to the
// next multiple of 4 as in CommonMark.
func indentWidth(line string) int {
	w := 0
	for _, r := range line {
		switch r {
		case ' ':
			w++
		case '\t':
			w += 4 - w%4
		default:
			return w
		}
	}
	return w
}

// rootRelative turns a link target written relative to fragmentDir into one relative to the
// repository root. URLs and same-file anchors are returned unchanged.
func rootRelative(target string) (string, error) {
	if strings.HasPrefix(target, "#") || strings.Contains(target, "://") ||
		strings.HasPrefix(target, "mailto:") || strings.HasPrefix(target, "tel:") {
		return target, nil
	}
	if strings.HasPrefix(target, "/") {
		return "", fmt.Errorf("link %q: use a path relative to the fragment, such as ../../docs/concepts.md", target)
	}
	// path.Join resolves only a literal ../; a browser also resolves an encoded one, so a target
	// with one could still point outside the repository after the rewrite. The whole target is
	// checked: a character reference has a '#' that is not an anchor.
	for _, step := range parentDir.FindAllString(target, -1) {
		if step != "../" {
			return "", fmt.Errorf("link %q: write each parent directory step as ../, not %q", target, step)
		}
	}
	file, anchor, hasAnchor := strings.Cut(target, "#")
	p := path.Clean(path.Join(fragmentDir, file))
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", fmt.Errorf("link %q: points outside the repository", target)
	}
	if p == "CHANGELOG.md" && hasAnchor {
		return "#" + anchor, nil
	}
	if hasAnchor {
		p += "#" + anchor
	}
	return p, nil
}

// file is a directory entry under fragmentDir with its contents.
type file struct {
	name string
	data []byte
	dir  bool
}

// parseFragments validates every entry of fragmentDir and returns the fragments in release order:
// category order, then file name. Anything other than a valid fragment or keepFile is an error, so
// a misnamed fragment cannot be skipped silently.
func parseFragments(files []file) ([]fragment, error) {
	var frags []fragment
	var errs []error
	for _, f := range files {
		switch {
		case f.name == keepFile:
		case f.dir:
			errs = append(errs, fmt.Errorf("%s/: directories are not allowed in %s", f.name, fragmentDir))
		default:
			frag, err := parseFragment(f.name, f.data)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			frags = append(frags, frag)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	slices.SortFunc(frags, func(a, b fragment) int {
		if c := slices.Index(categories, a.category) - slices.Index(categories, b.category); c != 0 {
			return c
		}
		return strings.Compare(a.name, b.name)
	})
	return frags, nil
}

// renderSection renders the CHANGELOG.md section for a release from fragments in release order.
func renderSection(version, date string, frags []fragment) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## %s - %s\n", version, date)
	current := ""
	for _, f := range frags {
		if f.category != current {
			current = f.category
			fmt.Fprintf(&b, "\n### %s\n\n", current)
		}
		b.WriteString(f.body)
		b.WriteString("\n")
	}
	return b.String()
}

// unreleasedHeading reports whether changelog has an "## Unreleased" section, which fragments
// replace.
func unreleasedHeading(changelog string) bool {
	for line := range strings.SplitSeq(changelog, "\n") {
		if strings.EqualFold(strings.TrimSpace(line), "## Unreleased") {
			return true
		}
	}
	return false
}
