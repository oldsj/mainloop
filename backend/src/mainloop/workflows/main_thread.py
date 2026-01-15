"""Main thread workflow - per-user conversation orchestrator using DBOS."""

import logging

from dbos import DBOS, SetWorkflowID
from mainloop.workflows.transactions import (
    get_main_thread_by_user,
    save_main_thread,
    save_queue_item,
    update_queue_item_response,
)

from models import (
    MainThread,
    QueueItem,
    QueueItemPriority,
    QueueItemType,
)

logger = logging.getLogger(__name__)

# Message topics for workflow communication
TOPIC_USER_MESSAGE = "user_message"
TOPIC_QUEUE_RESPONSE = "queue_response"


@DBOS.workflow()
async def main_thread_workflow(user_id: str) -> None:
    """Run the main thread workflow for a user.

    This workflow runs as long as needed, processing queue responses.
    Chat messages are handled directly by chat_handler with Claude Agent SDK.

    The workflow is started per-user and identified by user_id.
    """
    logger.info(f"Starting main thread for user {user_id}")

    # Create or get existing main thread record
    existing = get_main_thread_by_user(user_id)
    if existing:
        thread = existing
    else:
        thread = MainThread(user_id=user_id, workflow_run_id=DBOS.workflow_id)
        thread = save_main_thread(thread)

    # Main event loop - wait for messages
    while True:
        # Wait for any message (queue responses)
        # Timeout after 1 hour - workflow will be recovered and continue
        message = await DBOS.recv_async(timeout_seconds=3600)

        if message is None:
            # Timeout - just continue waiting
            logger.debug(f"Main thread {user_id} heartbeat")
            continue

        try:
            msg_type = message.get("type")
            payload = message.get("payload", {})

            if msg_type == TOPIC_QUEUE_RESPONSE:
                await handle_queue_response(thread, payload)
            else:
                logger.warning(f"Unknown message type: {msg_type}")

        except Exception as e:
            logger.error(f"Error processing message: {e}")
            # Add error to queue for human review
            add_to_queue(
                thread,
                item_type=QueueItemType.ERROR,
                title="Error processing message",
                content=str(e),
                priority=QueueItemPriority.HIGH,
            )


async def handle_queue_response(thread: MainThread, payload: dict) -> None:
    """Handle a human response to a queue item."""
    queue_item_id = payload.get("queue_item_id")
    response = payload.get("response")

    logger.info(f"Queue response for {queue_item_id}: {response}")

    # Update the queue item
    update_queue_item_response(queue_item_id, response)


def add_to_queue(
    thread: MainThread,
    item_type: QueueItemType,
    title: str,
    content: str,
    task_id: str | None = None,
    priority: QueueItemPriority = QueueItemPriority.NORMAL,
    options: list[str] | None = None,
    context: dict | None = None,
) -> QueueItem:
    """Add an item to the human queue."""
    item = QueueItem(
        main_thread_id=thread.id,
        task_id=task_id,
        user_id=thread.user_id,
        item_type=item_type,
        priority=priority,
        title=title,
        content=content,
        options=options,
        context=context or {},
    )
    return save_queue_item(item)


def get_or_start_main_thread(user_id: str) -> str:
    """Get existing main thread workflow or start a new one.

    Returns the workflow_id.
    """
    # Use user_id as the workflow ID for idempotency
    # This ensures only one main thread per user
    with SetWorkflowID(f"main-thread-{user_id}"):
        handle = DBOS.start_workflow(main_thread_workflow, user_id)
        return handle.get_workflow_id()


def send_queue_response(
    user_id: str,
    queue_item_id: str,
    response: str,
    task_id: str | None = None,
) -> None:
    """Send a queue response to the main thread workflow."""
    workflow_id = f"main-thread-{user_id}"
    DBOS.send(
        workflow_id,
        {
            "type": TOPIC_QUEUE_RESPONSE,
            "payload": {
                "queue_item_id": queue_item_id,
                "response": response,
                "task_id": task_id,
            },
        },
    )
