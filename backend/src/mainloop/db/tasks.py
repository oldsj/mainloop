"""Caller-owned transactions for task admission and idempotency.

Lock order: global admission key, request key, parent task, branch claim. Never take
runtime/publication locks here. Unknown attempts retain capacity until a confirmed fence.
"""

import hashlib
import json
import uuid
from datetime import UTC, datetime

from mainloop.services.github_repo import parse_github_repo
from mainloop.tasks.events import reserve_event

from models.task import (
    ProjectProviderPreference,
    Task,
    TaskAttempt,
    TaskOperation,
    TaskProjection,
)


class TaskError(ValueError):
    def __init__(self, status: int, code: str):
        super().__init__(code)
        self.status = status
        self.code = code


def decode(value):
    return json.loads(value) if isinstance(value, str) else value


def digest(payload) -> str:
    return hashlib.sha256(
        json.dumps(
            payload, sort_keys=True, separators=(",", ":"), ensure_ascii=False
        ).encode()
    ).hexdigest()


def require_transaction(conn):
    if not conn.is_in_transaction():
        raise TaskError(500, "transaction_required")


async def admission_lock(conn):
    require_transaction(conn)
    await conn.execute(
        "SELECT pg_advisory_xact_lock(hashtextextended('mainloop:task-admission',0))"
    )


async def validate_principal(conn, principal):
    if principal.role == "owner":
        return
    binding = await conn.fetchrow(
        """SELECT n.role,n.token_hash,n.kagent_deleted_at,s.user_id,s.archived_at
           FROM native_bindings n JOIN sessions s ON s.id=n.session_id WHERE n.session_id=$1""",
        principal.binding_id,
    )
    if (
        not binding
        or binding["user_id"] != principal.owner_id
        or binding["role"] != principal.role
        or binding["token_hash"] is None
        or binding["archived_at"] is not None
        or binding["kagent_deleted_at"] is not None
    ):
        raise TaskError(403, "inactive_principal")
    if principal.role == "main":
        return
    row = await conn.fetchrow(
        """SELECT a.*,t.current_attempt_id,t.owner_id,t.project_id,t.root_task_id
           FROM task_attempts a JOIN tasks t ON t.id=a.task_id WHERE a.id=$1""",
        principal.attempt_id,
    )
    if (
        not row
        or row["current_attempt_id"] != principal.attempt_id
        or row["state"] != "active"
        or row["task_id"] != principal.task_id
        or row["binding_id"] != principal.binding_id
        or row["owner_id"] != principal.owner_id
        or row["role"] != principal.role
        or row["depth"] != principal.depth
        or row["project_id"] != principal.project_id
        or row["root_task_id"] != principal.root_task_id
    ):
        raise TaskError(403, "inactive_principal")


async def get_task(conn, task_id, principal, *, manage=False, lock=False):
    await validate_principal(conn, principal)
    row = await conn.fetchrow(
        (
            "SELECT * FROM tasks WHERE id=$1 FOR UPDATE"
            if lock
            else "SELECT * FROM tasks WHERE id=$1"
        ),
        task_id,
    )
    if not row or row["owner_id"] != principal.owner_id:
        raise TaskError(404, "task_not_found")
    task = Task.model_validate(decode(row["snapshot"]))
    if principal.role in ("owner", "main"):
        return task
    if (
        principal.role == "supervisor"
        and task.project_id == principal.project_id
        and task.root_task_id == principal.root_task_id
    ):
        if task.parent_task_id == principal.task_id or (
            not manage and task.id == principal.task_id
        ):
            return task
    if principal.role == "child" and not manage and task.id == principal.task_id:
        return task
    raise TaskError(404, "task_not_found")


