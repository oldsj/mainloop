"""Kubernetes Job management for worker tasks."""

import logging
from typing import Literal

from kubernetes import client
from kubernetes.client.rest import ApiException

from mainloop.services.k8s_namespace import get_k8s_client, WORKER_SERVICE_ACCOUNT
from mainloop.config import settings

logger = logging.getLogger(__name__)

# Job configuration
WORKER_IMAGE = "ghcr.io/oldsj/mainloop-agent-controller:latest"
JOB_TTL_SECONDS = 3600  # Keep completed jobs for 1 hour


async def create_worker_job(
    task_id: str,
    namespace: str,
    prompt: str,
    mode: Literal["plan", "implement", "feedback"],
    callback_url: str,
    model: str | None = None,
    repo_url: str | None = None,
    pr_number: int | None = None,
    feedback_context: str | None = None,
    iteration: int = 0,
) -> str:
    """Create a worker Job in the task namespace.

    Args:
        task_id: The task ID
        namespace: Target namespace for the Job
        prompt: The task prompt/description
        mode: "plan" for draft PR with plan, "implement" for code, "feedback" for comments
        callback_url: URL to POST results to
        model: Claude model to use (defaults to settings.claude_worker_model)
        repo_url: Repository URL to clone
        pr_number: PR number (for implement and feedback modes)
        feedback_context: PR comments/feedback to address
        iteration: Iteration number for jobs (ensures unique names)

    Returns:
        The Job name
    """
    _, batch_v1 = get_k8s_client()

    # Include iteration in job name to ensure uniqueness across feedback rounds
    if iteration > 0:
        job_name = f"worker-{task_id[:8]}-{mode[:3]}-{iteration}"
    else:
        job_name = f"worker-{task_id[:8]}-{mode[:3]}"

    # Check if job already exists and is completed - delete it to allow retry
    try:
        existing_job = batch_v1.read_namespaced_job(name=job_name, namespace=namespace)
        status = existing_job.status
        if (status.succeeded and status.succeeded > 0) or (status.failed and status.failed > 0):
            # Job completed, delete it to allow re-creation
            logger.info(f"Deleting completed job {job_name} for retry")
            batch_v1.delete_namespaced_job(
                name=job_name,
                namespace=namespace,
                body=client.V1DeleteOptions(propagation_policy="Background"),
            )
            # Brief wait for deletion to propagate
            import asyncio
            await asyncio.sleep(1)
    except ApiException as e:
        if e.status != 404:
            raise
        # Job doesn't exist, continue with creation
    model = model or settings.claude_worker_model

    # Environment variables for the job
    env_vars = [
        client.V1EnvVar(name="TASK_ID", value=task_id),
        client.V1EnvVar(name="TASK_PROMPT", value=prompt),
        client.V1EnvVar(name="CALLBACK_URL", value=callback_url),
        client.V1EnvVar(name="MODE", value=mode),
        client.V1EnvVar(name="CLAUDE_MODEL", value=model),
        # Claude credentials from secret
        client.V1EnvVar(
            name="CLAUDE_CODE_OAUTH_TOKEN",
            value_from=client.V1EnvVarSource(
                secret_key_ref=client.V1SecretKeySelector(
                    name="mainloop-secrets",
                    key="claude-secret-token",
                )
            ),
        ),
        # GitHub token from secret
        client.V1EnvVar(
            name="GH_TOKEN",
            value_from=client.V1EnvVarSource(
                secret_key_ref=client.V1SecretKeySelector(
                    name="mainloop-secrets",
                    key="github-token",
                    optional=True,
                )
            ),
        ),
    ]

    # Add optional env vars
    if repo_url:
        env_vars.append(client.V1EnvVar(name="REPO_URL", value=repo_url))
    if pr_number:
        env_vars.append(client.V1EnvVar(name="PR_NUMBER", value=str(pr_number)))
    if feedback_context:
        env_vars.append(client.V1EnvVar(name="FEEDBACK_CONTEXT", value=feedback_context))

    # Job spec
    job = client.V1Job(
        metadata=client.V1ObjectMeta(
            name=job_name,
            namespace=namespace,
            labels={
                "app.kubernetes.io/managed-by": "mainloop",
                "mainloop.dev/task-id": task_id,
                "mainloop.dev/mode": mode,
            },
        ),
        spec=client.V1JobSpec(
            ttl_seconds_after_finished=JOB_TTL_SECONDS,
            backoff_limit=0,  # Don't retry failed jobs
            template=client.V1PodTemplateSpec(
                metadata=client.V1ObjectMeta(
                    labels={
                        "app.kubernetes.io/managed-by": "mainloop",
                        "mainloop.dev/task-id": task_id,
                    },
                ),
                spec=client.V1PodSpec(
                    restart_policy="Never",
                    service_account_name=WORKER_SERVICE_ACCOUNT,
                    image_pull_secrets=[
                        client.V1LocalObjectReference(name="ghcr-secret"),
                    ],
                    containers=[
                        client.V1Container(
                            name="claude-agent",
                            image=WORKER_IMAGE,
                            command=["/app/.venv/bin/python", "/app/job_runner.py"],
                            env=env_vars,
                            resources=client.V1ResourceRequirements(
                                requests={"memory": "512Mi", "cpu": "500m"},
                                limits={"memory": "2Gi", "cpu": "2"},
                            ),
                            volume_mounts=[
                                client.V1VolumeMount(
                                    name="workspace",
                                    mount_path="/workspace",
                                ),
                            ],
                        ),
                    ],
                    volumes=[
                        client.V1Volume(
                            name="workspace",
                            empty_dir=client.V1EmptyDirVolumeSource(),
                        ),
                    ],
                ),
            ),
        ),
    )

    try:
        batch_v1.create_namespaced_job(namespace=namespace, body=job)
        logger.info(f"Created job {job_name} in namespace {namespace}")
    except ApiException as e:
        if e.status == 409:
            logger.info(f"Job {job_name} already exists in {namespace}")
        else:
            raise

    return job_name


