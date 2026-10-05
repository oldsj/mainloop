"""Per-binding identity; raw tokens never enter an agent workspace."""

import hashlib
import hmac

from mainloop.config import settings


def _key() -> str:
    """Return the HMAC key: ``AGENT_TOKEN_KEY``, or the DB password in dev mode only."""
    return settings.agent_token_key or (settings.db_password if settings.is_dev else "")


def require_token_key() -> None:
    """Fail at startup when the agent token key is missing outside dev mode."""
    if not _key():
        raise RuntimeError(
            "AGENT_TOKEN_KEY must be set (DEV_MODE=true allows the DB password instead)"
        )


def token_for(session_id: str) -> str:
    key = _key()
    if not key:
        raise RuntimeError(
            "AGENT_TOKEN_KEY must be set (DEV_MODE=true allows the DB password instead)"
        )
    return (
        "ml_" + hmac.new(key.encode(), session_id.encode(), hashlib.sha256).hexdigest()
    )


def hash_token(token: str) -> str:
    return hashlib.sha256(token.encode()).hexdigest()
