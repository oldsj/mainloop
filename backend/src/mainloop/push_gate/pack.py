"""Bounded PACK framing walker. Git, not this walker, resolves objects and deltas."""

import hashlib
import struct
import time
import zlib
from dataclasses import dataclass
from typing import BinaryIO

from mainloop.push_gate.protocol import Limits, TransportError


@dataclass(frozen=True)
class PackFacts:
    objects: int
    expanded_bytes: int
    bytes: int
    sha1: str


def _byte(stream: BinaryIO) -> int:
    data = stream.read(1)
    if not data:
        raise TransportError("pack_truncated")
    return data[0]


def _delta_size(data: bytes, offset: int) -> tuple[int, int]:
    value = shift = 0
    while True:
        if offset >= len(data) or shift > 63:
            raise TransportError("delta_header")
        byte = data[offset]
        offset += 1
        value |= (byte & 127) << shift
        shift += 7
        if not byte & 128:
            return value, offset


def walk_pack(stream: BinaryIO, limits: Limits) -> PackFacts:
    start = stream.tell()
    deadline = time.monotonic() + limits.validation_seconds
    header = stream.read(12)
    if len(header) != 12 or header[:4] != b"PACK":
        raise TransportError("pack_header")
    version, count = struct.unpack(">II", header[4:])
    if version not in (2, 3) or count > limits.objects:
        raise TransportError("pack_limit")
    expanded = 0
    offsets = set()
    for _ in range(count):
        if time.monotonic() > deadline:
            raise TransportError("validation_timeout")
        object_offset = stream.tell() - start
        first = _byte(stream)
        kind, size, shift = (first >> 4) & 7, first & 15, 4
        byte = first
        while byte & 128:
            if shift > 63:
                raise TransportError("pack_object_size")
            byte = _byte(stream)
            size |= (byte & 127) << shift
            shift += 7
        if kind not in (1, 2, 3, 4, 6, 7) or size > limits.expanded_bytes:
            raise TransportError("pack_object_size")
        if kind == 6:
            byte = _byte(stream)
            distance = byte & 127
            while byte & 128:
                byte = _byte(stream)
                distance = ((distance + 1) << 7) | (byte & 127)
                if distance > object_offset:
                    raise TransportError("pack_delta_offset")
            if distance == 0 or object_offset - distance not in offsets:
                raise TransportError("pack_delta_offset")
        elif kind == 7 and len(stream.read(20)) != 20:
            raise TransportError("pack_truncated")
        offsets.add(object_offset)
        inflater = zlib.decompressobj()
        consumed = 0
        prefix = bytearray()
        while not inflater.eof:
            if time.monotonic() > deadline:
                raise TransportError("validation_timeout")
            chunk = stream.read(64 * 1024)
            if not chunk:
                raise TransportError("pack_truncated")
            try:
                output = inflater.decompress(chunk, min(64 * 1024, size - consumed + 1))
            except zlib.error:
                raise TransportError("pack_compression") from None
            consumed += len(output)
            if consumed > size:
                raise TransportError("pack_object_size")
            if len(prefix) < 20:
                prefix.extend(output[: 20 - len(prefix)])
            unused = inflater.unused_data if inflater.eof else inflater.unconsumed_tail
            if unused:
                stream.seek(-len(unused), 1)
        if consumed != size:
            raise TransportError("pack_object_size")
        object_expansion = size
        if kind in (6, 7):
            base_size, offset = _delta_size(prefix, 0)
            result_size, _ = _delta_size(prefix, offset)
            if base_size > limits.expanded_bytes or result_size > limits.expanded_bytes:
                raise TransportError("pack_expansion_limit")
            object_expansion = max(size, result_size)
        expanded += object_expansion
        if expanded > limits.expanded_bytes:
            raise TransportError("pack_expansion_limit")
    trailer_offset = stream.tell()
    trailer = stream.read(20)
    if len(trailer) != 20 or stream.read(1):
        raise TransportError("pack_trailing_or_truncated")
    digest = hashlib.sha1(usedforsecurity=False)
    stream.seek(start)
    remaining = trailer_offset - start
    while remaining:
        chunk = stream.read(min(64 * 1024, remaining))
        digest.update(chunk)
        remaining -= len(chunk)
    if digest.digest() != trailer:
        raise TransportError("pack_checksum")
    stream.seek(trailer_offset + 20)
    return PackFacts(count, expanded, trailer_offset + 20 - start, digest.hexdigest())
