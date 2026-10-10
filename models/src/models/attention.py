"""Owner attention is a read projection, independent of activity and read state."""

from datetime import datetime
from typing import Literal

from pydantic import BaseModel, Field

from models.session import Session
from models.workflow import QueueItem


class AttentionItem(BaseModel):
    id: str
    kind: Literal["approval", "question", "owner_decision"]
    title: str
    needed: str
    href: str
    session_ids: list[str] = Field(default_factory=list)
    task_id: str | None = None
    queue_item: QueueItem | None = None


class AttentionSnapshot(BaseModel):
    items: list[AttentionItem] = Field(default_factory=list)
    count: int = 0
    sessions: list[Session] = Field(default_factory=list)


class ActivityItem(BaseModel):
    id: str
    title: str
    content: str
    href: str | None = None
    created_at: datetime
