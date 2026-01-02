#!/usr/bin/env python3
"""
Test K8s namespace and job creation directly.

This tests the K8s integration without running the full DBOS workflow.
Useful for debugging K8s issues.

Prerequisites:
1. kubectl configured with cluster access
2. RBAC manifests applied (make k8s-apply)

Usage:
    cd backend
    uv run python scripts/test_k8s_components.py
"""

import asyncio
import os
import sys
import uuid

# Add src to path
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))


async def test_namespace():
    """Test namespace creation and deletion."""
    from mainloop.services.k8s_namespace import (
        copy_secrets_to_namespace,
        create_task_namespace,
        delete_task_namespace,
        setup_worker_rbac,
    )

    task_id = f"test-{uuid.uuid4().hex[:8]}"

    print(f"Testing with task_id: {task_id}")
    print()

    # Test 1: Create namespace
    print("1. Creating namespace...")
    try:
        namespace = await create_task_namespace(task_id)
        print(f"   ✓ Created namespace: {namespace}")
    except Exception as e:
        print(f"   ✗ Failed: {e}")
        return

    # Test 2: Copy secrets
    print("2. Copying secrets...")
    try:
        await copy_secrets_to_namespace(task_id, namespace)
        print("   ✓ Secrets copied")
    except Exception as e:
        print(f"   ✗ Failed: {e}")

    # Test 3: Setup worker RBAC
    print("3. Setting up worker RBAC...")
    try:
        await setup_worker_rbac(task_id, namespace)
        print("   ✓ Worker RBAC configured")
    except Exception as e:
        print(f"   ✗ Failed: {e}")

    # Test 4: Verify namespace exists
    print("4. Verifying namespace...")
    from mainloop.services.k8s_namespace import get_k8s_client

    core_v1, _ = get_k8s_client()
    try:
        ns = core_v1.read_namespace(namespace)
        print(f"   ✓ Namespace exists with status: {ns.status.phase}")
    except Exception as e:
        print(f"   ✗ Failed: {e}")

    # Test 5: List secrets in namespace
    print("5. Checking secrets...")
    try:
        secrets = core_v1.list_namespaced_secret(namespace)
        secret_names = [
            s.metadata.name
            for s in secrets.items
            if not s.metadata.name.startswith("default")
        ]
        print(f"   ✓ Found secrets: {secret_names}")
    except Exception as e:
        print(f"   ✗ Failed: {e}")

    # Test 6: Delete namespace
    print("6. Deleting namespace...")
    try:
        await delete_task_namespace(task_id)
        print("   ✓ Deletion initiated")
    except Exception as e:
        print(f"   ✗ Failed: {e}")

    print()
    print("Done! Check with: kubectl get ns | grep task-")


async def test_job():
    """Test job creation in a namespace."""
    from mainloop.services.k8s_jobs import (
        create_worker_job,
        get_job_status,
    )
    from mainloop.services.k8s_namespace import (
        copy_secrets_to_namespace,
        create_task_namespace,
        delete_task_namespace,
        setup_worker_rbac,
    )

    task_id = f"test-{uuid.uuid4().hex[:8]}"

    print(f"Testing job with task_id: {task_id}")
    print()

    # Setup namespace
    print("1. Setting up namespace...")
    namespace = await create_task_namespace(task_id)
    await copy_secrets_to_namespace(task_id, namespace)
    await setup_worker_rbac(task_id, namespace)
    print(f"   ✓ Namespace ready: {namespace}")

    # Create job
    print("2. Creating job...")
    try:
        job_name = await create_worker_job(
            task_id=task_id,
            namespace=namespace,
            prompt="Echo 'Hello from test job' and exit",
            mode="initial",
            callback_url="",  # No callback for test
        )
        print(f"   ✓ Created job: {job_name}")
    except Exception as e:
        print(f"   ✗ Failed: {e}")
        await delete_task_namespace(task_id)
        return

    # Wait a bit and check status
    print("3. Waiting for job to start...")
    await asyncio.sleep(5)

    status = await get_job_status(task_id, namespace)
    if status:
        print(
            f"   Active: {status['active']}, Succeeded: {status['succeeded']}, Failed: {status['failed']}"
        )
    else:
        print("   No status yet")

    # Show how to watch
    print()
    print("Watch the job with:")
    print(f"  kubectl get pods -n {namespace} -w")
    print(f"  kubectl logs -n {namespace} -l mainloop.dev/task-id={task_id} -f")
    print()
    print("Cleanup with:")
    print(f"  kubectl delete ns {namespace}")


async def main():
    """Parse arguments and run tests."""
    import argparse

    parser = argparse.ArgumentParser(description="Test K8s components")
    parser.add_argument("--job", action="store_true", help="Test job creation (slower)")
    args = parser.parse_args()

    if args.job:
        await test_job()
    else:
        await test_namespace()


if __name__ == "__main__":
    asyncio.run(main())
