"""Main thread workflow - per-user conversation orchestrator using DBOS."""

import logging
from typing import Any

from dbos import DBOS, SetWorkflowID, SetEnqueueOptions

from models import (
    MainThread,
    WorkerTask,
    QueueItem,
    QueueItemType,
    QueueItemPriority,
    TaskStatus,
)
from mainloop.db import db
from mainloop.workflows.dbos_config import worker_queue

logger = logging.getLogger(__name__)

# Message topics for workflow communication
TOPIC_USER_MESSAGE = "user_message"
TOPIC_QUEUE_RESPONSE = "queue_response"
TOPIC_WORKER_RESULT = "worker_result"


@DBOS.step()
async def save_main_thread(thread: MainThread) -> MainThread:
    """Save main thread to database."""
    return await db.create_main_thread(thread)


@DBOS.step()
async def get_main_thread_by_user(user_id: str) -> MainThread | None:
    """Get main thread for user."""
    return await db.get_main_thread_by_user(user_id)


@DBOS.step()
async def save_queue_item(item: QueueItem) -> QueueItem:
    """Save queue item to database."""
    return await db.create_queue_item(item)


@DBOS.step()
async def save_assistant_message(conversation_id: str, content: str) -> None:
    """Save an assistant message to a conversation."""
    await db.create_message(conversation_id, "assistant", content)


@DBOS.step()
async def update_queue_item_response(item_id: str, response: str) -> None:
    """Update queue item with response."""
    await db.update_queue_item(item_id, status="responded", response=response)


@DBOS.step()
async def save_worker_task(task: WorkerTask) -> WorkerTask:
    """Save worker task to database."""
    return await db.create_worker_task(task)


@DBOS.step()
async def update_task_status(
    task_id: str,
    status: TaskStatus,
    result: dict | None = None,
    error: str | None = None,
    pr_url: str | None = None,
) -> None:
    """Update worker task status."""
    await db.update_worker_task(
        task_id,
        status=status,
        result=result,
        error=error,
        pr_url=pr_url,
    )


@DBOS.workflow()
async def main_thread_workflow(user_id: str) -> None:
    """
    Main thread workflow for a user.

    This workflow runs as long as needed, processing user messages
    and coordinating worker agents. It uses DBOS.recv() to wait for
    messages from the user or from workers.

    The workflow is started per-user and identified by user_id.
    """
    logger.info(f"Starting main thread for user {user_id}")

    # Create or get existing main thread record
    existing = await get_main_thread_by_user(user_id)
    if existing:
        thread = existing
    else:
        thread = MainThread(user_id=user_id, workflow_run_id=DBOS.workflow_id)
        thread = await save_main_thread(thread)

    # Main event loop - wait for messages
    while True:
        # Wait for any message (user input, queue response, or worker result)
        # Timeout after 1 hour - workflow will be recovered and continue
        message = await DBOS.recv_async(timeout_seconds=3600)

        if message is None:
            # Timeout - just continue waiting
            logger.debug(f"Main thread {user_id} heartbeat")
            continue

        try:
            msg_type = message.get("type")
            payload = message.get("payload", {})

            if msg_type == TOPIC_USER_MESSAGE:
                await handle_user_message(thread, payload)
            elif msg_type == TOPIC_QUEUE_RESPONSE:
                await handle_queue_response(thread, payload)
            elif msg_type == TOPIC_WORKER_RESULT:
                await handle_worker_result(thread, payload)
            else:
                logger.warning(f"Unknown message type: {msg_type}")

        except Exception as e:
            logger.error(f"Error processing message: {e}")
            # Add error to queue for human review
            await add_to_queue(
                thread,
                item_type=QueueItemType.ERROR,
                title="Error processing message",
                content=str(e),
                priority=QueueItemPriority.HIGH,
            )


