#!/usr/bin/env python3
"""
E2E test for the worker task workflow.

Prerequisites:
1. Backend running with DBOS (locally or in k8s)
2. PostgreSQL running
3. K8s cluster accessible (kubectl configured)
4. Claude credentials available
5. GitHub token configured

Usage:
    # From backend directory
    uv run python scripts/test_worker_e2e.py

    # Or with a specific repo
    REPO_URL=https://github.com/youruser/test-repo uv run python scripts/test_worker_e2e.py
"""

import asyncio
import os
import sys
import uuid

# Add src to path
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))

from dbos import DBOS, SetWorkflowID
from mainloop.workflows.dbos_config import dbos_config  # noqa: F401
from mainloop.db import db
from models import WorkerTask, TaskStatus


# Test configuration
TEST_REPO_URL = os.environ.get("REPO_URL", "https://github.com/oldsj/mainloop")
TEST_TASK_DESCRIPTION = os.environ.get(
    "TASK_DESCRIPTION",
    "Create a file called test-worker-output.txt in the root directory with the current timestamp and a message saying 'E2E test successful'"
)


async def run_test():
    """Run the E2E test."""
    print("=" * 60)
    print("Worker Task E2E Test")
    print("=" * 60)
    print(f"Repo URL: {TEST_REPO_URL}")
    print(f"Task: {TEST_TASK_DESCRIPTION[:50]}...")
    print()

    # Connect to database
    await db.connect()
    await db.ensure_tables_exist()

    # Initialize DBOS
    DBOS.launch()

    # Create a test main thread first (required for FK)
    from models import MainThread
    test_user_id = f"test-user-{uuid.uuid4().hex[:8]}"
    main_thread = MainThread(user_id=test_user_id, workflow_run_id="test-workflow")
    main_thread = await db.create_main_thread(main_thread)
    print(f"Created main thread: {main_thread.id}")

    # Create a test task
    task_id = str(uuid.uuid4())
    task = WorkerTask(
        id=task_id,
        main_thread_id=main_thread.id,
        user_id=test_user_id,
        task_type="feature",
        description=TEST_TASK_DESCRIPTION,
        prompt=TEST_TASK_DESCRIPTION,
        repo_url=TEST_REPO_URL,
        status=TaskStatus.PENDING,
    )

    # Save task to database
    task = await db.create_worker_task(task)
    print(f"Created task: {task.id}")

    # Import and start the workflow
    from mainloop.workflows.worker import worker_task_workflow

    print(f"Starting worker workflow...")
    print()
    print("Watch the workflow with:")
    print(f"  kubectl get ns -w | grep task-")
    print(f"  kubectl get jobs -A -w | grep worker")
    print()

    # Start workflow with task_id as workflow ID (for callback routing)
    with SetWorkflowID(task_id):
        result = await worker_task_workflow(task_id)

    print()
    print("=" * 60)
    print("Result:")
    print(f"  Status: {result.get('status')}")
    print(f"  PR URL: {result.get('pr_url', 'N/A')}")
    if result.get("error"):
        print(f"  Error: {result.get('error')}")
    print("=" * 60)

    await db.disconnect()


if __name__ == "__main__":
    asyncio.run(run_test())
