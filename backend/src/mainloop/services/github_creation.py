"""Fixed-host PR creation; PostgreSQL owns authority and durable creation intent.

There are no automatic POST retries, including after restart. A lost response is reconciled
only by listing the persisted repo/head/base tuple. Raw GitHub errors never leave this module.
"""

from __future__ import annotations

import asyncio
import hashlib
import json
import logging
from urllib.parse import quote

import httpx
from mainloop.db import db
from mainloop.db.postgres import PRCreationConflict
from mainloop.runtime.policy import PolicyError
from mainloop.services.github_auth import (
    GitHubError,
    GitHubRefusal,
    app_auth,
    bounded_response,
    endpoint,
    http_client,
    repository_name,
)
from mainloop.services.workspace_authority import (
    ScopeUnavailable,
    repository_scope,
)
from mainloop.tasks import publication
from pydantic import BaseModel, ConfigDict, Field, ValidationError

from models.agent_tools import OpenPullRequest

REQUEST_TIMEOUT_SECONDS = 15
logger = logging.getLogger(__name__)


class Repo(BaseModel):
    model_config = ConfigDict(strict=True)
    id: int = Field(gt=0)
    full_name: str
    default_branch: str


class Commit(BaseModel):
    sha: str = Field(pattern=r"^[0-9a-f]{40}$")


class Branch(BaseModel):
    name: str
    commit: Commit


class PRRef(BaseModel):
    ref: str
    sha: str = Field(pattern=r"^[0-9a-f]{40}$")
    repo: Repo


class PullRequest(BaseModel):
    model_config = ConfigDict(strict=True)
    number: int = Field(gt=0)
    state: str
    head: PRRef
    base: PRRef


class GitHubCreationClient:
    def __init__(
        self, repository: str, *, transport: httpx.AsyncBaseTransport | None = None
    ):
        self.repository_name = repository_name(repository)
        self.auth = app_auth()
        self.client = http_client(transport)

    async def __aenter__(self):
        return self

    async def __aexit__(self, *_):
        await self.client.aclose()

    def _permissions(self, method: str, path: str) -> dict[str, str]:
        return endpoint(self.repository_name, method, path)

    async def _response(self, method: str, path: str, *, refusal=None, **kwargs):
        permissions = self._permissions(method, path)
        try:
            async with asyncio.timeout(REQUEST_TIMEOUT_SECONDS):
                token = await self.auth.token(
                    self.client, self.repository_name, permissions
                )
                # Internal callers cannot substitute credential or routing headers.
                kwargs["headers"] = {"Authorization": f"Bearer {token}"}
                return await bounded_response(
                    self.client,
                    method,
                    path,
                    refusal=refusal,
                    **kwargs,
                )
        except (httpx.HTTPError, ValueError, TimeoutError):
            if refusal is not None and refusal.status is not None:
                raise refusal from None
            raise GitHubError from None

    async def _request(self, method: str, path: str, **kwargs):
        response = await self._response(method, path, **kwargs)
        try:
            return response.json()
        except ValueError:
            raise GitHubError from None

    async def repo(self, full_name: str) -> Repo:
        return Repo.model_validate(await self._request("GET", f"/repos/{full_name}"))

    async def branch(self, full_name: str, branch: str) -> Branch:
        return Branch.model_validate(
            await self._request(
                "GET", f"/repos/{full_name}/branches/{quote(branch, safe='')}"
            )
        )

    async def find(self, full_name: str, head: str, base: str) -> list[PullRequest]:
        owner = full_name.split("/")[0]
        found = []
        # Reconstruct every page at the fixed origin; never follow upstream Link URLs.
        for page in range(1, 11):
            data = await self._request(
                "GET",
                f"/repos/{full_name}/pulls",
                params={
                    "state": "all",
                    "head": f"{owner}:{head}",
                    "base": base,
                    "per_page": 100,
                    "page": page,
                },
            )
            if not isinstance(data, list) or len(data) > 100:
                raise GitHubError
            found.extend(PullRequest.model_validate(p) for p in data)
            if len(data) < 100:
                return found
        raise GitHubError

    async def create(
        self, full_name: str, body: OpenPullRequest, base: str
    ) -> PullRequest:
        return PullRequest.model_validate(
            await self._request(
                "POST",
                f"/repos/{full_name}/pulls",
                refusal=GitHubRefusal(),
                json={
                    "head": body.branch,
                    "base": base,
                    "title": body.title,
                    "body": body.body,
                },
            )
        )


def _project_repo(project: dict, body: OpenPullRequest) -> str:
    try:
        return repository_scope(project, project_id=body.project_id, branch=body.branch)
    except (ScopeUnavailable, KeyError, TypeError):
        raise PolicyError(
            "ownership", "project and session workspace must match"
        ) from None


def _verify_repo(repo: Repo, full_name: str, body: OpenPullRequest) -> None:
    if repo.full_name.lower() != full_name.lower():
        raise PolicyError(
            "ownership", "GitHub repository identity does not match project"
        )
    if body.branch == repo.default_branch:
        raise PolicyError("branch", "default branch is not allowed")


def _verify_head(branch: Branch, body: OpenPullRequest) -> None:
    if branch.name != body.branch or branch.commit.sha != body.expected_sha:
        raise PolicyError("head", "remote feature branch does not match expected SHA")


