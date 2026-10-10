"""Trusted, reproducible Git enrollment. PostgreSQL stores references and hashes only."""

import asyncio
import base64
import hashlib
import hmac
import json
import logging
import uuid
from contextlib import asynccontextmanager
from dataclasses import asdict

from kubernetes import client, config
from kubernetes.client.exceptions import ApiException
from mainloop.config import settings
from mainloop.push_gate import store
from mainloop.push_gate.authorization import WORKSPACE_WRITER_PAIRS, protected_reason
from mainloop.runtime import native_sessions as ns
from mainloop.runtime.agent_credentials import _binding_lock, reference_from_data
from mainloop.runtime.kagent_client import (
    AGENT_SETUP_DIGEST,
    CHILD_SETUP_DIGEST,
    SUPERVISOR_SETUP_DIGEST,
    DevelopmentEnvironment,
    OutcomeUnknown,
    PreparationRequest,
    RuntimeComposition,
    RuntimeOperation,
    RuntimeState,
    SessionCredential,
    SessionError,
    SessionWorkspace,
    Unreachable,
)
from mainloop.services import github_checkout
from mainloop.services.github_repo import parse_github_repo
from mainloop.tasks import lifecycle

from models.push_gate import (
    GitCreatePlan,
    GitEnrollment,
    GitObservation,
    GitRuntimeAssociation,
    ProtectedBranchPolicy,
    PushGrant,
)

logger = logging.getLogger(__name__)
AUTHORIZATION_FIELD = "authorization"
# kagent's preparation verifier compares the stored header byte for byte, lowercase.
GIT_HEADER = "authorization"


def no_transaction(conn):
    if conn.is_in_transaction():
        raise RuntimeError("Git external operation requires a committed connection")


@asynccontextmanager
async def connection(conn=None, *, database=None):
    if conn is not None:
        yield conn
    else:
        if database is None:
            from mainloop.db import db

            database = db
        async with database.connection() as owned:
            try:
                yield owned
            finally:
                if not owned.is_closed():
                    try:
                        held = await owned.fetchval(
                            "SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=pg_backend_pid() AND locktype='advisory' AND granted)"
                        )
                    except BaseException:
                        await owned.close()
                        raise
                    if held:
                        # A failed unlock must discard the session, never lend its locks.
                        await owned.close()


@asynccontextmanager
async def locked(conn, binding_id):
    from mainloop.push_gate import lifecycle as push_lifecycle

    no_transaction(conn)
    async with push_lifecycle.locked(conn, binding_id), lifecycle.locked(
        conn, binding_id
    ), _binding_lock(conn, binding_id):
        yield


def capability_for(plan: GitCreatePlan, purpose: str) -> str:
    if purpose not in ("git-read", "git-push") or not settings.agent_token_key.strip():
        raise ValueError("git_key_unavailable")
    message = json.dumps(
        [
            "mainloop/git-capability/v1",
            purpose,
            str(plan.issuance_id),
            plan.issuance_version,
            plan.binding_id,
            str(plan.create_request_id),
        ],
        separators=(",", ":"),
        ensure_ascii=False,
    ).encode()
    digest = hmac.new(
        settings.agent_token_key.encode(), message, hashlib.sha256
    ).hexdigest()
    return ("gread_" if purpose == "git-read" else "push_") + digest


def plan_digest(plan):
    return hashlib.sha256(
        json.dumps(
            plan.model_dump(mode="json"), sort_keys=True, separators=(",", ":")
        ).encode()
    ).hexdigest()


def _git_reference(purpose, issuance_id):
    return SessionCredential(
        origin=(
            settings.git_read_origin
            if purpose == "git-read"
            else settings.git_push_origin
        ),
        header=GIT_HEADER,
        secret_name=f"mainloop-{purpose}-{issuance_id}",
        secret_key=AUTHORIZATION_FIELD,
    )


def reference(plan, purpose):
    """Return the plan's frozen reference; older plans keep their header spelling."""
    built = _git_reference(purpose, plan.issuance_id)
    for frozen in plan.references:
        if frozen.secret_name == built.secret_name:
            return SessionCredential(**frozen.model_dump())
    return built


def git_reference_counts(references):
    """Mainloop's mirror of kagent's preparation check of the original Git references."""
    counts = {"read": 0, "push": 0}
    for ref in references:
        data = ref if isinstance(ref, dict) else ref.model_dump()
        if data["header"] != GIT_HEADER or data["secret_key"] != AUTHORIZATION_FIELD:
            continue
        if data["origin"] == settings.git_read_origin:
            counts["read"] += 1
        elif data["origin"] == settings.git_push_origin:
            counts["push"] += 1
    return counts


async def _note_attempt(conn, attempt_id, evidence):
    """Append one bounded, secret-free reason to the task attempt (caller's transaction)."""
    current = await lifecycle.load_attempt(conn, attempt_id, lock=True)
    if (
        current is None
        or evidence in current.evidence_refs
        or len(current.evidence_refs) >= 64
    ):
        return
    await lifecycle.save_attempt(
        conn,
        current.model_copy(
            update={"evidence_refs": (*current.evidence_refs, evidence)}
        ),
    )


async def note_binding(conn, binding, evidence):
    attempt = await ns.attempt_row(binding, conn=conn)
    if attempt is not None:
        async with conn.transaction():
            await _note_attempt(conn, attempt["id"], evidence)


async def _hold_create(conn, binding, reason):
    """Hold the writer before any enrollment exists; the next pass retries."""
    await note_binding(conn, binding, "git-hold:" + reason)
    raise ValueError("git_" + reason)


