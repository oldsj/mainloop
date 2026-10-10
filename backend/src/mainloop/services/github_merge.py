"""Bounded, fixed-origin merge evidence. No remote writes during preparation."""

import hashlib
import logging
from datetime import datetime, timedelta, timezone
from typing import NamedTuple
from urllib.parse import quote

import httpx
from mainloop.runtime.policy import PolicyError
from mainloop.services.github_auth import GitHubNotFound
from mainloop.services.github_creation import (
    GitHubCreationClient,
    GitHubError,
    PullRequest,
    Repo,
)
from mainloop.services.merge_summary import canonical_digest
from pydantic import BaseModel, ConfigDict, Field, HttpUrl, model_validator

from models.merge_policy import ChangedPath, protected_matches

PENDING_CHECK_STATES = frozenset(
    {"queued", "in_progress", "pending", "waiting", "requested"}
)
ABANDONED_SUITE_GRACE = timedelta(minutes=10)
PR_RULE_REQUIREMENTS = (
    "dismiss_stale_reviews_on_push",
    "require_code_owner_review",
    "require_last_push_approval",
    "required_review_thread_resolution",
)
# GitHub's documented rule types. Others are logged as UNKNOWN_BRANCH_RULE so
# upstream identifiers are never copied into policy reasons or logs.
KNOWN_RULE_TYPES = frozenset(
    {
        "branch_name_pattern",
        "code_scanning",
        "commit_author_email_pattern",
        "commit_message_pattern",
        "committer_email_pattern",
        "copilot_code_review",
        "creation",
        "deletion",
        "file_extension_restriction",
        "file_path_restriction",
        "max_file_path_length",
        "max_file_size",
        "merge_queue",
        "non_fast_forward",
        "pull_request",
        "required_deployments",
        "required_linear_history",
        "required_signatures",
        "required_status_checks",
        "tag_name_pattern",
        "update",
        "workflows",
    }
)
UNKNOWN_BRANCH_RULE = "unknown_branch_rule"
UNKNOWN_PR_PARAMETER = "unknown_pull_request_parameter"
# GitHub reports mergeable_state "behind" only when a branch rule requires the
# head to contain the latest base; Mainloop performs no update-branch write.
OUT_OF_DATE = (
    "branch is out of date with the default branch and GitHub requires it to be "
    "up to date; merge or rebase the default branch into the head, push, then "
    "prepare again"
)
MERGE_RESULT_PENDING = "merge_result_pending"
# GitHub's compare API lists at most 300 changed files for a comparison; a list
# at the cap cannot prove the inventory is complete.
MERGE_FILES_LIMIT = 300

logger = logging.getLogger(__name__)


class MergeRepo(Repo):
    allow_squash_merge: bool


class MergePR(PullRequest):
    title: str = Field(max_length=512)
    body: str | None = Field(max_length=2_000_000)
    draft: bool
    changed_files: int = Field(ge=0, le=3000)
    additions: int = Field(ge=0)
    deletions: int = Field(ge=0)
    mergeable: bool | None
    mergeable_state: str
    merged: bool
    merge_commit_sha: str | None = None


class ChangedFile(BaseModel):
    model_config = ConfigDict(strict=True, extra="forbid")
    filename: str = Field(min_length=1, max_length=4096)
    status: str = Field(min_length=1, max_length=32)
    previous_filename: str | None = Field(default=None, min_length=1, max_length=4096)
    additions: int = Field(ge=0)
    deletions: int = Field(ge=0)


def changed_file(item):
    if not isinstance(item, dict):
        raise GitHubError
    return ChangedFile.model_validate(
        {
            key: item[key]
            for key in (
                "filename",
                "status",
                "previous_filename",
                "additions",
                "deletions",
            )
            if key in item
        }
    )


def changed_path(item):
    return ChangedPath.model_validate(
        item.model_dump(include={"filename", "status", "previous_filename"})
    )


def inventory(files):
    rows = [
        item.model_dump(mode="json")
        for item in sorted(files, key=lambda file: file.filename)
    ]
    return rows, canonical_digest(rows)


class MergeResultPending(PolicyError):
    """GitHub's test merge for the pinned base and head is not ready yet.

    Execution keeps evaluating unless the CI collected first already failed.
    """

    def __init__(self, message, ci=None):
        super().__init__(MERGE_RESULT_PENDING, message)
        self.ci = ci


