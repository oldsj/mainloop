"""Owned loopback Git client/server evidence, with dummy capabilities and no provider."""

import asyncio
import os
import signal
import socket
import tempfile
import unittest
from contextlib import asynccontextmanager
from pathlib import Path

import uvicorn
from mainloop.push_gate.authorization import authorize
from mainloop.push_gate.protocol import Limits, TransportError, pkt
from mainloop.push_gate.transport import (
    DispatchProof,
    RuntimeAssociation,
    TrustedBinding,
    create_git_applications,
)
from mainloop.push_gate.upstream import FixedGitUpstream, LoopbackFixture

from models.push_gate import ProtectedBranchPolicy, PublicationState, PushGrant

READ = "fixture_read_capability_123456789"
PUSH = "fixture_push_capability_123456789"
PAT = "fixture_owner_pat_123456789"
ASSOCIATION = RuntimeAssociation(
    session_id="native",
    generation_id="generation",
    atespace="space",
    actor_name="actor",
    actor_uid="uid",
    revision="1",
)
GRANT = PushGrant(
    id="grant",
    owner_id="owner",
    project_id="project",
    repository="Owner/Repo",
    branch="feature",
    workspace_id="session",
    session_id="session",
    runtime_identity="native",
)
POLICY = ProtectedBranchPolicy(
    project_id="project",
    version=1,
    default_branch="main",
    previous_defaults=("old-default",),
    patterns=("release/*",),
)


def fixture_root():
    root = Path.home() / ".cache" / "mainloop-p1-fixtures"
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    return root


async def command(*args, cwd=None, data=None, env=None, success=True):
    clean = {
        "PATH": "/usr/bin:/bin",
        "HOME": str(fixture_root()),
        "LANG": "C",
        "GIT_CONFIG_NOSYSTEM": "1",
        "GIT_CONFIG_GLOBAL": "/dev/null",
        "GIT_CONFIG_SYSTEM": "/dev/null",
        "GIT_TERMINAL_PROMPT": "0",
        "GIT_AUTHOR_NAME": "Fixture",
        "GIT_AUTHOR_EMAIL": "fixture@example.invalid",
        "GIT_COMMITTER_NAME": "Fixture",
        "GIT_COMMITTER_EMAIL": "fixture@example.invalid",
    }
    clean.update(env or {})
    process = await asyncio.create_subprocess_exec(
        *args,
        cwd=cwd,
        env=clean,
        stdin=(
            asyncio.subprocess.PIPE if data is not None else asyncio.subprocess.DEVNULL
        ),
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.PIPE,
        start_new_session=True,
    )
    try:
        out, err = await asyncio.wait_for(process.communicate(data), 20)
    except BaseException:
        if process.returncode is None:
            os.killpg(process.pid, signal.SIGKILL)
        await process.wait()
        raise
    if success and process.returncode:
        raise AssertionError(
            f"fixture command failed ({process.returncode}): {args!r}\n{err.decode(errors='replace')}"
        )
    return process.returncode, out, err


class FixtureAuthority:
    def __init__(self):
        self.grant, self.policy, self.association = GRANT, POLICY, ASSOCIATION
        self.lock = asyncio.Lock()
        self.records = []
        self.states = {}
        self.history = []
        self.fail_state = None
        self.revoked = False
        self.final_change = None
        self.before_dispatch = None

    def binding(self, purpose):
        return TrustedBinding(
            purpose=purpose,
            repository="Owner/Repo",
            association=self.association,
            grant=self.grant if purpose == "git-push" else None,
            policy=self.policy if purpose == "git-push" else None,
        )

    async def authenticate(self, capability, purpose):
        if self.revoked or capability != (READ if purpose == "git-read" else PUSH):
            raise TransportError("fixture_auth_denied")
        return self.binding(purpose)

    @asynccontextmanager
    async def authorize_dispatch(self, binding, prepared):
        async with self.lock:
            if self.before_dispatch and prepared:
                await self.before_dispatch(prepared)
            if self.final_change and prepared:
                self.final_change(self)
            if self.revoked:
                raise TransportError("fixture_auth_denied")
            if any(
                state in (PublicationState.DISPATCHING, PublicationState.UNKNOWN)
                for state in self.states.values()
            ):
                raise TransportError("publication_unresolved")
            current = self.binding(binding.purpose)
            if prepared:
                reason = authorize(
                    current.grant,
                    current.policy,
                    current.repository,
                    [prepared.commands.update],
                    lambda old, new: prepared.ancestor,
                )
                if reason:
                    raise TransportError(reason)
            yield DispatchProof(current)

    async def record(self, evidence):
        if not self.lock.locked():
            raise AssertionError("fixture authority lock is absent")
        self.records.append(evidence)
        self.states[evidence.attempt.request_id] = PublicationState.PENDING
        self.history.append(PublicationState.PENDING)

    async def transition(self, evidence, state):
        if not self.lock.locked():
            raise AssertionError("fixture authority lock is absent")
        if state == self.fail_state:
            raise RuntimeError("fixture durability loss")
        from mainloop.push_gate.store import TRANSITIONS

        previous = self.states[evidence.attempt.request_id]
        if state not in TRANSITIONS.get(previous, set()):
            raise AssertionError("fixture transition is invalid")
        self.states[evidence.attempt.request_id] = state
        self.history.append(state)


