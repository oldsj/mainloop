"""Repository cache service for fast planning access.

Manages cached git repositories on a PVC for fast read access during planning.
Clones repos on first access, pulls to refresh on subsequent accesses.
"""

import asyncio
import logging
import os
import re
from pathlib import Path

import git
from mainloop.config import settings

logger = logging.getLogger(__name__)

# Default cache path (can be overridden via settings)
DEFAULT_CACHE_PATH = Path("/repos")


class RepoCache:
    """Manages cached git repositories for planning access."""

    def __init__(self, cache_path: Path | None = None):
        """Initialize the repo cache.

        Args:
            cache_path: Base path for cached repos. Defaults to /repos (PVC mount).

        """
        self.cache_path = cache_path or DEFAULT_CACHE_PATH
        self._lock = asyncio.Lock()

    def _parse_repo_url(self, repo_url: str) -> tuple[str, str]:
        """Parse GitHub repo URL into owner and name.

        Args:
            repo_url: GitHub repository URL (e.g., https://github.com/owner/repo)

        Returns:
            Tuple of (owner, repo_name)

        Raises:
            ValueError: If URL format is invalid

        """
        # Handle both https://github.com/owner/repo and https://github.com/owner/repo.git
        pattern = r"https://github\.com/([^/]+)/([^/]+?)(?:\.git)?$"
        match = re.match(pattern, repo_url)
        if not match:
            raise ValueError(f"Invalid GitHub repo URL: {repo_url}")
        return match.group(1), match.group(2)

    def get_repo_path(self, repo_url: str) -> Path:
        """Get the local path for a cached repository.

        Args:
            repo_url: GitHub repository URL

        Returns:
            Path to the cached repo directory

        """
        owner, name = self._parse_repo_url(repo_url)
        return self.cache_path / owner / name

    def _get_authenticated_url(self, repo_url: str) -> str:
        """Get repo URL with authentication token embedded.

        Args:
            repo_url: Original GitHub repo URL

        Returns:
            URL with x-access-token authentication

        """
        token = settings.github_token
        if not token:
            logger.warning("No GITHUB_TOKEN available, using unauthenticated access")
            return repo_url

        # Convert https://github.com/owner/repo to https://x-access-token:TOKEN@github.com/owner/repo
        return repo_url.replace(
            "https://github.com/", f"https://x-access-token:{token}@github.com/"
        )

    async def _clone_repo(self, repo_url: str, target: Path) -> None:
        """Clone a repository to the target path.

        Uses shallow clone for faster initial checkout.

        Args:
            repo_url: GitHub repository URL
            target: Local path to clone to

        """
        auth_url = self._get_authenticated_url(repo_url)

        def _do_clone():
            target.parent.mkdir(parents=True, exist_ok=True)
            logger.info(f"Cloning {repo_url} to {target}")
            git.Repo.clone_from(
                auth_url,
                target,
                depth=1,  # Shallow clone for speed
                single_branch=True,
            )

        # Run blocking git operation in thread pool
        loop = asyncio.get_event_loop()
        await loop.run_in_executor(None, _do_clone)
        logger.info(f"Successfully cloned {repo_url}")

    async def _pull_repo(self, repo_path: Path) -> None:
        """Pull latest changes for an existing repository.

        Args:
            repo_path: Path to the local repository

        """

        def _do_pull():
            repo = git.Repo(repo_path)
            origin = repo.remotes.origin

            # Update remote URL with current token (in case it changed)
            # This is safe because we control the cache directory
            logger.info(f"Pulling latest changes for {repo_path}")
            origin.pull()

        # Run blocking git operation in thread pool
        loop = asyncio.get_event_loop()
        await loop.run_in_executor(None, _do_pull)
        logger.info(f"Successfully pulled {repo_path}")

    async def ensure_fresh(self, repo_url: str) -> Path:
        """Ensure a repository is cached and up-to-date.

        Clones the repo if not present, pulls if already cached.

        Args:
            repo_url: GitHub repository URL

        Returns:
            Path to the cached repository

        Raises:
            ValueError: If repo URL is invalid
            git.GitCommandError: If git operation fails

        """
        repo_path = self.get_repo_path(repo_url)

        async with self._lock:
            if repo_path.exists() and (repo_path / ".git").exists():
                # Repo exists, try to pull latest
                try:
                    await self._pull_repo(repo_path)
                except git.GitCommandError as e:
                    # Pull failed - could be network issue, no remote, etc.
                    # Just use existing repo rather than re-cloning
                    logger.warning(
                        f"Pull failed for {repo_url}, using existing cached repo: {e}"
                    )
                except Exception as e:
                    # Catch any other errors (e.g., no remote configured)
                    logger.warning(
                        f"Could not update {repo_url}, using existing cached repo: {e}"
                    )
            else:
                # Repo doesn't exist, clone it
                await self._clone_repo(repo_url, repo_path)

        return repo_path

    async def is_cached(self, repo_url: str) -> bool:
        """Check if a repository is already cached.

        Args:
            repo_url: GitHub repository URL

        Returns:
            True if repo is cached, False otherwise

        """
        repo_path = self.get_repo_path(repo_url)
        return repo_path.exists() and (repo_path / ".git").exists()


# Global instance (initialized on first use)
_repo_cache: RepoCache | None = None


def get_repo_cache() -> RepoCache:
    """Get the global RepoCache instance."""
    global _repo_cache
    if _repo_cache is None:
        # Use environment variable if set, otherwise default
        cache_path_str = os.environ.get("REPO_CACHE_PATH")
        cache_path = Path(cache_path_str) if cache_path_str else None
        _repo_cache = RepoCache(cache_path)
    return _repo_cache
