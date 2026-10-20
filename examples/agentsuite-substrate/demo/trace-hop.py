#!/usr/bin/env python3
"""Prints one hop of a traced turn: the earliest log line that matches, shortened to one screen row.

usage: trace-hop.py LABEL REGEX KEYS FILE...

Finds the lines, across FILEs, that the Python regular expression REGEX matches (a `[pod/...] `
prefix that `kubectl logs --prefix` adds is dropped first) and keeps the earliest by its own time
(the first in file order when no matching line has one). The line is a JSON object
(slog's JSON handler, substrate's components) or key=value pairs (slog's text handler,
agentsessionsd), or plain text. Prints LABEL, then the line's time (HH:MM:SS.mmm, when it has one),
its message (for key=value pairs with no msg, the first bare word, such as agentctl's EVENT_*; a
message that starts with Go's log timestamp, as a container's stderr line does in substrate's JSON
wrapper, loses that second timestamp), and the comma-separated KEYS it has, as key=value. A dotted
key reaches into a JSON object, and NAME=KEY prints KEY's value as NAME=value (so
actor=req.actor.metadata.name prints actor=sess-...). With KEYS empty, a JSON line prints its time
and message and any other line is printed as it is. The row is cut to COLUMNS (default 129)
characters, ending in "…" when cut. Exits 1, naming LABEL, when no line matches.
"""
import json
import os
import re
import sys

PREFIX = re.compile(r"^\[pod/[^\]]*\] ")
# The timestamp Go's log package writes before a message: 2026/10/15 05:23:32[.mmm] .
GO_LOG_TIME = re.compile(r"^\d{4}/\d\d/\d\d \d\d:\d\d:\d\d(?:\.\d+)? ")
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


def sort_key(t):
    """An RFC 3339 UTC time as a sortable tuple, its fraction padded to nanoseconds; None otherwise."""
    m = re.match(r"(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d{1,9}))?Z$", str(t))
    return (m.group(1), (m.group(2) or "").ljust(9, "0")) if m else None


def row_text(line, fl, keys):
    if fl is None or (keys == [""] and not line.startswith("{")):
        return line
    t = fl.get("time") or fl.get("ts") or ""
    msg = str(fl.get("msg") or fl.get("message") or "")
    if t:
        msg = GO_LOG_TIME.sub("", msg)
    kv = []
    for k in keys:
        name, _, key = k.rpartition("=")
        if key and key in fl:
            kv.append(f"{name or key}={fl[key]}")
    return " ".join(p for p in [hms(t), msg] + kv if p)


def main():
    if len(sys.argv) < 5:
        sys.exit(__doc__.split("\n\n")[1])
    label, pattern, keys, files = sys.argv[1], re.compile(sys.argv[2]), sys.argv[3].split(","), sys.argv[4:]
    # The driver shows output two columns in, and keeps two columns spare.
    width = int(os.environ.get("COLUMNS", "129")) - 4
    best = None  # (sort key or None, text)
    for path in files:
        with open(path, errors="replace") as f:
            for raw in f:
                line = PREFIX.sub("", raw.rstrip("\n"))
                if not pattern.search(line):
                    continue
                fl = fields(line)
                at = sort_key((fl or {}).get("time") or (fl or {}).get("ts") or "")
                # The earliest line that has a time; a line without one only when it is the first
                # match and nothing later has a time either.
                if best is None or (at is not None and (best[0] is None or at < best[0])):
                    best = (at, row_text(line, fl, keys))
    if best is None:
        sys.exit(f"trace-hop.py: no {label} line matches {sys.argv[2]} in {', '.join(os.path.basename(p) for p in files)}")
    row = f"{label:<15} {best[1]}"
    if len(row) > width:
        row = row[:width - 1] + "…"
    print(row)


if __name__ == "__main__":
    main()
