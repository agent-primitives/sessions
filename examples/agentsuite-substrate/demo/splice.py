#!/usr/bin/env python3
"""Splices the two diagrams into a recording of driver.sh show.

usage: splice.py IN.mp4 OUT.mp4 COLOR=DIAGRAM.png...

driver.sh draws a screen of solid rows in one color where a diagram goes: magenta for the build
diagram (end of chapter 2), green for the run diagram (start of chapter 4). For each COLOR this
finds the frames that show its block and the block's bounding box, then overlays DIAGRAM (scaled to
fit the box, centered on the terminal's background color) on exactly those frames. Everything else
is copied frame for frame (re-encoded with the same size and frame rate). Prints each diagram's
time span and box.
"""
import json
import subprocess
import sys

PROBE_W, PROBE_H = 96, 54
COLORS = {
    "magenta": lambda r, g, b: r > 200 and g < 70 and b > 200,
    "green": lambda r, g, b: r < 70 and g > 200 and b < 70,
}


def run(cmd, **kw):
    return subprocess.run(cmd, check=True, **kw)


def probe(path):
    out = run(["ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries",
               "stream=width,height,r_frame_rate", "-of", "json", path], capture_output=True).stdout
    s = json.loads(out)["streams"][0]
    num, den = s["r_frame_rate"].split("/")
    return s["width"], s["height"], float(num) / float(den)


def frames(path, w, h, fps):
    cmd = ["ffmpeg", "-v", "error", "-i", path, "-vf", f"fps={fps},scale={w}:{h}:flags=neighbor",
           "-f", "rawvideo", "-pix_fmt", "rgb24", "-"]
    size = w * h * 3
    with subprocess.Popen(cmd, stdout=subprocess.PIPE) as p:
        while True:
            buf = p.stdout.read(size)
            if len(buf) < size:
                break
            yield buf
    if p.returncode != 0:
        sys.exit(f"ffmpeg exited {p.returncode}")


def share(buf, is_color):
    n = len(buf) // 3
    return sum(1 for i in range(0, len(buf), 3) if is_color(buf[i], buf[i + 1], buf[i + 2])) / n


def main():
    if len(sys.argv) < 4:
        sys.exit(__doc__.split("\n\n")[1])
    src, dst = sys.argv[1:3]
    diagrams = []
    for arg in sys.argv[3:]:
        color, _, png = arg.partition("=")
        if color not in COLORS or not png:
            sys.exit(f"splice.py: want COLOR=PNG with COLOR one of {', '.join(COLORS)}, got {arg}")
        diagrams.append((color, png))
    width, height, fps = probe(src)
    shown = {color: [] for color, _ in diagrams}
    for n, buf in enumerate(frames(src, PROBE_W, PROBE_H, fps)):
        for color, _ in diagrams:
            if share(buf, COLORS[color]) > 0.2:
                shown[color].append(n)
    inputs, graph, last_label, spans = ["-i", src], [], "0:v", []
    for k, (color, png) in enumerate(diagrams, 1):
        hits = shown[color]
        if not hits:
            sys.exit(f"no frame shows the {color} block")
        first, last = hits[0], hits[-1]
        if last - first + 1 != len(hits):
            sys.exit(f"the {color} block is not one span of frames: {len(hits)} frames between {first} and {last}")
        # The box and the terminal's background, from one full-size frame in the middle of the span.
        mid = (first + last) // 2
        raw = run(["ffmpeg", "-v", "error", "-ss", f"{mid / fps:.3f}", "-i", src, "-frames:v", "1",
                   "-f", "rawvideo", "-pix_fmt", "rgb24", "-"], capture_output=True).stdout
        px = lambda x, y: raw[(y * width + x) * 3:(y * width + x) * 3 + 3]
        is_color = COLORS[color]
        rows = [y for y in range(height) if is_color(*px(width // 2, y))]
        cols = [x for x in range(width) if is_color(*px(x, rows[len(rows) // 2]))]
        # Out past the block on every side, on even rows and columns: the encoder blends the block's
        # edge into the next rows (the recording showed them green two rows below the block), and
        # overlay places a yuv420p image on even coordinates only, so an odd box start moved the cover
        # up a row. The top margin stays small: the caption is the row above.
        x0, y0 = (cols[0] - 2) & ~1, (rows[0] - 2) & ~1
        x1, y1 = min(cols[-1] + 2, width - 1), min(rows[-1] + 4, height - 1)
        bw, bh = (x1 - x0 + 2) & ~1, (y1 - y0 + 2) & ~1
        bg = "0x%02x%02x%02x" % tuple(px(width - 4, height - 4))
        t0, t1 = first / fps, (last + 1) / fps
        inputs += ["-loop", "1", "-i", png]
        # The background and the looped image never end; shortest=1 ends the output with the recording.
        graph.append(f"color=c={bg}:s={bw}x{bh}[bg{k}];"
                     f"[{k}:v]scale={bw - 24}:{bh - 24}:force_original_aspect_ratio=decrease[img{k}];"
                     f"[bg{k}][img{k}]overlay=(W-w)/2:(H-h)/2[d{k}];"
                     f"[{last_label}][d{k}]overlay={x0}:{y0}:shortest=1:enable='between(t,{t0 - 0.001:.3f},{t1 - 0.001:.3f})'[v{k}]")
        last_label = f"v{k}"
        spans.append(f"{color}\t{t0:.1f}\t{t1:.1f}\tbox {bw}x{bh}+{x0}+{y0}\tbackground {bg}")
    run(["ffmpeg", "-v", "error", "-y", *inputs, "-filter_complex", ";".join(graph),
         "-map", f"[{last_label}]", "-r", f"{fps:g}", "-c:v", "libx264", "-preset", "fast", "-crf", "20",
         "-pix_fmt", "yuv420p", "-movflags", "+faststart", dst])
    print("\n".join(spans))


if __name__ == "__main__":
    main()