async def get_job_status(task_id: str, namespace: str) -> dict | None:
    """Get the status of a worker Job.

    Args:
        task_id: The task ID
        namespace: Namespace where the Job is running

    Returns:
        Job status dict or None if not found
    """
    _, batch_v1 = get_k8s_client()

    try:
        jobs = batch_v1.list_namespaced_job(
            namespace=namespace,
            label_selector=f"mainloop.dev/task-id={task_id}",
        )

        if not jobs.items:
            return None

        job = jobs.items[0]
        status = job.status

        return {
            "name": job.metadata.name,
            "active": status.active or 0,
            "succeeded": status.succeeded or 0,
            "failed": status.failed or 0,
            "start_time": status.start_time.isoformat() if status.start_time else None,
            "completion_time": status.completion_time.isoformat() if status.completion_time else None,
        }

    except ApiException as e:
        if e.status == 404:
            return None
        raise


async def delete_job(task_id: str, namespace: str) -> None:
    """Delete a worker Job.

    Args:
        task_id: The task ID
        namespace: Namespace where the Job is running
    """
    _, batch_v1 = get_k8s_client()

    try:
        jobs = batch_v1.list_namespaced_job(
            namespace=namespace,
            label_selector=f"mainloop.dev/task-id={task_id}",
        )

        for job in jobs.items:
            batch_v1.delete_namespaced_job(
                name=job.metadata.name,
                namespace=namespace,
                body=client.V1DeleteOptions(
                    propagation_policy="Background",
                ),
            )
            logger.info(f"Deleted job {job.metadata.name} from namespace {namespace}")

    except ApiException as e:
        if e.status == 404:
            logger.info(f"No jobs found for task {task_id} in {namespace}")
        else:
            raise


async def get_job_logs(task_id: str, namespace: str) -> str | None:
    """Get logs from a worker Job's pod.

    Args:
        task_id: The task ID
        namespace: Namespace where the Job is running

    Returns:
        Pod logs as string or None if not found
    """
    core_v1, _ = get_k8s_client()

    try:
        pods = core_v1.list_namespaced_pod(
            namespace=namespace,
            label_selector=f"mainloop.dev/task-id={task_id}",
        )

        if not pods.items:
            return None

        pod = pods.items[0]
        logs = core_v1.read_namespaced_pod_log(
            name=pod.metadata.name,
            namespace=namespace,
            container="claude-agent",
        )

        return logs

    except ApiException as e:
        if e.status == 404:
            return None
        raise
