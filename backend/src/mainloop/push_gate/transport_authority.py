"""PostgreSQL authority for the injectable P1 listeners; no listener is installed here."""

import asyncio
from contextlib import asynccontextmanager
from contextvars import ContextVar
from dataclasses import dataclass
from datetime import UTC, datetime

from mainloop.config import settings
from mainloop.push_gate import credentials, store
from mainloop.push_gate.authorization import authorize
from mainloop.push_gate.protocol import TransportError
from mainloop.push_gate.transport import AttemptEvidence, DispatchProof, TrustedBinding
from mainloop.services.github_pr import RepoMetadata
from mainloop.services.github_repo import parse_github_repo
from mainloop.tasks.lifecycle import LifecycleDenied

from models.push_gate import (
    CredentialStamp,
    ProtectedBranchPolicy,
    PublicationState,
    TransportReceipt,
)


@dataclass
class _Dispatch:
    authority: object
    conn: object
    proof: DispatchProof
    prepared: object
    serial: asyncio.Lock
    active: bool = True
    recorded: AttemptEvidence | None = None


_context: ContextVar[_Dispatch | None] = ContextVar("git_dispatch", default=None)


def _denial(exc):
    # External validation exceptions must never become protocol diagnostics.
    safe = {
        "publication_unresolved",
        "authority_changed",
        "credential_revoked",
        "credential_unpublished",
        "credential_stamp_changed",
        "credential_changed",
        "runtime_changed",
        "runtime_unattested",
        "git_transport_disabled",
        "credential_unavailable",
        "git_key_mismatch",
        "git_key_unavailable",
        "git_scope_unavailable",
        "git_scope_revoked",
        "git_scope_conflict",
        "git_plan_conflict",
        "writer_claim_lost",
        "stale_writer_generation",
        "attempt_not_current",
        "metadata_unavailable",
        "metadata_scope",
        "default_branch",
        "protected_branch",
        "branch_mismatch",
        "non_fast_forward",
    }
    code = str(exc)
    return TransportError(code if code in safe else "authority_unavailable")


class _SeedRead:
    """Request-local fixed upstream read facade; it has no receive port."""

    def __init__(self, authority, binding, upstream):
        if (
            parse_github_repo(upstream.repository).full_name.lower()
            != binding.repository.lower()
        ):
            raise TransportError("binding_scope")
        self._authority, self._binding, self._upstream = authority, binding, upstream

    async def discovery(self, service, *, protocol=None, secrets=()):
        if service != "git-upload-pack":
            raise TransportError("seed_read_only")
        async with self._authority._authorize_dispatch(self._binding, None, seed=True):
            return await self._upstream.discovery(
                service, protocol=protocol, secrets=secrets
            )

    async def upload_pack(self, body, *, protocol=None, secrets=()):
        async with self._authority._authorize_dispatch(self._binding, None, seed=True):
            return await self._upstream.upload_pack(
                body, protocol=protocol, secrets=secrets
            )

    def check_output(self, body, secrets=()):
        return self._upstream.check_output(body, secrets)


