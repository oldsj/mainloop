"""Pure apt input preparation. Trusted inputs must come from a trusted caller.

Nothing here authenticates policy/receipts or authorizes execution. Maintainer
scripts and inherited image content still require a credential-free sandbox.
"""

import hashlib
import io
import json
import re
import tarfile
from typing import Annotated, Literal
from urllib.parse import urlsplit

from mainloop.environments.registry import REFERENCE, VALIDATOR_VERSION
from pydantic import BaseModel, ConfigDict, Field, ValidationError, model_validator

from models.environment import (
    Digest,
    EnvironmentVersion,
    PackageDeclaration,
    PackageSpec,
)

GENERATOR_VERSION = "apt-inputs-v2"
MAX_PACKAGES = 32
MAX_REQUEST_BYTES = 16 * 1024
# Fixed shell source; package selectors/names are positional argv, never source.
# apt patterns may match nothing successfully, so require a real Package record
# for each operand before install. A virtual provider is not an exact-name match.
EXACT_PACKAGE_CHECK = (
    'while [ "$#" -gt 0 ]; do '
    'metadata=$(/usr/bin/apt-cache show --no-all-versions -- "$1"); '
    'printf "%s\\n" "$metadata" | /usr/bin/grep -Fx -- "Package: $2" >/dev/null; '
    "shift 2; done"
)
Token = Annotated[
    str, Field(min_length=1, max_length=128, pattern=r"^[a-zA-Z0-9_.-]+$")
]
PackageName = Annotated[
    str, Field(min_length=2, max_length=128, pattern=r"^[a-z0-9][a-z0-9+.-]*[a-z0-9.]$")
]
PackageVersion = Annotated[
    str,
    Field(
        min_length=1,
        max_length=128,
        pattern=r"^(?:[0-9]+:)?[0-9](?:[A-Za-z0-9.+~-]*[A-Za-z0-9.+~])?$",
    ),
]
Platform = Annotated[
    str, Field(min_length=1, max_length=128, pattern=r"^[a-z0-9]+/[a-z0-9]+$")
]


class BuildInputError(ValueError):
    """A bounded reason code; never includes rejected input or a partial recipe."""

    def __init__(self, code: str):
        self.code = code
        super().__init__(code)


class InputModel(BaseModel):
    model_config = ConfigDict(
        extra="forbid", frozen=True, strict=True, revalidate_instances="always"
    )


class AptSource(InputModel):
    uri: Annotated[str, Field(min_length=1, max_length=2048)]
    suites: Annotated[tuple[Token, ...], Field(min_length=1, max_length=32)]
    components: Annotated[tuple[Token, ...], Field(min_length=1, max_length=32)]
    keyring_path: Annotated[str, Field(min_length=1, max_length=256)]
    keyring_digest: Digest

    @model_validator(mode="after")
    def normalized_source(self):
        uri = urlsplit(self.uri)
        if (
            uri.scheme not in ("http", "https")
            or not uri.hostname
            or uri.netloc != uri.hostname
            or uri.query
            or uri.fragment
            or re.fullmatch(r"https?://[a-z0-9.-]+(?:/[a-zA-Z0-9._/-]+)?", self.uri)
            is None
            or any(part in (".", "..") for part in uri.path.split("/"))
            or re.fullmatch(r"/usr/share/keyrings/[a-zA-Z0-9_.-]+", self.keyring_path)
            is None
            or len(set(self.suites)) != len(self.suites)
            or len(set(self.components)) != len(self.components)
        ):
            raise ValueError("source must be normalized and credential-free")
        return self


class ParentSourceReceipt(InputModel):
    platform_manifest_digest: Digest
    config_digest: Digest
    platform: Platform
    distribution: Token
    distribution_version: Token
    apt_version: PackageVersion | None
    sources: Annotated[tuple[AptSource, ...], Field(min_length=1, max_length=32)]
    inventory_digest: Digest


class PermittedPackage(InputModel):
    name: PackageName
    # Exact requested versions, with None explicitly permitting an unpinned request.
    requested_versions: Annotated[
        tuple[PackageVersion | None, ...], Field(min_length=1, max_length=32)
    ]

    @model_validator(mode="after")
    def distinct_versions(self):
        if len(set(self.requested_versions)) != len(self.requested_versions):
            raise ValueError("duplicate permitted version")
        return self


class PackageBuildPolicy(InputModel):
    id: Token
    version: Annotated[int, Field(ge=1)]
    parent_environment_id: Token
    parent_version_id: Token
    parent_image: Annotated[str, Field(min_length=1, max_length=2048)]
    parent_config_digest: Digest
    platform: Platform
    manager: Literal["apt", "apk"]
    source_inventory_digest: Digest
    packages: Annotated[
        tuple[PermittedPackage, ...], Field(min_length=1, max_length=32)
    ]
    content_digest: Digest