class GitBackedUpstream:
    def __init__(self, repo, authority):
        self.repo, self.authority = repo, authority
        self.receives = []
        self.requests = []
        self.mode = "normal"
        self.dispatched = asyncio.Event()
        self.release = asyncio.Event()
        self.replace_discovery = None

    async def __call__(self, scope, receive, send):
        if scope["type"] != "http":
            return
        headers = dict(scope["headers"])
        self.requests.append(
            (scope["method"], scope["path"], scope["query_string"], headers)
        )
        import base64

        if headers[b"authorization"] != b"Basic " + base64.b64encode(
            f"x-access-token:{PAT}".encode()
        ):
            raise AssertionError("fixture upstream credential mismatch")
        if b"cookie" in headers or b"x-forwarded-host" in headers:
            raise AssertionError("actor headers reached upstream")
        body = bytearray()
        while True:
            msg = await receive()
            body.extend(msg.get("body", b""))
            if (
                not msg.get("more_body")
                or self.mode == "partial"
                and scope["path"].endswith("git-receive-pack")
            ):
                break
        service = (
            "git-receive-pack"
            if b"git-receive-pack" in scope["query_string"]
            or scope["path"].endswith("git-receive-pack")
            else "git-upload-pack"
        )
        discovery = scope["method"] == "GET"
        if discovery:
            _, out, _ = await command(
                "/usr/bin/git",
                service[4:],
                "--stateless-rpc",
                "--advertise-refs",
                str(self.repo),
                env={"GIT_PROTOCOL": headers.get(b"git-protocol", b"").decode()},
            )
            out = pkt(f"# service={service}\n".encode()) + b"0000" + out
            if self.replace_discovery:
                out = self.replace_discovery(out)
        else:
            if service == "git-receive-pack":
                if (
                    not self.authority.lock.locked()
                    or self.authority.history[-1] != PublicationState.DISPATCHING
                ):
                    raise AssertionError(
                        "outbound receive before durable dispatch under lock"
                    )
                self.receives.append(bytes(body))
                self.dispatched.set()
                if self.mode == "partial":
                    await send(
                        {"type": "http.response.start", "status": 500, "headers": []}
                    )
                    await send({"type": "http.response.body", "body": b""})
                    return
                if self.mode == "wait":
                    await self.release.wait()
            if service == "git-receive-pack" and self.mode in ("ng", "before"):
                from mainloop.push_gate.protocol import report

                out = (
                    report("refs/heads/feature", accepted=False, sideband=True)
                    if self.mode == "ng"
                    else b"0008unpa"
                )
            else:
                _, out, _ = await command(
                    "/usr/bin/git",
                    service[4:],
                    "--stateless-rpc",
                    str(self.repo),
                    data=bytes(body),
                    env={"GIT_PROTOCOL": headers.get(b"git-protocol", b"").decode()},
                )
            if service == "git-receive-pack":
                if self.mode == "loss":
                    out = out[:8]
                elif self.mode == "extra":
                    out += b"0000"
                elif self.mode == "reflect":
                    out += PAT.encode()
        await send(
            {
                "type": "http.response.start",
                "status": 200,
                "headers": [
                    (
                        b"content-type",
                        f"application/x-{service}-{'advertisement' if discovery else 'result'}".encode(),
                    )
                ],
            }
        )
        await send({"type": "http.response.body", "body": out})


