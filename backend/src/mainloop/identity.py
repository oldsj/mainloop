"""Who is making the request.

Mainloop has one configured owner and is reached only over the tailnet, so every request is the
owner's. ``current_user`` is the one place that decides that. It deliberately ignores any
``X-User-ID`` header: nothing authenticates that header, so honouring it would let any caller pick
an identity.

A later multi-user setup reads the identity a trusted gateway sets, here and only here. Every
handler and the preview proxy take their user from this function, so nothing else changes.
"""

from mainloop.config import settings


def current_user() -> str:
    """Return the user making the request: the configured owner."""
    return settings.owner_id
