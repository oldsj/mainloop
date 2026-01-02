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

# Worker service account name
WORKER_SERVICE_ACCOUNT = "worker"

# ClusterRole to bind to worker service account
WORKER_CLUSTER_ROLE = "mainloop-worker-role"


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


def get_rbac_client() -> client.RbacAuthorizationV1Api:
    """Get Kubernetes RBAC API client."""
    try:
        config.load_incluster_config()
    except config.ConfigException:
        config.load_kube_config()

    return client.RbacAuthorizationV1Api()


def get_networking_client() -> client.NetworkingV1Api:
    """Get Kubernetes Networking API client."""
    try:
        config.load_incluster_config()
    except config.ConfigException:
        config.load_kube_config()

    return client.NetworkingV1Api()


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
                logger.warning(
                    f"Secret {secret_name} not found in {SOURCE_NAMESPACE}, skipping"
                )
            else:
                raise


async def setup_worker_rbac(task_id: str, namespace: str) -> None:
    """Create ServiceAccount and RoleBinding for worker in task namespace.

    This gives the worker pod permissions to deploy resources within its namespace.

    Args:
        task_id: The task ID
        namespace: Target namespace

    """
    core_v1, _ = get_k8s_client()
    rbac_v1 = get_rbac_client()

    # Create worker ServiceAccount
    service_account = client.V1ServiceAccount(
        metadata=client.V1ObjectMeta(
            name=WORKER_SERVICE_ACCOUNT,
            namespace=namespace,
            labels={
                "app.kubernetes.io/managed-by": "mainloop",
                "mainloop.dev/task-id": task_id,
            },
        ),
    )

    try:
        core_v1.create_namespaced_service_account(
            namespace=namespace, body=service_account
        )
        logger.info(f"Created ServiceAccount {WORKER_SERVICE_ACCOUNT} in {namespace}")
    except ApiException as e:
        if e.status == 409:
            logger.info(
                f"ServiceAccount {WORKER_SERVICE_ACCOUNT} already exists in {namespace}"
            )
        else:
            raise

    # Create RoleBinding to bind ClusterRole to ServiceAccount
    role_binding = client.V1RoleBinding(
        metadata=client.V1ObjectMeta(
            name="worker-role-binding",
            namespace=namespace,
            labels={
                "app.kubernetes.io/managed-by": "mainloop",
                "mainloop.dev/task-id": task_id,
            },
        ),
        subjects=[
            client.RbacV1Subject(
                kind="ServiceAccount",
                name=WORKER_SERVICE_ACCOUNT,
                namespace=namespace,
            ),
        ],
        role_ref=client.V1RoleRef(
            kind="ClusterRole",
            name=WORKER_CLUSTER_ROLE,
            api_group="rbac.authorization.k8s.io",
        ),
    )

    try:
        rbac_v1.create_namespaced_role_binding(namespace=namespace, body=role_binding)
        logger.info(f"Created RoleBinding for {WORKER_SERVICE_ACCOUNT} in {namespace}")
    except ApiException as e:
        if e.status == 409:
            logger.info(f"RoleBinding already exists in {namespace}")
        else:
            raise


async def apply_task_namespace_network_policies(task_id: str, namespace: str) -> None:
    """Apply network policies to task namespace for security isolation.

    Applies strict network policies:
    - Default deny-all ingress and egress
    - Allow DNS (required for internet access)
    - Allow egress to internet ONLY (blocks all cluster internal communication)

    This ensures worker agents can only communicate with external services,
    not with other cluster resources or each other.

    Args:
        task_id: The task ID
        namespace: Target namespace

    """
    networking_v1 = get_networking_client()

    # Default deny-all policy
    deny_all_policy = client.V1NetworkPolicy(
        metadata=client.V1ObjectMeta(
            name="default-deny-all",
            namespace=namespace,
            labels={
                "app.kubernetes.io/managed-by": "mainloop",
                "mainloop.dev/task-id": task_id,
            },
        ),
        spec=client.V1NetworkPolicySpec(
            pod_selector=client.V1LabelSelector(),
            policy_types=["Ingress", "Egress"],
        ),
    )

    # Allow DNS policy
    allow_dns_policy = client.V1NetworkPolicy(
        metadata=client.V1ObjectMeta(
            name="allow-dns",
            namespace=namespace,
            labels={
                "app.kubernetes.io/managed-by": "mainloop",
                "mainloop.dev/task-id": task_id,
            },
        ),
        spec=client.V1NetworkPolicySpec(
            pod_selector=client.V1LabelSelector(),
            policy_types=["Egress"],
            egress=[
                client.V1NetworkPolicyEgressRule(
                    to=[
                        client.V1NetworkPolicyPeer(
                            namespace_selector=client.V1LabelSelector(
                                match_labels={
                                    "kubernetes.io/metadata.name": "kube-system"
                                }
                            )
                        )
                    ],
                    ports=[client.V1NetworkPolicyPort(protocol="UDP", port=53)],
                )
            ],
        ),
    )

    # Allow internet-only egress (block cluster internal)
    allow_internet_policy = client.V1NetworkPolicy(
        metadata=client.V1ObjectMeta(
            name="allow-internet-only",
            namespace=namespace,
            labels={
                "app.kubernetes.io/managed-by": "mainloop",
                "mainloop.dev/task-id": task_id,
            },
        ),
        spec=client.V1NetworkPolicySpec(
            pod_selector=client.V1LabelSelector(),
            policy_types=["Egress"],
            egress=[
                client.V1NetworkPolicyEgressRule(
                    to=[
                        client.V1NetworkPolicyPeer(
                            ip_block=client.V1IPBlock(
                                cidr="0.0.0.0/0",
                                _except=[
                                    "10.0.0.0/8",  # Private class A
                                    "172.16.0.0/12",  # Private class B
                                    "192.168.0.0/16",  # Private class C
                                    "169.254.0.0/16",  # Link-local
                                    "127.0.0.0/8",  # Loopback
                                ],
                            )
                        )
                    ],
                )
            ],
        ),
    )

    # Apply policies
    for policy in [deny_all_policy, allow_dns_policy, allow_internet_policy]:
        try:
            networking_v1.create_namespaced_network_policy(
                namespace=namespace, body=policy
            )
            logger.info(f"Applied NetworkPolicy {policy.metadata.name} to {namespace}")
        except ApiException as e:
            if e.status == 409:
                logger.info(
                    f"NetworkPolicy {policy.metadata.name} already exists in {namespace}"
                )
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
