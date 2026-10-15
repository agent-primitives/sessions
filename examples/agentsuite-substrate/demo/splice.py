#!/usr/bin/env python3
"""Splices the turn-flow diagram into a recording of driver.sh show.

usage: splice.py IN.mp4 DIAGRAM.png OUT.mp4

driver.sh draws a screen of magenta rows where chapter 3 shows the diagram. This finds the frames
that show that block and its bounding box, then overlays DIAGRAM (scaled to fit the box, centered on
the terminal's background color) on exactly those frames. Everything else is copied frame for frame
(re-encoded with the same size and frame rate). Prints the diagram's time span and box.
"""
import json
import subprocess
import sys

PROBE_W, PROBE_H = 96, 54


def run(cmd, **kw):
    return subprocess.run(cmd, check=True, **kw)


def probe(path):
    out = run(["ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries",
               "stream=width,height,r_frame_rate", "-of", "json", path], capture_output=True).stdout
    s = json.loads(out)["streams"][0]
    num, den = s["r_frame_rate"].split("/")
    return s["width"], s["height"], float(num) / float(den)


def magenta(r, g, b):
    return r > 200 and g < 70 and b > 200


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


def share(buf):
    n = len(buf) // 3
    hits = sum(1 for i in range(0, len(buf), 3) if magenta(buf[i], buf[i + 1], buf[i + 2]))
    return hits / n


def main():
    src, diagram, dst = sys.argv[1:4]
    width, height, fps = probe(src)
    shown = [n for n, buf in enumerate(frames(src, PROBE_W, PROBE_H, fps)) if share(buf) > 0.2]
    if not shown:
        sys.exit("no frame shows the diagram block")
    first, last = shown[0], shown[-1]
    if last - first + 1 != len(shown):
        sys.exit(f"the diagram block is not one span of frames: {len(shown)} frames between {first} and {last}")
    # The box and the terminal's background, from one full-size frame in the middle of the span.
    mid = (first + last) // 2
    raw = run(["ffmpeg", "-v", "error", "-ss", f"{mid / fps:.3f}", "-i", src, "-frames:v", "1",
               "-f", "rawvideo", "-pix_fmt", "rgb24", "-"], capture_output=True).stdout
    px = lambda x, y: raw[(y * width + x) * 3:(y * width + x) * 3 + 3]
    rows = [y for y in range(height) if magenta(*px(width // 2, y))]
    cols = [x for x in range(width) if magenta(*px(x, rows[len(rows) // 2]))]
    x0, x1, y0, y1 = cols[0], cols[-1], rows[0], rows[-1]
    bw, bh = x1 - x0 + 1, y1 - y0 + 1
    bg = "0x%02x%02x%02x" % tuple(px(width - 4, height - 4))
    t0, t1 = first / fps, (last + 1) / fps
    # The background and the looped image never end; shortest=1 ends the output with the recording.
    graph = (f"color=c={bg}:s={bw}x{bh}[bg];"
             f"[1:v]scale={bw - 24}:{bh - 24}:force_original_aspect_ratio=decrease[img];"
             f"[bg][img]overlay=(W-w)/2:(H-h)/2[d];"
             f"[0:v][d]overlay={x0}:{y0}:shortest=1:enable='between(t,{t0 - 0.001:.3f},{t1 - 0.001:.3f})'[v]")
    run(["ffmpeg", "-v", "error", "-y", "-i", src, "-loop", "1", "-i", diagram, "-filter_complex", graph,
         "-map", "[v]", "-r", f"{fps:g}", "-c:v", "libx264", "-preset", "fast", "-crf", "20",
         "-pix_fmt", "yuv420p", "-movflags", "+faststart", dst])
    print(f"diagram\t{t0:.1f}\t{t1:.1f}\tbox {bw}x{bh}+{x0}+{y0}\tbackground {bg}")


if __name__ == "__main__":
    main()