class Parent(BaseModel):
    sha: str = Field(pattern=r"^[0-9a-f]{40}$")


class MergeCommit(BaseModel):
    sha: str = Field(pattern=r"^[0-9a-f]{40}$")
    parents: list[Parent]


class Comparison(BaseModel):
    status: str = Field(max_length=32)
    ahead_by: int = Field(ge=0)
    behind_by: int = Field(ge=0)
    base_commit: Parent
    merge_base_commit: Parent
    files: list = Field(max_length=MERGE_FILES_LIMIT)


class App(BaseModel):
    model_config = ConfigDict(strict=True)
    id: int = Field(gt=0)
    slug: str | None = Field(default=None, min_length=1, max_length=512)


class SuiteRef(BaseModel):
    model_config = ConfigDict(strict=True)
    id: int = Field(gt=0)


class Check(BaseModel):
    model_config = ConfigDict(strict=True)
    id: int = Field(gt=0)
    name: str = Field(min_length=1, max_length=512)
    head_sha: str = Field(pattern=r"^[0-9a-f]{40}$")
    app: App
    # GitHub exposes no run creation timestamp. Numeric run IDs order reruns,
    # including queued runs without started_at (documented owner decision).
    status: str = Field(max_length=32)
    conclusion: str | None = Field(max_length=64)
    check_suite: SuiteRef
    html_url: HttpUrl | None = None


class Suite(BaseModel):
    model_config = ConfigDict(strict=True)
    id: int = Field(gt=0)
    head_sha: str = Field(pattern=r"^[0-9a-f]{40}$")
    status: str = Field(max_length=32)
    conclusion: str | None = Field(max_length=64)
    app: App | None = None
    created_at: str | None = Field(default=None, max_length=64)
    latest_check_runs_count: int | None = Field(default=None, ge=0)

    def abandoned(self, captured_at, suites_with_runs, required_apps):
        if (
            self.status != "queued"
            or self.conclusion is not None
            or self.id in suites_with_runs
            or (
                "latest_check_runs_count" in self.model_fields_set
                and self.latest_check_runs_count != 0
            )
            or self.app is None
            or self.app.id in required_apps
            or self.created_at is None
        ):
            return False
        try:
            created_at = datetime.fromisoformat(self.created_at)
        except ValueError:
            return False
        return (
            created_at.tzinfo is not None
            and captured_at - created_at > ABANDONED_SUITE_GRACE
        )


class RequiredCheck(BaseModel):
    model_config = ConfigDict(strict=True, extra="forbid")
    context: str = Field(min_length=1, max_length=512)
    integration_id: int | None = None


class ClassicRequiredCheck(BaseModel):
    model_config = ConfigDict(strict=True, extra="forbid")
    context: str = Field(min_length=1, max_length=512)
    app_id: int | None


class ClassicChecks(BaseModel):
    model_config = ConfigDict(strict=True, extra="forbid")
    checks: list[ClassicRequiredCheck]
    contexts: list[str]
    strict: bool = False
    url: str | None = None
    contexts_url: str | None = None
    enforcement_level: str | None = None


class ProtectionFlag(BaseModel):
    model_config = ConfigDict(strict=True, extra="forbid")
    enabled: bool
    url: str | None = None


class Protection(BaseModel):
    model_config = ConfigDict(strict=True, extra="forbid")
    required_status_checks: ClassicChecks | None
    required_pull_request_reviews: dict | None = None
    restrictions: dict | None = None
    url: str | None = None
    enabled: bool | None = None
    name: str | None = None
    protection_url: str | None = None
    enforce_admins: ProtectionFlag | None = None
    required_linear_history: ProtectionFlag | None = None
    allow_force_pushes: ProtectionFlag | None = None
    allow_deletions: ProtectionFlag | None = None
    allow_fork_syncing: ProtectionFlag | None = None
    block_creations: ProtectionFlag | None = None
    required_conversation_resolution: ProtectionFlag | None = None
    required_signatures: ProtectionFlag | None = None
    lock_branch: ProtectionFlag | None = None

    @model_validator(mode="after")
    def validate_present_flags(self):
        for field in self.model_fields_set:
            if (
                field
                not in {
                    "required_status_checks",
                    "required_pull_request_reviews",
                    "restrictions",
                    "url",
                    "name",
                    "protection_url",
                }
                and getattr(self, field) is None
            ):
                raise ValueError("present protection flags must be explicit")
        return self


