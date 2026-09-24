"""Provision one branch workspace actor and its control-plane shim credential."""

from __future__ import annotations

import asyncio
import base64
import secrets
from dataclasses import dataclass
from typing import Protocol

from kubernetes import client, config
from kubernetes.client.rest import ApiException
from mainloop.config import settings
from mainloop.runtime.substrate import ActorRecord, SubstrateControl


@dataclass(frozen=True, slots=True)
class ProvisionedActor:
    actor: ActorRecord
    shim_token_secret_name: str


class ActorProvisioner(Protocol):
    async def create(
        self,
        *,
        atespace: str,
        actor_name: str,
        template: str,
        shim_token_secret_name: str,
    ) -> ProvisionedActor: ...

    async def delete(
        self, *, atespace: str, actor_name: str, shim_token_secret_name: str | None
    ) -> None: ...


class SubstrateActorProvisioner:
    """Uses the existing Substrate control adapter and a namespaced Secret."""

    def __init__(self, control: SubstrateControl | None = None, core_api=None):
        self.control = control or SubstrateControl()
        if core_api is not None:
            self.core_api = core_api
        else:
            try:
                config.load_incluster_config()
            except config.ConfigException:
                config.load_kube_config(
                    config_file=settings.substrate_kubeconfig or None,
                    context=settings.substrate_context or None,
                )
            self.core_api = client.CoreV1Api()

    async def create(
        self,
        *,
        atespace: str,
        actor_name: str,
        template: str,
        shim_token_secret_name: str,
    ) -> ProvisionedActor:
        token = secrets.token_urlsafe(32)
        try:
            await asyncio.to_thread(
                self.core_api.create_namespaced_secret,
                settings.substrate_shim_secret_namespace,
                client.V1Secret(
                    metadata=client.V1ObjectMeta(name=shim_token_secret_name),
                    type="Opaque",
                    data={"token": base64.b64encode(token.encode()).decode()},
                ),
            )
        except ApiException as exc:
            if exc.status != 409:
                raise RuntimeError(
                    f"workspace shim Secret creation failed (status {exc.status})"
                ) from exc

        # An existing actor after a timeout belongs to this persisted binding. Inspect first;
        # never issue a second create for an uncertain outcome.
        actor = await self.control.get_actor(atespace, actor_name)
        if actor is None:
            actor = await self.control.create_actor(
                atespace, actor_name, template=template
            )
        return ProvisionedActor(
            actor=actor, shim_token_secret_name=shim_token_secret_name
        )

    async def delete(
        self, *, atespace: str, actor_name: str, shim_token_secret_name: str | None
    ) -> None:
        await self.control.delete_actor(atespace, actor_name, any_state=True)
        if shim_token_secret_name:
            try:
                await asyncio.to_thread(
                    self.core_api.delete_namespaced_secret,
                    shim_token_secret_name,
                    settings.substrate_shim_secret_namespace,
                )
            except ApiException as exc:
                if exc.status != 404:
                    raise RuntimeError(
                        f"workspace shim Secret deletion failed (status {exc.status})"
                    ) from exc


class FakeActorProvisioner:
    """In-memory provisioner for tests; it never accesses Kubernetes or Substrate."""

    def __init__(self):
        self.actors: dict[tuple[str, str], ActorRecord] = {}
        self.secrets: set[str] = set()

    async def create(
        self,
        *,
        atespace: str,
        actor_name: str,
        template: str,
        shim_token_secret_name: str,
    ) -> ProvisionedActor:
        from mainloop.runtime.substrate import ActorState

        key = (atespace, actor_name)
        actor = self.actors.get(key) or ActorRecord(
            atespace=atespace,
            name=actor_name,
            uid=f"fake-{actor_name}",
            state=ActorState.RUNNING,
            external_snapshot_uri=None,
            current_actor_template_uid=template,
            raw={},
        )
        self.actors[key] = actor
        self.secrets.add(shim_token_secret_name)
        return ProvisionedActor(
            actor=actor, shim_token_secret_name=shim_token_secret_name
        )

    async def delete(
        self, *, atespace: str, actor_name: str, shim_token_secret_name: str | None
    ) -> None:
        self.actors.pop((atespace, actor_name), None)
        if shim_token_secret_name:
            self.secrets.discard(shim_token_secret_name)


_provisioner: ActorProvisioner | None = None


def get_actor_provisioner() -> ActorProvisioner:
    global _provisioner
    if _provisioner is None:
        _provisioner = SubstrateActorProvisioner()
    return _provisioner


def set_actor_provisioner(provisioner: ActorProvisioner | None) -> None:
    """Replace the provisioner in tests or during application assembly."""
    global _provisioner
    _provisioner = provisioner
