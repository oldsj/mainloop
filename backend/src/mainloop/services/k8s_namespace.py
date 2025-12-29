"""Kubernetes namespace management for task isolation."""

import logging
from kubernetes import client, config
from kubernetes.client.rest import ApiException

logger = logging.getLogger(__name__)

# Namespace prefix for task namespaces
TASK_NAMESPACE_PREFIX = "task-"

# Secrets to copy from mainloop namespace to task namespaces
DEFAULT_SECRETS_TO_COPY = [
    "mainloop-secrets",
    "ghcr-secret",  # Image pull secret for ghcr.io
]

# Source namespace for secrets
SOURCE_NAMESPACE = "mainloop"


def get_k8s_client() -> tuple[client.CoreV1Api, client.BatchV1Api]:
    """Get Kubernetes API clients.

    Loads in-cluster config when running in K8s, falls back to kubeconfig for local dev.
    """
    try:
        config.load_incluster_config()
        logger.info("Loaded in-cluster Kubernetes config")
    except config.ConfigException:
        config.load_kube_config()
        logger.info("Loaded kubeconfig for local development")

    return client.CoreV1Api(), client.BatchV1Api()


async def create_task_namespace(task_id: str) -> str:
    """Create an isolated namespace for a task.

    Args:
        task_id: The task ID (will be used in namespace name)

    Returns:
        The namespace name that was created
    """
    core_v1, _ = get_k8s_client()
    namespace_name = f"{TASK_NAMESPACE_PREFIX}{task_id[:8]}"

    namespace = client.V1Namespace(
        metadata=client.V1ObjectMeta(
            name=namespace_name,
            labels={
                "app.kubernetes.io/managed-by": "mainloop",
                "mainloop.dev/task-id": task_id,
            },
        )
    )

    try:
        core_v1.create_namespace(body=namespace)
        logger.info(f"Created namespace: {namespace_name}")
    except ApiException as e:
        if e.status == 409:
            logger.info(f"Namespace {namespace_name} already exists")
        else:
            raise

    return namespace_name


async def copy_secrets_to_namespace(
    task_id: str,
    namespace: str,
    secrets: list[str] | None = None,
) -> None:
    """Copy secrets from mainloop namespace to task namespace.

    Args:
        task_id: The task ID
        namespace: Target namespace to copy secrets to
        secrets: List of secret names to copy (defaults to DEFAULT_SECRETS_TO_COPY)
    """
    core_v1, _ = get_k8s_client()
    secrets_to_copy = secrets or DEFAULT_SECRETS_TO_COPY

    for secret_name in secrets_to_copy:
        try:
            # Read secret from source namespace
            source_secret = core_v1.read_namespaced_secret(
                name=secret_name,
                namespace=SOURCE_NAMESPACE,
            )

            # Create new secret in target namespace
            new_secret = client.V1Secret(
                metadata=client.V1ObjectMeta(
                    name=secret_name,
                    namespace=namespace,
                    labels={
                        "app.kubernetes.io/managed-by": "mainloop",
                        "mainloop.dev/task-id": task_id,
                        "mainloop.dev/copied-from": SOURCE_NAMESPACE,
                    },
                ),
                type=source_secret.type,
                data=source_secret.data,
            )

            try:
                core_v1.create_namespaced_secret(namespace=namespace, body=new_secret)
                logger.info(f"Copied secret {secret_name} to namespace {namespace}")
            except ApiException as e:
                if e.status == 409:
                    logger.info(f"Secret {secret_name} already exists in {namespace}")
                else:
                    raise

        except ApiException as e:
            if e.status == 404:
                logger.warning(f"Secret {secret_name} not found in {SOURCE_NAMESPACE}, skipping")
            else:
                raise


async def delete_task_namespace(task_id: str) -> None:
    """Delete a task namespace and all its resources.

    Args:
        task_id: The task ID (used to construct namespace name)
    """
    core_v1, _ = get_k8s_client()
    namespace_name = f"{TASK_NAMESPACE_PREFIX}{task_id[:8]}"

    try:
        core_v1.delete_namespace(
            name=namespace_name,
            body=client.V1DeleteOptions(
                propagation_policy="Foreground",
            ),
        )
        logger.info(f"Deleted namespace: {namespace_name}")
    except ApiException as e:
        if e.status == 404:
            logger.info(f"Namespace {namespace_name} already deleted")
        else:
            raise


async def namespace_exists(task_id: str) -> bool:
    """Check if a task namespace exists.

    Args:
        task_id: The task ID

    Returns:
        True if namespace exists, False otherwise
    """
    core_v1, _ = get_k8s_client()
    namespace_name = f"{TASK_NAMESPACE_PREFIX}{task_id[:8]}"

    try:
        core_v1.read_namespace(name=namespace_name)
        return True
    except ApiException as e:
        if e.status == 404:
            return False
        raise


async def list_task_namespaces() -> list[str]:
    """List all task namespaces managed by mainloop.

    Returns:
        List of namespace names
    """
    core_v1, _ = get_k8s_client()

    namespaces = core_v1.list_namespace(
        label_selector="app.kubernetes.io/managed-by=mainloop"
    )

    return [ns.metadata.name for ns in namespaces.items]