def _result(pr: PullRequest, repo: Repo, head: str, base: str, sha: str) -> dict:
    if (
        pr.head.repo.id != repo.id
        or pr.base.repo.id != repo.id
        or pr.head.repo.full_name.lower() != repo.full_name.lower()
        or pr.base.repo.full_name.lower() != repo.full_name.lower()
        or pr.head.ref != head
        or pr.base.ref != base
        or pr.head.sha != sha
    ):
        raise PolicyError(
            "identity", "PR repository, branches or SHA do not match intent"
        )
    url = f"https://github.com/{repo.full_name}/pull/{pr.number}"
    return {
        "text": f"PR {pr.number}: {url}",
        "state": "created",
        "pr_number": pr.number,
        "url": url,
        "head_sha": sha,
        "base": base,
    }


def _uncertain(request_id: str) -> dict:
    return {
        "text": "PR creation outcome is uncertain; retry the same request_id to reconcile. "
        "No second creation POST will be sent.",
        "state": "uncertain",
        "request_id": request_id,
    }


def _refused(error: GitHubRefusal) -> dict:
    diagnostics = {"errors": error.errors}
    if error.message is not None:
        diagnostics["message"] = error.message
    if error.hints:
        diagnostics["hints"] = error.hints
    logger.warning(
        "PR creation refused: status=%s diagnostics=%s", error.status, diagnostics
    )
    message = f": {error.message}" if error.message is not None else ""
    result = {
        "text": f"GitHub refused PR creation (HTTP {error.status}){message}. "
        "No PR was created by this POST. Correct the cause and use a new request_id.",
        "state": "refused",
        "http_status": error.status,
        "github_errors": error.errors,
    }
    if error.message is not None:
        result["github_message"] = error.message
    if error.hints:
        result["github_hints"] = error.hints
    return result


async def _settled_result(binding: dict, intent: dict, full_name: str) -> dict:
    if intent["state"] == "created":
        await publication.attach_creation(db, binding, intent, full_name)
    result = intent["result"]
    return json.loads(result) if isinstance(result, str) else result


@publication.guarded(schema=OpenPullRequest)
async def open_pull_request(binding: dict, arguments: dict) -> dict:
    body = OpenPullRequest.model_validate(arguments)
    project = await db.pr_project_authority(
        binding, body.project_id, branch=body.branch
    )
    if not project:
        raise PolicyError("ownership", "no active binding for this owner-owned project")
    full_name = _project_repo(project, body)
    payload_hash = hashlib.sha256(
        json.dumps(body.model_dump(exclude={"request_id"}), sort_keys=True).encode()
    ).hexdigest()
    try:
        prior = await db.get_pr_creation_request(
            binding["user_id"], body.request_id, payload_hash
        )
    except PRCreationConflict:
        raise PolicyError("conflict", "request ID has a different payload") from None
    if prior and prior["state"] in ("created", "refused"):
        await publication.bind_creation(db, binding, prior)
        return await _settled_result(binding, prior, full_name)
    async with GitHubCreationClient(full_name) as github:
        try:
            repo = await github.repo(full_name)
            _verify_repo(repo, full_name, body)
            if not prior:
                _verify_head(await github.branch(full_name, body.branch), body)
            try:
                intent, creator = await db.claim_pr_creation(
                    user_id=binding["user_id"],
                    project_id=body.project_id,
                    request_id=body.request_id,
                    payload_hash=payload_hash,
                    repo_id=repo.id,
                    head=body.branch,
                    base=repo.default_branch,
                    expected_sha=body.expected_sha,
                )
            except PRCreationConflict:
                raise PolicyError(
                    "conflict", "request ID or repo/head/base has a different payload"
                ) from None
            await publication.bind_creation(db, binding, intent, newly_claimed=creator)
            if intent["state"] in ("created", "refused"):
                return await _settled_result(binding, intent, full_name)
            matches = await github.find(full_name, intent["head"], intent["base"])
            if len(matches) > 1:
                return _uncertain(body.request_id)
            if matches:
                result = _result(
                    matches[0],
                    repo,
                    intent["head"],
                    intent["base"],
                    intent["expected_sha"],
                )
                settled = await db.finish_pr_creation(intent["id"], result)
                return await _settled_result(binding, settled, full_name)
            if not creator:
                return _uncertain(body.request_id)
            # Refresh immediately before dispatch; a moved default/base is not silently retargeted.
            fresh = await github.repo(full_name)
            _verify_repo(fresh, full_name, body)
            _verify_head(await github.branch(full_name, body.branch), body)
            current = await db.pr_project_authority(
                binding, body.project_id, branch=body.branch
            )
            if not current or _project_repo(current, body) != full_name:
                raise PolicyError(
                    "ownership", "project binding changed before PR creation"
                )
            if fresh.id != repo.id or fresh.default_branch != intent["base"]:
                raise PolicyError(
                    "base", "repository or default branch changed; creation not sent"
                )
            try:
                pr = await github.create(full_name, body, intent["base"])
                result = _result(
                    pr, repo, intent["head"], intent["base"], intent["expected_sha"]
                )
            except GitHubRefusal as error:
                result = _refused(error)
                settled = await db.refuse_pr_creation(intent["id"], result)
                return await _settled_result(binding, settled, full_name)
            except (GitHubError, ValidationError, PolicyError):
                # POST may have succeeded even when its response is unusable. Never repeat it.
                return _uncertain(body.request_id)
            settled = await db.finish_pr_creation(intent["id"], result)
            return await _settled_result(binding, settled, full_name)
        except (GitHubError, ValidationError):
            raise PolicyError(
                "github",
                "GitHub request failed or returned invalid evidence; retry to reconcile",
            ) from None
