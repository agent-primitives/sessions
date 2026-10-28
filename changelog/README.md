# Changelog fragments

Each pull request that needs a release note adds a fragment file here instead of editing
[CHANGELOG.md](../CHANGELOG.md), so concurrent pull requests never edit the same lines. A release
assembles the fragments into a new section of `CHANGELOG.md` and deletes them.

## Format

A fragment is `changelog/unreleased/<slug>.md`, where `<slug>` is short, lowercase, and made of
letters, digits and single hyphens, such as `create-harness.md`. It starts with front matter that
names one category, followed by the entry:

```markdown
---
category: Added
---

- `HarnessRegistry.CreateHarness`: atomic create-only registration (spec `REG-3a`), needing the
  `registry.register` action.
```

- `category` is the only front matter key, and must be one of the `CHANGELOG.md` section headings,
  spelled exactly:
  - `Compatibility`: what an operator or client must do or expect on upgrade or rollback: schema
    migrations, new refusals, changed error codes, generated interfaces that gain methods.
  - `Added`: new RPCs, fields, exported Go API, flags and capabilities.
  - `Changed`: changed behavior that is not a compatibility concern.
  - `Fixed`: bug fixes worth a note beyond the commit message.
  - `Security`: a fix for a credential leak or other vulnerability.
- The entry is one or more Markdown list items (`- `), in the style of the existing `CHANGELOG.md`
  entries: wrapped at 100 columns, continuation lines indented two spaces. No headings at any
  indentation, neither `#` lines nor lines of only `=` or `-`: the release section supplies them.
  Put code in a fenced block, with every line of the block indented at least as far as its
  opening fence. Close the block at the fence's indentation with a fence of the same character
  (backtick or tilde), at least as long, and nothing after it.
- A change with entries in two categories, such as a new RPC (`Added`) that older servers refuse
  (`Compatibility`), adds one fragment per category: `create-harness.md` and
  `create-harness-compat.md`.
- Write each link inline: the target in parentheses right after the link text, on the same line,
  with an optional `"title"` after it. Images (`![alt](target)`) likewise. Write relative targets
  relative to the fragment, so they resolve on GitHub (and links are checked by
  `hack/check-doc-links.py`): `../../docs/allocation-ownership.md`, or
  `../../CHANGELOG.md#sqlite-v4-rollout` for a section of the changelog. Assembly rewrites them to
  resolve from `CHANGELOG.md` (`docs/allocation-ownership.md`, `#sqlite-v4-rollout`): it rewrites
  the path, up to the first `?` or `#`, and keeps the query and anchor as written. A target with a
  scheme, such as `https:` or `mailto:`, is left as it is. A link to another fragment, or anywhere
  else under `changelog/unreleased/`, is refused, since a release deletes the fragments; link to
  the documentation or code the entry describes. A relative target with a backslash, `&`, `(` or
  `)` is refused: Markdown decodes backslash escapes (`\/` is `/`) and character references
  (`&#47;`), and runs a target past a `)` that closes a `(` in it, so the link would point
  elsewhere than the path assembly checks; percent-encode the character (`%5C`, `%26`, `%28`,
  `%29`). Assembly cannot rewrite other link forms, so the tool refuses them:
  reference-style link definitions (`[label]: target`), a target in angle brackets, with a backtick
  or with a `'title'`, and HTML `href`, `src` and `srcset` attributes. As a backstop for any other
  form, it refuses `../`, also with its dots or slash encoded (`%2e`, `&#46;`), anywhere but an
  inline link target, a fenced block and inline code that starts and ends on one line; in a link
  target, write `../` literally.
  Keep inline code on one line: after a line that leaves a backtick or HTML tag open, the tool
  cannot tell code from a link, so it refuses a relative link target until the paragraph ends.
- No footnotes, neither references (`[^1]`) nor definitions (`[^1]: note`): a release puts every
  fragment in one document, where footnotes with the same label collide. Write the note in the
  entry. Footnote-shaped text in inline code or a fenced block is fine.
- No HTML comments (`<!-- -->`), other `<!` or `<?` markup, or `pre`, `script`, `style` or
  `textarea` elements outside inline code and fenced blocks, even closed ones. Markdown ends such
  HTML only at its closing marker, so one left open would hide every later entry of the release; the
  tool refuses them all rather than check that each one closes. A line that starts with a
  block-level tag (`<div>`, `<details>`) or with a tag alone on the line opens an HTML block up to
  the next blank line (only spaces or tabs; a line of no-break spaces is not blank), and there
  backticks are text, so the tool checks such lines as written. An inline tag followed by text, such
  as `<a name="x"></a> Moved.`, opens none. In an HTML block the tool refuses a fence, since it
  cannot tell whether the fence opens a code block; put a blank line before it. It also refuses an
  entry that ends in an HTML block: a release joins entries without a blank line, so the block would
  take in the next entry, whose inline code would become raw HTML. End the entry with a line of text
  after a blank line.
- A later pull request that changes an unreleased feature edits that feature's fragment rather than
  adding a second one.

## When a fragment is required

The `changelog` workflow fails a pull request that changes a public contract path without adding or
editing a fragment. Public contract paths are:

- `spec.md`;
- everything under `api/` (the protos, the generated stubs and the Go Runtime SPI), and any `.proto`
  file;
- the non-test Go source (`*.go` but not `*_test.go`) of an importable package: any directory
  outside `internal/`, `testdata/`, `cmd/`, `hack/` and `examples/`, including the
  `integrations/substrate` module.

A change outside those paths still needs a fragment when users need to know about it, for example a
changed `agentsessionsd` or `agentctl` flag, a new default, or a relicensing; reviewers check that.

If a change to a public contract path needs no release note (a refactor, a comment, an internal
helper in an exported package), a maintainer adds the `skip-changelog` label to the pull request and
the check passes. The label waives only the requirement: a fragment that is present is still
validated. The repository has no workflow that manages labels; maintainers create the label once.

The workflow runs on every pull request, and again when a label is added or removed. To run the
same check locally against your branch (`upstream` is the agent-primitives/sessions remote; in a
clone with only `origin`, the command uses `origin/main`):

```bash
base="$(git merge-base upstream/main HEAD 2>/dev/null || git merge-base origin/main HEAD)" &&
  go run ./hack/changelog check -base "${base}" -head HEAD
```

## Release assembly

Before tagging `vX.Y.Z`, a maintainer opens a release pull request that runs:

```bash
go run ./hack/changelog assemble vX.Y.Z
```

The date defaults to today (UTC). To set it, pass the flag before the version:
`go run ./hack/changelog assemble -date YYYY-MM-DD vX.Y.Z`.

This inserts `## vX.Y.Z - <date>` above the previous release, with one `###` subsection per category
in the order Compatibility, Added, Changed, Fixed, Security, and the entries in each ordered by
fragment file name. It then deletes the fragments, keeping `unreleased/.gitkeep`. It refuses, and
changes nothing, when a fragment is malformed or names an unknown category, when `CHANGELOG.md`
already has a `vX.Y.Z` section, or when it still has an `## Unreleased` section, in any heading
syntax (`## Unreleased ##`, a tab after `##`, or `Unreleased` underlined with `-`). With no
fragments it changes nothing, so running it twice is safe. The maintainer then edits the section as
before: a `Compatibility` preamble with upgrade steps, `####` subsections, anchors.

The `release` workflow runs `go run ./hack/changelog verify-assembled` on the tagged commit and
stops before publishing anything if a fragment is still there.
