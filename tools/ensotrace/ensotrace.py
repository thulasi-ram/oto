#!/usr/bin/env python3
"""
Trace oto's painted ensō into a weight ladder of optimised SVG paths.

⭐⭐ WHY THIS EXISTS AT ALL. The severity mark is the one glyph in the product
that wants to look HAND-MADE, and the first attempt at it generated the brush
stroke programmatically — a circular arc with a quadratic width profile, sampled
into a polygon. It was a perfectly good drawing of a brush stroke and it read as
exactly that: a drawing of one. A real stroke has a dry break where the bristles
skipped, ends that are blunt on one side and feathered on the other, and a spine
that wanders off the circle. None of that is parameterisable, and all of it is
already sitting in `assets/logo/oto-icon-mark.svg`, which wraps the original
watercolour painting as an embedded PNG.

So: paint once, trace, and let the artwork be the artwork.

⛔ IT IS NOT A PIXEL TRACE. A naive threshold-and-trace of a watercolour edge
emits thousands of tiny segments chasing paper texture that is invisible below
about 200 px — megabytes of path data to render a 22 px mark. The blur pass below
is what makes the output ~110 Bézier curves and ~4 KB instead: the outline is
smoothed BEFORE potrace sees it, so what gets traced is the stroke's silhouette
rather than its grain.

⭐ THE BLUR IS ALSO THE WEIGHT LADDER, WHICH IS THE ONE IDEA HERE. Blurring
spreads the stroke's edge into a ramp about 2r wide; sweeping a threshold across
that ramp walks the outline inward or outward. One blur, three thresholds, three
weights — all from the same painting, so they are the same stroke rather than
three drawings that have to be kept consistent by hand. Morphological erode and
dilate would do the same job in many more lines and would not smooth anything.

⭐ AND IT IS WORTH KEEPING RATHER THAN DELETING AFTER THE ONE RUN, which is the
instinct a one-shot generator usually deserves. Two reasons it does not here.
`ensoPaths.ts` is eight kilobytes of Bézier coordinates: without this file it is
unmaintainable by construction — nobody can nudge a weight, retrace a repainted
ensō, or answer "why is `heavy` 0.12" by reading it. And the constants below ARE
the design: `RING_MIN_R`, `BLUR`, and the two thresholds each encode a decision
taken against evidence, and several of them encode a decision taken against a
WRONG first answer. Delete the script and the next person rediscovers that coverage
is alpha and not luminance, by seeing 82% of the canvas reported as ink.

Run it with `just enso-trace`. It needs `potrace` (from `just setup`) and `sips`,
which ships with macOS; everything else is the standard library on purpose, so the
recipe has no Python environment to get wrong.

⛔ IT IS NOT IN `just ci`, AND MUST NOT BE. It writes a checked-in generated file,
it needs two tools not every contributor has, and its output changes only when the
artwork or the ladder does — which is a deliberate design edit, not something a
build performs behind somebody's back. Run it by hand and commit the result.
"""
from __future__ import annotations

import base64
import pathlib
import re
import shutil
import struct
import subprocess
import sys
import tempfile

REPO = pathlib.Path(__file__).resolve().parents[2]
SOURCE_SVG = REPO / "assets" / "logo" / "oto-icon-mark.svg"
OUT_TS = REPO / "web" / "src" / "components" / "ensoPaths.ts"

# The painting is 1375x1401. Tracing at 900 px wide is well past the point where
# more resolution changes the traced silhouette, and it keeps the pure-Python blur
# under two seconds.
TRACE_WIDTH = 900

# Coverage threshold on the source alpha, used only to locate the ink.
ALPHA_ON = 64

# ⭐ THE RADIUS THAT SEPARATES THE RING FROM THE FŪRIN HANGING INSIDE IT. The
# painting is the ensō *plus* the wind-bell, and only the ring is wanted. They are
# separable by radius rather than by colour or by connected components, because the
# radial ink profile has two disjoint bands with a genuinely empty gap between
# them: the bell and its tassel live below r≈325 and the ring sits at r≈351..433,
# measured from the ink bbox centre of the 900 px trace. 338 is that gap.
#
# ⛔ IT IS IN TRACE-WIDTH PIXELS, so it moves if `TRACE_WIDTH` does.
RING_MIN_R = 338