async def learn_default_branch(conn, binding):
    """Persist GitHub's default branch before a writer plan depends on it."""
    no_transaction(conn)
    project = await conn.fetchrow(
        """SELECT p.id,p.full_name,p.default_branch FROM sessions s
        JOIN projects p ON p.id=s.project_id WHERE s.id=$1""",
        binding["session_id"],
    )
    if project is None or project["default_branch"]:
        return
    try:
        branch = await github_checkout.resolve_default_branch(project["full_name"])
    except github_checkout.DefaultBranchUnavailable:
        await _hold_create(conn, binding, "default_branch_unavailable")
    from mainloop.db.postgres import record_default_branch

    await record_default_branch(conn, project["id"], branch)


async def enrollment_row(conn, issuance_id):
    row = await conn.fetchrow(
        "SELECT * FROM git_enrollments WHERE issuance_id=$1", str(issuance_id)
    )
    if not row:
        raise ValueError("git_enrollment_unavailable")
    plan = store._decode(row["plan"], GitCreatePlan)
    if row["plan_digest"] != plan_digest(plan) or (
        row["binding_id"],
        row["create_request_id"],
        row["issuance_version"],
        row["owner_id"],
        row["project_id"],
        row["repository"],
        row["branch"],
    ) != (
        plan.binding_id,
        str(plan.create_request_id),
        plan.issuance_version,
        plan.owner_id,
        plan.project_id,
        plan.repository,
        plan.branch,
    ):
        raise ValueError("git_plan_conflict")
    return row, GitEnrollment(
        plan=plan,
        association=(
            store._decode(row["association"], GitObservation)
            if row["association"]
            else None
        ),
        read_state=row["read_state"],
        push_state=row["push_state"],
    )


async def validate_key(conn, plan):
    row = await conn.fetchrow(
        "SELECT read_token_hash FROM git_enrollments WHERE issuance_id=$1",
        str(plan.issuance_id),
    )
    if not row or not hmac.compare_digest(
        row["read_token_hash"], store.token_hash(capability_for(plan, "git-read"))
    ):
        raise ValueError("git_key_mismatch")


async def read_scope(conn, binding_id, *, creating=False):
    """Default/protected reads share ownership/ancestry checks, never push policy."""
    row = await conn.fetchrow(
        """SELECT b.*,s.user_id,s.project_id,s.status AS session_status,s.archived_at,
        s.repo_url,s.branch_name,p.user_id AS project_owner,p.full_name,p.html_url,
        p.default_branch,w.repo,w.ref,w.branch,w.depth,w.development_environment
        FROM native_bindings b JOIN sessions s ON s.id=b.session_id
        JOIN projects p ON p.id=s.project_id JOIN workspaces w ON w.session_id=s.id
        WHERE b.session_id=$1""",
        binding_id,
    )
    if not row or (row["role"], row["mcp_grant_kind"]) not in WORKSPACE_WRITER_PAIRS:
        raise ValueError("git_scope_unavailable")
    if (
        not row["token_hash"]
        or row["kagent_deleted_at"]
        or row["archived_at"]
        or row["session_status"] in ("completed", "failed", "cancelled")
    ):
        raise ValueError("git_scope_revoked")
    repository = parse_github_repo(row["full_name"]).full_name.lower()
    from mainloop.runtime.agent_identity import hash_token, token_for

    if row["token_hash"] != hash_token(token_for(binding_id)):
        raise ValueError("mcp_grant_revoked")
    if (
        row["project_owner"] != row["user_id"]
        or row["branch_name"] != row["branch"]
        or any(
            parse_github_repo(row[field]).full_name.lower() != repository
            for field in ("repo_url", "repo", "html_url")
        )
    ):
        raise ValueError("git_scope_conflict")
    if row["role"] != "agent":
        await lifecycle.check(conn, binding_id, "create" if creating else "submit")
        await lifecycle.authenticate_binding(conn, dict(row), allow_creating=creating)
    return dict(row), repository


async def validate_scope(conn, enrollment, *, creating=False):
    row, repository = await read_scope(
        conn, enrollment.plan.binding_id, creating=creating
    )
    plan = enrollment.plan
    agent = await ns.binding_agent_ref(row, conn=conn)
    workspace = await ns.ledger.get_workspace(plan.binding_id, conn=conn)
    environment = await ns.ledger.get_development_environment(
        plan.binding_id, conn=conn
    )
    if (
        (
            row["user_id"],
            row["project_id"],
            repository,
            row["branch"],
            ns._request_id(row),
        )
        != (
            plan.owner_id,
            plan.project_id,
            plan.repository,
            plan.branch,
            str(plan.create_request_id),
        )
        or asdict(agent) != plan.agent.model_dump()
        or asdict(workspace) != plan.workspace.model_dump()
        or environment
        != (
            plan.development_environment.model_dump()
            if plan.development_environment
            else None
        )
        or reference_from_data(row["credential_ref"])
        != SessionCredential(**plan.references[0].model_dump())
    ):
        raise ValueError("git_plan_conflict")
    if (
        enrollment.association
        and row["kagent_session_id"] != enrollment.association.runtime.session_id
    ):
        raise ValueError("runtime_changed")
    attempt = await ns.attempt_row(row, conn=conn)
    if (attempt["id"] if attempt else None) != plan.attempt_id:
        raise ValueError("attempt_not_current")
    if plan.branch_claim_generation is not None:
        claim = await conn.fetchrow(
            "SELECT * FROM workspace_writer_claims WHERE owner_id=$1 AND repository=$2 AND branch=$3",
            plan.owner_id,
            plan.repository,
            plan.branch,
        )
        if (
            not claim
            or not claim["held"]
            or claim["generation"] != plan.branch_claim_generation
            or (
                claim["attempt_id"] != plan.attempt_id
                or (plan.attempt_id is None and claim["binding_id"] != plan.binding_id)
            )
        ):
            raise ValueError("writer_claim_lost")
    return row


