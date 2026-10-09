"""Keep githubkit's response schemas over the shared repository-scoped transport."""

from typing import Any

from githubkit import GitHub, Response
from mainloop.runtime.policy import PolicyError
from mainloop.services.github_auth import app_auth, repository_name
from mainloop.services.github_creation import GitHubCreationClient


class RepositoryGitHub(GitHub):
    def __init__(self, repository: str, *, transport=None):
        self.repository = repository_name(repository)
        app_auth()  # Validate configuration before monitoring's optional-error handlers.
        self._transport = transport
        super().__init__(
            base_url="https://api.github.com",
            follow_redirects=False,
            trust_env=False,
            auto_retry=False,
            timeout=10,
        )

    async def arequest(self, method, url, *, response_model=Any, **kwargs):
        # REST helpers supply their schema, params, JSON and optional media headers.
        # Authentication, deadlines, size limits and scope come from the same client
        # as creation/merge. No SDK retry loop or unbounded SDK body read runs.
        async with GitHubCreationClient(
            self.repository, transport=self._transport
        ) as client:
            response = await client._response(
                method,
                str(url),
                **{
                    key: value
                    for key, value in kwargs.items()
                    if key in ("params", "json") and value is not None
                }
            )
        return Response(response, response_model)

    def request(self, *_args, **_kwargs):
        raise PolicyError("github", "synchronous GitHub access is unsupported")