# Box-blur radius in trace pixels, and the number of passes. Three passes of a box
# approximate a gaussian closely enough that no edge shows the box's corners.
BLUR = 8
BLUR_PASSES = 3

# ⭐⭐ TWO WEIGHTS, NOT FOUR — AND THE ALLOCATION IS THE DESIGN.
#
# This started as four rungs, one per severity, and lost two of them to evidence.
#
# The first cut put `unknown` at the thin end, which forced a choice between a
# rankable range and a legible floor: widen the range and `unknown` is a hairline,
# lift the floor and `unknown` and `info` become indistinguishable. `unknown` came
# off the ramp — it is the ABSENCE of a reading, not the lightest one, which is
# exactly why the bar ruler draws it as zero filled bars rather than as one.
#
# Then `info` came off too, and for a better reason than tuning. A brush carrying
# less ink does not merely draw thinner, it SKIPS — so the light rung's dry breaks,
# which are the most convincing thing about it at 40 px, fragment into disconnected
# specks at 14 px. It stopped reading as a light ring and started reading as a
# rendering fault. That is not a threshold that needs another pass; it is the size
# telling us a three-rung weight ramp does not fit.
#
# ⭐ SO THE SHAPE BUDGET IS SPENT ON THE ONE BOUNDARY THAT PAGES SOMEBODY. `heavy`
# is `critical` and `regular` is everything else, which means the mark answers "is
# this the one that wakes me" by SILHOUETTE — legible at 14 px, in greyscale, and
# past a colour-vision deficiency. `info` and `warning` are then separated by hue
# alone, and that is defensible here in a way it would not be for critical: the
# pair is oto's info blue against its warning amber, which is the BLUE-YELLOW axis
# — the one that survives protanopia and deuteranopia. Spending shape on
# critical-vs-rest and hue on info-vs-warning puts each channel where it is strong,
# rather than asking one ramp to carry three distinctions badly.
#
# Thresholds are a fraction of the blurred plane's own PEAK, never of 255: the
# source is watercolour and tops out around alpha 200, and blurring pulls the peak
# down further, so absolute cutoffs put the ladder off the end of the ramp entirely
# (the first run traced two empty bitmaps out of four).
#
# Lower threshold = more of the ramp counts as ink = fatter stroke. `heavy` is 0.12
# rather than the 0.08 that would maximise the gap: below about 0.10 the outline
# starts following the BLUR instead of the painting, and the notch at the top and
# the tapered ends — the evidence that a hand made this — round away.
LEVELS: list[tuple[str, float]] = [
    ("regular", 0.38),
    ("heavy", 0.12),
]

# The box the glyph is authored in, matching `~/components/glyphs`: every mark in
# the alphabet inks the same extent inside a 14-unit box so that a column of forty
# of them cannot appear to jitter as the eye runs down it.
BOX = 14.0
INSET = 0.6

NUMBER = r"[-+]?(?:\d*\.\d+|\d+\.?)(?:[eE][-+]?\d+)?"


# --------------------------------------------------------------------------- #
# The source raster                                                           #
# --------------------------------------------------------------------------- #


def require_tools() -> None:
    """
    Fail with a sentence rather than a traceback.

    ⛔ `sips` IS macOS-ONLY, AND THIS IS WHERE THAT IS SAID OUT LOUD. It is used for
    exactly one thing — PNG to BMP, because potrace reads pnm and bmp and not png —
    so a Linux contributor substitutes any converter that writes a 24- or 32-bit
    uncompressed BMP (`magick in.png -resize 900x out.bmp`) and points `to_bmp` at
    it. Discovering that from a `FileNotFoundError` inside a temp directory is a
    considerably worse afternoon.
    """
    missing = [tool for tool in ("potrace", "sips") if shutil.which(tool) is None]
    if missing:
        raise SystemExit(
            f"enso-trace needs {' and '.join(missing)} on PATH.\n"
            "  potrace: `just setup`, or `brew install potrace`.\n"
            "  sips:    ships with macOS. Elsewhere, swap `to_bmp` for any converter\n"
            "           that writes an uncompressed 24/32-bit BMP.",
        )


