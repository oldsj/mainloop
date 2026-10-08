"""Strict SHA-1 smart-HTTP framing; no authority is inferred from packets."""

import math
import re
from dataclasses import dataclass
from typing import BinaryIO

from mainloop.push_gate.authorization import ZERO_OID
from models.push_gate import RefUpdate


class TransportError(ValueError):
    """Only a constant, non-sensitive code may cross the HTTP boundary."""

    def __init__(self, code: str):
        self.code = (
            code
            if re.fullmatch(r"[a-z][a-z_]{1,63}", code)
            else "transport_unavailable"
        )
        super().__init__(self.code)


@dataclass(frozen=True)
class Limits:
    command_bytes: int = 256 * 1024
    body_bytes: int = 256 * 1024 * 1024
    response_bytes: int = 256 * 1024 * 1024
    objects: int = 100_000
    expanded_bytes: int = 512 * 1024 * 1024
    disk_bytes: int = 2 * 1024 * 1024 * 1024
    free_bytes: int = 64 * 1024 * 1024
    memory_bytes: int = 1024 * 1024 * 1024
    cpu_seconds: int = 30
    validation_seconds: float = 60
    request_seconds: float = 60
    dispatch_seconds: float = 60

    def __post_init__(self):
        for name, value in self.__dict__.items():
            deadline = name in (
                "validation_seconds",
                "request_seconds",
                "dispatch_seconds",
            )
            if (
                type(value) not in ((int, float) if deadline else (int,))
                or value <= 0
                or not math.isfinite(value)
                or value > 2**63 - 1
            ):
                raise ValueError("finite_positive_limits_required")


DEFAULT_LIMITS = Limits()


def pkt(payload: bytes) -> bytes:
    if len(payload) > 65516:
        raise TransportError("pkt_limit")
    return f"{len(payload) + 4:04x}".encode() + payload


def packet(stream: BinaryIO, *, controls: bool = False) -> bytes | int:
    header = stream.read(4)
    if len(header) != 4 or not re.fullmatch(b"[0-9a-fA-F]{4}", header):
        raise TransportError("pkt_header")
    size = int(header, 16)
    if size == 0 or (controls and size in (1, 2)):
        return size
    if not 4 <= size <= 65520:
        raise TransportError("pkt_length")
    data = stream.read(size - 4)
    if len(data) != size - 4:
        raise TransportError("pkt_truncated")
    return data


def valid_ref(ref: str) -> bool:
    return (
        ref.startswith("refs/")
        and not any(char in ref for char in " ~^:?*[\\")
        and not any(ord(char) < 32 or ord(char) == 127 for char in ref)
        and ".." not in ref
        and "@{" not in ref
        and not ref.endswith(("/", "."))
        and all(
            part and not part.startswith(".") and not part.endswith(".lock")
            for part in ref.split("/")
        )
    )


CAPABILITIES = (
    b"report-status side-band-64k ofs-delta object-format=sha1 agent=mainloop"
)
_AGENT = re.compile(r"agent=[A-Za-z0-9./_+-]{1,128}")


@dataclass(frozen=True)
class Commands:
    update: RefUpdate
    capabilities: frozenset[str]
    pack_offset: int

    @property
    def sideband(self) -> bool:
        return "side-band-64k" in self.capabilities


def receive_commands(stream: BinaryIO, limits: Limits) -> Commands:
    commands = []
    caps: frozenset[str] = frozenset()
    while True:
        item = packet(stream)
        if stream.tell() >= limits.command_bytes:
            raise TransportError("command_limit")
        if item == 0:
            break
        if not isinstance(item, bytes):
            raise TransportError("command_framing")
        if not commands:
            if item.count(b"\0") != 1:
                raise TransportError("capability_placement")
            item, raw_caps = item.split(b"\0")
            try:
                words = (
                    raw_caps.removesuffix(b"\n")
                    .removeprefix(b" ")
                    .decode("ascii")
                    .split(" ")
                )
            except UnicodeError:
                raise TransportError("capability_encoding") from None
            if not all(words) or len(words) != len(set(words)):
                raise TransportError("capability_duplicate")
            caps = frozenset(words)
            if (
                "report-status" not in caps
                or any(
                    word
                    not in {
                        "report-status",
                        "side-band-64k",
                        "ofs-delta",
                        "object-format=sha1",
                    }
                    and not _AGENT.fullmatch(word)
                    for word in caps
                )
                or sum(word.startswith("agent=") for word in caps) > 1
            ):
                raise TransportError("unsupported_capability")
        elif b"\0" in item:
            raise TransportError("capability_placement")
        try:
            old, new, ref = item.removesuffix(b"\n").decode("ascii").split(" ")
            update = RefUpdate(ref=ref, old_oid=old, new_oid=new)
        except (ValueError, UnicodeError):
            raise TransportError("command_invalid") from None
        if not valid_ref(ref):
            raise TransportError("ref_invalid")
        commands.append(update)
    if len(commands) != 1:
        raise TransportError("single_ref_required")
    return Commands(commands[0], caps, stream.tell())


