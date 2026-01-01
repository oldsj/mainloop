"""DBOS configuration and initialization."""

import os
from dbos import DBOS, DBOSConfig, Queue

from mainloop.config import settings

# DBOS configuration
# application_version prevents recovery of old workflows after code changes
# Bump this when workflow step order/logic changes to avoid DBOSUnexpectedStepError
WORKFLOW_VERSION = "7"  # v7: Fix PR URL extraction after implement job (was only in skip_plan mode)

dbos_config: DBOSConfig = {
    "name": "mainloop",
    "system_database_url": settings.database_url or os.environ.get("DBOS_SYSTEM_DATABASE_URL"),
    "application_version": WORKFLOW_VERSION,
}

# Initialize DBOS - must be done before defining workflows
DBOS(config=dbos_config)

# Queue for worker tasks with concurrency limit
# This ensures we don't overwhelm resources with too many concurrent workers
worker_queue = Queue(
    "worker_tasks",
    concurrency=3,  # Max 3 workers running at once globally
)

# Queue for user main threads - one at a time per partition (user)
main_thread_queue = Queue(
    "main_threads",
    partition_queue=True,  # Partition by user_id
    concurrency=1,  # One active main thread per user at a time
)