@asynccontextmanager
async def server(app):
    sock = socket.socket()
    sock.bind(("127.0.0.1", 0))
    sock.listen(64)
    port = sock.getsockname()[1]
    instance = uvicorn.Server(
        uvicorn.Config(
            app,
            host="127.0.0.1",
            port=port,
            log_level="critical",
            access_log=False,
            lifespan="off",
        )
    )
    task = asyncio.create_task(instance.serve(sockets=[sock]))
    try:
        async with asyncio.timeout(10):
            while not instance.started:
                if task.done():
                    await task
                    raise AssertionError("fixture server stopped")
                await asyncio.sleep(0.01)
        yield f"http://127.0.0.1:{port}", f"127.0.0.1:{port}"
    finally:
        instance.should_exit = True
        await asyncio.wait_for(task, 10)
        sock.close()


class GitFixtureCase(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.directory = tempfile.TemporaryDirectory(dir=fixture_root(), prefix="git-")
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.repo, self.client = self.root / "upstream.git", self.root / "client"
        self.spool = self.root / "spool"
        self.spool.mkdir(mode=0o700)
        self.limits = Limits(
            body_bytes=8 * 1024 * 1024,
            response_bytes=8 * 1024 * 1024,
            disk_bytes=64 * 1024 * 1024,
            expanded_bytes=16 * 1024 * 1024,
            free_bytes=1024 * 1024,
        )
        await command(
            "/usr/bin/git", "init", "--bare", "--initial-branch=main", str(self.repo)
        )
        await command("/usr/bin/git", "init", "--initial-branch=main", str(self.client))
        (self.client / "file").write_text("base\n" * 1000)
        await command("/usr/bin/git", "add", ".", cwd=self.client)
        await command("/usr/bin/git", "commit", "-m", "base", cwd=self.client)
        await command("/usr/bin/git", "push", str(self.repo), "main", cwd=self.client)
        self.base = (
            (await command("/usr/bin/git", "rev-parse", "HEAD", cwd=self.client))[1]
            .strip()
            .decode()
        )
        await command("/usr/bin/git", "checkout", "-b", "feature", cwd=self.client)
        self.authority = FixtureAuthority()
        self.fake = GitBackedUpstream(self.repo, self.authority)
        self.gate_responses = []
        self.client_bodies = []

    async def commit(self, message):
        with (self.client / "file").open("a") as target:
            target.write(message + "\n")
        await command("/usr/bin/git", "add", ".", cwd=self.client)
        await command("/usr/bin/git", "commit", "-m", message, cwd=self.client)
        return (
            (await command("/usr/bin/git", "rev-parse", "HEAD", cwd=self.client))[1]
            .strip()
            .decode()
        )

    @asynccontextmanager
    async def transport(self):
        async with server(self.fake) as (up_origin, _):
            upstream = FixedGitUpstream(
                "Owner/Repo", PAT, self.limits, fixture=LoopbackFixture(up_origin)
            )
            read, push = create_git_applications(
                authority=self.authority,
                upstream=upstream,
                spool_root=self.spool,
                limits=self.limits,
            )

            async def observed(scope, receive, send):
                async def capture(message):
                    if message["type"] == "http.response.body":
                        self.gate_responses.append(message.get("body", b""))
                    await send(message)

                async def capture_receive():
                    message = await receive()
                    if scope["method"] == "POST":
                        self.client_bodies.append(message.get("body", b""))
                    return message

                await push(scope, capture_receive, capture)

            async with server(read) as (read_origin, read_host), server(observed) as (
                push_origin,
                push_host,
            ):
                read.hosts, push.hosts = (read_host,), (push_host,)
                yield read_origin + "/Owner/Repo.git", push_origin + "/Owner/Repo.git", push
        self.assertEqual(list(self.spool.iterdir()), [])

    async def push(self, url, *refs, success=True):
        result = await command(
            "/usr/bin/git",
            "-c",
            f"http.extraHeader=Authorization: Bearer {PUSH}",
            "push",
            url,
            *(refs or ("feature",)),
            cwd=self.client,
            success=False,
        )
        if success and result[0]:
            raise AssertionError(
                f"Git push failed: {result[2]!r}; gate={self.gate_responses[-1:]!r}"
            )
        return result

    async def raw_body(self, old, new, ref="refs/heads/feature", *, thin=False):
        data = (
            new.encode()
            + b"\n"
            + (b"^" + old.encode() + b"\n" if old != "0" * 40 else b"")
        )
        args = ["/usr/bin/git", "pack-objects", "--stdout", "--revs"]
        if thin:
            args += ["--thin"]
        _, pack, _ = await command(*args, cwd=self.client, data=data)
        return (
            pkt(
                f"{old} {new} {ref}".encode()
                + b"\0report-status side-band-64k ofs-delta\n"
            )
            + b"0000"
            + pack
        )


class RealGitTransportTests(GitFixtureCase):
    async def test_clone_fetch_versions_shallow_exact_sha_and_create_fast_forward(self):
        async with self.transport() as (read_url, push_url, _):
            first = await self.commit("one")
            await self.push(push_url)
            self.assertEqual(len(self.fake.receives), 1)
            second = await self.commit("two")
            await self.push(push_url, "+feature:feature")
            self.assertEqual(len(self.fake.receives), 2)
            self.assertEqual(
                self.authority.history,
                [
                    PublicationState.PENDING,
                    PublicationState.DISPATCHING,
                    PublicationState.CONFIRMED,
                ]
                * 2,
            )
            for version in (0, 1, 2):
                clone = self.root / f"clone-{version}"
                await command(
                    "/usr/bin/git",
                    "-c",
                    f"http.extraHeader=Authorization: Bearer {READ}",
                    "-c",
                    f"protocol.version={version}",
                    "clone",
                    "--depth=1",
                    read_url,
                    str(clone),
                )
                await command(
                    "/usr/bin/git",
                    "-c",
                    f"http.extraHeader=Authorization: Bearer {READ}",
                    "-c",
                    f"protocol.version={version}",
                    "fetch",
                    "--unshallow",
                    "origin",
                    cwd=clone,
                )
                await command(
                    "/usr/bin/git",
                    "-c",
                    f"http.extraHeader=Authorization: Bearer {READ}",
                    "fetch",
                    "origin",
                    second,
                    cwd=clone,
                )
                self.assertEqual(
                    (await command("/usr/bin/git", "cat-file", "-t", first, cwd=clone))[
                        1
                    ],
                    b"commit\n",
                )
            for evidence, original in zip(
                self.authority.records, self.fake.receives, strict=True
            ):
                import hashlib

                self.assertEqual(
                    evidence.body_sha256, hashlib.sha256(original).hexdigest()
                )
                self.assertEqual(evidence.body_bytes, len(original))

    async def test_git_forbidden_refs_mixed_deletion_rewind_and_divergence_zero_writes(
        self,
    ):
        async with self.transport() as (_, push_url, _):
            first = await self.commit("one")
            await self.push(push_url)
            second = await self.commit("two")
            await self.push(push_url)
            count = len(self.fake.receives)
            await command("/usr/bin/git", "tag", "v1", cwd=self.client)
            cases = [
                ("+HEAD:main",),
                ("HEAD:release/1",),
                ("HEAD:old-default",),
                ("HEAD:other",),
                ("refs/tags/v1",),
                (":feature",),
                ("+" + first + ":feature",),
                ("feature", "HEAD:other"),
            ]
            for refs in cases:
                code, _, _ = await self.push(push_url, *refs, success=False)
                self.assertNotEqual(code, 0, refs)
                self.assertEqual(len(self.fake.receives), count, refs)
            await command("/usr/bin/git", "reset", "--hard", self.base, cwd=self.client)
            await self.commit("divergent")
            code, _, _ = await self.push(push_url, "+feature:feature", success=False)
            self.assertNotEqual(code, 0)
            self.assertEqual(len(self.fake.receives), count)
            self.assertEqual(
                (
                    await command(
                        "/usr/bin/git",
                        "--git-dir=" + str(self.repo),
                        "rev-parse",
                        "feature",
                    )
                )[1]
                .strip()
                .decode(),
                second,
            )

    async def test_read_binding_on_default_is_independent_of_push_grant(self):
        self.authority.grant = GRANT.model_copy(update={"branch": "main"})
        async with self.transport() as (read_url, push_url, _):
            await command(
                "/usr/bin/git",
                "-c",
                f"http.extraHeader=Authorization: Bearer {READ}",
                "clone",
                read_url,
                str(self.root / "readonly"),
            )
            code, _, _ = await self.push(push_url, "HEAD:main", success=False)
            self.assertNotEqual(code, 0)
            self.assertEqual(self.fake.receives, [])