def advertised_refs(data: bytes, service: str) -> dict[str, str]:
    """Decode a complete v0 trusted advertisement, including its service envelope."""
    import io

    stream = io.BytesIO(data)
    if packet(stream) != f"# service={service}\n".encode() or packet(stream) != 0:
        raise TransportError("upstream_advertisement")
    refs = {}
    first = True
    while True:
        line = packet(stream)
        if line == 0:
            break
        if not isinstance(line, bytes):
            raise TransportError("upstream_advertisement")
        if b"\0" in line:
            if not first or line.count(b"\0") != 1:
                raise TransportError("upstream_advertisement")
            line, raw_caps = line.split(b"\0")
            if any(
                cap.startswith(b"object-format=") and cap != b"object-format=sha1"
                for cap in raw_caps.strip().split()
            ):
                raise TransportError("upstream_object_format")
        first = False
        try:
            oid, ref = line.rstrip(b"\n").decode("ascii").split(" ")
        except (ValueError, UnicodeError):
            raise TransportError("upstream_advertisement") from None
        if not re.fullmatch(r"[0-9a-f]{40}", oid) or ref in refs:
            raise TransportError("upstream_advertisement")
        if (
            ref != "capabilities^{}"
            and ref != "HEAD"
            and not valid_ref(ref.removesuffix("^{}"))
        ):
            raise TransportError("upstream_advertisement")
        refs[ref] = oid
    if stream.read():
        raise TransportError("upstream_advertisement")
    return refs


def push_advertisement(branch: str, oid: str = ZERO_OID) -> bytes:
    ref = f"refs/heads/{branch}" if oid != ZERO_OID else "capabilities^{}"
    return (
        pkt(b"# service=git-receive-pack\n")
        + b"0000"
        + pkt(f"{oid} {ref}".encode() + b"\0" + CAPABILITIES + b"\n")
        + b"0000"
    )


def report(ref: str, *, accepted: bool, sideband: bool) -> bytes:
    result = (
        pkt(b"unpack ok\n")
        + pkt((f"ok {ref}\n" if accepted else f"ng {ref} upstream_rejected\n").encode())
        + b"0000"
    )
    return pkt(b"\1" + result) + b"0000" if sideband else result


def receipt(data: bytes, ref: str, *, sideband: bool) -> bool:
    """HTTP 200 is not a receipt. Require unpack and exactly one ref report plus EOF."""
    import io

    if sideband:
        outer = io.BytesIO(data)
        chunks = []
        while True:
            part = packet(outer)
            if part == 0:
                break
            if not isinstance(part, bytes) or not part or part[0] not in (1, 2):
                raise TransportError("upstream_receipt")
            if part[0] == 1:
                chunks.append(part[1:])
        if outer.read():
            raise TransportError("upstream_receipt")
        data = b"".join(chunks)
    stream = io.BytesIO(data)
    unpack, status, end = packet(stream), packet(stream), packet(stream)
    if (
        not isinstance(unpack, bytes)
        or not unpack.startswith(b"unpack ")
        or not unpack.endswith(b"\n")
        or len(unpack) <= len(b"unpack \n")
        or any(byte < 32 or byte == 127 for byte in unpack[:-1])
        or end != 0
        or stream.read()
    ):
        raise TransportError("upstream_receipt")
    if not isinstance(status, bytes):
        raise TransportError("upstream_receipt")
    if status == f"ok {ref}\n".encode():
        if unpack != b"unpack ok\n":
            raise TransportError("upstream_receipt")
        return True
    if (
        status.startswith(f"ng {ref} ".encode())
        and status.endswith(b"\n")
        and len(status) > len(f"ng {ref} \n".encode())
        and not any(byte < 32 or byte == 127 for byte in status[:-1])
    ):
        return False
    raise TransportError("upstream_receipt")