def extract_png(dest: pathlib.Path) -> pathlib.Path:
    """Pull the embedded painting out of the logo SVG."""
    text = SOURCE_SVG.read_text(errors="replace")
    match = re.search(r'href="data:image/png;base64,([^"]+)"', text)
    if match is None:
        raise SystemExit(f"{SOURCE_SVG} carries no embedded PNG")
    png = dest / "enso-source.png"
    png.write_bytes(base64.b64decode(match.group(1)))
    return png


def to_bmp(png: pathlib.Path, dest: pathlib.Path) -> pathlib.Path:
    """PNG -> BMP via `sips`, because potrace reads pnm and bmp and not png."""
    bmp = dest / "enso-source.bmp"
    subprocess.run(
        ["sips", "-s", "format", "bmp", "--resampleWidth", str(TRACE_WIDTH),
         str(png), "--out", str(bmp)],
        check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    return bmp


def read_bmp(path: pathlib.Path) -> tuple[int, int, int, list[bytes]]:
    """
    The 32bpp BI_BITFIELDS BMP `sips` writes, top-down.

    Only what is needed: the channel order is the standard BGRA and the masks are
    not re-read, because sips does not vary them.
    """
    data = path.read_bytes()
    if data[:2] != b"BM":
        raise SystemExit(f"{path} is not a BMP")
    offset = struct.unpack("<I", data[10:14])[0]
    width, height_signed = struct.unpack("<ii", data[18:26])
    bpp = struct.unpack("<H", data[28:30])[0]
    if bpp not in (24, 32):
        raise SystemExit(f"{path}: unsupported {bpp}bpp")
    height = abs(height_signed)
    pixel = bpp // 8
    stride = ((width * pixel + 3) // 4) * 4
    rows = [data[offset + y * stride : offset + y * stride + width * pixel]
            for y in range(height)]
    if height_signed > 0:
        rows.reverse()
    return width, height, pixel, rows


def coverage(bmp: pathlib.Path) -> tuple[int, int, bytearray]:
    """
    The ring's coverage as a 0..255 plane, with the fūrin masked out.

    ⛔ COVERAGE IS ALPHA, NOT LUMINANCE. The painting has a transparent
    background, and `sips` composites nothing — so RGB is 0 where alpha is 0, and
    reading luminance calls the entire background ink. The first run of this script
    reported 82% coverage for that reason.
    """
    width, height, pixel, rows = read_bmp(bmp)
    if pixel != 4:
        raise SystemExit("expected the 32bpp BMP sips writes")

    plane = bytearray(width * height)
    for y in range(height):
        row = rows[y]
        base = y * width
        for x in range(width):
            plane[base + x] = row[x * pixel + 3]

    xs: list[int] = []
    ys: list[int] = []
    for y in range(height):
        base = y * width
        for x in range(width):
            if plane[base + x] > ALPHA_ON:
                xs.append(x)
                ys.append(y)
    if not xs:
        raise SystemExit("the source raster has no ink")
    cx = (min(xs) + max(xs)) / 2
    cy = (min(ys) + max(ys)) / 2

    cut = RING_MIN_R * RING_MIN_R
    for y in range(height):
        dy2 = (y - cy) ** 2
        base = y * width
        for x in range(width):
            if dy2 + (x - cx) ** 2 < cut:
                plane[base + x] = 0
    return width, height, plane


def box_blur(width: int, height: int, plane: bytearray,
             radius: int, passes: int) -> list[float]:
    """Separable box blur over prefix sums — O(pixels) per pass, no dependencies."""
    current = [float(v) for v in plane]
    for _ in range(passes):
        horizontal = [0.0] * (width * height)
        for y in range(height):
            base = y * width
            acc = [0.0] * (width + 1)
            for x in range(width):
                acc[x + 1] = acc[x] + current[base + x]
            for x in range(width):
                lo = max(0, x - radius)
                hi = min(width, x + radius + 1)
                horizontal[base + x] = (acc[hi] - acc[lo]) / (hi - lo)
        current = [0.0] * (width * height)
        for x in range(width):
            acc = [0.0] * (height + 1)
            for y in range(height):
                acc[y + 1] = acc[y] + horizontal[y * width + x]
            for y in range(height):
                lo = max(0, y - radius)
                hi = min(height, y + radius + 1)
                current[y * width + x] = (acc[hi] - acc[lo]) / (hi - lo)
    return current


def write_pbm(path: pathlib.Path, width: int, height: int,
              blurred: list[float], cut: float) -> int:
    """P4 (binary) PBM — a set bit is ink, rows padded to whole bytes."""
    stride = (width + 7) // 8
    data = bytearray(stride * height)
    on = 0
    for y in range(height):
        base = y * stride
        src = y * width
        for x in range(width):
            if blurred[src + x] >= cut:
                data[base + (x >> 3)] |= 0x80 >> (x & 7)
                on += 1
    with path.open("wb") as handle:
        handle.write(b"P4\n%d %d\n" % (width, height))
        handle.write(data)
    return on


# --------------------------------------------------------------------------- #
# potrace, and flattening what it emits                                       #
# --------------------------------------------------------------------------- #


def run_potrace(pbm: pathlib.Path, svg: pathlib.Path) -> None:
    subprocess.run(
        [
            "potrace", str(pbm), "--svg", "--flat", "-o", str(svg),
            # alphamax 1.0 keeps a corner where the brush really has one and
            # smooths everywhere else; opttolerance 0.6 is comfortably past the
            # point where fewer Béziers stop being visible at glyph size; turdsize
            # drops the speckle a watercolour edge always leaves behind.
            "-a", "1.0", "-O", "0.6", "-t", "60",
        ],
        check=True,
    )


Segment = tuple[str, list[tuple[float, float]]]


def parse_path(d: str) -> list[Segment]:
    """
    potrace's flat SVG subset: an absolute `M`, then relative `c`/`l` runs, `z`.

    Coordinate sets repeat without repeating the command letter, and a relative
    curve's three points are all relative to the segment's START — so the pen only
    advances once the whole set is resolved. Getting that wrong silently produces a
    path that still draws, just not the one that was traced.
    """
    tokens = re.findall(r"[MmLlCcZz]|" + NUMBER, d)
    out: list[Segment] = []
    index = 0
    command = "M"
    x = y = start_x = start_y = 0.0
    while index < len(tokens):
        if tokens[index] in "MmLlCcZz":
            command = tokens[index]
            index += 1
            if index >= len(tokens):
                break
        if command in "Zz":
            out.append(("Z", []))
            x, y = start_x, start_y
            continue
        count = 6 if command in "Cc" else 2
        values = [float(tokens[index + k]) for k in range(count)]
        index += count
        if command.islower():
            points = [(values[k] + x, values[k + 1] + y) for k in range(0, count, 2)]
        else:
            points = [(values[k], values[k + 1]) for k in range(0, count, 2)]
        out.append((command.upper(), points))
        x, y = points[-1]
        if command == "M":
            start_x, start_y = x, y
            command = "L"
        elif command == "m":
            start_x, start_y = x, y
            command = "l"
    return out


def load_traced(svg: pathlib.Path) -> list[Segment]:
    """Read one potrace SVG and bake its group transform into the coordinates."""
    text = svg.read_text()
    transform = re.search(
        r'transform="translate\(([-\d.]+),([-\d.]+)\) scale\(([-\d.]+),([-\d.]+)\)"',
        text,
    )
    path = re.search(r'<path d="(.*?)"', text, re.S)
    if transform is None or path is None:
        raise SystemExit(f"{svg}: not the potrace output this expects")
    tx, ty, sx, sy = (float(v) for v in transform.groups())
    return [
        (command, [(px * sx + tx, py * sy + ty) for px, py in points])
        for command, points in parse_path(path.group(1))
    ]


def emit(segments: list[Segment], x0: float, y0: float,
         scale: float, ox: float, oy: float) -> str:
    parts: list[str] = []
    for command, points in segments:
        if command == "Z":
            parts.append("Z")
            continue
        parts.append(command + " ".join(
            f"{(px - x0) * scale + ox:.2f} {(py - y0) * scale + oy:.2f}"
            for px, py in points
        ))
    return "".join(parts)


HEADER = '''/**
 * The ensō, traced — GENERATED by `just enso-trace`, do not hand-edit.
 *
 * Three weights of ONE brush stroke, taken from the painting embedded in
 * `assets/logo/oto-icon-mark.svg`. `tools/ensotrace/ensotrace.py` carries the
 * whole argument: why the stroke is traced rather than generated, why the blur
 * pass is what keeps this file kilobytes instead of megabytes, and why the blur
 * is also what produces the three weights from a single source.
 *
 * ⭐ ALL THREE SHARE ONE COORDINATE FRAME, measured across the union of their
 * outlines, so the ring does not grow as severity rises. A mark that changes SIZE
 * per row makes a column of them breathe as the eye runs down it, which is the
 * failure the alphabet's constant ink box exists to prevent.
 *
 * ⛔ THERE IS NO PATH PER SEVERITY, AND THAT IS THE DESIGN. `heavy` is `critical`;
 * `regular` is everything else. The shape budget is spent on the one boundary that
 * pages somebody, so that question is answered by silhouette — at 14 px, in
 * greyscale, past a colour-vision deficiency — and `info` versus `warning` is left
 * to hue, which for oto's blue against its amber is the axis that survives. An
 * unknown severity takes `regular` at ghost opacity: it is the ABSENCE of a
 * reading, not the lightest one, which is why the bar ruler draws it as zero
 * filled bars.
 *
 * Draw with `fill="currentColor"` in a `viewBox="0 0 %(box)g %(box)g"`. The hue is
 * the caller's: Tier B belongs to `SEVERITY_TONE` in `~/components/StateChip`.
 */

/** The authored box, matching every other mark in `~/components/glyphs`. */
export const ENSO_BOX = %(box)g;

/** The two weights. `heavy` is critical; `regular` is every other severity. */
export type EnsoWeight = "regular" | "heavy";

export const ENSO_PATH: Record<EnsoWeight, string> = {
'''


def main() -> None:
    require_tools()
    with tempfile.TemporaryDirectory() as raw:
        work = pathlib.Path(raw)
        png = extract_png(work)
        bmp = to_bmp(png, work)
        width, height, plane = coverage(bmp)
        blurred = box_blur(width, height, plane, BLUR, BLUR_PASSES)
        peak = max(blurred)
        print(f"traced {width}x{height}, blurred peak {peak:.1f}/255", file=sys.stderr)

        traced: dict[str, list[Segment]] = {}
        for name, threshold in LEVELS:
            pbm = work / f"{name}.pbm"
            svg = work / f"{name}.svg"
            ink = write_pbm(pbm, width, height, blurred, threshold * peak)
            run_potrace(pbm, svg)
            traced[name] = load_traced(svg)
            curves = sum(1 for command, _ in traced[name] if command == "C")
            print(f"  {name:9s} t={threshold:.2f} ink={ink:7d} curves={curves:4d}",
                  file=sys.stderr)

    xs = [px for segments in traced.values() for _, points in segments for px, _ in points]
    ys = [py for segments in traced.values() for _, points in segments for _, py in points]
    x0, y0, x1, y1 = min(xs), min(ys), max(xs), max(ys)
    span = max(x1 - x0, y1 - y0)
    scale = (BOX - 2 * INSET) / span
    ox = INSET + ((BOX - 2 * INSET) - (x1 - x0) * scale) / 2
    oy = INSET + ((BOX - 2 * INSET) - (y1 - y0) * scale) / 2

    body = "".join(
        f'  {name}:\n    "{emit(traced[name], x0, y0, scale, ox, oy)}",\n'
        for name, _ in LEVELS
    )
    OUT_TS.write_text(HEADER % {"box": BOX} + body + "};\n")
    print(f"wrote {OUT_TS.relative_to(REPO)} ({OUT_TS.stat().st_size} bytes)",
          file=sys.stderr)


main()