async def list_tasks(conn, principal, *, project_id=None, parent_task_id=None):
    await validate_principal(conn, principal)
    rows = await conn.fetch(
        "SELECT id FROM tasks WHERE owner_id=$1 AND ($2::text IS NULL OR project_id=$2) AND ($3::text IS NULL OR parent_task_id=$3) ORDER BY created_at,id",
        principal.owner_id,
        project_id,
        parent_task_id,
    )
    results = []
    for row in rows:
        try:
            results.append(await get_task(conn, row["id"], principal))
        except TaskError as exc:
            if exc.status != 404:
                raise
    return results


async def attempts(conn, task_id):
    rows = await conn.fetch(
        "SELECT * FROM task_attempts WHERE task_id=$1 ORDER BY number", task_id
    )
    # SQL routing is authoritative even when another slice updates an observation snapshot.
    return [
        TaskAttempt.model_validate(
            {
                **decode(r["snapshot"]),
                **{
                    key: r[key]
                    for key in (
                        "id",
                        "task_id",
                        "number",
                        "profile_id",
                        "native_provider",
                        "configuration_revision",
                        "role",
                        "depth",
                        "state",
                        "session_id",
                        "binding_id",
                        "workspace_id",
                        "writer_generation",
                    )
                },
                "agent_ref": decode(r["agent_ref"]),
            }
        )
        for r in rows
    ]


async def projection(conn, task_id):
    return TaskProjection.model_validate(
        decode(await conn.fetchval("SELECT projection FROM tasks WHERE id=$1", task_id))
    )


async def project(conn, project_id, owner):
    row = await conn.fetchrow(
        "SELECT * FROM projects WHERE id=$1 AND user_id=$2", project_id, owner
    )
    if row is None:
        raise TaskError(404, "project_not_found")
    return row


async def preference(conn, project_id, owner):
    await project(conn, project_id, owner)
    row = await conn.fetchrow(
        "SELECT * FROM project_provider_preferences WHERE project_id=$1", project_id
    )
    return (
        ProjectProviderPreference(**dict(row))
        if row
        else ProjectProviderPreference(project_id=project_id)
    )


async def set_preference(conn, project_id, owner, request):
    require_transaction(conn)
    await conn.execute(
        "SELECT pg_advisory_xact_lock(hashtextextended($1,0))",
        f"provider-preference:{project_id}",
    )
    current = await preference(conn, project_id, owner)
    if current.version != request.expected_version:
        raise TaskError(409, "stale_provider_preference")
    await conn.execute(
        """INSERT INTO project_provider_preferences VALUES($1,$2,$3)
        ON CONFLICT(project_id) DO UPDATE SET profile_id=excluded.profile_id,version=excluded.version""",
        project_id,
        request.profile_id,
        current.version + 1,
    )
    return await preference(conn, project_id, owner)


async def lock_operation_request(conn, principal, request_id):
    require_transaction(conn)
    await conn.execute(
        "SELECT pg_advisory_xact_lock(hashtextextended($1,0))",
        f"task-request:{principal.owner_id}:{principal.key}:{request_id}",
    )


async def begin_operation(conn, principal, request_id, kind, payload, *, task_id=None):
    await validate_principal(conn, principal)
    await lock_operation_request(conn, principal, request_id)
    request_digest = digest({"kind": kind, "task_id": task_id, "payload": payload})
    row = await conn.fetchrow(
        "SELECT * FROM task_operations WHERE owner_id=$1 AND principal_key=$2 AND request_id=$3",
        principal.owner_id,
        principal.key,
        request_id,
    )
    if row:
        if row["request_digest"] != request_digest:
            raise TaskError(409, "request_payload_conflict")
        return TaskOperation.model_validate(decode(row["snapshot"])), False
    now = datetime.now(UTC)
    value = TaskOperation(
        id=uuid.uuid4().hex,
        owner_id=principal.owner_id,
        principal_key=principal.key,
        request_id=request_id,
        request_digest=request_digest,
        request_payload=payload,
        kind=kind,
        task_id=task_id,
        created_at=now,
        updated_at=now,
    )
    await save_operation(conn, value, insert=True)
    return value, True


