"""Deterministic scope decisions over parsed updates and trusted authority snapshots."""

from collections.abc import Callable, Sequence
from fnmatch import fnmatchcase

from mainloop.services.github_repo import InvalidGithubRepo, parse_github_repo
from mainloop.services.workspace_authority import _feature_branch

from models.push_gate import ProtectedBranchPolicy, PushGrant, RefUpdate

ZERO_OID = "0" * 40

# The only (role, grant kind) pairs that may hold push authority. Coordination grants of
# any role, including main and delegated coordination children, never appear here.
WORKSPACE_WRITER_PAIRS = frozenset(
    {("agent", "workspace"), ("supervisor", "workspace"), ("child", "workspace")}
)
DELEGATED_WRITER_DEPTHS = {"supervisor": 1, "child": 2}


def protected_reason(branch: str, policy: ProtectedBranchPolicy) -> str | None:
    if not policy.default_branch:
        return "metadata_unavailable"
    if branch == policy.default_branch or branch in policy.previous_defaults:
        return "default_branch"
    if any(fnmatchcase(branch, pattern) for pattern in policy.patterns):
        return "protected_branch"
    return None


def authorize(
    grant: PushGrant,
    policy: ProtectedBranchPolicy,
    repository: str,
    updates: Sequence[RefUpdate],
    is_ancestor: Callable[[str, str], bool | None],
) -> str | None:
    """None permits scope; a stable reason denies. Ancestry must come from trusted objects.

    Creation still requires connected commit/pack validation at the future listener.
    Runtime/session freshness must be loaded by the store under its publication lock.
    """
    if not grant.active:
        return "grant_revoked"
    if grant.archived:
        return "session_archived"
    if grant.terminal:
        return "session_terminal"
    if (grant.role, grant.grant_kind) not in WORKSPACE_WRITER_PAIRS:
        return "grant_kind"
    if grant.role != "agent" and (
        grant.attempt_id is None or grant.writer_generation is None
    ):
        return "attempt_not_current"
    if not all(
        (grant.owner_id, grant.workspace_id, grant.session_id, grant.runtime_identity)
    ):
        return "binding_unavailable"
    if grant.project_id != policy.project_id:
        return "policy_mismatch"
    if not _feature_branch(grant.branch):
        return "invalid_branch"
    try:
        if (
            parse_github_repo(repository).full_name.lower()
            != parse_github_repo(grant.repository).full_name.lower()
        ):
            return "repository_mismatch"
    except (InvalidGithubRepo, TypeError):
        return "repository_unavailable"
    if len(updates) != 1:
        return "single_ref_required"
    update = updates[0]
    if not update.ref.startswith("refs/heads/"):
        return "ref_namespace"
    branch = update.ref[len("refs/heads/") :]
    reason = protected_reason(branch, policy)
    if reason:
        return reason
    if update.ref != f"refs/heads/{grant.branch}":
        return "branch_mismatch"
    if update.new_oid == ZERO_OID:
        return "deletion"
    if update.old_oid != ZERO_OID:
        ancestor = is_ancestor(update.old_oid, update.new_oid)
        if ancestor is not True:
            return "non_fast_forward" if ancestor is False else "ancestry_unavailable"
    return None
