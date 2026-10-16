#!/usr/bin/env bash
# Regenerates docs/api-reference.md from the .proto comments.
#
# Run this after any change to api/*.proto. CI enforces that the checked-in reference
# matches what this script produces, so a schema change that skips it fails the build.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

out="docs/api-reference.md"

buf generate --template buf.gen.docs.yaml

# protoc-gen-doc writes multi-line proto comments into table cells verbatim. A newline ends a
# markdown table, so every row after such a comment is dropped from the rendered page: the Sessions
# service table silently lost Suspend, Resume, and Fork. Fold continuation lines back into the cell.
python3 - "${out}" <<'PYFOLD'
import re
import sys

path = sys.argv[1]
lines = open(path).read().split("\n")
out, i = [], 0
while i < len(lines):
    line = lines[i]
    if line.startswith("|") and not line.rstrip().endswith("|"):
        parts = [line.rstrip()]
        i += 1
        while i < len(lines) and not lines[i].rstrip().endswith("|"):
            parts.append(lines[i].strip())
            i += 1
        if i < len(lines):
            parts.append(lines[i].strip())
        merged = " ".join(p for p in parts if p)
        out.append(re.sub(r"\s+", " ", merged))
    else:
        out.append(line)
    i += 1
open(path, "w").write("\n".join(out))
PYFOLD

# protoc-gen-doc links the well-known types to anchors it never emits, because those messages are
# not part of this proto set. The links land nowhere, so point them at the upstream reference.
python3 - "${out}" <<'PYWKT'
import re
import sys

path = sys.argv[1]
text = open(path).read()
text = re.sub(
    r"\[google\.protobuf\.(\w+)\]\(#google-protobuf-\w+\)",
    lambda m: f"[google.protobuf.{m.group(1)}]"
    f"(https://protobuf.dev/reference/protobuf/google.protobuf/#{m.group(1).lower()})",
    text,
)
open(path, "w").write(text)
PYWKT

# protoc-gen-doc's template opens with a ~90-entry table of contents and closes with a 9-column
# table mapping each scalar type to seven languages. The contents list repeats the outline that
# Starlight ("On this page") and GitHub both build from the headings, and on a phone it fills the
# first screens. Drop it. The scalar table is reference boilerplate, so fold it into a <details>
# that is closed by default; field types still link into it.
python3 - "${out}" <<'PYTRIM'
import re
import sys

path = sys.argv[1]
text = open(path).read()

toc = re.compile(r"^## Table of Contents\n.*?(?=^<a name=)", re.M | re.S)
text, n = toc.subn("", text, count=1)
if n != 1:
    sys.exit("gen-api-reference: generated table of contents not found; update this step")

scalar = re.compile(r"^(## Scalar Value Types\n\n)((?:\|.*\n?)+)", re.M)
text, n = scalar.subn(
    lambda m: m.group(1)
    + "<details>\n<summary>"
    + "How each scalar type maps to C++, Java, Python, Go, C#, PHP and Ruby"
    + "</summary>\n\n"
    + m.group(2).rstrip("\n")
    + "\n\n</details>\n",
    text,
    count=1,
)
if n != 1:
    sys.exit("gen-api-reference: Scalar Value Types table not found; update this step")
open(path, "w").write(text)
PYTRIM

# protoc-gen-doc emits no provenance header. A 1300-line file with no banner invites
# hand-edits that the next regeneration silently reverts, so prepend one.
tmp="$(mktemp)"
trap 'rm -f "${tmp}"' EXIT
cat >"${tmp}" <<'EOF'
<!--
GENERATED FILE. DO NOT EDIT.

Regenerate with: ./hack/gen-api-reference.sh
Source of truth: api/*.proto. Edit the proto comments, not this file.
-->

EOF
cat "${out}" >>"${tmp}"
mv "${tmp}" "${out}"

echo "wrote ${out}"
