#!/usr/bin/env python3
"""Shows files side by side, syntax-highlighted by bat, with chosen lines highlighted.

usage: split-view.py [--rows N] [--cols N] FILE[:LINES]... -- FILE[:LINES]...

The files before "--" go in the left column, the rest in the right one, each under a header with
its path. LINES lists the lines to highlight, as comma-separated N or N-M (file line numbers, as
bat prints them in the gutter). A highlighted line has a yellow bar in the first column and a
tinted background.

Each file is printed whole when a column fits in --rows rows (default: the terminal's height).
When it does not, the lines furthest from any highlighted line are left out, replaced by one "⋮"
row per gap, with as much context kept around the highlights as fits; the gutter keeps every
line's own number. Long lines wrap inside their column, as bat wraps them. Nothing else is changed.
"""
import os
import re
import shutil
import subprocess
import sys

ANSI = re.compile(r"\x1b\[[0-9;]*m")
RESET = "\x1b[0m"
HL_BG = "\x1b[48;2;72;60;20m"
BAR = "\x1b[1;38;2;255;214;0m▌" + RESET
HEADER = "\x1b[1;38;2;0;175;215m"
DIM = "\x1b[38;2;120;120;120m"


def visible(s):
    return len(ANSI.sub("", s))


def parse_spec(spec):
    path, _, lines = spec.partition(":")
    marked = set()
    for part in filter(None, lines.split(",")):
        lo, _, hi = part.partition("-")
        marked.update(range(int(lo), int(hi or lo) + 1))
    return path, marked


def bat_lines(path, width):
    """The file as bat renders it in a column `width` wide: one list of rows per file line."""
    res = subprocess.run(
        ["bat", "--no-config", "--color=always", "--style=numbers", "--paging=never", "--wrap=character",
         f"--terminal-width={width}", "--theme=Monokai Extended", path],
        capture_output=True, text=True)
    if res.returncode != 0:
        sys.exit(f"split-view.py: bat {path}: {res.stderr.strip()}")
    out = res.stdout
    lines = []
    for row in out.rstrip("\n").split("\n"):
        if re.match(r"\s*\d+ ", ANSI.sub("", row)) or not lines:
            lines.append([row])
        else:
            lines[-1].append(row)
    return lines


def column(specs, width, rows):
    files = []
    for spec in specs:
        path, marked = parse_spec(spec)
        lines = bat_lines(path, width - 1)
        bad = [n for n in marked if not 1 <= n <= len(lines)]
        if bad:
            sys.exit(f"split-view.py: {path} has {len(lines)} lines, not line {min(bad)}")
        files.append((path, marked, lines))
    for context in (None, 12, 10, 8, 6, 5, 4, 3, 2, 1, 0):
        out = []
        for path, marked, lines in files:
            out.append(HEADER + path[-(width - 2):] + RESET)
            gap = False
            for n, rows_of_line in enumerate(lines, 1):
                keep = context is None or any(abs(n - m) <= context for m in marked)
                if not keep:
                    if not gap:
                        out.append(DIM + "      ⋮" + RESET)
                    gap = True
                    continue
                gap = False
                for r in rows_of_line:
                    if n in marked:
                        pad = " " * max(0, width - 1 - visible(r))
                        out.append(BAR + HL_BG + r.replace(RESET, RESET + HL_BG) + pad + RESET)
                    else:
                        out.append(" " + r)
        if len(out) <= rows:
            return out
    sys.exit(f"split-view.py: {', '.join(f[0] for f in files)} do not fit in {rows} rows even with no context")


def main():
    size = shutil.get_terminal_size((130, 39))
    rows, cols, argv = size.lines, size.columns, sys.argv[1:]
    while argv and argv[0] in ("--rows", "--cols"):
        if len(argv) < 2 or not argv[1].isdigit():
            sys.exit(f"split-view.py: {argv[0]} wants a number")
        rows, cols = (int(argv[1]), cols) if argv[0] == "--rows" else (rows, int(argv[1]))
        argv = argv[2:]
    if argv.count("--") != 1:
        sys.exit("usage: split-view.py [--rows N] [--cols N] FILE[:LINES]... -- FILE[:LINES]...")
    cut = argv.index("--")
    left, right = argv[:cut], argv[cut + 1:]
    if not left or not right:
        sys.exit("split-view.py: want at least one file on each side of --")
    for spec in left + right:
        if not os.path.isfile(parse_spec(spec)[0]):
            sys.exit(f"split-view.py: no file {parse_spec(spec)[0]}")
    width = (cols - 3) // 2
    a, b = column(left, width, rows), column(right, width, rows)
    sep = DIM + " │ " + RESET
    for i in range(max(len(a), len(b))):
        l = a[i] if i < len(a) else ""
        r = b[i] if i < len(b) else ""
        print(l + " " * max(0, width - visible(l)) + sep + r)


if __name__ == "__main__":
    main()