async def plan_for_create(conn, binding_id):
    no_transaction(conn)
    binding = await ns.get_binding(binding_id, conn=conn)
    if binding is None:
        raise ValueError("binding_unavailable")
    existing = await conn.fetchval(
        "SELECT issuance_id FROM git_enrollments WHERE binding_id=$1 AND create_request_id=$2",
        binding_id,
        ns._request_id(binding),
    )
    if existing:
        row, enrollment = await enrollment_row(conn, existing)
        await validate_key(conn, enrollment.plan)
        if row["revoked_at"] is not None:
            raise ValueError("git_enrollment_revoked")
        await validate_scope(conn, enrollment, creating=True)
        return enrollment.plan
    if not settings.git_transport_enabled:
        return None
    if not settings.agent_token_key.strip():
        raise ValueError("git_key_unavailable")
    if (
        not await ns.ledger.get_workspace(binding_id, conn=conn)
        or binding["mcp_grant_kind"] != "workspace"
    ):
        return None
    if (
        binding.get("git_create_dispatched") is not False
        or binding["kagent_session_id"] is not None
    ):
        raise ValueError("original_create_history_unavailable")
    if settings.push_gate_enabled:
        # A cached '' default branch would freeze a writer read-only for good.
        await learn_default_branch(conn, binding)
    async with locked(conn, binding_id):
        # Serialize original request identity and version reservation.
        existing = await conn.fetchval(
            "SELECT issuance_id FROM git_enrollments WHERE binding_id=$1 AND create_request_id=$2",
            binding_id,
            ns._request_id(binding),
        )
        if existing:
            return await plan_for_create(conn, binding_id)
        row, repository = await read_scope(conn, binding_id, creating=True)
        await store.assert_branch_resolved(
            conn, row["user_id"], repository, row["branch"]
        )
        mcp = reference_from_data(row["credential_ref"])
        if mcp is None:
            raise ValueError("mcp_reference_unavailable")
        attempt = await ns.attempt_row(row, conn=conn)
        claim = await conn.fetchrow(
            "SELECT * FROM workspace_writer_claims WHERE owner_id=$1 AND repository=$2 AND branch=$3",
            row["user_id"],
            repository,
            row["branch"],
        )
        claim_valid = (
            claim
            and claim["held"]
            and (
                claim["attempt_id"] == (attempt["id"] if attempt else None)
                and (attempt is not None or claim["binding_id"] == binding_id)
            )
        )
        policy = None
        if settings.push_gate_enabled and not row["default_branch"]:
            await _hold_create(conn, binding, "default_branch_unavailable")
        if row["default_branch"]:
            previous = await conn.fetchval(
                "SELECT policy FROM push_branch_policies WHERE project_id=$1",
                row["project_id"],
            )
            if previous is None:
                await store.set_policy(
                    conn,
                    ProtectedBranchPolicy(
                        project_id=row["project_id"],
                        version=1,
                        default_branch=row["default_branch"],
                    ),
                )
            policy = await store.load_policy(conn, row["project_id"])
        if not settings.push_gate_enabled:
            absent = "push_gate_disabled"
        elif not claim_valid:
            absent = "writer_claim_unavailable"
        else:
            absent = protected_reason(row["branch"], policy)
        push = absent is None
        previous = await conn.fetchval(
            "SELECT grant_data FROM push_grants WHERE id=$1", binding_id
        )
        version = store._decode(previous, PushGrant).version + 1 if previous else 1
        issuance_id = uuid.uuid4()
        refs = [
            asdict(mcp),
            *[
                asdict(_git_reference(purpose, issuance_id))
                for purpose in (("git-read", "git-push") if push else ("git-read",))
            ],
        ]
        plan = GitCreatePlan(
            issuance_id=issuance_id,
            issuance_version=1,
            binding_id=binding_id,
            create_request_id=ns._request_id(row),
            owner_id=row["user_id"],
            project_id=row["project_id"],
            repository=repository,
            branch=row["branch"],
            agent=asdict(await ns.binding_agent_ref(row, conn=conn)),
            workspace=asdict(await ns.ledger.get_workspace(binding_id, conn=conn)),
            development_environment=await ns.ledger.get_development_environment(
                binding_id, conn=conn
            ),
            references=refs,
            attempt_id=attempt["id"] if attempt else None,
            branch_claim_generation=claim["generation"] if claim_valid else None,
            push_version=version if push else None,
        )
        async with conn.transaction():
            await conn.execute(
                """INSERT INTO git_enrollments(issuance_id,binding_id,create_request_id,issuance_version,
                owner_id,project_id,repository,branch,plan,plan_digest,read_token_hash,push_state)
                VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10,$11,$12)""",
                str(issuance_id),
                binding_id,
                str(plan.create_request_id),
                plan.issuance_version,
                plan.owner_id,
                plan.project_id,
                repository,
                plan.branch,
                plan.model_dump_json(),
                plan_digest(plan),
                store.token_hash(capability_for(plan, "git-read")),
                "planned" if push else "absent",
            )
            if absent and attempt and settings.push_gate_enabled:
                await _note_attempt(conn, attempt["id"], "git-push-absent:" + absent)
        return plan


