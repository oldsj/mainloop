"""Generated test-only App credentials and an offline auth endpoint facade."""

import base64
from datetime import UTC, datetime, timedelta
from unittest.mock import patch

import httpx
from cryptography.hazmat.primitives.asymmetric import rsa
from cryptography.hazmat.primitives.serialization import (
    Encoding,
    NoEncryption,
    PrivateFormat,
)
from mainloop.config import settings
from pydantic import SecretStr

KEY = rsa.generate_private_key(public_exponent=65537, key_size=2048)
PEM = KEY.private_bytes(Encoding.PEM, PrivateFormat.PKCS8, NoEncryption())
ENCODED_KEY = base64.b64encode(PEM).decode()


def app_settings():
    return patch.multiple(
        settings, github_app_id="123", github_app_private_key=SecretStr(ENCODED_KEY)
    )


def app_transport(handler):
    async def handle(request):
        if request.url.path.endswith("/installation"):
            return httpx.Response(200, json={"id": 456})
        if request.url.path == "/app/installations/456/access_tokens":
            return httpx.Response(
                201,
                json={
                    "token": "secret-fixture",
                    "expires_at": (datetime.now(UTC) + timedelta(hours=1)).isoformat(),
                },
            )
        result = handler(request)
        if hasattr(result, "__await__"):
            result = await result
        return result

    return httpx.MockTransport(handle)
