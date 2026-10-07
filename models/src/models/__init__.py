"""Shared Pydantic models for mainloop."""

from models.conversation import Conversation, Message
from models.environment import (
    DefinitionReference,
    DevEnvironment,
    EnvironmentVersion,
    PackageDeclaration,
    PackageSpec,
    ProjectEnvironmentGrant,
    ProjectEnvironmentSelection,
    RegisterEnvironment,
    SelectEnvironment,
)
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
)
from models.provider import ProviderAgentRef, ProviderProfile, ProviderProfileId
from models.push_gate import (
    ProtectedBranchPolicy,
    PublicationAttempt,
    PublicationState,
    PushGrant,
    RefUpdate,
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
    WorkspaceDev,
    WorkspaceLifecycle,
    WorkspaceManifest,
    WorkspaceObservedState,
    WorkspacePort,
)

__all__ = [
    # Existing
    "Conversation",
    "Message",
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
    "WorkspaceAgentKind",
    "WorkspaceDev",
    "WorkspaceLifecycle",
    "WorkspaceManifest",
    "WorkspaceObservedState",
    "WorkspacePort",
]


__all__ += ["ProviderAgentRef", "ProviderProfile", "ProviderProfileId"]

__all__ += [
    "DefinitionReference",
    "DevEnvironment",
    "EnvironmentVersion",
    "PackageDeclaration",
    "PackageSpec",
    "ProjectEnvironmentGrant",
    "ProjectEnvironmentSelection",
    "RegisterEnvironment",
    "SelectEnvironment",
    "ProtectedBranchPolicy",
    "PublicationAttempt",
    "PublicationState",
    "PushGrant",
    "RefUpdate",
]
