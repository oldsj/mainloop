"""Purpose-separated publication contracts; these confer no MCP or owner authority."""

from enum import StrEnum
from typing import Annotated, Literal
from uuid import UUID

from pydantic import AwareDatetime, BaseModel, ConfigDict, Field, model_validator

from models.workspace import WorkspaceEnvironment


class PushContract(BaseModel):
    model_config = ConfigDict(extra="forbid", frozen=True)


class GitRuntimeAssociation(PushContract):
    session_id: str = Field(min_length=1, max_length=256)
    generation_id: str = Field(min_length=1, max_length=256)
    atespace: str = Field(min_length=1, max_length=256)
    actor_name: str = Field(min_length=1, max_length=256)
    actor_uid: str = Field(min_length=1, max_length=256)
    revision: str = Field(min_length=1, max_length=256)


class GitAgent(PushContract):
    namespace: str = Field(min_length=1)
    name: str = Field(min_length=1)


class GitWorkspace(PushContract):
    repo: str = Field(min_length=1)
    ref: str = ""
    branch: str = Field(min_length=1)
    depth: int = Field(default=0, ge=0)


class GitReference(PushContract):
    origin: str
    header: str
    secret_name: str
    secret_key: str


class GitEnvironment(PushContract):
    image: str
    platform: str
    policy_identity: str


class GitComposition(PushContract):
    payload_image: str
    provider: str
    schema: int
    cli_version: str


class GitCreatePlan(PushContract):
    issuance_id: UUID
    issuance_version: int = Field(ge=1)
    binding_id: str = Field(min_length=1)
    create_request_id: UUID
    owner_id: str
    project_id: str
    repository: str
    branch: str
    agent: GitAgent
    workspace: GitWorkspace
    development_environment: WorkspaceEnvironment | None = None
    references: tuple[GitReference, ...] = Field(min_length=2, max_length=3)
    attempt_id: str | None = None
    branch_claim_generation: int | None = Field(default=None, ge=1)
    push_version: int | None = Field(default=None, ge=1)


class GitObservation(PushContract):
    runtime: GitRuntimeAssociation
    context_id: str
    agent: GitAgent
    workspace: GitWorkspace
    development_environment: GitEnvironment | None = None
    runtime_composition: GitComposition | None = None


class CredentialStamp(PushContract):
    issuance_id: UUID
    binding_id: str
    purpose: Annotated[str, Field(pattern=r"^git-(read|push)$")]
    issuance_version: int = Field(ge=1)
    capability_hash: str = Field(pattern=r"^[0-9a-f]{64}$")


class GitEnrollment(PushContract):
    plan: GitCreatePlan
    association: GitObservation | None
    read_state: str
    push_state: str


class ProtectedBranchPolicy(PushContract):
    project_id: Annotated[str, Field(min_length=1)]
    version: Annotated[int, Field(ge=1)]
    default_branch: Annotated[str, Field(min_length=1)]
    patterns: tuple[str, ...] = ()
    previous_defaults: tuple[str, ...] = ()


class PushGrant(PushContract):
    id: Annotated[str, Field(min_length=1)]
    owner_id: Annotated[str, Field(min_length=1)]
    project_id: Annotated[str, Field(min_length=1)]
    repository: Annotated[str, Field(min_length=1)]
    branch: Annotated[str, Field(min_length=1)]
    workspace_id: Annotated[str, Field(min_length=1)]
    session_id: Annotated[str, Field(min_length=1)]
    runtime_identity: Annotated[str, Field(min_length=1)]
    version: Annotated[int, Field(ge=1)] = 1
    role: str = "agent"
    grant_kind: str = "workspace"
    active: bool = True
    archived: bool = False
    terminal: bool = False
    # Delegated supervisor/child writers: the attempt and writer-claim generation the grant
    # was issued for, recorded from the live snapshot. Owner agent grants leave both unset.
    attempt_id: Annotated[str | None, Field(min_length=1)] = None
    writer_generation: Annotated[int | None, Field(ge=1)] = None
    branch_claim_generation: int | None = Field(default=None, ge=1)
    git_issuance_id: UUID | None = None
    runtime_association: GitRuntimeAssociation | None = None

    @model_validator(mode="after")
    def _writer_proof_is_paired(self):
        if (self.attempt_id is None) != (self.writer_generation is None):
            raise ValueError("attempt_id and writer_generation are set together")
        if self.role == "agent" and self.attempt_id is not None:
            raise ValueError("owner agent grants carry no attempt")
        if (self.git_issuance_id is None) != (self.runtime_association is None):
            raise ValueError("Git issuance and runtime association are set together")
        if self.git_issuance_id is not None and (
            self.branch_claim_generation is None
            or (
                self.attempt_id
                and self.branch_claim_generation != self.writer_generation
            )
        ):
            raise ValueError("Git grants require the exact branch claim generation")
        return self


class RefUpdate(PushContract):
    ref: Annotated[str, Field(min_length=1)]
    old_oid: Annotated[str, Field(pattern=r"^[0-9a-f]{40}$")]
    new_oid: Annotated[str, Field(pattern=r"^[0-9a-f]{40}$")]


class PublicationState(StrEnum):
    PENDING = "pending"
    DISPATCHING = "dispatching"
    CONFIRMED = "confirmed"
    REJECTED = "rejected"
    UNKNOWN = "unknown"


class TransportReceipt(PushContract):
    classification: PublicationState
    ref: str
    validated: bool
    failure_code: Literal["publication_unknown", "upstream_rejected"] | None = None
    recorded_at: AwareDatetime


class PublicationAttempt(PushContract):
    request_id: Annotated[str, Field(min_length=1)]
    grant_id: Annotated[str, Field(min_length=1)]
    repository: Annotated[str, Field(min_length=1)]
    update: RefUpdate
    grant_version: Annotated[int, Field(ge=1)]
    policy_version: Annotated[int, Field(ge=1)]
    state: PublicationState = PublicationState.PENDING


class TransportEvidence(PushContract):
    attempt: PublicationAttempt
    stamp: CredentialStamp
    grant: PushGrant
    association: GitRuntimeAssociation
    body_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    body_bytes: int = Field(gt=0)
    incoming_objects: int = Field(ge=0)
    expanded_bytes: int = Field(gt=0)
    validated_objects: int = Field(gt=0)
    disk_bytes: int = Field(gt=0)

    @model_validator(mode="after")
    def _exact_proof(self):
        if (
            self.stamp.purpose != "git-push"
            or self.stamp.issuance_id != self.grant.git_issuance_id
            or self.stamp.binding_id != self.grant.session_id
            or self.association != self.grant.runtime_association
            or self.attempt.grant_id != self.grant.id
            or self.attempt.grant_version != self.grant.version
            or self.attempt.repository.lower() != self.grant.repository.lower()
            or self.attempt.update.ref != "refs/heads/" + self.grant.branch
            or self.attempt.state != PublicationState.PENDING
        ):
            raise ValueError("transport proof disagreement")
        return self
