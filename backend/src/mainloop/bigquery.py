"""BigQuery client for conversation persistence."""

import asyncio
import uuid
from datetime import datetime, timezone
from google.cloud import bigquery
from google.cloud.exceptions import NotFound
from models import Conversation, Message
from mainloop.config import settings


class BigQueryClient:
    """Client for managing conversations in BigQuery."""

    def __init__(self):
        """Initialize BigQuery client."""
        self.client = bigquery.Client(project=settings.google_cloud_project) if settings.google_cloud_project else None
        self.dataset_id = settings.bigquery_dataset
        self.conversations_table = "conversations"
        self.messages_table = "messages"

    def _get_table_ref(self, table_name: str) -> str:
        """Get fully qualified table reference."""
        return f"{settings.google_cloud_project}.{self.dataset_id}.{table_name}"

    async def ensure_tables_exist(self):
        """Create tables if they don't exist."""
        if not self.client:
            return  # Skip if no GCP project configured (local dev)

        await asyncio.to_thread(self._create_tables_sync)

    def _create_tables_sync(self):
        """Create BigQuery tables (synchronous)."""
        # Create dataset if it doesn't exist
        dataset_ref = f"{settings.google_cloud_project}.{self.dataset_id}"
        try:
            self.client.get_dataset(dataset_ref)
        except NotFound:
            dataset = bigquery.Dataset(dataset_ref)
            dataset.location = "US"
            self.client.create_dataset(dataset)

        # Create conversations table
        conversations_schema = [
            bigquery.SchemaField("id", "STRING", mode="REQUIRED"),
            bigquery.SchemaField("user_id", "STRING", mode="REQUIRED"),
            bigquery.SchemaField("title", "STRING", mode="NULLABLE"),
            bigquery.SchemaField("created_at", "TIMESTAMP", mode="REQUIRED"),
            bigquery.SchemaField("updated_at", "TIMESTAMP", mode="REQUIRED"),
        ]
        self._create_table_if_not_exists(self.conversations_table, conversations_schema)

        # Create messages table
        messages_schema = [
            bigquery.SchemaField("id", "STRING", mode="REQUIRED"),
            bigquery.SchemaField("conversation_id", "STRING", mode="REQUIRED"),
            bigquery.SchemaField("role", "STRING", mode="REQUIRED"),
            bigquery.SchemaField("content", "STRING", mode="REQUIRED"),
            bigquery.SchemaField("created_at", "TIMESTAMP", mode="REQUIRED"),
        ]
        self._create_table_if_not_exists(self.messages_table, messages_schema)

    def _create_table_if_not_exists(self, table_name: str, schema: list):
        """Create table if it doesn't exist."""
        table_ref = self._get_table_ref(table_name)
        try:
            self.client.get_table(table_ref)
        except NotFound:
            table = bigquery.Table(table_ref, schema=schema)
            self.client.create_table(table)

    async def create_conversation(self, user_id: str, title: str | None = None) -> Conversation:
        """Create a new conversation."""
        conversation = Conversation(
            id=str(uuid.uuid4()),
            user_id=user_id,
            title=title or "New Conversation",
            created_at=datetime.now(timezone.utc),
            updated_at=datetime.now(timezone.utc)
        )

        if self.client:
            await asyncio.to_thread(self._insert_conversation_sync, conversation)

        return conversation

    def _insert_conversation_sync(self, conversation: Conversation):
        """Insert conversation into BigQuery (synchronous)."""
        table_ref = self._get_table_ref(self.conversations_table)
        rows_to_insert = [{
            "id": conversation.id,
            "user_id": conversation.user_id,
            "title": conversation.title,
            "created_at": conversation.created_at.isoformat(),
            "updated_at": conversation.updated_at.isoformat(),
        }]
        self.client.insert_rows_json(table_ref, rows_to_insert)

    async def get_conversation(self, conversation_id: str) -> Conversation | None:
        """Get a conversation by ID."""
        if not self.client:
            return None

        query = f"""
            SELECT id, user_id, title, created_at, updated_at
            FROM `{self._get_table_ref(self.conversations_table)}`
            WHERE id = @conversation_id
            LIMIT 1
        """
        job_config = bigquery.QueryJobConfig(
            query_parameters=[
                bigquery.ScalarQueryParameter("conversation_id", "STRING", conversation_id)
            ]
        )

        results = await asyncio.to_thread(
            lambda: list(self.client.query(query, job_config=job_config).result())
        )

        if not results:
            return None

        row = results[0]
        return Conversation(
            id=row.id,
            user_id=row.user_id,
            title=row.title,
            created_at=row.created_at,
            updated_at=row.updated_at
        )

    async def list_conversations(self, user_id: str, limit: int = 50) -> list[Conversation]:
        """List conversations for a user."""
        if not self.client:
            return []

        query = f"""
            SELECT id, user_id, title, created_at, updated_at
            FROM `{self._get_table_ref(self.conversations_table)}`
            WHERE user_id = @user_id
            ORDER BY updated_at DESC
            LIMIT @limit
        """
        job_config = bigquery.QueryJobConfig(
            query_parameters=[
                bigquery.ScalarQueryParameter("user_id", "STRING", user_id),
                bigquery.ScalarQueryParameter("limit", "INT64", limit)
            ]
        )

        results = await asyncio.to_thread(
            lambda: list(self.client.query(query, job_config=job_config).result())
        )

        return [
            Conversation(
                id=row.id,
                user_id=row.user_id,
                title=row.title,
                created_at=row.created_at,
                updated_at=row.updated_at
            )
            for row in results
        ]

    async def create_message(
        self,
        conversation_id: str,
        role: str,
        content: str
    ) -> Message:
        """Create a new message in a conversation."""
        message = Message(
            id=str(uuid.uuid4()),
            conversation_id=conversation_id,
            role=role,  # type: ignore
            content=content,
            created_at=datetime.now(timezone.utc)
        )

        if self.client:
            await asyncio.to_thread(self._insert_message_sync, message)

        return message

    def _insert_message_sync(self, message: Message):
        """Insert message into BigQuery (synchronous)."""
        table_ref = self._get_table_ref(self.messages_table)
        rows_to_insert = [{
            "id": message.id,
            "conversation_id": message.conversation_id,
            "role": message.role,
            "content": message.content,
            "created_at": message.created_at.isoformat(),
        }]
        self.client.insert_rows_json(table_ref, rows_to_insert)

    async def get_messages(self, conversation_id: str) -> list[Message]:
        """Get all messages for a conversation."""
        if not self.client:
            return []

        query = f"""
            SELECT id, conversation_id, role, content, created_at
            FROM `{self._get_table_ref(self.messages_table)}`
            WHERE conversation_id = @conversation_id
            ORDER BY created_at ASC
        """
        job_config = bigquery.QueryJobConfig(
            query_parameters=[
                bigquery.ScalarQueryParameter("conversation_id", "STRING", conversation_id)
            ]
        )

        results = await asyncio.to_thread(
            lambda: list(self.client.query(query, job_config=job_config).result())
        )

        return [
            Message(
                id=row.id,
                conversation_id=row.conversation_id,
                role=row.role,  # type: ignore
                content=row.content,
                created_at=row.created_at
            )
            for row in results
        ]


# Global client instance
bq_client = BigQueryClient()