async def handle_user_message(thread: MainThread, payload: dict) -> None:
    """Process a user message with smart routing."""
    message = payload.get("message", "")
    conversation_id = payload.get("conversation_id")
    message_id = payload.get("message_id")
    skip_routing = payload.get("skip_routing", False)

    logger.info(f"User message: {message[:100]}...")

    # Import routing service
    from mainloop.services.task_router import (
        find_matching_tasks,
        extract_keywords,
        should_skip_plan,
    )

    # Check for routing to existing tasks (unless we've already processed this)
    if not skip_routing:
        matches = await find_matching_tasks(thread.user_id, message)

        if matches:
            best_match = matches[0]

            if best_match.confidence >= 0.7:
                # High confidence - suggest routing with confirmation
                logger.info(f"High confidence match ({best_match.confidence:.2f}): {best_match.task.id}")

                # Save response to conversation for chat UI
                response_content = f"This looks related to an existing task: {best_match.task.description[:100]}. Check your inbox to confirm routing."
                if conversation_id:
                    await save_assistant_message(conversation_id, response_content)

                await add_to_queue(
                    thread,
                    item_type=QueueItemType.ROUTING_SUGGESTION,
                    title="Route to existing task?",
                    content=f"This looks related to: {best_match.task.description[:100]}",
                    task_id=best_match.task.id,
                    priority=QueueItemPriority.HIGH,
                    options=["Route to this task", "Create new task"],
                    context={
                        "suggested_task_id": best_match.task.id,
                        "confidence": best_match.confidence,
                        "match_reasons": best_match.match_reasons,
                        "original_message": message,
                        "conversation_id": conversation_id,
                        "message_id": message_id,
                    },
                )
                return
            elif len(matches) > 1 and matches[0].confidence >= 0.4:
                # Multiple possible matches - ask user to choose
                logger.info(f"Multiple matches found, asking user to choose")
                task_options = [f"{m.task.description[:50]}..." for m in matches[:3]]
                task_options.append("Create new task")

                # Save response to conversation for chat UI
                response_content = f"Multiple active tasks might match. Check your inbox to choose one."
                if conversation_id:
                    await save_assistant_message(conversation_id, response_content)

                await add_to_queue(
                    thread,
                    item_type=QueueItemType.ROUTING_SUGGESTION,
                    title="Which task?",
                    content=f"Multiple active tasks might match: {message[:100]}",
                    priority=QueueItemPriority.HIGH,
                    options=task_options,
                    context={
                        "matches": [
                            {"task_id": m.task.id, "confidence": m.confidence}
                            for m in matches[:3]
                        ],
                        "original_message": message,
                        "conversation_id": conversation_id,
                        "message_id": message_id,
                    },
                )
                return

    # No match or low confidence - check if message needs worker
    needs_worker = any(
        keyword in message.lower()
        for keyword in ["build", "fix", "create", "update", "implement", "add", "remove", "change"]
    )

    if needs_worker:
        # Extract keywords for future routing
        keywords = extract_keywords(message)
        skip_plan = should_skip_plan(message)

        task = WorkerTask(
            main_thread_id=thread.id,
            user_id=thread.user_id,
            task_type="feature",
            description=message,
            prompt=message,
            status=TaskStatus.PENDING,
            conversation_id=conversation_id,
            message_id=message_id,
            keywords=keywords,
            skip_plan=skip_plan,
        )
        task = await save_worker_task(task)

        # Enqueue the worker task
        from mainloop.workflows.worker import worker_task_workflow
        with SetWorkflowID(task.id):
            worker_queue.enqueue(worker_task_workflow, task.id)

        logger.info(f"Spawned worker task: {task.id} (skip_plan={skip_plan})")

        # Create response message
        response_content = f"I'm working on: {task.description[:100]}. I'll update you when I have progress."

        # Save assistant message to conversation for chat UI
        if conversation_id:
            await save_assistant_message(conversation_id, response_content)

        # Add acknowledgment to queue (for inbox)
        await add_to_queue(
            thread,
            task_id=task.id,
            item_type=QueueItemType.NOTIFICATION,
            title="Task started",
            content=response_content,
            priority=QueueItemPriority.LOW,
        )
    else:
        # Direct response (will use Claude for real responses)
        response_content = f"I received your message: {message}. For now I can only spawn workers for tasks that involve building, fixing, or implementing something."

        # Save assistant message to conversation for chat UI
        if conversation_id:
            await save_assistant_message(conversation_id, response_content)

        await add_to_queue(
            thread,
            item_type=QueueItemType.NOTIFICATION,
            title="Response",
            content=response_content,
            priority=QueueItemPriority.NORMAL,
        )


