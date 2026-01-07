"""Unit tests for the repo cache service."""

import tempfile
from pathlib import Path

import pytest
from mainloop.services.repo_cache import RepoCache


class TestRepoCache:
    """Test RepoCache functionality."""

    def test_parse_repo_url_https(self):
        """Test parsing standard HTTPS URLs."""
        cache = RepoCache(cache_path=Path("/tmp/test-repos"))

        owner, name = cache._parse_repo_url("https://github.com/oldsj/mainloop")
        assert owner == "oldsj"
        assert name == "mainloop"

    def test_parse_repo_url_with_git_suffix(self):
        """Test parsing URLs with .git suffix."""
        cache = RepoCache(cache_path=Path("/tmp/test-repos"))

        owner, name = cache._parse_repo_url("https://github.com/oldsj/mainloop.git")
        assert owner == "oldsj"
        assert name == "mainloop"

    def test_parse_repo_url_invalid(self):
        """Test that invalid URLs raise ValueError."""
        cache = RepoCache(cache_path=Path("/tmp/test-repos"))

        with pytest.raises(ValueError, match="Invalid GitHub repo URL"):
            cache._parse_repo_url("not-a-url")

        with pytest.raises(ValueError, match="Invalid GitHub repo URL"):
            cache._parse_repo_url("https://gitlab.com/owner/repo")

    def test_get_repo_path(self):
        """Test repo path generation."""
        cache = RepoCache(cache_path=Path("/repos"))

        path = cache.get_repo_path("https://github.com/oldsj/mainloop")
        assert path == Path("/repos/oldsj/mainloop")

    def test_get_repo_path_with_git_suffix(self):
        """Test repo path generation handles .git suffix."""
        cache = RepoCache(cache_path=Path("/repos"))

        path = cache.get_repo_path("https://github.com/oldsj/mainloop.git")
        assert path == Path("/repos/oldsj/mainloop")

    @pytest.mark.asyncio
    async def test_ensure_fresh_clones_new_repo(self):
        """Test that ensure_fresh clones a repo that doesn't exist."""
        with tempfile.TemporaryDirectory() as tmpdir:
            cache = RepoCache(cache_path=Path(tmpdir))

            # Use a small public repo for testing
            repo_url = "https://github.com/octocat/Hello-World"

            # This will clone the repo
            path = await cache.ensure_fresh(repo_url)

            assert path.exists()
            assert (path / ".git").exists()
            assert (path / "README").exists()

    @pytest.mark.asyncio
    async def test_ensure_fresh_pulls_existing_repo(self):
        """Test that ensure_fresh pulls an existing repo."""
        with tempfile.TemporaryDirectory() as tmpdir:
            cache = RepoCache(cache_path=Path(tmpdir))

            repo_url = "https://github.com/octocat/Hello-World"

            # Clone first
            path1 = await cache.ensure_fresh(repo_url)
            assert path1.exists()

            # Second call should pull (not clone)
            path2 = await cache.ensure_fresh(repo_url)
            assert path2 == path1
            assert path2.exists()
