"""Resolve project selection once, within the workspace creation transaction."""

import os

from mainloop.db import environments as store
from mainloop.environments.registry import VALIDATOR_VERSION

from models.workspace import WorkspaceEnvironment


async def resolve(conn, project_id: str, owner: str) -> WorkspaceEnvironment | None:
    await store.project(conn, project_id, owner, lock=True)
    selected = await store.selection(conn, project_id, owner)
    if selected is None:
        return None
    env = await store.environment(conn, selected.environment_id, lock=True)
    if not await store.access(conn, env, project_id, owner):
        raise store.EnvironmentError(403, "Environment use grant revoked")
    target = (
        env.accepted_default_version_id
        if selected.follow_default
        else selected.version_id
    )
    if target is None:
        raise store.EnvironmentError(422, "Environment has no accepted default")
    version = await store.version(conn, env.id, target)
    if version.validation_status != "static_validated":
        raise store.EnvironmentError(
            422, "Environment version is not statically validated"
        )
    if version.validator_version != VALIDATOR_VERSION:
        raise store.EnvironmentError(422, "Environment validation policy is stale")
    platform = os.environ.get("WORKSPACE_DEVELOPMENT_PLATFORM", "linux/arm64")
    if platform not in ("linux/arm64", "linux/amd64"):
        raise store.EnvironmentError(422, "Invalid deployment development platform")
    if platform != f"linux/{version.architecture}":
        raise store.EnvironmentError(
            422, f"Environment version lacks platform {platform}"
        )
    return WorkspaceEnvironment(
        environment_id=env.id,
        version_id=version.id,
        image=f"{version.registry}/{version.repository}@{version.platform_manifest_digest}",
        platform=platform,
        policy_identity=f"{version.id}:{version.validator_version}",
    )