async def handle_queue_response(thread: MainThread, payload: dict) -> None:
    """Handle a human response to a queue item."""
    queue_item_id = payload.get("queue_item_id")
    response = payload.get("response")
    task_id = payload.get("task_id")
    item_context = payload.get("context", {})
    item_type = payload.get("item_type")

    logger.info(f"Queue response for {queue_item_id}: {response}")

    # Update the queue item
    await update_queue_item_response(queue_item_id, response)

    # Handle routing suggestions
    if item_type == QueueItemType.ROUTING_SUGGESTION.value:
        original_message = item_context.get("original_message", "")
        conversation_id = item_context.get("conversation_id")
        message_id = item_context.get("message_id")

        if response == "Route to this task":
            # User chose to route to existing task
            target_task_id = item_context.get("suggested_task_id")
            if target_task_id:
                # Send the message as additional context to the worker
                DBOS.send(
                    target_task_id,
                    {
                        "type": "additional_context",
                        "message": original_message,
                        "conversation_id": conversation_id,
                    },
                )

                await add_to_queue(
                    thread,
                    task_id=target_task_id,
                    item_type=QueueItemType.NOTIFICATION,
                    title="Message routed",
                    content=f"Added context to task: {original_message[:50]}...",
                    priority=QueueItemPriority.LOW,
                )
        elif response == "Create new task":
            # User wants a new task - process as new message
            await handle_user_message(
                thread,
                {
                    "message": original_message,
                    "conversation_id": conversation_id,
                    "message_id": message_id,
                    "skip_routing": True,  # Prevent infinite loop
                },
            )
        elif item_context.get("matches"):
            # User selected from multiple matches
            try:
                # Response might be the task description or an index
                matches = item_context["matches"]
                for i, match in enumerate(matches):
                    if response.startswith(f"{i+1}.") or response == match.get("task_id"):
                        target_task_id = match["task_id"]
                        DBOS.send(
                            target_task_id,
                            {
                                "type": "additional_context",
                                "message": original_message,
                            },
                        )
                        await add_to_queue(
                            thread,
                            task_id=target_task_id,
                            item_type=QueueItemType.NOTIFICATION,
                            title="Message routed",
                            content=f"Added context to selected task",
                            priority=QueueItemPriority.LOW,
                        )
                        break
            except Exception as e:
                logger.error(f"Error routing to selected task: {e}")
        return

    # If this was for a worker task, forward the response
    if task_id:
        # Send message to the worker workflow
        DBOS.send(task_id, {"type": "human_response", "response": response})