class PreparedPackageBuild(InputModel):
    # Deterministic tar containing only Dockerfile; no filesystem is consulted.
    context: bytes
    context_digest: Digest
    request_digest: Digest
    manifest: bytes
    manifest_digest: Digest
    execution_authorized: Literal[False] = False


def _json(value) -> bytes:
    return json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        allow_nan=False,
    ).encode("utf-8")


def _digest(value: bytes) -> str:
    return "sha256:" + hashlib.sha256(value).hexdigest()


def _validated(model, value, code):
    # Revalidate even model_construct/model_copy instances and mutable nested lists.
    try:
        if isinstance(value, model):
            if any(
                field.is_required() and name not in value.__dict__
                for name, field in model.model_fields.items()
            ):
                raise BuildInputError(code)
            records = [value]
            if isinstance(value, PackageDeclaration):
                records.extend(
                    package
                    for package in value.packages
                    if isinstance(package, PackageSpec)
                )
            if any(
                set(record.__dict__) - set(type(record).model_fields)
                or record.model_extra
                for record in records
            ):
                raise BuildInputError(code)
            value = value.model_dump(exclude_unset=True, warnings=False)
        return model.model_validate(value, strict=True)
    except (ValidationError, ValueError, TypeError):
        raise BuildInputError(code) from None


def _source_inventory(receipt):
    value = receipt.model_dump(mode="json", exclude={"inventory_digest"})
    for source in value["sources"]:
        source["suites"].sort()
        source["components"].sort()
    value["sources"].sort(key=_json)
    if len({_json(source) for source in value["sources"]}) != len(value["sources"]):
        raise BuildInputError("duplicate_source")
    return value


def _policy_content(policy):
    value = policy.model_dump(mode="json", exclude={"content_digest"})
    for package in value["packages"]:
        package["requested_versions"].sort(key=_json)
    value["packages"].sort(key=lambda package: package["name"])
    if len({package["name"] for package in value["packages"]}) != len(
        value["packages"]
    ):
        raise BuildInputError("duplicate_policy_package")
    return value


def _request(declaration):
    if declaration.repositories:
        raise BuildInputError("repositories_override")
    if {"generator_version", "metadata_hashes"} & declaration.model_fields_set:
        raise BuildInputError("caller_evidence")
    if len(declaration.packages) > MAX_PACKAGES:
        raise BuildInputError("too_many_packages")
    packages = []
    names = set()
    for package in declaration.packages:
        package = _validated(PackageSpec, package, "invalid_request")
        if "resolved_version" in package.model_fields_set:
            raise BuildInputError("caller_evidence")
        if len(package.name) > 128 or (
            package.requested_version is not None
            and len(package.requested_version) > 128
        ):
            raise BuildInputError("package_string_limit")
        if re.fullmatch(r"[a-z0-9][a-z0-9+.-]*[a-z0-9.]", package.name) is None or (
            package.requested_version is not None
            and re.fullmatch(
                r"(?:[0-9]+:)?[0-9](?:[A-Za-z0-9.+~-]*[A-Za-z0-9.+~])?",
                package.requested_version,
            )
            is None
        ):
            raise BuildInputError("invalid_package_operand")
        if package.name in names:
            raise BuildInputError("duplicate_package")
        names.add(package.name)
        packages.append(
            {"name": package.name, "requested_version": package.requested_version}
        )
    value = {
        "manager": declaration.manager,
        "packages": sorted(packages, key=lambda package: package["name"]),
        "policy_id": declaration.policy_id,
        "policy_version": declaration.policy_version,
        "project_id": declaration.project_id,
        "task_id": declaration.task_id,
    }
    try:
        raw = _json(value)
    except (ValueError, UnicodeError):
        raise BuildInputError("invalid_request") from None
    if len(raw) > MAX_REQUEST_BYTES:
        raise BuildInputError("request_byte_limit")
    for field in ("policy_id", "project_id", "task_id"):
        identifier = value[field]
        if identifier is not None and (
            len(identifier) > 128
            or re.fullmatch(r"[a-zA-Z0-9_.-]+", identifier) is None
        ):
            raise BuildInputError("invalid_request_scope")
    return value, _digest(raw)


