"""Adversarial packet/PACK/receipt framing independent of role-policy replicas."""

import hashlib
import io
import struct
import unittest
import zlib
from dataclasses import replace

from mainloop.push_gate.pack import walk_pack
from mainloop.push_gate.protocol import (
    Limits,
    TransportError,
    pkt,
    receipt,
    receive_commands,
    report,
)

OLD, NEW, REF = "1" * 40, "2" * 40, "refs/heads/feature"
COMMAND = pkt(
    f"{OLD} {NEW} {REF}".encode()
    + b"\0report-status side-band-64k ofs-delta agent=git/2.53.0\n"
)


def packed(data=b"hello", *, count=1, kind=3, declared=None):
    size = len(data) if declared is None else declared
    header = bytearray([(kind << 4) | (size & 15)])
    size >>= 4
    while size:
        header[-1] |= 128
        header.append(size & 127)
        size >>= 7
    content = (
        b"PACK"
        + struct.pack(">II", 2, count)
        + (
            bytes(header) + (b"1" * 20 if kind == 7 else b"") + zlib.compress(data)
            if count
            else b""
        )
    )
    return content + hashlib.sha1(content, usedforsecurity=False).digest()


class PacketTransportTests(unittest.TestCase):
    def test_limits_reject_unbounded_and_wrong_types(self):
        for field, value in (
            ("body_bytes", float("inf")),
            ("objects", 1.5),
            ("cpu_seconds", True),
            ("request_seconds", float("nan")),
            ("memory_bytes", 2**64),
            ("disk_bytes", 0),
        ):
            with self.subTest(field=field, value=value), self.assertRaises(ValueError):
                replace(Limits(), **{field: value})

    def test_complete_command_identity_and_flush_position(self):
        body = COMMAND + b"0000" + packed()
        stream = io.BytesIO(body)
        parsed = receive_commands(stream, Limits())
        self.assertEqual(
            (parsed.update.old_oid, parsed.update.new_oid, parsed.update.ref),
            (OLD, NEW, REF),
        )
        self.assertEqual(parsed.pack_offset, len(COMMAND) + 4)
        self.assertTrue(parsed.sideband)
        self.assertEqual(walk_pack(stream, Limits()).objects, 1)

    def test_shallow_lines_precede_commands_and_are_kept_verbatim(self):
        shallow = pkt(f"shallow {'3' * 40}\n".encode()) + pkt(
            f"shallow {'4' * 40}".encode()
        )
        body = shallow + COMMAND + b"0000" + packed()
        parsed = receive_commands(io.BytesIO(body), Limits())
        self.assertEqual(parsed.shallow, ("3" * 40, "4" * 40))
        self.assertEqual(parsed.update.ref, REF)
        self.assertEqual(parsed.pack_offset, len(shallow) + len(COMMAND) + 4)
        self.assertEqual(
            receive_commands(io.BytesIO(COMMAND + b"0000"), Limits()).shallow, ()
        )
        line = pkt(f"shallow {'3' * 40}\n".encode())
        for data in (
            COMMAND + line + b"0000",
            line + line + COMMAND + b"0000",
            line + b"0000",
            pkt(f"shallow {'0' * 40}\n".encode()) + COMMAND + b"0000",
            pkt(f"shallow {'A' * 40}\n".encode()) + COMMAND + b"0000",
            pkt(f"shallow {'3' * 39}\n".encode()) + COMMAND + b"0000",
            pkt(f"shallow {'3' * 40} extra\n".encode()) + COMMAND + b"0000",
            pkt(f"shallow {'3' * 40}\0report-status\n".encode()) + b"0000",
        ):
            with self.subTest(data=data[:24]), self.assertRaises(TransportError):
                receive_commands(io.BytesIO(data), Limits())
        with self.assertRaises(TransportError):
            receive_commands(
                io.BytesIO(line * 10 + COMMAND + b"0000"),
                replace(Limits(), command_bytes=len(line) * 5),
            )

    def test_packet_and_command_boundaries(self):
        command = f"{OLD} {NEW} {REF}".encode()
        cases = [
            b"",
            b"000",
            b"zzzz",
            b"0001",
            b"0002",
            b"0003",
            b"ffff",
            b"0005",
            COMMAND,
            COMMAND[:-1] + b"0000",
            pkt(command) + b"0000",
            COMMAND + pkt(command) + b"0000",
            COMMAND + pkt(command + b"\0report-status") + b"0000",
            pkt(command + b"\0report-status report-status") + b"0000",
            pkt(
                command.replace(OLD.encode(), OLD.upper().replace("1", "A").encode())
                + b"\0report-status"
            )
            + b"0000",
            pkt(command.replace(b"feature", b"../feature") + b"\0report-status")
            + b"0000",
        ]
        for data in cases:
            with self.subTest(data=data[:10]), self.assertRaises(TransportError):
                receive_commands(io.BytesIO(data), Limits())
        for cap in (
            b"push-options",
            b"report-status-v2",
            b"delete-refs",
            b"atomic",
            b"object-format=sha256",
            b"agent=bad value",
            b"side-band",
            b"signed-push",
            b"quiet",
        ):
            with self.subTest(cap=cap), self.assertRaises(TransportError):
                receive_commands(
                    io.BytesIO(pkt(command + b"\0report-status " + cap) + b"0000"),
                    Limits(),
                )
        for maximum in (len(COMMAND), len(COMMAND) + 4):
            with self.assertRaises(TransportError):
                receive_commands(
                    io.BytesIO(COMMAND + b"0000"),
                    replace(Limits(), command_bytes=maximum),
                )

    def test_pack_consumption_checksum_object_and_expansion_limits(self):
        good = packed(b"a" * 100000)
        cases = [
            good[:-1],
            good + b"trailing",
            good + packed(),
            good[:20] + b"bad" + good[23:],
            packed(count=0) + b"extra",
            packed(b"hello", declared=4),
            packed(b"hello", declared=6),
            packed(kind=5),
            b"PACK" + struct.pack(">II", 4, 0) + b"0" * 20,
        ]
        for data in cases:
            with self.subTest(size=len(data)), self.assertRaises(TransportError):
                walk_pack(io.BytesIO(data), Limits())
        with self.assertRaises(TransportError):
            walk_pack(io.BytesIO(good), replace(Limits(), expanded_bytes=99999))
        with self.assertRaises(TransportError):
            walk_pack(
                io.BytesIO(b"PACK" + struct.pack(">II", 2, 2)),
                replace(Limits(), objects=1),
            )
        facts = walk_pack(io.BytesIO(good), replace(Limits(), expanded_bytes=100000))
        self.assertEqual(facts.expanded_bytes, 100000)
        self.assertEqual(facts.bytes, len(good))
        # A short delta advertises a huge reconstructed result; bound before Git runs.
        delta = b"\x01\x80\x80\x40"
        with self.assertRaisesRegex(TransportError, "pack_expansion_limit"):
            walk_pack(
                io.BytesIO(packed(delta, kind=7)), replace(Limits(), expanded_bytes=100)
            )

    def test_receipt_requires_exact_unpack_ref_flush_and_eof(self):
        for sideband in (False, True):
            self.assertTrue(
                receipt(
                    report(REF, accepted=True, sideband=sideband),
                    REF,
                    sideband=sideband,
                )
            )
            self.assertFalse(
                receipt(
                    report(REF, accepted=False, sideband=sideband),
                    REF,
                    sideband=sideband,
                )
            )
        cases = [
            b"",
            b"ok",
            pkt(b"unpack ok\n") + b"0000",
            report(REF, accepted=True, sideband=False)[:-1],
            report(REF, accepted=True, sideband=False) + b"0000",
            report("refs/heads/other", accepted=True, sideband=False),
            pkt(b"unpack error\n") + pkt(f"ok {REF}\n".encode()) + b"0000",
            pkt(b"unpack ok\n")
            + pkt(f"ok {REF}\n".encode())
            + pkt(b"ok refs/heads/other\n")
            + b"0000",
            pkt(b"unpack ok\n") + pkt(f"ng {REF} \n".encode()) + b"0000",
            pkt(b"unpack ok\nextra\n") + pkt(f"ng {REF} error\n".encode()) + b"0000",
            pkt(b"unpack ok\n") + pkt(f"ng {REF} bad\0error\n".encode()) + b"0000",
        ]
        for data in cases:
            with self.subTest(data=data[:20]), self.assertRaises(TransportError):
                receipt(data, REF, sideband=False)
        for data in (
            pkt(b"\3failure") + b"0000",
            pkt(b"\0bad") + b"0000",
            pkt(b"\1" + report(REF, accepted=True, sideband=False)),
        ):
            with self.assertRaises(TransportError):
                receipt(data, REF, sideband=True)
        status = report(REF, accepted=True, sideband=False)
        progress = pkt(b"\2working\n")
        split = progress + pkt(b"\1" + status[:7]) + pkt(b"\1" + status[7:]) + b"0000"
        self.assertTrue(receipt(split, REF, sideband=True))
