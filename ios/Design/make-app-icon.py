#!/usr/bin/env python3
"""Draws the placeholder app icon: a white open loop with an arrowhead dot on blue.

Writes App/Assets.xcassets/AppIcon.appiconset/AppIcon.png (1024x1024, opaque RGB, as App Store
icons must be). Standard library only. Run from ios/:  python3 Design/make-app-icon.py
"""
import math
import struct
import zlib

SIZE = 1024
BG = (0x1E, 0x3A, 0x5F)
FG = (0xFF, 0xFF, 0xFF)
C = SIZE / 2
R_OUT, R_IN = 330, 230          # the ring
GAP_FROM, GAP_TO = -60, -20      # degrees left open, measured from 3 o'clock, counterclockwise
DOT_R = 78                       # the dot at the loop's open end
SS = 4                           # supersampling per axis, for smooth edges


def ring_end():
    a = math.radians(GAP_TO)
    r = (R_OUT + R_IN) / 2
    return C + r * math.cos(a), C - r * math.sin(a)


DOT = ring_end()


def inside(x, y):
    dx, dy = x - C, C - y
    d = math.hypot(dx, dy)
    if (x - DOT[0]) ** 2 + (y - DOT[1]) ** 2 <= DOT_R**2:
        return True
    if not R_IN <= d <= R_OUT:
        return False
    ang = math.degrees(math.atan2(dy, dx))
    return not GAP_FROM <= ang <= GAP_TO


rows = []
for py in range(SIZE):
    row = bytearray([0])
    for px in range(SIZE):
        hits = sum(
            inside(px + (i + 0.5) / SS, py + (j + 0.5) / SS) for i in range(SS) for j in range(SS)
        )
        t = hits / (SS * SS)
        row += bytes(round(b + (f - b) * t) for b, f in zip(BG, FG))
    rows.append(bytes(row))


def chunk(kind, data):
    return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data))


png = b"\x89PNG\r\n\x1a\n"
png += chunk(b"IHDR", struct.pack(">IIBBBBB", SIZE, SIZE, 8, 2, 0, 0, 0))
png += chunk(b"IDAT", zlib.compress(b"".join(rows), 9))
png += chunk(b"IEND", b"")
with open("App/Assets.xcassets/AppIcon.appiconset/AppIcon.png", "wb") as f:
    f.write(png)
