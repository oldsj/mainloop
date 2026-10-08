"""Private disk spooling and credential-free, resource-limited Git validation."""

import asyncio
import hashlib
import io
import os
import shutil
import signal
import tempfile
from contextlib import asynccontextmanager
from dataclasses import dataclass
from pathlib import Path

from mainloop.push_gate.authorization import ZERO_OID
from mainloop.push_gate.pack import PackFacts, walk_pack
from mainloop.push_gate.protocol import (
    Commands,
    Limits,
    TransportError,
    packet,
    pkt,
    receive_commands,
)
from mainloop.push_gate.upstream import FixedGitUpstream


@dataclass(frozen=True)
class PreparedPush:
    body: Path
    body_sha256: str
    body_bytes: int
    commands: Commands
    incoming: PackFacts
    seeded: PackFacts | None
    ancestor: bool
    remote_oid: str
    validated_objects: int
    validated_expanded_bytes: int
    disk_bytes: int

    def verify_identity(self):
        digest = hashlib.sha256()
        with self.body.open("rb") as source:
            while chunk := source.read(64 * 1024):
                digest.update(chunk)
        if (
            self.body.stat().st_size != self.body_bytes
            or digest.hexdigest() != self.body_sha256
        ):
            raise TransportError("body_identity_changed")


def disk_root(root: Path, limits: Limits):
    root = root.resolve()
    # Spool must be persistent disk, including when a non-/tmp path mounts tmpfs.
    mounts = []
    for line in Path("/proc/self/mountinfo").read_text().splitlines():
        left, right = line.split(" - ", 1)
        mount = Path(left.split()[4].replace("\\040", " "))
        if root == mount or mount in root.parents:
            mounts.append((len(mount.parts), right.split()[0]))
    if not mounts or max(mounts)[1] in ("tmpfs", "ramfs"):
        raise TransportError("disk_spool_required")
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    if root.stat().st_uid != os.getuid() or root.stat().st_mode & 0o077:
        raise TransportError("private_spool_required")
    if shutil.disk_usage(root).free < limits.disk_bytes + limits.free_bytes:
        raise TransportError("disk_unavailable")


def disk_usage(root: Path, limits: Limits) -> int:
    size = sum(path.stat().st_size for path in root.rglob("*") if path.is_file())
    if size > limits.disk_bytes:
        raise TransportError("disk_limit")
    if shutil.disk_usage(root).free < limits.free_bytes:
        raise TransportError("disk_unavailable")
    return size


async def git(
    root: Path,
    limits: Limits,
    *args: str,
    stdin: Path | None = None,
    allowed: tuple[int, ...] = (0,),
    output_bytes: int | None = None,
) -> bytes:
    """No shell, inherited environment, configuration, hooks, replacements or credentials."""
    available = limits.disk_bytes - disk_usage(root, limits)
    if available < 4:
        raise TransportError("disk_limit")
    env = {
        "PATH": "/usr/bin:/bin",
        "LANG": "C",
        "HOME": str(root),
        "XDG_CONFIG_HOME": str(root),
        "GIT_CONFIG_NOSYSTEM": "1",
        "GIT_CONFIG_GLOBAL": "/dev/null",
        "GIT_CONFIG_SYSTEM": "/dev/null",
        "GIT_NO_REPLACE_OBJECTS": "1",
        "GIT_TERMINAL_PROMPT": "0",
    }
    command = [
        "/usr/bin/prlimit",
        f"--as={limits.memory_bytes}",
        f"--cpu={limits.cpu_seconds}",
        f"--fsize={available // 4}",
        "--nofile=64",
        "--",
        "/usr/bin/git",
        "-c",
        "core.hooksPath=/dev/null",
        "-c",
        "protocol.allow=never",
        "-c",
        "gc.auto=0",
        f"--git-dir={root / 'repo.git'}",
        *args,
    ]
    with tempfile.TemporaryFile(dir=root) as out, tempfile.TemporaryFile(
        dir=root
    ) as err:
        source = stdin.open("rb") if stdin else None
        try:
            process = await asyncio.create_subprocess_exec(
                *command,
                stdin=source if source else asyncio.subprocess.DEVNULL,
                stdout=out,
                stderr=err,
                env=env,
                start_new_session=True,
            )
            try:
                await asyncio.wait_for(process.wait(), limits.validation_seconds)
            except BaseException:
                if process.returncode is None:
                    os.killpg(process.pid, signal.SIGKILL)
                await process.wait()
                raise
            if process.returncode not in allowed:
                raise TransportError("git_validation")
            if (
                out.tell() > (output_bytes or limits.command_bytes)
                or err.tell() > limits.command_bytes
            ):
                raise TransportError("git_output_limit")
            disk_usage(root, limits)
            out.seek(0)
            return out.read() if process.returncode == 0 else b"not_ancestor"
        finally:
            if source:
                source.close()