async def frozen_references(conn, binding_id, request_id):
    issuance = await conn.fetchval(
        "SELECT issuance_id FROM git_enrollments WHERE binding_id=$1 AND create_request_id=$2",
        binding_id,
        request_id,
    )
    if issuance is None:
        return ()
    _, enrollment = await enrollment_row(conn, issuance)
    # Recovery of a revoked unknown create deliberately needs no usable key/value.
    return tuple(
        SessionCredential(**r.model_dump()) for r in enrollment.plan.references
    )


async def mark_create_dispatched(conn, plan):
    no_transaction(conn)
    async with conn.transaction():
        await conn.execute(
            "UPDATE git_enrollments SET create_dispatched=TRUE,updated_at=now() WHERE issuance_id=$1",
            str(plan.issuance_id),
        )


def observation(plan, session):
    association = session.runtime_association
    if (
        session.state != RuntimeState.READY
        or not session.settled
        or session.creator != "mainloop"
        or session.context_id != session.id
        or not session.context_confirmed
        or not session.prepared_revision
        or association is None
        or association.phase != "active"
        or not association.current_active
        or session.agent is None
        or asdict(session.agent) != plan.agent.model_dump()
        or session.workspace is None
        or asdict(session.workspace) != plan.workspace.model_dump()
        or (
            asdict(session.development_environment)
            if session.development_environment
            else None
        )
        != (
            plan.development_environment.model_dump(
                include={"image", "platform", "policy_identity"}
            )
            if plan.development_environment
            else None
        )
        or (
            plan.development_environment is not None
            and session.runtime_composition is None
        )
    ):
        raise ValueError("runtime_unattested")
    return GitObservation(
        runtime=GitRuntimeAssociation(
            session_id=session.id,
            revision=session.prepared_revision,
            generation_id=association.generation_id,
            atespace=association.atespace,
            actor_name=association.actor_name,
            actor_uid=association.actor_uid,
        ),
        context_id=session.context_id,
        agent=asdict(session.agent),
        workspace=asdict(session.workspace),
        development_environment=(
            asdict(session.development_environment)
            if session.development_environment
            else None
        ),
        runtime_composition=(
            asdict(session.runtime_composition) if session.runtime_composition else None
        ),
    )


async def confirm_ready(conn, binding_id, observed_get):
    no_transaction(conn)
    binding = await ns.get_binding(binding_id, conn=conn)
    issuance = await conn.fetchval(
        "SELECT issuance_id FROM git_enrollments WHERE binding_id=$1 AND create_request_id=$2",
        binding_id,
        ns._request_id(binding),
    )
    row, enrollment = await enrollment_row(conn, issuance)
    await validate_key(conn, enrollment.plan)
    await validate_scope(conn, enrollment, creating=True)
    if (
        row["revoked_at"]
        or not row["create_dispatched"]
        or binding["kagent_session_id"] != observed_get.id
    ):
        raise ValueError("git_enrollment_unconfirmed")
    current = observation(enrollment.plan, observed_get)
    if row["prepared_revision"] != observed_get.prepared_revision or (
        (
            json.loads(row["reported_composition"])
            if row["reported_composition"]
            else None
        )
        != (
            asdict(observed_get.runtime_composition)
            if observed_get.runtime_composition
            else None
        )
    ):
        raise ValueError("prepared_contract_changed")
    if enrollment.association is not None and current != enrollment.association:
        raise ValueError("runtime_changed")
    async with conn.transaction():
        await conn.execute(
            "UPDATE git_enrollments SET association=$2::jsonb,read_state=CASE WHEN read_state='planned' THEN 'confirmed' ELSE read_state END WHERE issuance_id=$1",
            issuance,
            current.model_dump_json(),
        )
    return enrollment.model_copy(
        update={
            "association": current,
            "read_state": (
                "confirmed"
                if enrollment.read_state == "planned"
                else enrollment.read_state
            ),
        }
    )


async def freeze_prepared(conn, issuance_id, session):
    """Freeze observed preparation before warmup; this alone grants no capability."""
    no_transaction(conn)
    row, enrollment = await enrollment_row(conn, issuance_id)
    if not session.prepared_revision:
        raise ValueError("prepared_contract_unavailable")
    composition = (
        asdict(session.runtime_composition) if session.runtime_composition else None
    )
    if row["prepared_revision"] is not None and (
        row["prepared_revision"] != session.prepared_revision
        or (
            json.loads(row["reported_composition"])
            if row["reported_composition"]
            else None
        )
        != composition
    ):
        raise ValueError("prepared_contract_changed")
    async with conn.transaction():
        await conn.execute(
            "UPDATE git_enrollments SET prepared_revision=$2,reported_composition=$3::jsonb WHERE issuance_id=$1",
            str(issuance_id),
            session.prepared_revision,
            json.dumps(composition) if composition is not None else None,
        )


