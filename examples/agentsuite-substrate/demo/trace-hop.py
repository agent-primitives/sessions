#!/usr/bin/env python3
"""Prints one hop of a traced turn: the first log line that matches, shortened to one screen row.

usage: trace-hop.py LABEL REGEX KEYS FILE...

Finds the first line, across FILEs in order, that the Python regular expression REGEX matches (a
`[pod/...] ` prefix that `kubectl logs --prefix` adds is dropped first). The line is a JSON object
(slog's JSON handler, substrate's components) or key=value pairs (slog's text handler,
agentsessionsd), or plain text. Prints LABEL, then the line's time (HH:MM:SS.mmm, when it has one),
its message (for key=value pairs with no msg, the first bare word, such as agentctl's EVENT_*), and
the comma-separated KEYS it has, as key=value (a dotted key reaches into a JSON object). With KEYS
empty, a JSON line prints its time and message and any other line is printed as it is. The row is
cut to COLUMNS (default 129) characters, ending in "…" when cut. Exits 1, naming LABEL, when no
line matches.
"""
import json
import os
import re
import sys

PREFIX = re.compile(r"^\[pod/[^\]]*\] ")
PAIR = re.compile(r'([A-Za-z_][A-Za-z0-9_.]*)=("(?:[^"\\]|\\.)*"|\S*)')


def fields(line):
    if line.startswith("{"):
        try:
            obj = json.loads(line)
        except ValueError:
            return None
        flat = {}

        def walk(prefix, v):
            if isinstance(v, dict):
                for k, x in v.items():
                    walk(f"{prefix}.{k}" if prefix else k, x)
            else:
                flat[prefix] = v
        walk("", obj)
        return flat
    pairs = PAIR.findall(line)
    if len(pairs) >= 3:
        out = {k: v[1:-1] if v.startswith('"') else v for k, v in pairs}
        # A record kind or message written as a bare word (agentctl replay's EVENT_*), when the
        # line has no msg key.
        bare = [w for w in line.split() if "=" not in w and not w.startswith('"')]
        if "msg" not in out and bare:
            out["msg"] = bare[0]
        return out
    return None


def hms(t):
    m = re.search(r"T(\d\d:\d\d:\d\d(?:\.\d{1,3})?)", str(t))
    return m.group(1) if m else ""


def main():
    if len(sys.argv) < 5:
        sys.exit(__doc__.split("\n\n")[1])
    label, pattern, keys, files = sys.argv[1], re.compile(sys.argv[2]), sys.argv[3].split(","), sys.argv[4:]
    # The driver shows output two columns in, and keeps two columns spare.
    width = int(os.environ.get("COLUMNS", "129")) - 4
    for path in files:
        with open(path, errors="replace") as f:
            for raw in f:
                line = PREFIX.sub("", raw.rstrip("\n"))
                if not pattern.search(line):
                    continue
                fl = fields(line)
                if fl is None or (keys == [""] and not line.startswith("{")):
                    text = line
                else:
                    t = fl.get("time") or fl.get("ts") or ""
                    msg = fl.get("msg") or fl.get("message") or ""
                    kv = [f"{k}={fl[k]}" for k in keys if k and k in fl]
                    text = " ".join(p for p in [hms(t), str(msg)] + kv if p)
                row = f"{label:<15} {text}"
                if len(row) > width:
                    row = row[:width - 1] + "…"
                print(row)
                return
    sys.exit(f"trace-hop.py: no {label} line matches {sys.argv[2]} in {', '.join(os.path.basename(p) for p in files)}")


if __name__ == "__main__":
    main()