async def save_operation(conn, value, *, insert=False):
    require_transaction(conn)
    if insert:
        await conn.execute(
            """INSERT INTO task_operations(id,owner_id,principal_key,request_id,request_digest,kind,task_id,attempt_id,state,snapshot)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb)""",
            value.id,
            value.owner_id,
            value.principal_key,
            value.request_id,
            value.request_digest,
            value.kind,
            value.task_id,
            value.attempt_id,
            value.state,
            value.model_dump_json(),
        )
    else:
        await conn.execute(
            "UPDATE task_operations SET task_id=$2,attempt_id=$3,state=$4,snapshot=$5::jsonb,updated_at=NOW() WHERE id=$1",
            value.id,
            value.task_id,
            value.attempt_id,
            value.state,
            value.model_dump_json(),
        )


async def operation(conn, operation_id, principal):
    await validate_principal(conn, principal)
    row = await conn.fetchrow(
        "SELECT * FROM task_operations WHERE id=$1 AND owner_id=$2",
        operation_id,
        principal.owner_id,
    )
    if not row:
        raise TaskError(404, "operation_not_found")
    value = TaskOperation.model_validate(decode(row["snapshot"]))
    if principal.role not in ("owner", "main"):
        if value.task_id is None:
            if value.principal_key != principal.key:
                raise TaskError(404, "operation_not_found")
        else:
            await get_task(conn, value.task_id, principal)
    return value


async def insert_task(conn, task):
    require_transaction(conn)
    await conn.execute(
        """INSERT INTO tasks(id,owner_id,project_id,parent_task_id,root_task_id,creator_binding_id,mode,status,current_attempt_id,version,snapshot,topic_id)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb,$12)""",
        task.id,
        task.owner_id,
        task.project_id,
        task.parent_task_id,
        task.root_task_id,
        task.creator_binding_id,
        task.mode,
        task.status,
        task.current_attempt_id,
        task.version,
        task.model_dump_json(),
        task.topic_id,
    )


async def save_task(conn, task, expected_version, event_key):
    require_transaction(conn)
    row = await conn.fetchrow(
        """UPDATE tasks SET current_attempt_id=$2,status=$3,version=$4,snapshot=$5::jsonb,updated_at=NOW()
        WHERE id=$1 AND version=$6 RETURNING id""",
        task.id,
        task.current_attempt_id,
        task.status,
        task.version,
        task.model_dump_json(),
        expected_version,
    )
    if not row:
        raise TaskError(409, "stale_task_version")
    await reserve_event(conn, task, event_key)


async def reserve_writer(
    conn, *, owner_id, repository, branch, attempt_id=None, binding_id=None
):
    # S1 calls this for owner workspaces with binding_id and no task/attempt.
    await admission_lock(conn)
    canonical = parse_github_repo(repository).full_name.lower()
    await conn.execute(
        "SELECT pg_advisory_xact_lock(hashtextextended($1,0))",
        f"writer:{owner_id}:{canonical}:{branch}",
    )
    row = await conn.fetchrow(
        "SELECT * FROM workspace_writer_claims WHERE owner_id=$1 AND repository=$2 AND branch=$3 FOR UPDATE",
        owner_id,
        canonical,
        branch,
    )
    if row and row["held"]:
        raise TaskError(409, "branch_writer_exists")
    generation = row["generation"] + 1 if row else 1
    await conn.execute(
        """INSERT INTO workspace_writer_claims(owner_id,repository,branch,generation,attempt_id,binding_id)
        VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(owner_id,repository,branch)
        DO UPDATE SET generation=excluded.generation,attempt_id=excluded.attempt_id,binding_id=excluded.binding_id,held=TRUE,fence_evidence_ref=NULL,fenced_at=NULL""",
        owner_id,
        canonical,
        branch,
        generation,
        attempt_id,
        binding_id,
    )
    return generation