class GitSecretStore:
    def __init__(self, api=None):
        self.api = api

    def _api(self):
        if self.api is None:
            config.load_incluster_config()
            self.api = client.CoreV1Api()
        return self.api

    def body(self, plan, purpose):
        ref = reference(plan, purpose)
        return {
            "metadata": {
                "name": ref.secret_name,
                "labels": {
                    "mainloop.dev/actor-egress": "true",
                    "mainloop.dev/purpose": purpose,
                    "mainloop.dev/binding": plan.binding_id,
                    "mainloop.dev/issuance": str(plan.issuance_id),
                },
            },
            "type": "Opaque",
            "immutable": True,
            "data": {
                "authorization": base64.b64encode(
                    ("Bearer " + capability_for(plan, purpose)).encode()
                ).decode()
            },
        }

    def _matches(self, obj, body):
        if (
            obj.data != body["data"]
            or obj.type != body["type"]
            or obj.immutable is not True
            or obj.metadata.labels != body["metadata"]["labels"]
            or obj.metadata.name != body["metadata"]["name"]
            or not obj.metadata.uid
        ):
            raise ValueError("git_secret_conflict")
        return obj.metadata.uid

    def _publish(self, plan, purpose):
        body = self.body(plan, purpose)
        try:
            obj = self._api().create_namespaced_secret(
                settings.kagent_namespace, body, _request_timeout=(5, 15)
            )
        except ApiException as exc:
            if exc.status != 409:
                raise RuntimeError("git_secret_unavailable") from None
            obj = self._api().read_namespaced_secret(
                body["metadata"]["name"],
                settings.kagent_namespace,
                _request_timeout=(5, 15),
            )
        return self._matches(obj, body)

    async def publish(self, plan, purpose):
        try:
            return await asyncio.to_thread(self._publish, plan, purpose)
        except ValueError:
            raise
        except Exception:
            raise RuntimeError("git_secret_unavailable") from None

    def _cleanup(self, plan, purpose, uid):
        ref = reference(plan, purpose)
        try:
            obj = self._api().read_namespaced_secret(
                ref.secret_name, settings.kagent_namespace, _request_timeout=(5, 15)
            )
        except ApiException as exc:
            if exc.status == 404:
                return None
            raise RuntimeError("git_cleanup_unavailable") from None
        # Even a recorded UID cannot authorize deleting a same-name replacement.
        actual = self._matches(obj, self.body(plan, purpose))
        if uid is not None and actual != uid:
            raise ValueError("git_cleanup_uid_conflict")
        return actual

    async def cleanup_uid(self, plan, purpose, uid):
        try:
            return await asyncio.to_thread(self._cleanup, plan, purpose, uid)
        except ValueError:
            raise
        except Exception:
            raise RuntimeError("git_cleanup_unavailable") from None

    async def remove(self, plan, purpose, uid):
        def delete():
            try:
                self._api().delete_namespaced_secret(
                    reference(plan, purpose).secret_name,
                    settings.kagent_namespace,
                    body={"preconditions": {"uid": uid}},
                    _request_timeout=(5, 15),
                )
            except ApiException as exc:
                if exc.status != 404:
                    raise RuntimeError("git_cleanup_unavailable") from None

        await asyncio.to_thread(delete)


secrets = GitSecretStore()


async def _ready_preparation_session(client, runtime_id):
    """Read the original runtime without turning lifecycle projections into failure.

    kagent projects receipts as historical during suspension and operations. Only
    a fresh READY/NONE GetSession can classify that evidence as terminal.
    """
    try:
        current = await client.get_session(runtime_id)
    except (OutcomeUnknown, Unreachable) as exc:
        raise ValueError("git_prepare_pending") from exc
    if (
        current.state != RuntimeState.READY
        or current.operation != RuntimeOperation.NONE
    ):
        raise ValueError("git_prepare_pending")
    if current.id != runtime_id:
        raise ValueError("runtime_changed")
    return current


async def _activate_runtime(client, runtime_id):
    """Wake a Ready-but-quiesced runtime before attestation, without trusting the reply.

    An unknown outcome is reconciled through GetSession and never replayed here. If kagent
    still holds the operation, nothing is sent; otherwise the caller's fresh observation
    fails closed unless the same frozen runtime is running.
    """
    try:
        await client.activate_session(runtime_id)
    except (OutcomeUnknown, Unreachable) as exc:
        try:
            current = await client.get_session(runtime_id)
        except (OutcomeUnknown, Unreachable):
            raise ValueError("runtime_activation_pending") from exc
        if (
            current.id != runtime_id
            or current.state != RuntimeState.READY
            or current.operation != RuntimeOperation.NONE
        ):
            raise ValueError("runtime_activation_pending") from exc


async def reobserve(conn, enrollment, *, creating=False, trusted_client=None):
    no_transaction(conn)
    await validate_scope(conn, enrollment, creating=creating)
    if not enrollment.association:
        raise ValueError("git_enrollment_unconfirmed")
    client = trusted_client or ns.get_client()
    runtime_id = enrollment.association.runtime.session_id
    session = (
        await _ready_preparation_session(client, runtime_id)
        if settings.git_transport_enabled and settings.push_gate_enabled
        else await client.get_session(runtime_id)
    )
    if observation(enrollment.plan, session) != enrollment.association:
        raise ValueError("runtime_changed")
    await validate_scope(conn, enrollment, creating=creating)