def prepare_package_build(
    parent: EnvironmentVersion,
    declaration: PackageDeclaration,
    policy: PackageBuildPolicy,
    parent_source_receipt: ParentSourceReceipt,
) -> PreparedPackageBuild:
    """Compile trusted-policy-checked inputs without granting any authority.

    The caller must authenticate both trusted inputs, enforce scope/grants and
    recheck policy before a future isolated execution. Digests correlate content;
    they are not signatures, approvals, resolution receipts or runtime probes.
    """
    parent = _validated(EnvironmentVersion, parent, "invalid_parent")
    declaration = _validated(PackageDeclaration, declaration, "invalid_request")
    policy = _validated(PackageBuildPolicy, policy, "invalid_policy")
    receipt = _validated(
        ParentSourceReceipt, parent_source_receipt, "invalid_source_receipt"
    )
    request, request_digest = _request(declaration)
    if parent.validation_status != "static_validated":
        raise BuildInputError("parent_not_static_validated")
    if parent.validator_version != VALIDATOR_VERSION:
        raise BuildInputError("stale_parent_validator")
    platform = f"linux/{parent.architecture}"
    if (
        platform != "linux/arm64"
        or policy.platform != platform
        or receipt.platform != platform
    ):
        raise BuildInputError("unsupported_platform")
    if declaration.manager != "apt" or policy.manager != "apt":
        raise BuildInputError("unsupported_manager")
    if (
        receipt.distribution != "debian"
        or receipt.apt_version is None
        or int(re.match(r"^(?:[0-9]+:)?([0-9]+)", receipt.apt_version)[1]) < 2
    ):
        raise BuildInputError("unsupported_apt_parent")
    image = f"{parent.registry}/{parent.repository}@{parent.platform_manifest_digest}"
    reference = REFERENCE.fullmatch(image)
    if reference is None or any(
        part in ("", ".", "..") for part in parent.repository.split("/")
    ):
        raise BuildInputError("invalid_parent_reference")
    if (
        policy.parent_environment_id != parent.environment_id
        or policy.parent_version_id != parent.id
        or policy.parent_image != image
        or policy.parent_config_digest != parent.config_digest
    ):
        raise BuildInputError("policy_parent_mismatch")
    if (
        declaration.policy_id != policy.id
        or declaration.policy_version != policy.version
    ):
        raise BuildInputError("request_policy_mismatch")
    if (
        receipt.platform_manifest_digest != parent.platform_manifest_digest
        or receipt.config_digest != parent.config_digest
    ):
        raise BuildInputError("receipt_parent_mismatch")
    inventory = _source_inventory(receipt)
    if _digest(_json(inventory)) != receipt.inventory_digest:
        raise BuildInputError("source_inventory_digest_mismatch")
    if receipt.inventory_digest != policy.source_inventory_digest:
        raise BuildInputError("policy_source_mismatch")
    if _digest(_json(_policy_content(policy))) != policy.content_digest:
        raise BuildInputError("policy_content_digest_mismatch")
    permitted = {
        package.name: package.requested_versions for package in policy.packages
    }
    operands = []
    checked_operands = []
    for package in request["packages"]:
        name, version = package["name"], package["requested_version"]
        if name not in permitted or version not in permitted[name]:
            raise BuildInputError("package_not_permitted")
        # ?exact-name takes a literal name, unlike bare apt regex fallback. The
        # validated name cannot contain pattern delimiters or version operators.
        selector = f"?exact-name({name})"
        operand = selector if version is None else f"{selector}={version}"
        operands.append(operand)
        checked_operands.extend((operand, name))
    commands = [
        ["/usr/bin/apt-get", "-o", "APT::Update::Error-Mode=any", "update"],
        [
            "/bin/sh",
            "-ec",
            EXACT_PACKAGE_CHECK,
            "mainloop-exact-package-check",
            *checked_operands,
        ],
        [
            "/usr/bin/env",
            "DEBIAN_FRONTEND=noninteractive",
            "/usr/bin/apt-get",
            "--yes",
            "--no-install-recommends",
            "--no-remove",
            "--no-upgrade",
            "install",
            "--",
            *operands,
        ],
        ["/bin/sh", "-c", "/usr/bin/apt-get clean && /bin/rm -rf /var/lib/apt/lists/*"],
    ]
    dockerfile = (
        f"FROM --platform={platform} {image}\nUSER 0:0\n"
        + "".join(
            "RUN " + _json(command).decode("utf-8") + "\n" for command in commands
        )
        + "USER 65532:65532\n"
    ).encode("utf-8")
    buffer = io.BytesIO()
    with tarfile.open(fileobj=buffer, mode="w", format=tarfile.USTAR_FORMAT) as archive:
        entry = tarfile.TarInfo("Dockerfile")
        entry.size = len(dockerfile)
        entry.mode = 0o644
        archive.addfile(entry, io.BytesIO(dockerfile))
    context = buffer.getvalue()
    context_digest = _digest(context)
    manifest = _json(
        {
            "schema": "mainloop-package-build-inputs-v1",
            "generator": GENERATOR_VERSION,
            "parent": {
                "environment_id": parent.environment_id,
                "version_id": parent.id,
                "image": image,
                "index_digest": parent.index_digest,
                "platform_manifest_digest": parent.platform_manifest_digest,
                "config_digest": parent.config_digest,
                "validator_version": parent.validator_version,
            },
            "platform": platform,
            "request": request,
            "request_digest": request_digest,
            "policy": {
                "id": policy.id,
                "version": policy.version,
                "content_digest": policy.content_digest,
            },
            "source_inventory": inventory,
            "source_inventory_digest": receipt.inventory_digest,
            "context_digest": context_digest,
            "execution_authorized": False,
        }
    )
    return PreparedPackageBuild(
        context=context,
        context_digest=context_digest,
        request_digest=request_digest,
        manifest=manifest,
        manifest_digest=_digest(manifest),
    )
