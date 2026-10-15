#!/usr/bin/env python3
"""Two sessions' journals side by side: each one's turns (its inputs) and its last answer.

usage: side_by_side.py LEFT_TITLE LEFT_REPLAY RIGHT_TITLE RIGHT_REPLAY [COLUMNS]

LEFT_REPLAY and RIGHT_REPLAY are `agentctl replay` outputs. Prints five rows: the titles, turn 1,
turn 2, and the last answer in two rows; a cell that does not fit ends with "…".
"""
import re
import sys
import textwrap


def turns(path):
    inputs, outputs, cur = [], [], None
    with open(path) as f:
        for line in f:
            line = line.rstrip("\n")
            m = re.match(r"seq=\d+ +EVENT_(\w+)(?:  (\w+): (.*))?$", line)
            if m:
                cur = None
                if m.group(1) == "INPUT":
                    inputs.append(m.group(3))
                if m.group(1) == "OUTPUT":
                    outputs.append(m.group(3))
                    cur = len(outputs) - 1
            elif cur is not None and line.strip():
                outputs[cur] += " / " + line.strip()
    return inputs, outputs


def fit(text, width):
    return text if len(text) <= width else text[: width - 1] + "…"


def column(title, path, width):
    inputs, outputs = turns(path)
    rows = [title]
    for i in range(2):
        rows.append(fit("turn %d: %s" % (i + 1, inputs[i] if i < len(inputs) else "-"), width))
    answer = " ".join((outputs[-1] if outputs else "-").replace("**", "").split())
    rows += textwrap.wrap("answer: " + answer, width, subsequent_indent="        ", max_lines=2, placeholder=" …")
    return rows + [""] * (5 - len(rows))


def main():
    cols = int(sys.argv[5]) if len(sys.argv) > 5 else 108
    width = (cols - 5) // 2
    left = column(sys.argv[1], sys.argv[2], width)
    right = column(sys.argv[3], sys.argv[4], width)
    for a, b in zip(left, right):
        print(("%s │ %s" % (a.ljust(width), b)).rstrip())


if __name__ == "__main__":
    main()
