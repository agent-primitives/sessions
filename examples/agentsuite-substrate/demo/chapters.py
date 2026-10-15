#!/usr/bin/env python3
"""Prints "<chapter>\t<seconds>" for each chapter start in a recording of driver.sh show.

driver.sh draws a progress bar across the top row of each chapter: chapter k of 11 fills k/11 of it
with cyan, the rest with gray. A chapter starts at the first frame whose bar is k/11 full. The
geometry matches demo.tape (1920x1080, padding 36, font size 22).

usage: chapters.py demo.mp4
"""
import subprocess
import sys

CHAPTERS = 11
FPS = 10
W, X, Y, H = 1920 - 72, 36, 40, 16  # the bar row, inside the padding
COLS = 224  # samples across the row


def frames(path):
    cmd = ["ffmpeg", "-v", "error", "-i", path, "-vf",
           f"fps={FPS},crop={W}:{H}:{X}:{Y},scale={COLS}:1:flags=area",
           "-f", "rawvideo", "-pix_fmt", "rgb24", "-"]
    with subprocess.Popen(cmd, stdout=subprocess.PIPE) as p:
        while True:
            row = p.stdout.read(COLS * 3)
            if len(row) < COLS * 3:
                break
            yield row
    if p.returncode != 0:
        sys.exit(f"ffmpeg exited {p.returncode}")


def level(row):
    """The bar's fill in tenths, or None when the row is not a bar."""
    cyan = gray = 0
    for i in range(0, len(row), 3):
        r, g, b = row[i], row[i + 1], row[i + 2]
        if r < 60 and g > 120 and b > 160:
            cyan += 1
        elif 40 <= r <= 80 and abs(r - g) < 10 and abs(g - b) < 10:
            gray += 1
    if cyan + gray < 0.9 * COLS or cyan == 0:
        return None
    return round(CHAPTERS * cyan / (cyan + gray))


def main():
    seen = {}
    for n, row in enumerate(frames(sys.argv[1])):
        k = level(row)
        if k and k not in seen:
            seen[k] = n / FPS
    for k in sorted(seen):
        print(f"{k}\t{seen[k]:.1f}")
    if sorted(seen) != list(range(1, CHAPTERS + 1)):
        sys.exit(f"found chapters {sorted(seen)}, want 1-{CHAPTERS}")


if __name__ == "__main__":
    main()