async def admit_attempt(
    conn,
    task,
    profile,
    *,
    role,
    depth,
    per_parent_cap=None,
    global_cap=None,
    predecessor_id=None,
):
    from mainloop.config import settings

    per_parent_cap = (
        settings.task_max_children_per_parent
        if per_parent_cap is None
        else per_parent_cap
    )
    global_cap = settings.task_max_children_global if global_cap is None else global_cap
    await admission_lock(conn)
    # Lock the tree's parent before counting. An owner-created root belongs to the
    # owner's main admission bucket; child tasks belong to their persisted parent.
    if task.parent_task_id:
        await conn.fetchrow(
            "SELECT id FROM tasks WHERE id=$1 FOR UPDATE", task.parent_task_id
        )
    if (
        await conn.fetchval("SELECT count(*) FROM task_attempts WHERE capacity_held")
        >= global_cap
    ):
        raise TaskError(409, "global_capacity")
    count = await conn.fetchval(
        """SELECT count(*) FROM task_attempts a JOIN tasks t ON t.id=a.task_id
        WHERE a.capacity_held AND t.owner_id=$1 AND t.parent_task_id IS NOT DISTINCT FROM $2""",
        task.owner_id,
        task.parent_task_id,
    )
    if count >= per_parent_cap:
        raise TaskError(409, "parent_capacity")
    if (
        await conn.fetchval("SELECT current_attempt_id FROM tasks WHERE id=$1", task.id)
        is not None
    ):
        raise TaskError(409, "current_attempt_exists")
    now = datetime.now(UTC)
    number = await conn.fetchval(
        "SELECT COALESCE(MAX(number),0)+1 FROM task_attempts WHERE task_id=$1", task.id
    )
    attempt = TaskAttempt(
        id=uuid.uuid4().hex,
        task_id=task.id,
        number=number,
        profile_id=profile.id,
        native_provider=profile.native_provider,
        configuration_revision=profile.configuration_revision,
        agent_ref=profile.agents[role],
        role=role,
        depth=depth,
        predecessor_id=predecessor_id,
        environment=task.accepted_environment,
        initial_ref=task.checkout.ref if task.checkout else None,
        created_at=now,
        updated_at=now,
    )
    await conn.execute(
        """INSERT INTO task_attempts(id,task_id,number,profile_id,native_provider,configuration_revision,agent_ref,role,depth,state,snapshot)
        VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9,$10,$11::jsonb)""",
        attempt.id,
        task.id,
        attempt.number,
        attempt.profile_id,
        attempt.native_provider,
        attempt.configuration_revision,
        attempt.agent_ref.model_dump_json(),
        attempt.role,
        attempt.depth,
        attempt.state,
        attempt.model_dump_json(),
    )
    if task.mode == "code":
        p = await project(conn, task.project_id, task.owner_id)
        repo = parse_github_repo(p["full_name"]).full_name.lower()
        if parse_github_repo(p["html_url"]).full_name.lower() != repo:
            raise TaskError(409, "project_repository_mismatch")
        if task.checkout.branch == p["default_branch"]:
            raise TaskError(422, "default_branch_forbidden")
        generation = await reserve_writer(
            conn,
            owner_id=task.owner_id,
            repository=repo,
            branch=task.checkout.branch,
            attempt_id=attempt.id,
        )
        attempt = attempt.model_copy(update={"writer_generation": generation})
        await conn.execute(
            "UPDATE task_attempts SET writer_generation=$2,snapshot=$3::jsonb WHERE id=$1",
            attempt.id,
            generation,
            attempt.model_dump_json(),
        )
    updated = task.model_copy(
        update={
            "current_attempt_id": attempt.id,
            "version": task.version + 1,
            "updated_at": now,
        }
    )
    await save_task(conn, updated, task.version, f"attempt:{attempt.id}:admitted")
    return updated, attempt


