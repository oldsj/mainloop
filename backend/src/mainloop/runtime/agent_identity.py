"""Per-binding identity; raw tokens never enter an agent workspace."""

import hashlib
import hmac

from mainloop.config import settings


def token_for(session_id: str) -> str:
    key = settings.agent_token_key or settings.db_password
    if not key:
        raise RuntimeError(
            "AGENT_TOKEN_KEY (or DB password) must be set to issue agent tokens"
        )
    return (
        "ml_" + hmac.new(key.encode(), session_id.encode(), hashlib.sha256).hexdigest()
    )


def hash_token(token: str) -> str:
    return hashlib.sha256(token.encode()).hexdigest()
