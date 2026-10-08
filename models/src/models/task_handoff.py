"""Trusted S3 adapter contracts, never accepted as public agent tool arguments."""

from typing import Annotated, Literal

from pydantic import AwareDatetime, Field, model_validator

from models.native_agent import ContractModel
from models.task import EvidenceRef, Identifier, TaskCheckout
from models.workspace import WorkspaceEnvironment

CommitSHA = Annotated[str, Field(strict=True, pattern=r"^[0-9a-f]{40}$")]
Qualification = Literal["unsupported", "offline_fake", "qualified_live"]


class EvidenceScope(ContractModel):
    operation_id: Identifier
    attempt_id: Identifier
    session_id: Identifier
    binding_id: Identifier
    runtime_identity: Identifier
    writer_generation: Annotated[int, Field(strict=True, ge=1)]
    qualification: Qualification = "unsupported"
    provenance: EvidenceRef
    observed_at: AwareDatetime


class CheckpointEvidence(EvidenceScope):
    repository: Annotated[
        str, Field(strict=True, pattern=r"^[a-z0-9_.-]+/[a-z0-9_.-]+$")
    ]
    branch: str
    remote_sha: CommitSHA
    committed_checkpoint: bool
    no_start_initial_ref: str | None = None
    unverified_note: Annotated[str, Field(strict=True, max_length=4000)] | None = None
    # Actor observations are untrusted and do not establish hidden-file preservation.
    actor_clean_assertion: bool | None = None
    actor_assertion_provenance: EvidenceRef | None = None
    git_dispatch: Literal["settled", "unknown"]
    merge_dispatch: Literal["settled", "unknown"]
    evidence_ref: EvidenceRef

    @model_validator(mode="after")
    def feature_ref(self):
        TaskCheckout(branch=self.branch, ref=self.remote_sha)
        if (
            self.actor_clean_assertion is not None
            and self.actor_assertion_provenance is None
        ):
            raise ValueError("actor assertion requires untrusted provenance")
        return self


class SourceFenceEvidence(EvidenceScope):
    native_dispatch_settled: bool
    git_dispatch_settled: bool
    merge_dispatch_settled: bool
    credentials_revoked: bool
    runtime_quiescent: bool
    preview_closed: bool
    children_drained: bool
    pending_hitl_resolved: bool
    evidence_ref: EvidenceRef


class SuccessorResult(EvidenceScope):
    outcome: Literal["pending", "uncertain", "ready", "definite_failure", "absent"]
    checkpoint_sha: CommitSHA
    environment: WorkspaceEnvironment
    repository: str
    branch: str
    checkout_verified: bool = False
    environment_verified: bool = False
    grant_confirmed: bool = False
    grant_ref: EvidenceRef | None = None
    evidence_ref: EvidenceRef


class AdapterCapabilities(ContractModel):
    qualification: Qualification = "unsupported"
    provenance: EvidenceRef
    source_fence: bool = False
    preview_closure: bool = False
    exact_checkout: bool = False
    reconcile_original_create: bool = False


class ContinuationManifest(ContractModel):
    task_id: Identifier
    operation_id: Identifier
    predecessor_id: Identifier
    target_profile_id: Identifier
    repository: str
    branch: str
    checkpoint_sha: CommitSHA
    environment: WorkspaceEnvironment
    caller_instructions: Annotated[str, Field(strict=True, max_length=16384)]
    report_refs: Annotated[tuple[EvidenceRef, ...], Field(max_length=64)] = ()
    unverified_note: Annotated[str, Field(strict=True, max_length=4000)] | None = None
    note_label: Literal["unverified provider note"] = "unverified provider note"


class RetentionReceipt(ContractModel):
    runtime_identity: Identifier
    qualification: Qualification = "unsupported"
    attempt_id: Identifier
    session_id: Identifier
    action_id: Identifier
    confirmed: bool
    provenance: EvidenceRef
    observed_at: AwareDatetime