async def handle_worker_result(thread: MainThread, payload: dict) -> None:
    """Handle a result from a worker task."""
    task_id = payload.get("task_id")
    status = payload.get("status")
    result = payload.get("result", {})
    error = payload.get("error")

    logger.info(f"Worker result for {task_id}: {status}")

    if status == "plan_ready":
        # Plan draft PR is ready for review
        pr_url = result.get("pr_url")
        await add_to_queue(
            thread,
            task_id=task_id,
            item_type=QueueItemType.PLAN_READY,
            title="Plan ready for review",
            content=result.get("message", f"Draft PR created: {pr_url}"),
            priority=QueueItemPriority.HIGH,
            context={"pr_url": pr_url},
        )

    elif status == "plan_updated":
        # Plan was revised based on feedback
        pr_url = result.get("pr_url")
        await add_to_queue(
            thread,
            task_id=task_id,
            item_type=QueueItemType.NOTIFICATION,
            title="Plan updated",
            content=f"Plan revised based on your feedback: {pr_url}",
            priority=QueueItemPriority.NORMAL,
            context={"pr_url": pr_url},
        )

    elif status == "code_ready":
        # Code is ready for review
        pr_url = result.get("pr_url")
        await add_to_queue(
            thread,
            task_id=task_id,
            item_type=QueueItemType.CODE_READY,
            title="Code ready for review",
            content=result.get("message", f"Implementation complete: {pr_url}"),
            priority=QueueItemPriority.HIGH,
            context={"pr_url": pr_url},
        )

    elif status == "feedback_addressed":
        # Worker addressed PR feedback
        pr_url = result.get("pr_url")
        await add_to_queue(
            thread,
            task_id=task_id,
            item_type=QueueItemType.FEEDBACK_ADDRESSED,
            title="Feedback addressed",
            content=f"Changes pushed to address your feedback: {pr_url}",
            priority=QueueItemPriority.NORMAL,
            context={"pr_url": pr_url},
        )

    elif status == "completed":
        await update_task_status(
            task_id,
            TaskStatus.COMPLETED,
            result=result,
            pr_url=result.get("pr_url"),
        )

        # Add completion notification
        pr_url = result.get("pr_url")
        merged = result.get("merged", False)
        if pr_url and merged:
            await add_to_queue(
                thread,
                task_id=task_id,
                item_type=QueueItemType.NOTIFICATION,
                title="PR merged",
                content=f"Pull request merged: {pr_url}",
                priority=QueueItemPriority.NORMAL,
                context={"pr_url": pr_url, "merged": True},
            )
        elif pr_url:
            await add_to_queue(
                thread,
                task_id=task_id,
                item_type=QueueItemType.NOTIFICATION,
                title="Task completed",
                content=f"Task completed: {pr_url}",
                priority=QueueItemPriority.NORMAL,
                context={"pr_url": pr_url},
            )
        else:
            await add_to_queue(
                thread,
                task_id=task_id,
                item_type=QueueItemType.NOTIFICATION,
                title="Task completed",
                content=result.get("summary", "Task completed successfully"),
                priority=QueueItemPriority.NORMAL,
            )

    elif status == "cancelled":
        await update_task_status(task_id, TaskStatus.CANCELLED)

        pr_url = result.get("pr_url")
        await add_to_queue(
            thread,
            task_id=task_id,
            item_type=QueueItemType.NOTIFICATION,
            title="Task cancelled",
            content=f"PR was closed: {pr_url}" if pr_url else "Task was cancelled",
            priority=QueueItemPriority.NORMAL,
            context={"pr_url": pr_url} if pr_url else {},
        )

    elif status == "failed":
        await update_task_status(task_id, TaskStatus.FAILED, error=error)

        await add_to_queue(
            thread,
            task_id=task_id,
            item_type=QueueItemType.ERROR,
            title="Task failed",
            content=f"Error: {error}",
            priority=QueueItemPriority.URGENT,
            options=["Retry", "Cancel"],
        )

    elif status == "needs_input":
        await update_task_status(task_id, TaskStatus.UNDER_REVIEW)

        question = result.get("question", "The worker needs your input.")
        options = result.get("options")

        await add_to_queue(
            thread,
            task_id=task_id,
            item_type=QueueItemType.QUESTION,
            title="Worker needs input",
            content=question,
            priority=QueueItemPriority.HIGH,
            options=options,
        )


async def add_to_queue(
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
    return await save_queue_item(item)


def get_or_start_main_thread(user_id: str) -> str:
    """Get existing main thread workflow or start a new one.

    Returns the workflow_id.
    """
    # Use user_id as the workflow ID for idempotency
    # This ensures only one main thread per user
    with SetWorkflowID(f"main-thread-{user_id}"):
        handle = DBOS.start_workflow(main_thread_workflow, user_id)
        return handle.get_workflow_id()


def send_user_message(user_id: str, message: str, conversation_id: str | None = None) -> None:
    """Send a user message to the main thread workflow."""
    workflow_id = f"main-thread-{user_id}"
    DBOS.send(
        workflow_id,
        {
            "type": TOPIC_USER_MESSAGE,
            "payload": {
                "message": message,
                "conversation_id": conversation_id,
            },
        },
    )


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