async def release_writer(
    conn,
    *,
    owner_id,
    repository,
    branch,
    generation,
    attempt_id=None,
    binding_id=None,
    fence_evidence_ref=None,
):
    await admission_lock(conn)
    if not fence_evidence_ref:
        raise TaskError(409, "fence_evidence_required")
    # Callers provide confirmed fence evidence through S1's lifecycle port; attempts
    # additionally require the durable state to have reached fenced.
    if (
        attempt_id
        and await conn.fetchval(
            "SELECT state FROM task_attempts WHERE id=$1", attempt_id
        )
        != "fenced"
    ):
        raise TaskError(409, "source_not_fenced")
    row = await conn.fetchrow(
        """UPDATE workspace_writer_claims SET held=FALSE,attempt_id=NULL,binding_id=NULL,fence_evidence_ref=$7,fenced_at=NOW()
        WHERE owner_id=$1 AND repository=$2 AND branch=$3 AND generation=$4 AND held
        AND attempt_id IS NOT DISTINCT FROM $5 AND binding_id IS NOT DISTINCT FROM $6 RETURNING generation""",
        owner_id,
        parse_github_repo(repository).full_name.lower(),
        branch,
        generation,
        attempt_id,
        binding_id,
        fence_evidence_ref,
    )
    if row is None:
        raise TaskError(409, "stale_writer_generation")


async def update_projection(
    conn, principal, task_id, expected_version, expected_attempt_id, value, event_key
):
    """S4 stores read facts with the task event atomically; facts grant no authority."""
    task = await get_task(conn, task_id, principal, manage=True, lock=True)
    if await conn.fetchval(
        "SELECT id FROM task_events WHERE task_id=$1 AND event_key=$2",
        task_id,
        event_key,
    ):
        return task
    if (task.version, task.current_attempt_id) != (
        expected_version,
        expected_attempt_id,
    ):
        raise TaskError(409, "stale_task_attempt")
    await conn.execute(
        "UPDATE tasks SET projection=$2::jsonb WHERE id=$1",
        task_id,
        value.model_dump_json(),
    )
    task = task.model_copy(
        update={"version": task.version + 1, "updated_at": datetime.now(UTC)}
    )
    await save_task(conn, task, expected_version, event_key)
    return task


async def add_artifact(conn, operation_id, kind, payload):
    """S3 immutable canonical JSON. The optional provider note remains unverified."""
    require_transaction(conn)
    await conn.fetchrow(
        "SELECT id FROM task_operations WHERE id=$1 FOR UPDATE", operation_id
    )
    content = json.dumps(
        payload, sort_keys=True, separators=(",", ":"), ensure_ascii=False
    )
    limit = 8192 if kind == "unverified_provider_summary" else 32768
    if len(content.encode()) > limit:
        raise TaskError(422, "artifact_too_large")
    checksum = hashlib.sha256(content.encode()).hexdigest()
    existing = await conn.fetchrow(
        "SELECT id,sha256 FROM task_artifacts WHERE operation_id=$1 AND kind=$2",
        operation_id,
        kind,
    )
    if existing:
        if existing["sha256"] != checksum:
            raise TaskError(409, "artifact_payload_conflict")
        return existing["id"]
    artifact_id = uuid.uuid4().hex
    await conn.execute(
        "INSERT INTO task_artifacts(id,operation_id,kind,content,sha256) VALUES($1,$2,$3,$4,$5)",
        artifact_id,
        operation_id,
        kind,
        content,
        checksum,
    )
    return artifact_id


async def get_artifact(conn, artifact_id, principal):
    row = await conn.fetchrow(
        "SELECT a.*,o.owner_id,o.task_id,o.principal_key FROM task_artifacts a JOIN task_operations o ON o.id=a.operation_id WHERE a.id=$1",
        artifact_id,
    )
    if row is None or row["owner_id"] != principal.owner_id:
        raise TaskError(404, "artifact_not_found")
    await operation(conn, row["operation_id"], principal)
    if hashlib.sha256(row["content"].encode()).hexdigest() != row["sha256"]:
        raise TaskError(409, "artifact_integrity_error")
    return {
        "id": row["id"],
        "operation_id": row["operation_id"],
        "kind": row["kind"],
        "sha256": row["sha256"],
        "payload": json.loads(row["content"]),
    }