class RuleParameters(BaseModel):
    model_config = ConfigDict(strict=True, extra="forbid")
    required_status_checks: list[RequiredCheck]
    strict_required_status_checks_policy: bool
    do_not_enforce_on_create: bool = False


class PRRuleRefusal(NamedTuple):
    reason: str
    # Unknown semantics might hide a check requirement, not only a review.
    required_incomplete: bool = False


class PullRequestParameters(BaseModel):
    # Unknown fields are kept only so unsupported() can refuse them: the meaning
    # of a value (even false, null or []) is unknown until the field is
    # understood and allowlisted here by name with its inactive value.
    model_config = ConfigDict(strict=True, extra="allow")
    required_approving_review_count: int = Field(ge=0)
    dismiss_stale_reviews_on_push: bool
    require_code_owner_review: bool
    require_last_push_approval: bool
    required_review_thread_resolution: bool
    allowed_merge_methods: list[str] | None = None
    # Inactive only when empty; null or any reviewer is refused.
    required_reviewers: list = Field(default_factory=list)
    # GitHub adds one approval to the configured count for Copilot PRs not
    # attributed to a person; the setting "has no effect if the ruleset requires
    # zero approvals", and unsupported() refuses any nonzero count.
    require_extra_approval_for_unattributed_changes: bool = False

    def unsupported(self) -> PRRuleRefusal | None:
        """Refusal for anything that could require review or block our squash."""
        if self.model_extra:
            return PRRuleRefusal(UNKNOWN_PR_PARAMETER, required_incomplete=True)
        if self.required_approving_review_count != 0:
            return PRRuleRefusal("required_approving_review_count")
        for field in PR_RULE_REQUIREMENTS:
            if getattr(self, field):
                return PRRuleRefusal(field)
        if self.required_reviewers != []:
            return PRRuleRefusal("required_reviewers")
        if (
            self.allowed_merge_methods is not None
            and "squash" not in self.allowed_merge_methods
        ):
            return PRRuleRefusal("allowed_merge_methods")
        return None


class Rule(BaseModel):
    model_config = ConfigDict(strict=True)
    type: str
    parameters: dict | None = None


class Status(BaseModel):
    id: int = Field(gt=0)
    context: str = Field(min_length=1, max_length=512)
    state: str = Field(max_length=32)
    created_at: datetime
    target_url: HttpUrl | None = None


class GitHubPlanUnavailable(GitHubError):
    """Branch rules are explicitly unavailable on the repository's GitHub plan."""