@asynccontextmanager
async def receive_spool(stream, root: Path, limits: Limits):
    disk_root(root, limits)
    with tempfile.TemporaryDirectory(prefix="receive-", dir=root) as directory:
        work = Path(directory)
        body = work / "request"
        digest = hashlib.sha256()
        size = 0
        with body.open("xb") as target:
            async with asyncio.timeout(limits.request_seconds):
                async for chunk in stream:
                    size += len(chunk)
                    if size >= limits.body_bytes or size > limits.disk_bytes:
                        raise TransportError("body_limit")
                    digest.update(chunk)
                    target.write(chunk)
        body.chmod(0o400)
        yield work, body, digest.hexdigest(), size


async def prepare_receive(
    work: Path,
    body: Path,
    digest: str,
    size: int,
    refs: dict[str, str],
    default_branch: str,
    upstream: FixedGitUpstream,
    limits: Limits,
    *,
    secrets: tuple[bytes, ...] = (),
) -> PreparedPush:
    with body.open("rb") as source:
        commands = receive_commands(source, limits)
        incoming = walk_pack(source, limits)
    update = commands.update
    remote = refs.get(update.ref, ZERO_OID)
    if remote != update.old_oid:
        raise TransportError("remote_ref_changed")
    await git(
        work, limits, "init", "--bare", "--initial-branch=quarantine", "--template="
    )
    wants = sorted(
        {
            oid
            for ref, oid in refs.items()
            if ref in (update.ref, f"refs/heads/{default_branch}") and oid != ZERO_OID
        }
    )
    if f"refs/heads/{default_branch}" not in refs:
        raise TransportError("default_objects_unavailable")
    seeded = None
    if wants:
        request = (
            b"".join(pkt(f"want {oid}\n".encode()) for oid in wants)
            + b"0000"
            + pkt(b"done\n")
        )
        response = await upstream.upload_pack(request, secrets=secrets)
        source = io.BytesIO(response)
        if packet(source) != b"NAK\n":
            raise TransportError("seed_framing")
        seed_start = source.tell()
        seeded = walk_pack(source, limits)
        if (
            seeded.objects + incoming.objects > limits.objects
            or seeded.expanded_bytes + incoming.expanded_bytes > limits.expanded_bytes
        ):
            raise TransportError("combined_pack_limit")
        seed = work / "seed.pack"
        seed.write_bytes(response[seed_start:])
        disk_usage(work, limits)
        await git(work, limits, "index-pack", "--stdin", "--strict", stdin=seed)
    pack = work / "incoming.pack"
    with body.open("rb") as source, pack.open("xb") as target:
        source.seek(commands.pack_offset)
        shutil.copyfileobj(source, target, 64 * 1024)
    disk_usage(work, limits)
    await git(
        work, limits, "index-pack", "--stdin", "--fix-thin", "--strict", stdin=pack
    )
    if await git(work, limits, "cat-file", "-t", update.new_oid) != b"commit\n":
        raise TransportError("commit_required")
    await git(
        work,
        limits,
        "fsck",
        "--strict",
        "--no-reflogs",
        "--no-dangling",
        update.new_oid,
    )
    ancestor = (
        update.old_oid == ZERO_OID
        or await git(
            work,
            limits,
            "merge-base",
            "--is-ancestor",
            update.old_oid,
            update.new_oid,
            allowed=(0, 1),
        )
        == b""
    )
    if not ancestor:
        raise TransportError("non_fast_forward")
    # Measure actual reconstructed object count/size, including thin bases. This is
    # independent of PACK header sizes and bounds Git's delta reconstruction result.
    inventory = await git(
        work,
        limits,
        "cat-file",
        "--batch-all-objects",
        "--batch-check=%(objectname) %(objecttype) %(objectsize)",
        output_bytes=limits.objects * 80,
    )
    count = expanded = 0
    for line in inventory.splitlines():
        fields = line.split()
        if len(fields) != 3 or not fields[2].isdigit():
            raise TransportError("git_inventory")
        count += 1
        expanded += int(fields[2])
        if count > limits.objects or expanded > limits.expanded_bytes:
            raise TransportError("validated_object_limit")
    measured_disk = disk_usage(work, limits)
    return PreparedPush(
        body,
        digest,
        size,
        commands,
        incoming,
        seeded,
        ancestor,
        remote,
        count,
        expanded,
        measured_disk,
    )