class PostgresTransportAuthority:
    requires_seed_factory = True

    def __init__(self, database, trusted_kagent_client, trusted_metadata_reader):
        self.database = database
        self.client = trusted_kagent_client
        self.metadata = trusted_metadata_reader

    def seed_upstream(self, binding, upstream):
        return _SeedRead(self, binding, upstream)

    def _active(self):
        context = _context.get()
        if context is None or context.authority is not self or not context.active:
            raise TransportError("dispatch_context_required")
        credentials.no_transaction(context.conn)
        return context

    async def _resolve(self, conn, stamp, *, refresh=False, seed=False):
        credentials.no_transaction(conn)
        if not settings.git_transport_enabled or (
            stamp.purpose == "git-push" and not settings.push_gate_enabled
        ):
            raise ValueError("git_transport_disabled")
        row, enrollment = await credentials.enrollment_row(conn, stamp.issuance_id)
        plan = enrollment.plan
        if (
            row["revoked_at"] is not None
            or stamp.binding_id != plan.binding_id
            or stamp.issuance_version != plan.issuance_version
        ):
            raise ValueError("credential_revoked")
        if enrollment.association is None:
            raise ValueError("runtime_unattested")
        if stamp.purpose == "git-read":
            actual_hash = row["read_token_hash"]
            if row["read_state"] != "published":
                raise ValueError("credential_unpublished")
        else:
            current = await conn.fetchrow(
                "SELECT * FROM push_grants WHERE id=$1", plan.binding_id
            )
            if (
                current is None
                or current["revoked_at"] is not None
                or row["push_state"] != "published"
            ):
                raise ValueError("credential_revoked")
            actual_hash = current["token_hash"]
        if actual_hash != stamp.capability_hash:
            raise ValueError("credential_stamp_changed")
        await credentials.validate_key(conn, plan)
        await credentials.validate_scope(
            conn, enrollment, creating=stamp.purpose == "git-read"
        )
        policy, grant = None, None
        if stamp.purpose == "git-push":
            grant = await store.live_grant(conn, store.stored_grant(current))
            if (
                grant.git_issuance_id != plan.issuance_id
                or grant.runtime_association != enrollment.association.runtime
            ):
                raise ValueError("credential_stamp_changed")
            # Seed reads retain full live writer identity, but do not need permission
            # to publish or a resolved branch. They can assist later reconciliation.
            if not seed:
                await store.assert_branch_resolved(
                    conn, plan.owner_id, plan.repository, plan.branch
                )
            if refresh:
                metadata = await self.metadata("https://github.com/" + plan.repository)
                if metadata is None:
                    raise ValueError("metadata_unavailable")
                metadata = RepoMetadata.model_validate(metadata)
                if (
                    parse_github_repo(metadata.full_name).full_name.lower()
                    != plan.repository
                    or not metadata.default_branch
                ):
                    raise ValueError("metadata_scope")
                previous = await conn.fetchval(
                    "SELECT policy FROM push_branch_policies WHERE project_id=$1",
                    plan.project_id,
                )
                previous = (
                    store._decode(previous, ProtectedBranchPolicy) if previous else None
                )
                async with conn.transaction():
                    await conn.execute(
                        "UPDATE projects SET default_branch=$2 WHERE id=$1",
                        plan.project_id,
                        metadata.default_branch,
                    )
                    if (
                        previous is None
                        or previous.default_branch != metadata.default_branch
                    ):
                        await store.set_policy(
                            conn,
                            ProtectedBranchPolicy(
                                project_id=plan.project_id,
                                version=previous.version + 1 if previous else 1,
                                default_branch=metadata.default_branch,
                                patterns=previous.patterns if previous else (),
                                previous_defaults=(
                                    previous.previous_defaults if previous else ()
                                ),
                            ),
                        )
            policy = await store.load_policy(conn, plan.project_id)
        # This fresh Get follows metadata I/O, and SQL is reloaded after it.
        await credentials.reobserve(
            conn,
            enrollment,
            creating=stamp.purpose == "git-read",
            trusted_client=self.client,
        )
        row, latest = await credentials.enrollment_row(conn, stamp.issuance_id)
        if row["revoked_at"] is not None or latest != enrollment:
            raise ValueError("credential_changed")
        if grant is not None:
            if (
                await store.live_grant(
                    conn,
                    store.stored_grant(
                        await conn.fetchrow(
                            "SELECT * FROM push_grants WHERE id=$1", grant.id
                        )
                    ),
                )
                != grant
            ):
                raise ValueError("grant_changed")
        return TrustedBinding(
            purpose=stamp.purpose,
            repository=plan.repository,
            association=enrollment.association.runtime,
            grant=grant,
            policy=policy,
            stamp=stamp,
        )

    async def authenticate(self, capability, purpose):
        if purpose not in ("git-read", "git-push"):
            raise TransportError("authentication_denied")
        digest = store.token_hash(capability)
        try:
            async with credentials.connection(database=self.database) as conn:
                if purpose == "git-read":
                    row = await conn.fetchrow(
                        "SELECT issuance_id,binding_id,issuance_version FROM git_enrollments WHERE read_token_hash=$1",
                        digest,
                    )
                else:
                    row = await conn.fetchrow(
                        """SELECT e.issuance_id,e.binding_id,e.issuance_version
                        FROM push_grants g JOIN git_enrollments e ON e.issuance_id=g.grant_data->>'git_issuance_id'
                        WHERE g.token_hash=$1""",
                        digest,
                    )
                if row is None:
                    raise ValueError("credential_unavailable")
                stamp = CredentialStamp(
                    **dict(row), purpose=purpose, capability_hash=digest
                )
                async with credentials.locked(conn, stamp.binding_id):
                    return await self._resolve(conn, stamp)
        except (ValueError, LifecycleDenied) as exc:
            raise _denial(exc) from None

    def authorize_dispatch(self, binding, prepared):
        return self._authorize_dispatch(binding, prepared)

    @asynccontextmanager
    async def _authorize_dispatch(self, binding, prepared, *, seed=False):
        if binding.stamp is None:
            raise TransportError("credential_stamp_required")
        inherited = _context.get()
        if inherited is not None:
            context = self._active()
            if context.proof.binding.stamp != binding.stamp or prepared is not None:
                raise TransportError("nested_dispatch_denied")
            async with context.serial:
                current = await self._resolve(context.conn, binding.stamp, seed=True)
                if current != binding:
                    raise TransportError("authority_changed")
                yield DispatchProof(current)
            return
        try:
            async with credentials.connection(database=self.database) as conn:
                async with credentials.locked(conn, binding.stamp.binding_id):
                    current = await self._resolve(
                        conn, binding.stamp, refresh=True, seed=seed
                    )
                    if (
                        current.association != binding.association
                        or current.grant != binding.grant
                    ):
                        raise ValueError("authority_changed")
                    if prepared is not None:
                        reason = authorize(
                            current.grant,
                            current.policy,
                            current.repository,
                            [prepared.commands.update],
                            lambda *_: prepared.ancestor,
                        )
                        if reason:
                            raise ValueError(reason)
                    proof = DispatchProof(current)
                    context = _Dispatch(self, conn, proof, prepared, asyncio.Lock())
                    token = _context.set(context)
                    try:
                        yield proof
                    finally:
                        context.active = False
                        _context.reset(token)
        except (ValueError, LifecycleDenied) as exc:
            raise _denial(exc) from None

    def _payload(self, context, evidence):
        binding, prepared = context.proof.binding, context.prepared
        if prepared is None or binding.grant is None:
            raise TransportError("publication_context_required")
        expected = evidence.attempt
        if (
            expected.grant_id != binding.grant.id
            or expected.repository.lower() != binding.repository.lower()
            or expected.grant_version != binding.grant.version
            or expected.policy_version != binding.policy.version
            or expected.update != prepared.commands.update
            or expected.state != PublicationState.PENDING
            or evidence.association != binding.association
            or (
                evidence.body_sha256,
                evidence.body_bytes,
                evidence.incoming_objects,
                evidence.expanded_bytes,
                evidence.validated_objects,
                evidence.disk_bytes,
            )
            != (
                prepared.body_sha256,
                prepared.body_bytes,
                prepared.incoming.objects,
                prepared.validated_expanded_bytes,
                prepared.validated_objects,
                prepared.disk_bytes,
            )
        ):
            raise TransportError("publication_evidence_conflict")
        return {
            "attempt": expected.model_dump(mode="json"),
            "stamp": binding.stamp.model_dump(mode="json"),
            "grant": binding.grant.model_dump(mode="json"),
            "association": evidence.association.model_dump(mode="json"),
            "body_sha256": evidence.body_sha256,
            "body_bytes": evidence.body_bytes,
            "incoming_objects": evidence.incoming_objects,
            "expanded_bytes": evidence.expanded_bytes,
            "validated_objects": evidence.validated_objects,
            "disk_bytes": evidence.disk_bytes,
        }

    async def record(self, evidence):
        context = self._active()
        async with context.serial:
            payload = self._payload(context, evidence)
            if context.recorded is not None and context.recorded != evidence:
                raise TransportError("publication_evidence_conflict")
            await store.record_transport_attempt(
                context.conn, payload, context.proof.binding.stamp
            )
            context.recorded = evidence

    async def transition(self, evidence, state):
        context = self._active()
        async with context.serial:
            if context.recorded != evidence:
                raise TransportError("publication_evidence_conflict")
            payload = self._payload(context, evidence)
            if state == PublicationState.DISPATCHING:
                current = await self._resolve(
                    context.conn, context.proof.binding.stamp, refresh=True
                )
                if current != context.proof.binding:
                    raise TransportError("authority_changed")
                context.prepared.verify_identity()
            receipt = (
                None
                if state == PublicationState.DISPATCHING
                else TransportReceipt(
                    classification=state,
                    ref=evidence.attempt.update.ref,
                    validated=state
                    in (PublicationState.CONFIRMED, PublicationState.REJECTED),
                    failure_code=(
                        "publication_unknown"
                        if state == PublicationState.UNKNOWN
                        else (
                            "upstream_rejected"
                            if state == PublicationState.REJECTED
                            else None
                        )
                    ),
                    recorded_at=datetime.now(UTC),
                )
            )
            await store.transition_transport_attempt(
                context.conn, payload, state, receipt
            )