async def _publish(conn, issuance_id, purpose):
    if not settings.git_transport_enabled:
        raise ValueError("git_transport_disabled")
    row, enrollment = await enrollment_row(conn, issuance_id)
    async with locked(conn, enrollment.plan.binding_id):
        row, enrollment = await enrollment_row(conn, issuance_id)
        if row["revoked_at"] is not None or not enrollment.association:
            raise ValueError("git_enrollment_revoked")
        await validate_key(conn, enrollment.plan)
        if purpose == "git-push" and enrollment.plan.push_version is None:
            return None
        if purpose == "git-push" and (
            not settings.git_transport_enabled or not settings.push_gate_enabled
        ):
            raise ValueError("git_push_disabled")
        creating = purpose == "git-read"
        await reobserve(conn, enrollment, creating=creating)
        if purpose == "git-push":
            plan = enrollment.plan
            binding = await ns.get_binding(plan.binding_id, conn=conn)
            grant = PushGrant(
                id=plan.binding_id,
                owner_id=plan.owner_id,
                project_id=plan.project_id,
                repository=plan.repository,
                branch=plan.branch,
                workspace_id=plan.binding_id,
                session_id=plan.binding_id,
                runtime_identity=enrollment.association.runtime.session_id,
                version=plan.push_version,
                role=binding["role"],
                grant_kind="workspace",
                attempt_id=plan.attempt_id,
                writer_generation=(
                    plan.branch_claim_generation if plan.attempt_id else None
                ),
                branch_claim_generation=plan.branch_claim_generation,
                git_issuance_id=plan.issuance_id,
                runtime_association=enrollment.association.runtime,
            )
            async with conn.transaction():
                await store.issue_derived_locked(conn, grant, enrollment)
        # Commit the cleanup intent before the first potentially lost Secret reply.
        async with conn.transaction():
            await conn.execute(
                (
                    "UPDATE git_enrollments SET read_cleanup_pending=TRUE WHERE issuance_id=$1"
                    if purpose == "git-read"
                    else "UPDATE git_enrollments SET push_cleanup_pending=TRUE WHERE issuance_id=$1"
                ),
                str(issuance_id),
            )
        uid = await secrets.publish(enrollment.plan, purpose)
        await reobserve(conn, enrollment, creating=creating)
        async with conn.transaction():
            await conn.execute(
                (
                    "UPDATE git_enrollments SET read_state='published',read_secret_uid=$2,read_cleanup_pending=FALSE WHERE issuance_id=$1"
                    if purpose == "git-read"
                    else "UPDATE git_enrollments SET push_state='published',push_secret_uid=$2,push_cleanup_pending=FALSE WHERE issuance_id=$1"
                ),
                str(issuance_id),
                uid,
            )
        return reference(enrollment.plan, purpose)


async def publish_read(conn, issuance_id):
    return await _publish(conn, issuance_id, "git-read")


async def publish_push(conn, issuance_id):
    return await _publish(conn, issuance_id, "git-push")


def preparation_profile_for_binding_role(role):
    """Only the authoritative native binding selects standing, never a receipt."""
    profiles = {
        "agent": ("agent", AGENT_SETUP_DIGEST),
        "supervisor": ("supervisor", SUPERVISOR_SETUP_DIGEST),
        "child": ("child", CHILD_SETUP_DIGEST),
    }
    if role not in profiles:
        raise ValueError("git_prepare_role_unsupported")
    return profiles[role]


async def _preparation_failed(
    conn, issuance_id, code="git_prepare_failed", *, receipt=None, grpc_status=None
):
    _, enrollment = await enrollment_row(conn, issuance_id)
    refs = git_reference_counts(enrollment.plan.references)
    # Fixed tokens only: no runtime text, configuration or credential material.
    failure = {"code": code, "grpc_status": grpc_status, "git_refs": refs}
    evidence = f"git-prepare-failed:{code}" + (
        f":grpc={grpc_status}" if grpc_status is not None else ""
    )
    evidence += f":git-refs read={refs['read']} push={refs['push']}"
    attempt_id = enrollment.plan.attempt_id
    async with conn.transaction():
        await conn.execute(
            """UPDATE git_enrollments SET prepare_state='failed',prepare_receipt=COALESCE(CASE
            WHEN $2::jsonb IS NOT NULL AND prepare_receipt->'original'=$2::jsonb->'original'
            THEN $2::jsonb ELSE prepare_receipt END,'{}'::jsonb)
            || jsonb_build_object('failure',$3::jsonb) WHERE issuance_id=$1""",
            str(issuance_id),
            json.dumps(asdict(receipt)) if receipt else None,
            json.dumps(failure),
        )
        if attempt_id:
            await _note_attempt(conn, attempt_id, evidence)
    raise ValueError(code)


