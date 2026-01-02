"""Async conversation compaction service.

Summarizes older messages to manage context window while preserving conversation history.
Runs asynchronously to avoid blocking the main chat flow.
"""

import asyncio
import logging

from claude_agent_sdk import (
    AssistantMessage,
    ClaudeAgentOptions,
    ResultMessage,
    TextBlock,
    query,
)
from mainloop.config import settings
from mainloop.db import db

from models import Message

logger = logging.getLogger(__name__)

# Compaction thresholds
COMPACTION_THRESHOLD = 40  # Trigger compaction when message_count exceeds this
MESSAGES_TO_SUMMARIZE = 30  # Number of oldest messages to summarize
RECENT_MESSAGES_TO_KEEP = 10  # Keep this many recent messages unsummarized


async def summarize_messages(messages: list[Message]) -> str:
    """Use Claude to summarize a list of messages."""
    if not messages:
        return ""

    # Format messages for summarization
    formatted = []
    for msg in messages:
        role = "User" if msg.role == "user" else "Assistant"
        formatted.append(f"{role}: {msg.content}")

    conversation_text = "\n\n".join(formatted)

    prompt = f"""Summarize this conversation concisely, preserving key information:
- Important facts mentioned (names, preferences, decisions)
- Key topics discussed
- Any commitments or action items
- Context needed to continue the conversation naturally

Conversation to summarize:
{conversation_text}

Write a concise summary (2-4 paragraphs) that captures the essential context."""

    try:
        options = ClaudeAgentOptions(
            model=settings.claude_model,  # Use same model as main thread
            permission_mode="bypassPermissions",
        )

        collected_text: list[str] = []
        async for msg in query(prompt=prompt, options=options):
            if isinstance(msg, AssistantMessage):
                for block in msg.content:
                    if isinstance(block, TextBlock):
                        collected_text.append(block.text)
            elif isinstance(msg, ResultMessage):
                if msg.is_error:
                    logger.error(f"Summarization error: {msg.result}")
                    return ""

        return "\n".join(collected_text)
    except Exception as e:
        logger.error(f"Failed to summarize messages: {e}")
        return ""


async def compact_conversation(conversation_id: str) -> None:
    """Compact a conversation by summarizing older messages.

    This runs asynchronously and updates the conversation's summary field.
    """
    try:
        conversation = await db.get_conversation(conversation_id)
        if not conversation:
            logger.warning(f"Conversation {conversation_id} not found for compaction")
            return

        # Check if compaction is needed
        if conversation.message_count < COMPACTION_THRESHOLD:
            return

        logger.info(
            f"Starting compaction for conversation {conversation_id} "
            f"(message_count={conversation.message_count})"
        )

        # Get messages to summarize (oldest ones)
        messages_to_summarize = await db.get_messages_for_compaction(
            conversation_id, MESSAGES_TO_SUMMARIZE
        )

        if not messages_to_summarize:
            return

        # Include existing summary in new summary if present
        existing_summary = conversation.summary or ""

        if existing_summary:
            # Append new messages to existing summary context
            summary_prompt_messages = messages_to_summarize
            existing_context = (
                f"Previous summary:\n{existing_summary}\n\nNew messages to incorporate:"
            )
        else:
            existing_context = ""
            summary_prompt_messages = messages_to_summarize

        # Generate summary
        new_summary = await summarize_messages(summary_prompt_messages)

        if not new_summary:
            logger.warning(
                f"Failed to generate summary for conversation {conversation_id}"
            )
            return

        # Combine with existing summary if present
        if existing_context:
            final_summary = f"{existing_summary}\n\n{new_summary}"
        else:
            final_summary = new_summary

        # Get the ID of the last summarized message
        last_summarized = messages_to_summarize[-1]

        # Update conversation with new summary
        await db.update_conversation_summary(
            conversation_id=conversation_id,
            summary=final_summary,
            summarized_through_id=last_summarized.id,
        )

        logger.info(
            f"Compaction complete for conversation {conversation_id}: "
            f"summarized {len(messages_to_summarize)} messages"
        )

    except Exception as e:
        logger.error(f"Compaction failed for conversation {conversation_id}: {e}")


def trigger_compaction(conversation_id: str, message_count: int) -> None:
    """Fire-and-forget trigger for compaction.

    Checks if compaction is needed and schedules it asynchronously.
    Does not block the calling code.
    """
    if message_count < COMPACTION_THRESHOLD:
        return

    # Schedule compaction as a background task
    asyncio.create_task(compact_conversation(conversation_id))
    logger.debug(f"Scheduled compaction for conversation {conversation_id}")
