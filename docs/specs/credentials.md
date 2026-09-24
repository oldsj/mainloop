# Agent credentials and sign-in

Mainloop owns one credential set per account and provider. Real Codex `auth.json` and Claude
tokens are stored in pre-created Kubernetes Secrets. The backend may seed those Secrets from
configured file paths (`SUBSTRATE_CODEX_AUTH_PATH` and `SUBSTRATE_CLAUDE_TOKEN_PATH`); file
contents are never exposed through the API or logs. The egress credential provider consumes
the Secret's `injection-value` key.

## Actor boundary

Actors do not receive real provider credentials. Codex actors get a synthetic `auth.json` with
inert JWTs, an empty refresh token, a recent `last_refresh`, and the account ID. Claude actors
get a synthetic CLI token. The egress provider injects the current real token on approved
provider requests. The backend checks the Codex access JWT expiry and disables injection when
it is within five minutes of expiry. It does not implement a refresh-token exchange because the
request payload and rotation behavior have not been verified.

## Attention and recovery

If a configured credential is missing or expiring, Mainloop marks a delivery as not sent and
creates a deduplicated session attention item such as “Codex needs sign-in.” A failed native
turn whose shim status identifies an HTTP 401 or provider authentication failure also creates
that attention item. The original turn is not replayed automatically.

From the workspace page, the owner can start a provider sign-in job. The control-side Job runs
`codex login --device-auth` or `claude setup-token`, displays a filtered HTTPS device challenge
when the CLI provides one, and sends the result to a short-lived authenticated callback. The
broker validates the result and replaces the provider Secret data. Real credentials are kept
out of job logs, challenge responses, and browser responses.

## Implementation limits and evidence

Broker and runner behavior is covered by fake-backed tests. The Kubernetes Job path is not live
verified. Its image must contain the matching native CLI binaries and
`/usr/local/bin/mainloop-reauth`, and must be selected with `SUBSTRATE_REAUTH_JOB_IMAGE`. The
credential egress provider remains a separate Substrate deployment contract; this spec does not
claim that publishing the Secret key alone deploys or configures that provider. No real
credentials are included in tests or fixtures.

The native CLIs have not been live tested with the new Claude placeholder token. Re-auth job
state and callback authorization are held in backend memory; restarting the backend during a
sign-in attempt requires the owner to start that attempt again.
