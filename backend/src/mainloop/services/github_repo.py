"""Parse and canonicalise a GitHub repository reference.

The one place that turns user input (``owner/name`` or an ``https://github.com`` URL) into a
repository identity. Case is kept for display; GitHub names are case-insensitive, so the
database keys projects on the lower-cased ``full_name``. Everything that stores or clones a user-supplied repository goes through
``parse_github_repo`` so a project has one canonical ``full_name`` and ``html_url``.
"""

from __future__ import annotations

import re
from dataclasses import dataclass
from urllib.parse import urlsplit

# GitHub login: alphanumerics and hyphens, no leading or trailing hyphen, at most 39 characters.
_OWNER = re.compile(r"[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?")
# Repository name: alphanumerics, ``.``, ``_`` and ``-``, at most 100 characters.
_NAME = re.compile(r"[A-Za-z0-9._-]{1,100}")


class InvalidGithubRepo(ValueError):
    """The text is not a GitHub repository reference this service accepts."""


@dataclass(frozen=True)
class GithubRepo:
    owner: str
    name: str

    @property
    def full_name(self) -> str:
        return f"{self.owner}/{self.name}"

    @property
    def html_url(self) -> str:
        return f"https://github.com/{self.full_name}"


def parse_github_repo(value: str) -> GithubRepo:
    """Return the repository named by ``owner/name`` or ``https://github.com/owner/name[.git]``.

    Strict on purpose: only github.com over https, exactly an owner and a name (one optional
    trailing slash), no credentials, port, query or fragment. Raises ``InvalidGithubRepo``.
    """
    text = value.strip()
    if not text:
        raise InvalidGithubRepo("Repository is required")
    if "://" in text:
        text = _path_of_github_url(text)
    parts = text.removesuffix("/").split("/")
    if len(parts) != 2:
        raise InvalidGithubRepo(
            "Repository must be owner/name or https://github.com/owner/name"
        )
    owner, name = parts
    name = re.sub(r"\.git$", "", name, flags=re.IGNORECASE)
    if not _OWNER.fullmatch(owner):
        raise InvalidGithubRepo(f"Invalid GitHub owner: {owner!r}")
    if not _NAME.fullmatch(name) or name in (".", ".."):
        raise InvalidGithubRepo(f"Invalid GitHub repository name: {name!r}")
    return GithubRepo(owner=owner, name=name)


def _path_of_github_url(url: str) -> str:
    try:
        parsed = urlsplit(url)
        hostname = parsed.hostname
        port = parsed.port
    except ValueError as exc:
        raise InvalidGithubRepo("Repository URL is not valid") from exc
    if parsed.scheme != "https" or (hostname or "").lower() != "github.com":
        raise InvalidGithubRepo("Only https://github.com repositories are supported")
    if parsed.username or parsed.password or port or parsed.query or parsed.fragment:
        raise InvalidGithubRepo(
            "Repository URL must not carry credentials, a port, a query or a fragment"
        )
    return parsed.path.removeprefix("/")
