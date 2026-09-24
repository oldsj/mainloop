"""Private callback endpoint for control-side credential re-auth jobs."""

from __future__ import annotations

import json

from fastapi import APIRouter, Header, HTTPException, Request, Response
from mainloop.runtime.credential_broker import CredentialBroker, CredentialBrokerError
from mainloop.runtime.workspace_api import _reauth_runner

router = APIRouter(tags=["internal credential re-auth"])
_MAX_CALLBACK_BYTES = 300 * 1024


@router.post("/internal/reauth/{job_id}", status_code=204)
async def receive_reauth_result(
    job_id: str,
    request: Request,
    callback_token: str | None = Header(default=None, alias="X-Mainloop-Reauth-Token"),
):
    provider = await _reauth_runner.callback_provider(job_id, callback_token or "")
    if provider is None:
        raise HTTPException(status_code=401, detail="Invalid re-auth callback")
    raw = await request.body()
    if len(raw) > _MAX_CALLBACK_BYTES:
        raise HTTPException(status_code=413, detail="Re-auth result is too large")
    try:
        payload = json.loads(raw)
    except (json.JSONDecodeError, UnicodeDecodeError) as exc:
        raise HTTPException(status_code=400, detail="Invalid re-auth result") from exc
    if not isinstance(payload, dict) or payload.get("provider") != provider:
        raise HTTPException(status_code=400, detail="Invalid re-auth result")
    broker = CredentialBroker()
    try:
        if provider == "codex" and isinstance(payload.get("auth"), dict):
            await broker.store_codex_auth_document(
                json.dumps(payload["auth"], separators=(",", ":")).encode("utf-8")
            )
        elif provider == "claude" and isinstance(payload.get("token"), str):
            await broker.store_claude_token(payload["token"])
        else:
            raise CredentialBrokerError("invalid re-auth result")
    except CredentialBrokerError as exc:
        raise HTTPException(
            status_code=400, detail="Credential result was rejected"
        ) from exc
    await _reauth_runner.complete(job_id, callback_token or "")
    return Response(status_code=204)
