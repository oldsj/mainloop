"""Owner-controlled merge policy and immutable server path protection."""

from enum import StrEnum
from typing import Literal

from pydantic import BaseModel, ConfigDict, Field


class MergePolicy(StrEnum):
    AUTO = "auto"
    APPROVAL = "approval"


PROTECTED_GLOBS_VERSION = 1
PROTECTED_GLOBS = ("k8s/**", ".github/**", "**/migrations/**")


class MergePolicyUpdate(BaseModel):
    model_config = ConfigDict(extra="forbid")
    merge_policy: MergePolicy
    expected_version: int = Field(ge=1)


class MergePolicyView(BaseModel):
    merge_policy: MergePolicy
    merge_policy_version: int = Field(ge=1)
    protected_globs: tuple[str, ...] = PROTECTED_GLOBS
    protected_globs_version: Literal[1] = PROTECTED_GLOBS_VERSION


class ChangedPath(BaseModel):
    """Deleted files retain filename; renames must also supply previous_filename."""

    model_config = ConfigDict(extra="forbid")
    filename: str = Field(min_length=1, max_length=4096)
    status: Literal[
        "added", "modified", "removed", "renamed", "copied", "changed", "unchanged"
    ]
    previous_filename: str | None = Field(default=None, min_length=1, max_length=4096)


def protected_matches(paths: list[ChangedPath], *, complete: bool) -> tuple[str, ...]:
    """Case-sensitive POSIX matching of the fixed globs; fail closed on unknown paths."""
    if not complete or len(paths) > 3000:
        raise ValueError("Complete changed paths are required")
    matches = set()
    for path in paths:
        if path.status == "renamed" and not path.previous_filename:
            raise ValueError("Rename source is required")
        for name in (path.filename, path.previous_filename):
            if name is None:
                continue
            parts = name.split("/")
            if (
                any(p in ("", ".", "..") for p in parts)
                or "\\" in name
                or any(ord(c) < 32 for c in name)
            ):
                raise ValueError("Invalid repository-relative POSIX path")
            if (
                len(parts) > 1 and parts[0] in ("k8s", ".github")
            ) or "migrations" in parts[:-1]:
                matches.add(name)
    return tuple(sorted(matches))


def approval_required(
    policy: MergePolicy, paths: list[ChangedPath], *, complete: bool
) -> bool:
    matches = protected_matches(paths, complete=complete)
    return policy == MergePolicy.APPROVAL or bool(matches)