async def handoff_operation(conn, operation_id, *, lock=False):
    query = "SELECT snapshot FROM task_operations WHERE id=$1"
    if lock:
        query += " FOR UPDATE"
    value = await conn.fetchval(query, operation_id)
    return TaskOperation.model_validate(decode(value)) if value else None


def handoff_require_admission(task, operation):
    """Caller holds admission/task locks before reserving a replacement writer."""
    if task.status in ("completed", "failed", "cancelled"):
        raise TaskError(409, "task_terminal")
    if task.version != operation.request_payload.get("_s3", {}).get("expected_version"):
        raise TaskError(409, "stale_task_attempt")


async def handoff_save(conn, previous, value):
    """Admission -> task -> operation CAS, with a committed task-event projection."""
    require_transaction(conn)
    await admission_lock(conn)
    task_row = None
    if previous.task_id:
        task_row = await conn.fetchval(
            "SELECT snapshot FROM tasks WHERE id=$1 FOR UPDATE", previous.task_id
        )
    current = await handoff_operation(conn, previous.id, lock=True)
    if current != previous:
        raise TaskError(409, "stale_handoff_operation")
    if task_row is not None:
        task = Task.model_validate(decode(task_row))
        # A new authoritative no-start retry may leave its original failed
        # projection. Later records/errors must never undo a committed stop.
        starting_no_start = (
            task.status == "failed"
            and previous.source_attempt_id is None
            and "_s3" not in previous.request_payload
            and value.source_attempt_id is not None
            and value.state == "requested"
            and value.request_payload.get("_s3", {}).get("no_start") is True
        )
        if (
            task.status in ("completed", "failed", "cancelled")
            and not starting_no_start
        ):
            await save_operation(conn, value)
            return
        reason = value.reason or (
            None if value.state in ("completed", "target_ready") else "handoff"
        )
        status = (
            task.status
            if value.state in ("completed", "target_ready")
            else ("blocked" if value.state == "blocked" else "waiting")
        )
        updated = task.model_copy(
            update={
                "reason": reason,
                "status": status,
                "version": task.version + 1,
                "updated_at": datetime.now(UTC),
            }
        )
        await save_task(
            conn, updated, task.version, f"handoff:{value.id}:{updated.version}"
        )
        metadata = value.request_payload.get("_s3")
        if metadata is not None:
            value = value.model_copy(
                update={
                    "request_payload": {
                        **value.request_payload,
                        "_s3": {**metadata, "expected_version": updated.version},
                    }
                }
            )
    await save_operation(conn, value)


async def handoff_pending(conn, task_id, excluding):
    return await conn.fetchval(
        """SELECT EXISTS(SELECT 1 FROM task_operations WHERE task_id=$1
           AND id<>$2 AND kind IN ('retry','reassign','cancel')
           AND state NOT IN ('completed','blocked'))""",
        task_id,
        excluding,
    )


async def handoff_children_drained(conn, task_id):
    return not await conn.fetchval(
        """SELECT EXISTS(SELECT 1 FROM task_attempts a JOIN tasks t ON t.id=a.task_id
           WHERE t.parent_task_id=$1 AND a.state IN ('creating','active','draining'))""",
        task_id,
    )


async def retention_candidates(conn):
    """Do not delete around uncertain handoff/publication or native HITL."""
    rows = await conn.fetch(
        """SELECT a.task_id,a.id FROM task_attempts a JOIN tasks t ON t.id=a.task_id
           WHERE a.state='superseded' AND NOT EXISTS(
             SELECT 1 FROM task_operations o WHERE o.task_id=a.task_id
             AND o.state NOT IN ('completed','blocked'))
           AND NOT EXISTS(SELECT 1 FROM task_operations o WHERE o.task_id=a.task_id
             AND o.state='blocked' AND o.snapshot->>'reason' IS NOT NULL)
           ORDER BY a.id"""
    )
    return rows
