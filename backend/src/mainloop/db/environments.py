"""Environment data access. Call mutations inside a caller-owned transaction."""

import json

from models.environment import (
    DevEnvironment,
    EnvironmentVersion,
    ProjectEnvironmentSelection,
)


class EnvironmentError(ValueError):
    def __init__(self, status: int, message: str):
        super().__init__(message)
        self.status = status


def decode(value):
    return json.loads(value) if isinstance(value, str) else value


async def environment(conn, environment_id, owner=None, *, lock=False):
    row = await conn.fetchrow(
        (
            "SELECT * FROM dev_environments WHERE id=$1 FOR UPDATE"
            if lock
            else "SELECT * FROM dev_environments WHERE id=$1"
        ),
        environment_id,
    )
    if row is None or (owner is not None and row["owner_id"] != owner):
        raise EnvironmentError(404, "Environment not found")
    return DevEnvironment.model_validate(
        {
            **decode(row["snapshot"]),
            "accepted_default_version_id": row["default_version_id"],
        }
    )


async def list_environments(conn, owner):
    rows = await conn.fetch(
        "SELECT id FROM dev_environments WHERE owner_id=$1 ORDER BY id", owner
    )
    return [await environment(conn, row["id"], owner) for row in rows]


async def version(conn, environment_id, version_id):
    value = await conn.fetchval(
        "SELECT snapshot FROM environment_versions WHERE environment_id=$1 AND id=$2",
        environment_id,
        version_id,
    )
    if value is None:
        raise EnvironmentError(404, "Environment version not found")
    return EnvironmentVersion.model_validate(decode(value))


async def versions(conn, environment_id):
    rows = await conn.fetch(
        "SELECT snapshot FROM environment_versions WHERE environment_id=$1 ORDER BY id",
        environment_id,
    )
    return [EnvironmentVersion.model_validate(decode(row["snapshot"])) for row in rows]


async def add_version(conn, value):
    await conn.execute(
        "INSERT INTO environment_versions(id,environment_id,snapshot) VALUES($1,$2,$3::jsonb)",
        value.id,
        value.environment_id,
        value.model_dump_json(),
    )


async def register(conn, env, value):
    await conn.execute(
        "INSERT INTO dev_environments(id,owner_id,snapshot) VALUES($1,$2,$3::jsonb)",
        env.id,
        env.owner_id,
        env.model_dump_json(),
    )
    await add_version(conn, value)


async def set_default(conn, environment_id, owner, version_id):
    await environment(conn, environment_id, owner, lock=True)
    value = await version(conn, environment_id, version_id)
    if value.validation_status != "static_validated":
        raise EnvironmentError(422, "Version is not statically validated")
    await conn.execute(
        "UPDATE dev_environments SET default_version_id=$2 WHERE id=$1",
        environment_id,
        version_id,
    )
    return await environment(conn, environment_id, owner)


async def grant(conn, environment_id, owner, project_id, permission):
    await environment(conn, environment_id, owner, lock=True)
    if not await conn.fetchval("SELECT id FROM projects WHERE id=$1", project_id):
        raise EnvironmentError(404, "Project not found")
    if permission is None:
        await conn.execute(
            "DELETE FROM environment_grants WHERE environment_id=$1 AND project_id=$2",
            environment_id,
            project_id,
        )
    else:
        await conn.execute(
            "INSERT INTO environment_grants VALUES($1,$2,$3) ON CONFLICT(environment_id,project_id) DO UPDATE SET permission=excluded.permission",
            environment_id,
            project_id,
            permission,
        )


async def project(conn, project_id, owner, *, lock=False):
    # Serialize selection writers without conflicting with grant inserts
    # taking a foreign-key KEY SHARE lock on this unchanged project key.
    row = await conn.fetchrow(
        (
            "SELECT user_id FROM projects WHERE id=$1 FOR NO KEY UPDATE"
            if lock
            else "SELECT user_id FROM projects WHERE id=$1"
        ),
        project_id,
    )
    if row is None or row["user_id"] != owner:
        raise EnvironmentError(404, "Project not found")


async def access(conn, env, project_id, owner):
    return env.owner_id == owner or bool(
        await conn.fetchval(
            "SELECT permission FROM environment_grants WHERE environment_id=$1 AND project_id=$2",
            env.id,
            project_id,
        )
    )


async def selection(conn, project_id, owner):
    await project(conn, project_id, owner)
    row = await conn.fetchrow(
        "SELECT * FROM project_environment_selections WHERE project_id=$1", project_id
    )
    if row is None:
        return None
    env = await environment(conn, row["environment_id"])
    return ProjectEnvironmentSelection(
        **dict(row),
        access_revoked=not await access(conn, env, project_id, owner),
        resolved_version_id=(
            env.accepted_default_version_id
            if row["follow_default"]
            else row["version_id"]
        )
    )


async def select(conn, project_id, owner, request):
    # Project row serializes both first selection and concurrent replacements. Environment
    # row serializes default/grant changes with authorization and version resolution.
    await project(conn, project_id, owner, lock=True)
    env = await environment(conn, request.environment_id, lock=True)
    if not await access(conn, env, project_id, owner):
        raise EnvironmentError(403, "Environment use grant required")
    revision = (
        await conn.fetchval(
            "SELECT revision FROM project_environment_selections WHERE project_id=$1",
            project_id,
        )
        or 0
    )
    if revision != request.expected_version:
        raise EnvironmentError(409, "Stale expected version")
    target = (
        env.accepted_default_version_id
        if request.follow_default
        else request.version_id
    )
    if target is None:
        raise EnvironmentError(422, "Environment has no accepted default")
    candidate = await version(conn, env.id, target)
    if candidate.validation_status != "static_validated":
        raise EnvironmentError(422, "Version is not statically validated")
    await conn.execute(
        """INSERT INTO project_environment_selections VALUES($1,$2,$3,$4,$5)
        ON CONFLICT(project_id) DO UPDATE SET environment_id=excluded.environment_id,
        version_id=excluded.version_id,follow_default=excluded.follow_default,revision=excluded.revision""",
        project_id,
        env.id,
        request.version_id,
        request.follow_default,
        revision + 1,
    )
    return await selection(conn, project_id, owner)
