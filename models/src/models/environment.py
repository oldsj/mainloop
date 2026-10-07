"""Development environment records; validation evidence is not runtime proof."""

from typing import Annotated, Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator

Digest = Annotated[str, Field(pattern=r"^sha256:[0-9a-f]{64}$")]
Architecture = Literal["amd64", "arm64"]


class EnvironmentModel(BaseModel):
    model_config = ConfigDict(extra="forbid", frozen=True)


class DefinitionReference(EnvironmentModel):
    repository: Annotated[str, Field(min_length=1, max_length=2048)]
    commit_sha: Annotated[str, Field(pattern=r"^[0-9a-f]{40}$")]
    path: Annotated[str, Field(min_length=1, max_length=1024)]

    @model_validator(mode="after")
    def relative_path(self):
        if self.path.startswith("/") or ".." in self.path.split("/"):
            raise ValueError("definition path must stay within the repository")
        return self


class PackageSpec(EnvironmentModel):
    name: Annotated[
        str, Field(pattern=r"^[a-z0-9][a-z0-9+.-]*(?::(?:amd64|arm64|all))?$")
    ]
    requested_version: Annotated[str, Field(pattern=r"^[A-Za-z0-9.+:~_-]+$")] | None = (
        None
    )
    resolved_version: Annotated[str, Field(pattern=r"^[A-Za-z0-9.+:~_-]+$")] | None = (
        None
    )


class PackageDeclaration(EnvironmentModel):
    manager: Literal["apt", "apk"]
    packages: Annotated[list[PackageSpec], Field(min_length=1)]
    policy_id: str
    policy_version: Annotated[int, Field(ge=1)]
    repositories: list[str] = Field(default_factory=list)
    generator_version: str | None = None
    metadata_hashes: list[Digest] = Field(default_factory=list)
    project_id: str | None = None
    task_id: str | None = None


class DevEnvironment(EnvironmentModel):
    id: str
    owner_id: str
    name: str
    source_kind: Literal["prebuilt_image", "definition_repo", "operator_default"]
    watched_tag: str | None = None
    accepted_default_version_id: str | None = None
    definition: DefinitionReference | None = None


class EnvironmentVersion(EnvironmentModel):
    id: str
    environment_id: str
    registry: str | None = None
    repository: str | None = None
    index_digest: Digest | None = None
    platform_manifest_digest: Digest | None = None
    config_digest: Digest | None = None
    declared_user: str | None = None
    architecture: Architecture | None = None
    capability_profile: dict[str, str] = Field(default_factory=dict)
    validation_status: Literal["pending_build", "static_validated"]
    validation_result: dict[str, str] = Field(default_factory=dict)
    validator_version: str
    provenance_kind: Literal["user_pushed", "mainloop_built", "operator"]
    base_provenance: Literal["supplied", "verified", "unknown"] = "unknown"
    parent_version_id: str | None = None
    package_declaration: PackageDeclaration | None = None
    approval_reference: str | None = None
    definition: DefinitionReference | None = None

    @model_validator(mode="after")
    def evidence(self):
        if self.validation_status == "static_validated" and (
            not all(
                (
                    self.registry,
                    self.repository,
                    self.platform_manifest_digest,
                    self.config_digest,
                    self.architecture,
                )
            )
            or self.declared_user != "65532:65532"
        ):
            raise ValueError(
                "static validation requires platform evidence and USER 65532:65532"
            )
        if self.package_declaration is not None and self.parent_version_id is None:
            raise ValueError("derived package declaration requires a parent version")
        return self


class ProjectEnvironmentGrant(EnvironmentModel):
    environment_id: str
    project_id: str
    permission: Literal["use", "derive"]


class ProjectEnvironmentSelection(EnvironmentModel):
    project_id: str
    environment_id: str
    version_id: str | None = None
    follow_default: bool = False
    revision: Annotated[int, Field(ge=1)]
    access_revoked: bool = False
    resolved_version_id: str | None = None


class SelectEnvironment(EnvironmentModel):
    environment_id: str
    version_id: str | None = None
    follow_default: bool = False
    expected_version: Annotated[int, Field(ge=0)]

    @model_validator(mode="after")
    def target(self):
        if self.follow_default == (self.version_id is not None):
            raise ValueError("choose an explicit version or follow_default")
        return self


class RegisterEnvironment(EnvironmentModel):
    name: Annotated[str, Field(min_length=1, max_length=200)]
    source_kind: Literal["prebuilt_image", "definition_repo"] = "prebuilt_image"
    image: str | None = None
    architecture: Architecture = "amd64"
    watched_tag: Annotated[str, Field(pattern=r"^[\w][\w.-]{0,127}$")] | None = None
    definition: DefinitionReference | None = None

    @model_validator(mode="after")
    def source(self):
        if self.source_kind == "prebuilt_image":
            if not self.image or self.definition is not None:
                raise ValueError("prebuilt registration requires image only")
        elif (
            self.definition is None
            or self.image is not None
            or self.watched_tag is not None
        ):
            raise ValueError("definition registration requires definition only")
        return self
