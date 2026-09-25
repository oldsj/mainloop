#!/usr/bin/env python3
"""Send one bounded HTTP request through the explicitly forwarded Substrate router.

The request document is read from stdin. The shim bearer token is loaded from the
gate5 state file by path and is never written to stdout or stderr.
"""

import http.client
import json
import os
import sys


def main() -> int:
    try:
        request = json.load(sys.stdin)
        atespace = os.environ["LIVE_PROOF_ATESPACE"]
        actor = os.environ["LIVE_PROOF_ACTOR"]
        port = int(os.environ["LIVE_PROOF_ROUTER_PORT"])
        state_file = os.environ["LIVE_PROOF_STATE_FILE"]
        method = request["method"]
        path = request["path"]
        authenticated = bool(request.get("authenticated", True))
        body = request.get("body")
    except (KeyError, TypeError, ValueError, json.JSONDecodeError):
        print(json.dumps({"error": "invalid request configuration"}))
        return 2

    if (
        method not in {"GET", "POST"}
        or not isinstance(path, str)
        or not path.startswith("/")
    ):
        print(json.dumps({"error": "invalid request target"}))
        return 2

    headers = {"Connection": "close"}
    if authenticated:
        try:
            with open(state_file, encoding="utf-8") as state_handle:
                token = json.load(state_handle)["shim_token"]
        except (OSError, KeyError, TypeError, json.JSONDecodeError):
            print(json.dumps({"error": "shim state unavailable"}))
            return 2
        headers["Authorization"] = f"Bearer {token}"

    body_bytes = None
    if body is not None:
        body_bytes = json.dumps(body, separators=(",", ":")).encode("utf-8")
        headers["Content-Type"] = "application/json"

    connection = http.client.HTTPConnection("127.0.0.1", port, timeout=20)
    connection.set_tunnel(
        "actor-upstream:8090",
        headers={"ate-target-actor": f"{atespace}/{actor}"},
    )
    try:
        connection.request(method, path, body=body_bytes, headers=headers)
        response = connection.getresponse()
        response_bytes = response.read()
    except (OSError, http.client.HTTPException) as exc:
        print(json.dumps({"transport_error": type(exc).__name__}))
        return 1
    finally:
        connection.close()

    response_text = response_bytes.decode("utf-8", errors="replace")
    try:
        response_body = json.loads(response_text)
    except json.JSONDecodeError:
        response_body = response_text
    print(json.dumps({"status": response.status, "body": response_body}))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