async def prepare_for_binding(conn, issuance_id, client):
    """Reconcile one immutable non-turn action under the caller's enrollment locks."""
    if not (settings.git_transport_enabled and settings.push_gate_enabled):
        return
    no_transaction(conn)
    row, enrollment = await enrollment_row(conn, issuance_id)
    binding = await validate_scope(conn, enrollment, creating=True)
    profile = preparation_profile_for_binding_role(binding["role"])
    if profile is None:
        return
    if row["prepare_state"] == "failed":
        raise ValueError("git_prepare_failed")
    plan, association = enrollment.plan, enrollment.association
    if (
        not association
        or not plan.development_environment
        or not association.runtime_composition
        or row["read_state"] != "published"
    ):
        await _preparation_failed(conn, issuance_id, "git_prepare_contract_unavailable")
    action_id = "prep:" + str(plan.issuance_id)
    # Every input comes from committed selection/association, including the role
    # fetched above. Fresh Create/Get replies never supply setup inputs.
    request = PreparationRequest(
        session_id=association.runtime.session_id,
        action_id=action_id,
        create_request_id=str(plan.create_request_id),
        generation_id=association.runtime.generation_id,
        actor_uid=association.runtime.actor_uid,
        prepared_revision=row["prepared_revision"],
        workspace=SessionWorkspace(**plan.workspace.model_dump()),
        development_environment=DevelopmentEnvironment(
            **plan.development_environment.model_dump(
                include={"image", "platform", "policy_identity"}
            )
        ),
        runtime_composition=RuntimeComposition(
            **association.runtime_composition.model_dump()
        ),
        setup_profile=profile[0],
        setup_digest=profile[1],
    )
    original = asdict(request)
    stored = json.loads(row["prepare_receipt"]) if row["prepare_receipt"] else None
    if (row["prepare_action_id"] not in (None, action_id)) or (
        stored is not None and stored.get("original") != original
    ):
        await _preparation_failed(conn, issuance_id, "git_prepare_conflict")
    current = await _ready_preparation_session(client, request.session_id)
    receipt = current.workspace_preparation
    if receipt is None:
        if row["prepare_state"] == "confirmed":
            await _preparation_failed(conn, issuance_id, "git_prepare_receipt_missing")
        if observation(plan, current) != association:
            raise ValueError("runtime_changed")
        # The request-only record is a local reservation, never completion evidence.
        async with conn.transaction():
            await conn.execute(
                """UPDATE git_enrollments SET prepare_action_id=$2,
                prepare_state='requested',prepare_receipt=$3::jsonb WHERE issuance_id=$1""",
                str(issuance_id),
                action_id,
                json.dumps({"original": original}),
            )
        try:
            receipt = await client.prepare_session_workspace(
                request.session_id,
                **{
                    field: getattr(request, field)
                    for field in original
                    if field != "session_id"
                },
            )
        except (OutcomeUnknown, Unreachable) as exc:
            raise ValueError("git_prepare_pending") from exc
        except SessionError as exc:
            if exc.grpc_status != 6:
                # A lifecycle race can reject Prepare after our READY read. Hold
                # until READY again rather than permanently poisoning the action.
                await _ready_preparation_session(client, request.session_id)
            await _preparation_failed(
                conn,
                issuance_id,
                (
                    "git_prepare_conflict"
                    if exc.grpc_status == 6
                    else "git_prepare_failed"
                ),
                grpc_status=exc.grpc_status,
            )
        if receipt.historical or receipt.classification == "definite-failure":
            # A Prepare reply has no current lifecycle state. Classify terminal
            # evidence only through another fresh observation of the same runtime.
            current = await _ready_preparation_session(client, request.session_id)
            receipt = current.workspace_preparation
            if receipt is None:
                raise ValueError("git_prepare_pending")
    elif row["prepare_action_id"] is None or stored is None:
        # A foreign or historical action cannot be adopted into this enrollment.
        await _preparation_failed(conn, issuance_id, "git_prepare_conflict")
    if (
        receipt.original != request
        or receipt.context_id != association.context_id
        or receipt.atespace != association.runtime.atespace
        or receipt.actor_name != association.runtime.actor_name
    ):
        await _preparation_failed(conn, issuance_id, "git_prepare_conflict")
    if receipt.historical or receipt.classification == "definite-failure":
        state = "failed"
    elif receipt.classification == "confirmed":
        if observation(plan, current) != association:
            await _preparation_failed(conn, issuance_id, "runtime_changed")
        state = "confirmed"
    elif receipt.classification in ("pending", "uncertain"):
        # A read-only challenge can close admission after prior confirmation. Do
        # not reopen our durable terminal state or dispatch another action.
        if row["prepare_state"] == "confirmed":
            raise ValueError("git_prepare_pending")
        state = "requested"
    else:
        await _preparation_failed(conn, issuance_id, "git_prepare_receipt_invalid")
    if state == "failed":
        await _preparation_failed(conn, issuance_id, receipt=receipt)
    async with conn.transaction():
        await conn.execute(
            "UPDATE git_enrollments SET prepare_state=$2,prepare_receipt=$3::jsonb WHERE issuance_id=$1",
            str(issuance_id),
            state,
            json.dumps(asdict(receipt)),
        )
    if state != "confirmed":
        raise ValueError(
            "git_prepare_failed" if state == "failed" else "git_prepare_pending"
        )


async def revoke_deferred(conn, binding_id):
    if not conn.is_in_transaction():
        raise RuntimeError("Git revocation requires transaction")
    # Caller holds ordered policy/tree/publication/runtime locks; no I/O or reacquisition.
    await conn.execute(
        "UPDATE push_grants SET revoked_at=COALESCE(revoked_at,now()) WHERE id=$1",
        binding_id,
    )
    await conn.execute(
        """UPDATE git_enrollments SET revoked_at=COALESCE(revoked_at,now()),read_state='revoked',
            push_state=CASE WHEN push_state='absent' THEN 'absent' ELSE 'revoked' END,
            read_cleanup_pending=TRUE,push_cleanup_pending=(push_state<>'absent'),updated_at=now()
            WHERE binding_id=$1""",
        binding_id,
    )


async def reconcile_cleanup(conn, issuance_id):
    no_transaction(conn)
    row, enrollment = await enrollment_row(conn, issuance_id)
    if row["revoked_at"] is None:
        return
    async with _binding_lock(conn, enrollment.plan.binding_id):
        for prefix in ("read", "push"):
            row, enrollment = await enrollment_row(conn, issuance_id)
            if not row[f"{prefix}_cleanup_pending"]:
                continue
            purpose = "git-" + prefix
            uid = await secrets.cleanup_uid(
                enrollment.plan, purpose, row[f"{prefix}_secret_uid"]
            )
            if uid:
                # Capture a lost publication UID durably before conditional deletion.
                await conn.execute(
                    (
                        "UPDATE git_enrollments SET read_secret_uid=$2 WHERE issuance_id=$1"
                        if prefix == "read"
                        else "UPDATE git_enrollments SET push_secret_uid=$2 WHERE issuance_id=$1"
                    ),
                    str(issuance_id),
                    uid,
                )
                await secrets.remove(enrollment.plan, purpose, uid)
            await conn.execute(
                (
                    "UPDATE git_enrollments SET read_cleanup_pending=FALSE WHERE issuance_id=$1"
                    if prefix == "read"
                    else "UPDATE git_enrollments SET push_cleanup_pending=FALSE WHERE issuance_id=$1"
                ),
                str(issuance_id),
            )


