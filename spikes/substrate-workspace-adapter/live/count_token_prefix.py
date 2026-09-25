#!/usr/bin/env python3
"""Count credential-prefix occurrences in stdin, emitting a count only."""

import os
import sys


def main() -> int:
    token_path = os.environ.get("LIVE_PROOF_TOKEN_FILE")
    if not token_path:
        print("token file path is required", file=sys.stderr)
        return 2
    try:
        with open(token_path, "rb") as token_handle:
            token = token_handle.read().strip()
    except OSError:
        print("token file is unavailable", file=sys.stderr)
        return 2
    if not token:
        print("token file is empty", file=sys.stderr)
        return 2

    prefix = token[:64]
    carry = b""
    matches = 0
    for chunk in iter(lambda: sys.stdin.buffer.read(65536), b""):
        data = carry + chunk
        matches += data.count(prefix)
        carry = data[-(len(prefix) - 1) :] if len(prefix) > 1 else b""
    print(f"matches={matches}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
