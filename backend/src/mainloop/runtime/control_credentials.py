"""Bounded, reloadable control credentials. Never retain secret bytes in settings."""

from pathlib import Path

CONTROL_TOKEN_MAX_BYTES = 4096


class ControlCredentialError(ValueError):
    """The configured control credential cannot be used."""


def read_control_token(path: str) -> str:
    try:
        with Path(path).open("rb") as source:
            raw = source.read(CONTROL_TOKEN_MAX_BYTES + 1)
    except OSError:
        raise ControlCredentialError(
            "KAGENT_CONTROL_TOKEN_FILE must name a readable token file"
        ) from None
    if len(raw) > CONTROL_TOKEN_MAX_BYTES:
        raise ControlCredentialError("KAGENT_CONTROL_TOKEN_FILE exceeds 4096 bytes")
    try:
        token = raw.decode("ascii").removesuffix("\n").removesuffix("\r")
    except UnicodeDecodeError:
        raise ControlCredentialError(
            "KAGENT_CONTROL_TOKEN_FILE must contain an ASCII bearer"
        ) from None
    if not token or any(ord(char) <= 32 or ord(char) >= 127 for char in token):
        raise ControlCredentialError(
            "KAGENT_CONTROL_TOKEN_FILE must contain one nonempty bearer"
        )
    return token
