"""Shared Pydantic models for mainloop."""

from models.agent import AgentResponse, AgentTask
from models.conversation import Conversation, Message
from models.native_agent import (
    AttentionItem,
    AttentionRequest,
    CapabilityResult,
    CapabilityState,
    Checkpoint,
    DeliveryAttempt,
    DeliveryState,
    MessageEnvelope,
    NativeBinding,
    NativeEvent,
    NativeStatus,
    ProviderExtension,
    ReconciliationEvidence,
    WorkspaceBinding,
)
from models.session import (
    NativeDeliveryInfo,
    NativeSessionInfo,
    Session,
    SessionCreate,
    SessionNotification,
    SessionStatus,
)
from models.workflow import (
    EventTypes,
    GitHubPR,
    GitHubRepo,
    MainThread,
    Project,
    QueueItem,
    QueueItemPriority,
    QueueItemResponse,
    QueueItemType,
    WorkflowEvent,
)
from models.workspace import (
    WorkspaceAgentKind,
    WorkspaceCondition,
    WorkspaceConditionStatus,
    WorkspaceDesiredState,
    WorkspaceLifecycle,
    WorkspaceManifest,
    WorkspaceObservedState,
    WorkspaceTransition,
)

__all__ = [
    # Existing
    "Conversation",
    "Message",
    "AgentTask",
    "AgentResponse",
    # Session models
    "Session",
    "SessionCreate",
    "NativeSessionInfo",
    "NativeDeliveryInfo",
    "SessionNotification",
    "SessionStatus",
    # Workflow models
    "MainThread",
    "QueueItem",
    "QueueItemResponse",
    "QueueItemType",
    "QueueItemPriority",
    "WorkflowEvent",
    "EventTypes",
    "GitHubRepo",
    "GitHubPR",
    "Project",
]

__all__ += [
    "AttentionItem",
    "AttentionRequest",
    "CapabilityResult",
    "CapabilityState",
    "Checkpoint",
    "DeliveryAttempt",
    "DeliveryState",
    "MessageEnvelope",
    "NativeBinding",
    "NativeEvent",
    "NativeStatus",
    "ProviderExtension",
    "ReconciliationEvidence",
    "WorkspaceBinding",
    "WorkspaceAgentKind",
    "WorkspaceCondition",
    "WorkspaceConditionStatus",
    "WorkspaceDesiredState",
    "WorkspaceLifecycle",
    "WorkspaceManifest",
    "WorkspaceObservedState",
    "WorkspaceTransition",
]
