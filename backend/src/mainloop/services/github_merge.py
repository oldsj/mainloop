"""Bounded, fixed-origin merge evidence. No remote writes during preparation."""

from datetime import datetime
from urllib.parse import quote

from mainloop.runtime.policy import PolicyError
from mainloop.services.github_creation import (
    GitHubCreationClient,
    GitHubError,
    PullRequest,
    Repo,
)
from pydantic import BaseModel, ConfigDict, Field

from models.hitl import normalized_hash
from models.merge_policy import ChangedPath, protected_matches

PENDING_CHECK_STATES = frozenset(
    {"queued", "in_progress", "pending", "waiting", "requested"}
)


class MergeRepo(Repo):
    allow_squash_merge: bool


class MergePR(PullRequest):
    draft: bool
    changed_files: int = Field(ge=0, le=3000)
    mergeable: bool | None
    mergeable_state: str
    merged: bool
    merge_commit_sha: str | None = None


class App(BaseModel):
    model_config = ConfigDict(strict=True)
    id: int = Field(gt=0)


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


class Suite(BaseModel):
    model_config = ConfigDict(strict=True)
    id: int = Field(gt=0)
    head_sha: str = Field(pattern=r"^[0-9a-f]{40}$")
    status: str = Field(max_length=32)
    conclusion: str | None = Field(max_length=64)


class RequiredCheck(BaseModel):
    model_config = ConfigDict(strict=True)
    context: str = Field(min_length=1, max_length=512)
    app_id: int | None = None
    integration_id: int | None = None


class ClassicChecks(BaseModel):
    model_config = ConfigDict(strict=True)
    checks: list[RequiredCheck]
    contexts: list[str] = []


class Protection(BaseModel):
    model_config = ConfigDict(strict=True)
    required_status_checks: ClassicChecks | None
    required_pull_request_reviews: dict | None = None
    restrictions: dict | None = None


class RuleParameters(BaseModel):
    model_config = ConfigDict(strict=True)
    required_status_checks: list[RequiredCheck]


class Rule(BaseModel):
    model_config = ConfigDict(strict=True)
    type: str
    parameters: RuleParameters | None = None


class Status(BaseModel):
    id: int = Field(gt=0)
    context: str = Field(min_length=1, max_length=512)
    state: str = Field(max_length=32)
    created_at: datetime


