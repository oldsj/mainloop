#!/usr/bin/env python3
"""Draws the app icon from the web app's mark, web/public/favicon.svg: `>_` on the eighties
background, the prompt #6699cc and the cursor #99cc99, each a 3-unit stroke on a 32-unit grid
(butt caps, mitred join, as SVG draws them by default).

Writes App/Assets.xcassets/AppIcon.appiconset/AppIcon.png (1024x1024, opaque RGB, as App Store
icons must be). Standard library only. Run from ios/:  python3 Design/make-app-icon.py
"""
import math
import struct
import zlib

SIZE = 1024
UNIT = SIZE / 32
BG = (0x2D, 0x2D, 0x2D)
PROMPT = (0x66, 0x99, 0xCC)
CURSOR = (0x99, 0xCC, 0x99)
HALF = 1.5                       # half the stroke width, in grid units
SS = 4                           # supersampling per axis, for smooth edges


def offset(p, q, h):
    """The line p→q moved h to its left, as a point and a direction."""
    dx, dy = q[0] - p[0], q[1] - p[1]
    n = math.hypot(dx, dy)
    nx, ny = -dy / n, dx / n
    return (p[0] + nx * h, p[1] + ny * h), (dx, dy)


def meet(a, da, b, db):
    """Where the lines a + t·da and b + s·db cross."""
    det = da[0] * -db[1] - da[1] * -db[0]
    t = ((b[0] - a[0]) * -db[1] - (b[1] - a[1]) * -db[0]) / det
    return a[0] + t * da[0], a[1] + t * da[1]


def stroke(points, h):
    """The outline of a two-segment polyline stroked 2h wide, with butt caps and a mitred join."""
    a, b, c = points
    side = []
    for s in (h, -h):
        p1, d1 = offset(a, b, s)
        p2, d2 = offset(b, c, s)
        side.append([p1, meet(p1, d1, p2, d2), (c[0] + p2[0] - b[0], c[1] + p2[1] - b[1])])
    return side[0] + side[1][::-1]


def inside(poly, x, y):
    hit = False
    for (x1, y1), (x2, y2) in zip(poly, poly[1:] + poly[:1]):
        if (y1 > y) != (y2 > y) and x < x1 + (y - y1) * (x2 - x1) / (y2 - y1):
            hit = not hit
    return hit


CHEVRON = stroke([(8, 10), (15, 16), (8, 22)], HALF)
UNDERSCORE = [(17, 23 - HALF), (25, 23 - HALF), (25, 23 + HALF), (17, 23 + HALF)]


def colour(x, y):
    if inside(CHEVRON, x, y):
        return PROMPT
    if inside(UNDERSCORE, x, y):
        return CURSOR
    return BG


rows = []
for py in range(SIZE):
    row = bytearray([0])
    for px in range(SIZE):
        acc = [0, 0, 0]
        for j in range(SS):
            for i in range(SS):
                c = colour((px + (i + 0.5) / SS) / UNIT, (py + (j + 0.5) / SS) / UNIT)
                acc[0] += c[0]
                acc[1] += c[1]
                acc[2] += c[2]
        row += bytes(round(v / (SS * SS)) for v in acc)
    rows.append(bytes(row))


def chunk(kind, data):
    return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data))


png = b"\x89PNG\r\n\x1a\n"
png += chunk(b"IHDR", struct.pack(">IIBBBBB", SIZE, SIZE, 8, 2, 0, 0, 0))
png += chunk(b"IDAT", zlib.compress(b"".join(rows), 9))
png += chunk(b"IEND", b"")
with open("App/Assets.xcassets/AppIcon.appiconset/AppIcon.png", "wb") as f:
    f.write(png)