class GitHubMergeClient(GitHubCreationClient):
    evidence_step = "client"

    def _permissions(self, method: str, path: str) -> dict[str, str]:
        permissions = super()._permissions(method, path)
        if method == "GET" and path.lower() == f"/repos/{self.repository_name}":
            # GitHub omits merge settings without Contents read/write. Keep the
            # ordinary creation/monitoring repository lookup at Metadata read.
            return {"contents": "write"}
        return permissions

    async def branch_rules_request(self, path, **kwargs):
        # Only these read endpoints may interpret the specific plan refusal.
        try:
            response = await self._response("GET", path, accepted=(200, 403), **kwargs)
            payload = response.json()
            if response.status_code == 403:
                if (
                    isinstance(payload, dict)
                    and kwargs.get("params", {}).get("page", 1) == 1
                    and payload.get("message")
                    == "Upgrade to GitHub Pro or make this repository public to enable this feature."
                ):
                    raise GitHubPlanUnavailable
                raise GitHubError
            return payload
        except (httpx.HTTPError, ValueError, TimeoutError):
            raise GitHubError from None

    async def _request(self, method, path, **kwargs):
        if method == "GET" and (
            path.endswith("/protection") or "/rules/branches/" in path
        ):
            return await self.branch_rules_request(path, **kwargs)
        return await super()._request(method, path, **kwargs)

    async def repository(self, name):
        return MergeRepo.model_validate(await self._request("GET", f"/repos/{name}"))

    async def pull(self, name, number):
        return MergePR.model_validate(
            await self._request("GET", f"/repos/{name}/pulls/{number}")
        )

    async def pages(self, path, *, key=None, limit=1000, params=None):
        result, total = [], None
        for page in range(1, limit // 100 + 2):
            data = await self._request(
                "GET", path, params={**(params or {}), "per_page": 100, "page": page}
            )
            if key:
                if (
                    not isinstance(data, dict)
                    or type(data.get("total_count")) is not int
                ):
                    raise GitHubError
                if total is not None and total != data["total_count"]:
                    raise GitHubError
                total = data["total_count"]
                if not 0 <= total <= limit:
                    raise GitHubError
                data = data.get(key)
            if not isinstance(data, list) or len(data) > 100:
                raise GitHubError
            result.extend(data)
            if len(result) > limit:
                raise GitHubError
            if len(data) < 100:
                if total is not None and len(result) != total:
                    raise GitHubError
                return result
        raise GitHubError

    async def evidence(self, name, number, sha):
        self.evidence_step = "repository"
        repo = await self.repository(name)
        self.evidence_step = "pull_request"
        pr = await self.pull(name, number)
        self.evidence_step = "identity"
        if (
            repo.full_name.lower() != name.lower()
            or not repo.allow_squash_merge
            or pr.number != number
            or pr.state != "open"
            or pr.draft
            or pr.merged
            or pr.head.sha != sha
            or pr.base.ref != repo.default_branch
            or any(
                ref.repo.id != repo.id or ref.repo.full_name.lower() != name.lower()
                for ref in (pr.head, pr.base)
            )
        ):
            raise PolicyError(
                "identity",
                "PR must be an open same-repository feature head at the expected SHA and default base with squash enabled",
            )
        if pr.head.ref == repo.default_branch:
            raise PolicyError("branch", "default branch is not allowed")
        if pr.mergeable is False or pr.mergeable_state == "dirty":
            raise PolicyError(
                "merge_conflict",
                f"PR has merge conflicts with {repo.default_branch}; merge or rebase "
                f"{repo.default_branch} into {pr.head.ref}, push, then prepare again",
            )
        # GitHub updates pr.base.sha only when the PR is synchronized, so it is
        # not the base a squash would land on. Evaluate and pin the current
        # default-branch head instead; execution refuses if it moves.
        self.evidence_step = "branch"
        base = await self.branch(name, repo.default_branch)
        if base.name != repo.default_branch:
            raise GitHubError
        self.evidence_step = "files"
        raw = await self.pages(f"/repos/{name}/pulls/{number}/files", limit=3000)
        files = [changed_file(item) for item in raw]
        paths = [changed_path(item) for item in files]
        if (
            len(paths) != pr.changed_files
            or len({p.filename for p in paths}) != len(paths)
            or sum(item.additions for item in files) != pr.additions
            or sum(item.deletions for item in files) != pr.deletions
        ):
            raise GitHubError
        behind = pr.mergeable_state == "behind"
        ci = await self.checks(name, sha, repo.default_branch, behind=behind)
        if behind:
            raise PolicyError("rules", OUT_OF_DATE)
        try:
            merge_files = await self.merge_result(name, pr, base.commit.sha, sha)
        except MergeResultPending as pending:
            pending.ci = ci
            raise
        # The PR's three-dot file list can miss paths the squash changes: a
        # base-side rename redirects a head edit to the new path. Policy sees
        # both the PR inventory and the merge result against the pinned base.
        matches = protected_matches(
            paths + [changed_path(item) for item in merge_files], complete=True
        )
        self.evidence_step = "description"
        description_source = (pr.body or "").encode("utf-8")
        description = description_source[: 16 * 1024].decode("utf-8", errors="ignore")
        self.evidence_step = "pull_request_refresh"
        fresh = await self.pull(name, number)
        if fresh != pr:
            raise PolicyError(
                "stale", "PR or repository changed while reading evidence"
            )
        self.evidence_step = "repository_refresh"
        if await self.repository(name) != repo:
            raise PolicyError(
                "stale", "PR or repository changed while reading evidence"
            )
        self.evidence_step = "branch_refresh"
        if await self.branch(name, repo.default_branch) != base:
            raise PolicyError(
                "stale", "default branch moved while reading evidence; prepare again"
            )
        self.evidence_step = "summary"
        return {
            "repository_id": repo.id,
            "repository": name,
            "pr_number": number,
            "head_sha": sha,
            "head": pr.head.ref,
            "base": pr.base.ref,
            "base_sha": base.commit.sha,
            "title": pr.title,
            "description": description,
            "description_truncated": len(description_source) > 16 * 1024,
            "description_digest": hashlib.sha256(description_source).hexdigest(),
            "description_length": len(description_source),
            "changed_files_count": pr.changed_files,
            "additions": pr.additions,
            "deletions": pr.deletions,
            "files": inventory(files)[0],
            "files_digest": inventory(files)[1],
            # GitHub recreates test merge commits, so the SHA is recorded but
            # not pinned; the inventory and its parents (base_sha, head_sha) are.
            "merge_commit_sha": pr.merge_commit_sha,
            "merge_files": inventory(merge_files)[0],
            "merge_files_digest": inventory(merge_files)[1],
            "protected_matches": list(matches),
            "ci": ci,
            "mergeable": pr.mergeable is True
            and pr.mergeable_state in ("clean", "unstable", "has_hooks"),
        }

    async def merge_result(self, name, pr, base_sha, head_sha):
        """Files GitHub's test merge changes relative to the pinned base.

        Its parents must be exactly the pinned base and expected head. The
        documented compare of ``base_sha...merge`` then equals the first-parent
        diff, because the merge base is ``base_sha`` itself. A test merge that is
        missing, not yet recomputed for this base, or larger than compare can
        list completely never falls back to the PR inventory.
        """
        self.evidence_step = "merge_result"
        if pr.mergeable is not True or pr.merge_commit_sha is None:
            raise MergeResultPending(
                "GitHub has not computed this PR's merge result yet; try again shortly"
            )
        commit = MergeCommit.model_validate(
            await self._request("GET", f"/repos/{name}/commits/{pr.merge_commit_sha}")
        )
        if commit.sha != pr.merge_commit_sha:
            raise GitHubError
        if [parent.sha for parent in commit.parents] != [base_sha, head_sha]:
            raise MergeResultPending(
                "GitHub's merge result does not match the current default branch "
                "and PR head yet; try again shortly"
            )
        # Compare lists changed files only on the first page, up to 300 for
        # the whole comparison; per_page=1 keeps the commit list small.
        comparison = Comparison.model_validate(
            await self._request(
                "GET",
                f"/repos/{name}/compare/{base_sha}...{pr.merge_commit_sha}",
                params={"per_page": 1, "page": 1},
            )
        )
        if (
            comparison.status != "ahead"
            or comparison.behind_by != 0
            or comparison.ahead_by < 1
            or comparison.base_commit.sha != base_sha
            or comparison.merge_base_commit.sha != base_sha
        ):
            raise GitHubError
        if len(comparison.files) >= MERGE_FILES_LIMIT:
            raise PolicyError(
                "merge_result",
                f"merge result changes {MERGE_FILES_LIMIT} or more files, too many "
                "to evaluate completely; split the PR or update the branch",
            )
        files = [changed_file(item) for item in comparison.files]
        if len({item.filename for item in files}) != len(files):
            raise GitHubError
        return files

    async def checks(self, name, sha, base, *, enforce_policy=True, behind=False):
        """CI evidence for ``sha``. Merge policy refusals raise PolicyError.

        ``behind`` (GitHub's mergeable_state) turns a strict-checks refusal into
        the actionable out-of-date reason.

        With ``enforce_policy=False`` (observation only, never merge evidence),
        refusals are listed in ``policy_rejections`` instead. A refused source
        of required checks marks them incomplete, so CI is never green.
        """
        self.evidence_step = "checks"
        rejections = []
        incomplete = False

        def refuse(reason, *, required_incomplete=False):
            nonlocal incomplete
            if enforce_policy:
                raise PolicyError("rules", reason) from None
            rejections.append(reason)
            incomplete |= required_incomplete

        # Collection time must not age a pre-grace observation into eligibility.
        captured_at = datetime.now(timezone.utc)
        root = f"/repos/{name}"
        self.evidence_step = "check_suites"
        suites = await self.pages(
            f"{root}/commits/{sha}/check-suites", key="check_suites", limit=999
        )
        suite_ids = set()
        validated_suites = []
        for item in suites:
            suite = Suite.model_validate(item)
            if suite.head_sha != sha or suite.id in suite_ids:
                raise GitHubError
            suite_ids.add(suite.id)
            validated_suites.append(suite)
        self.evidence_step = "check_runs"
        raw = await self.pages(
            f"{root}/commits/{sha}/check-runs",
            key="check_runs",
            params={"filter": "all"},
        )
        latest = {}
        suites_with_runs = set()
        ids = set()
        for item in raw:
            run = Check.model_validate(item)
            sid = run.check_suite.id
            if run.head_sha != sha or sid not in suite_ids or run.id in ids:
                raise GitHubError
            ids.add(run.id)
            # Include superseded runs when deciding whether a suite is runless.
            suites_with_runs.add(sid)
            key = (run.app.id, run.name)
            rank = run.id
            if key not in latest or rank > latest[key][0]:
                latest[key] = (rank, run)
        self.evidence_step = "statuses"
        statuses = {}
        ids = set()
        for item in await self.pages(f"{root}/commits/{sha}/statuses"):
            status = Status.model_validate(item)
            if status.id in ids:
                raise GitHubError
            ids.add(status.id)
            old = statuses.get(status.context)
            if old is None or (status.created_at, status.id) > (old.created_at, old.id):
                statuses[status.context] = status
        unavailable = []
        # The protection endpoint returns 404 for ruleset-only/unprotected
        # branches. Other reads, including active rules, must still succeed.
        self.evidence_step = "protection"
        try:
            protection = await self._request(
                "GET", f"{root}/branches/{quote(base, safe='')}/protection"
            )
        except GitHubPlanUnavailable:
            unavailable.append("protection")
            protection = {"required_status_checks": None}
        except GitHubNotFound:
            protection = {"required_status_checks": None}
        required = []
        try:
            protection = Protection.model_validate(protection)
        except ValueError:
            refuse(
                "unsupported classic branch protection fields",
                required_incomplete=True,
            )
            protection = None
        if protection is not None:
            if (
                protection.required_pull_request_reviews is not None
                or protection.restrictions is not None
            ):
                refuse("unsupported classic branch protection")
            for field in (
                "block_creations",
                "required_conversation_resolution",
                "required_signatures",
                "lock_branch",
            ):
                flag = getattr(protection, field)
                if flag is not None and flag.enabled:
                    refuse(f"unsupported classic protection: {field}")
            # Squash preserves linear history; no force push, deletion or fork sync
            # is performed. Admin enforcement applies the same evaluated policy.
            classic = protection.required_status_checks
            if classic is not None:
                if classic.strict:
                    refuse(
                        OUT_OF_DATE
                        if behind
                        else "unsupported classic strict status checks"
                    )
                required.extend((c.context, c.app_id) for c in classic.checks)
                required.extend((c, None) for c in classic.contexts)
        self.evidence_step = "rules"
        try:
            rules = await self.pages(f"{root}/rules/branches/{quote(base, safe='')}")
        except GitHubPlanUnavailable:
            unavailable.append("rules")
            rules = []
        for raw_rule in rules:
            try:
                rule = Rule.model_validate(raw_rule)
            except ValueError:
                if enforce_policy:
                    raise
                refuse("malformed active branch rule", required_incomplete=True)
                continue
            if rule.type == "required_status_checks" and rule.parameters is not None:
                try:
                    parameters = RuleParameters.model_validate(rule.parameters)
                except ValueError:
                    refuse(
                        "unsupported required_status_checks parameters",
                        required_incomplete=True,
                    )
                    continue
                if parameters.strict_required_status_checks_policy:
                    refuse(
                        OUT_OF_DATE
                        if behind
                        else "unsupported strict_required_status_checks_policy"
                    )
                required.extend(
                    (c.context, c.integration_id)
                    for c in parameters.required_status_checks
                )
            elif rule.type == "pull_request":
                try:
                    parameters = PullRequestParameters.model_validate(rule.parameters)
                except ValueError:
                    refuse(
                        "unsupported pull_request parameters",
                        required_incomplete=True,
                    )
                    continue
                refusal = parameters.unsupported()
                if refusal is not None:
                    refuse(
                        f"unsupported pull_request requirement: {refusal.reason}",
                        required_incomplete=refusal.required_incomplete,
                    )
                # Our pinned squash PUT already goes through a PR.
            elif rule.type not in (
                "deletion",
                "non_fast_forward",
                "required_linear_history",
            ):
                # An unknown rule may itself require checks (e.g. workflows).
                known = rule.type in KNOWN_RULE_TYPES
                refuse(
                    "unsupported active branch rule: "
                    + (rule.type if known else UNKNOWN_BRANCH_RULE),
                    required_incomplete=True,
                )
        self.evidence_step = "checks_consistency"
        for context, app in required:
            if (
                not isinstance(context, str)
                or not context
                or (app is not None and type(app) is not int)
            ):
                raise GitHubError
        required_apps = {app for _, app in required if app not in (None, -1)}
        ignored_suites = []
        pending_suite = False
        blocked_suite = False
        for suite in validated_suites:
            if suite.abandoned(captured_at, suites_with_runs, required_apps):
                ignored_suites.append(
                    {
                        **suite.model_dump(mode="json", exclude_unset=True),
                        "reason": "queued_without_runs_past_grace_and_no_required_app",
                    }
                )
                continue
            pending_suite |= suite.status in PENDING_CHECK_STATES
            # Every completed suite must succeed, even if it emitted no runs or
            # a newer suite/run succeeded. Unknown states also fail closed.
            blocked_suite |= (
                suite.conclusion != "success"
                if suite.status == "completed"
                else suite.status not in PENDING_CHECK_STATES
            )
        green = bool(latest or statuses) and not pending_suite and not blocked_suite
        green &= all(
            r.status == "completed" and r.conclusion == "success"
            for _, r in latest.values()
        )
        green &= all(s.state == "success" for s in statuses.values())
        green &= not incomplete
        for context, app in required:
            green &= any(
                n == context
                and (app in (None, -1) or a == app)
                and r.status == "completed"
                and r.conclusion == "success"
                for (a, n), (_, r) in latest.items()
            ) or (
                app in (None, -1)
                and context in statuses
                and statuses[context].state == "success"
            )
        blocked = (
            blocked_suite
            or any(
                (
                    r.conclusion != "success"
                    if r.status == "completed"
                    else r.status not in PENDING_CHECK_STATES
                )
                for _, r in latest.values()
            )
            or any(s.state not in ("success", "pending") for s in statuses.values())
        )
        result = {
            "head_sha": sha,
            "captured_at": captured_at.isoformat().replace("+00:00", "Z"),
            "complete": True,
            "blocked": bool(blocked),
            "green": bool(green),
            "pending": not blocked
            and (
                pending_suite
                or any(r.status in PENDING_CHECK_STATES for _, r in latest.values())
                or any(s.state == "pending" for s in statuses.values())
            ),
            "suites": [
                s.model_dump(mode="json", exclude_unset=True) for s in validated_suites
            ],
            "ignored_suites": ignored_suites,
            "checks": [r.model_dump(mode="json") for _, r in latest.values()],
            "statuses": [s.model_dump(mode="json") for s in statuses.values()],
            "required": required,
            "github_rules_unavailable_on_plan": unavailable,
        }
        if not enforce_policy:
            result["policy_rejections"] = rejections
            result["required_checks_incomplete"] = incomplete
        return result

    async def observation(self, name, number):
        """Bounded read of any PR state; unavailable checks remain unknown.

        A closed PR is not merge execution evidence. Re-read the PR after CI so
        movement during collection cannot attribute checks to another head.
        Merge-policy refusals do not hide CI; they are reported alongside it.
        """
        self.evidence_step = "pull_request"
        pr = await self.pull(name, number)
        try:
            ci = await self.checks(
                name,
                pr.head.sha,
                pr.base.ref,
                enforce_policy=False,
                behind=pr.mergeable_state == "behind",
            )
        except (GitHubError, PolicyError, ValueError) as error:
            logger.warning(
                "PR CI observation unavailable for %s#%s at %s: %s%s",
                name,
                number,
                self.evidence_step,
                type(error).__name__,
                f" {error.message}" if isinstance(error, PolicyError) else "",
            )
            ci = None
        else:
            if ci["policy_rejections"]:
                logger.info(
                    "PR %s#%s is not mergeable by Mainloop policy: %s",
                    name,
                    number,
                    "; ".join(ci["policy_rejections"]),
                )
        self.evidence_step = "pull_request_refresh"
        fresh = await self.pull(name, number)
        if fresh != pr:
            raise GitHubError
        return pr, ci

    async def merge(self, name, number, sha):
        return await self._request(
            "PUT",
            f"/repos/{name}/pulls/{number}/merge",
            json={"sha": sha, "merge_method": "squash"},
        )