class GitHubMergeClient(GitHubCreationClient):
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
        repo = await self.repository(name)
        pr = await self.pull(name, number)
        if (
            repo.full_name.lower() != name.lower()
            or not repo.allow_squash_merge
            or pr.number != number
            or pr.state != "open"
            or pr.draft
            or pr.merged
            or pr.head.sha != sha
            or pr.head.ref == repo.default_branch
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
        base = await self.branch(name, repo.default_branch)
        if base.name != pr.base.ref or base.commit.sha != pr.base.sha:
            raise PolicyError("base", "default branch moved; prepare again")
        raw = await self.pages(f"/repos/{name}/pulls/{number}/files", limit=3000)
        paths = [
            ChangedPath.model_validate(
                {k: p[k] for k in ("filename", "status", "previous_filename") if k in p}
            )
            for p in raw
        ]
        if len(paths) != pr.changed_files or len({p.filename for p in paths}) != len(
            paths
        ):
            raise GitHubError
        matches = protected_matches(paths, complete=True)
        ci = await self.checks(name, sha, repo.default_branch)
        fresh = await self.pull(name, number)
        if fresh != pr or await self.repository(name) != repo:
            raise PolicyError(
                "stale", "PR or repository changed while reading evidence"
            )
        return {
            "repository_id": repo.id,
            "repository": name,
            "pr_number": number,
            "head_sha": sha,
            "head": pr.head.ref,
            "base": pr.base.ref,
            "base_sha": pr.base.sha,
            "files_digest": normalized_hash(
                sorted(
                    (p.model_dump(mode="json") for p in paths),
                    key=lambda p: p["filename"],
                )
            ),
            "protected_matches": list(matches),
            "ci": ci,
            "mergeable": pr.mergeable is True
            and pr.mergeable_state in ("clean", "unstable", "has_hooks"),
        }

    async def checks(self, name, sha, base):
        root = f"/repos/{name}"
        suites = await self.pages(
            f"{root}/commits/{sha}/check-suites", key="check_suites", limit=999
        )
        suite_ids = set()
        pending_suite = False
        blocked_suite = False
        normalized_suites = []
        for item in suites:
            suite = Suite.model_validate(item)
            if suite.head_sha != sha or suite.id in suite_ids:
                raise GitHubError
            suite_ids.add(suite.id)
            normalized_suites.append(suite.model_dump(mode="json"))
            pending_suite |= suite.status in PENDING_CHECK_STATES
            # Every completed suite must succeed, even if it emitted no runs or
            # a newer suite/run succeeded. Unknown states also fail closed.
            blocked_suite |= (
                suite.conclusion != "success"
                if suite.status == "completed"
                else suite.status not in PENDING_CHECK_STATES
            )
        raw = await self.pages(
            f"{root}/commits/{sha}/check-runs",
            key="check_runs",
            params={"filter": "all"},
        )
        latest = {}
        ids = set()
        for item in raw:
            run = Check.model_validate(item)
            sid = run.check_suite.id
            if run.head_sha != sha or sid not in suite_ids or run.id in ids:
                raise GitHubError
            ids.add(run.id)
            key = (run.app.id, run.name)
            rank = run.id
            if key not in latest or rank > latest[key][0]:
                latest[key] = (rank, run)
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
        # Never interpret a 404 as proof of absent classic protection: it may be
        # missing permission. Require explicit successful inventory instead.
        protection = await self._request(
            "GET", f"{root}/branches/{quote(base, safe='')}/protection"
        )
        protection = Protection.model_validate(protection)
        if protection.required_pull_request_reviews or protection.restrictions:
            raise PolicyError("rules", "unsupported classic branch protection")
        required = []
        classic = protection.required_status_checks
        if classic is not None:
            required.extend((c.context, c.app_id) for c in classic.checks)
            required.extend((c, None) for c in classic.contexts)
        rules = await self.pages(f"{root}/rules/branches/{quote(base, safe='')}")
        for raw_rule in rules:
            rule = Rule.model_validate(raw_rule)
            if rule.type == "required_status_checks" and rule.parameters is not None:
                required.extend(
                    (c.context, c.integration_id)
                    for c in rule.parameters.required_status_checks
                )
            elif rule.type not in (
                "deletion",
                "non_fast_forward",
                "required_linear_history",
            ):
                raise PolicyError("rules", "unsupported active branch rule")
        green = bool(latest or statuses) and not pending_suite and not blocked_suite
        green &= all(
            r.status == "completed" and r.conclusion == "success"
            for _, r in latest.values()
        )
        green &= all(s.state == "success" for s in statuses.values())
        for context, app in required:
            if (
                not isinstance(context, str)
                or not context
                or (app is not None and type(app) is not int)
            ):
                raise GitHubError
            green &= any(
                n == context and (app in (None, -1) or a == app) for a, n in latest
            ) or (app in (None, -1) and context in statuses)
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
        return {
            "green": bool(green),
            "pending": not blocked
            and (
                pending_suite
                or any(r.status in PENDING_CHECK_STATES for _, r in latest.values())
                or any(s.state == "pending" for s in statuses.values())
            ),
            "suites": normalized_suites,
            "checks": [r.model_dump(mode="json") for _, r in latest.values()],
            "statuses": [s.model_dump(mode="json") for s in statuses.values()],
            "required": required,
        }

    async def merge(self, name, number, sha):
        return await self._request(
            "PUT",
            f"/repos/{name}/pulls/{number}/merge",
            json={"sha": sha, "merge_method": "squash"},
        )
