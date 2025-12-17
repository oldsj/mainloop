"""BigQuery client for conversation persistence."""

from datetime import datetime
from google.cloud import bigquery
from models import Conversation, Message
from mainloop.config import settings


class BigQueryClient:
    """Client for managing conversations in BigQuery."""

    def __init__(self):
        """Initialize BigQuery client."""
        self.client = bigquery.Client(project=settings.google_cloud_project)
        self.dataset = settings.bigquery_dataset

    async def create_conversation(self, user_id: str, title: str | None = None) -> Conversation:
        """Create a new conversation."""
        # TODO: Implement BigQuery insert
        # For now, return a mock conversation
        conv_id = f"conv_{datetime.now().timestamp()}"
        return Conversation(
            id=conv_id,
            user_id=user_id,
            title=title or "New Conversation"
        )

    async def get_conversation(self, conversation_id: str) -> Conversation | None:
        """Get a conversation by ID."""
        # TODO: Implement BigQuery query
        return None

    async def list_conversations(self, user_id: str, limit: int = 50) -> list[Conversation]:
        """List conversations for a user."""
        # TODO: Implement BigQuery query
        return []

    async def create_message(
        self,
        conversation_id: str,
        role: str,
        content: str
    ) -> Message:
        """Create a new message in a conversation."""
        # TODO: Implement BigQuery insert
        msg_id = f"msg_{datetime.now().timestamp()}"
        return Message(
            id=msg_id,
            conversation_id=conversation_id,
            role=role,  # type: ignore
            content=content
        )

    async def get_messages(self, conversation_id: str) -> list[Message]:
        """Get all messages for a conversation."""
        # TODO: Implement BigQuery query
        return []


# Global client instance
bq_client = BigQueryClient()
