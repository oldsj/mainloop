"""Resolve a new checkout once, before its durable create identity is frozen."""

import asyncio
from urllib.parse import quote

from mainloop.runtime.policy import PolicyError
from mainloop.services.github_auth import REQUEST_TIMEOUT_SECONDS, bounded_response
from mainloop.services.github_creation import Commit, GitHubCreationClient, GitHubError
from mainloop.services.github_repo import parse_github_repo


class CheckoutRefUnavailable(ValueError):
    """Owner-safe refusal; upstream bodies and credentials must not escape."""


async def resolve_checkout_ref(repository: str, ref: str) -> str:
    repository = parse_github_repo(repository).full_name.lower()
    try:
        async with GitHubCreationClient(repository) as client:
            if not ref:
                repo = await client.repo(repository)
                if repo.full_name.lower() != repository or not repo.default_branch:
                    raise CheckoutRefUnavailable
                ref = repo.default_branch
            # This is always a commit lookup, including refs named feature/statuses or
            # feature/check-runs. Select contents:read explicitly so a decoded ref suffix
            # cannot select the generic client's status/check endpoint permissions.
            async with asyncio.timeout(REQUEST_TIMEOUT_SECONDS):
                token = await client.auth.token(
                    client.client, repository, {"contents": "read"}
                )
                response = await bounded_response(
                    client.client,
                    "GET",
                    f"/repos/{repository}/commits/{quote(ref, safe='')}",
                    headers={"Authorization": f"Bearer {token}"},
                )
            return Commit.model_validate(response.json()).sha
    except (GitHubError, PolicyError, ValueError, TimeoutError):
        raise CheckoutRefUnavailable(
            "Checkout ref could not be resolved to a GitHub commit."
        ) from None