async def ready_for_binding(conn, binding_id, session, *, push=True):
    """Warm up and enroll the owned Session, preserving durable restart markers."""
    no_transaction(conn)
    binding = await ns.get_binding(binding_id, conn=conn)
    issuance = await conn.fetchval(
        "SELECT issuance_id FROM git_enrollments WHERE binding_id=$1 AND create_request_id=$2",
        binding_id,
        ns._request_id(binding),
    )
    if issuance is None:
        if (
            settings.git_transport_enabled
            and await ns.ledger.get_workspace(binding_id, conn=conn)
            and binding["mcp_grant_kind"] == "workspace"
        ):
            raise ValueError("git_plan_missing")
        ready = await ns.get_client().ensure_ready(
            session, timeout=settings.kagent_session_ready_timeout_seconds
        )
        from mainloop.push_gate import lifecycle as push_lifecycle

        await push_lifecycle.enroll(conn, binding_id)
        return ready
    async with locked(conn, binding_id):
        row, enrollment = await enrollment_row(conn, issuance)
        if row["revoked_at"]:
            raise ValueError("git_enrollment_revoked")
        if (
            settings.git_transport_enabled
            and settings.push_gate_enabled
            and row["prepare_state"] == "failed"
        ):
            raise ValueError("git_prepare_failed")
        await validate_scope(conn, enrollment, creating=True)
        client = ns.get_client()
        current = (
            await client.ensure_ready(
                session, timeout=settings.kagent_session_ready_timeout_seconds
            )
            if row["warmup_state"] in ("pending", "complete")
            else await client.get_session(session.id)
        )
        # Only the original unconfirmed bootstrap uses Suspend/Resume. Unknown calls
        # reconcile the same Session operation via Get before dispatching another call.
        if row["warmup_state"] != "complete":
            current = await client.get_session(session.id)
            await ns.validate_bound_session(binding, current, conn=conn)
            await freeze_prepared(conn, issuance, current)
            if row["warmup_state"] in ("pending", "suspending"):
                await conn.execute(
                    "UPDATE git_enrollments SET warmup_state='suspending' WHERE issuance_id=$1",
                    issuance,
                )
                if current.state != RuntimeState.SUSPENDED or not current.settled:
                    if not current.settled:
                        raise ValueError("git_warmup_pending")
                    current = await client.suspend_session(session.id)
                if (
                    current.id != session.id
                    or current.state != RuntimeState.SUSPENDED
                    or not current.settled
                ):
                    raise ValueError("git_warmup_pending")
                await conn.execute(
                    "UPDATE git_enrollments SET warmup_state='suspended' WHERE issuance_id=$1",
                    issuance,
                )
            row, enrollment = await enrollment_row(conn, issuance)
            if row["warmup_state"] in ("suspended", "resuming"):
                await conn.execute(
                    "UPDATE git_enrollments SET warmup_state='resuming' WHERE issuance_id=$1",
                    issuance,
                )
                current = await client.get_session(session.id)
                if current.state == RuntimeState.SUSPENDED and current.settled:
                    current = await client.resume_session(session.id)
                current = await client.ensure_ready(
                    current, timeout=settings.kagent_session_ready_timeout_seconds
                )
        # kagent suspends an idle actor but keeps the Session Ready, so a fresh read has no
        # running association to attest until the same actor is woken.
        await _activate_runtime(client, session.id)
        # Never use Create/Resume/ensure_ready output as the association observation.
        if settings.git_transport_enabled and settings.push_gate_enabled:
            runtime_id = (
                enrollment.association.runtime.session_id
                if enrollment.association
                else binding["kagent_session_id"]
            )
            if runtime_id != session.id:
                raise ValueError("runtime_changed")
            current = await _ready_preparation_session(client, runtime_id)
            receipt = current.workspace_preparation
            if receipt and (
                receipt.historical or receipt.classification == "definite-failure"
            ):
                await _preparation_failed(conn, issuance, receipt=receipt)
        else:
            current = await client.get_session(session.id)
        enrollment = await confirm_ready(conn, binding_id, current)
        await conn.execute(
            "UPDATE git_enrollments SET warmup_state='complete' WHERE issuance_id=$1",
            issuance,
        )
        await publish_read(conn, issuance)
        if settings.git_transport_enabled and settings.push_gate_enabled:
            await prepare_for_binding(conn, issuance, client)
        from mainloop.push_gate import lifecycle as push_lifecycle

        if push:
            await publish_push(conn, issuance)
        else:
            await push_lifecycle.enroll(conn, binding_id)
        return current


async def cleanup_all():
    async with connection() as conn:
        rows = await conn.fetch(
            "SELECT issuance_id FROM git_enrollments WHERE revoked_at IS NOT NULL AND (read_cleanup_pending OR push_cleanup_pending)"
        )
        for row in rows:
            try:
                await reconcile_cleanup(conn, row["issuance_id"])
            except Exception:
                # Preserve the durable hold without logging external Secret exceptions.
                logger.warning("Git credential cleanup remains pending")
