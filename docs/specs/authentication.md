# Owner API authentication

The owner API (`serve-api`, `:8000`) verifies who is calling. Being on the LAN, the tailnet, inside
the cluster or on the pod's loopback (where a `kubectl port-forward` arrives) is not an identity.
Implemented in `server/internal/auth`. It is tested with a fake OIDC issuer and a fake TokenReview,
and is not yet proven live.

## Rollout status

| Piece                                                                                          | Status                           |
| ---------------------------------------------------------------------------------------------- | -------------------------------- |
| Contract: security schemes, `401`/`403` on every operation but `/health`                       | implemented                      |
| Backend verification, scopes, write checks, `off`/`observe`/`enforce`; chart default `observe` | implemented                      |
| Canary, in-pod smoke and operator ServiceAccount tokens (chart)                                | proposed                         |
| Web app sends `X-Mainloop-Client`, re-login on `401`, sign-out in Settings                     | implemented                      |
| Production web build served same-origin at `/api` (needs the gateway's `/api` route first)     | proposed                         |
| Pocket ID login at the admin gateway (Envoy SecurityPolicy) for dev, then prod                 | proposed (deployment repository) |
| `enforce` on dev, then prod; `mainloop-api.<domain>` route removed                             | proposed                         |

Workspace previews (`*.preview.<domain>`) are routed ahead of this layer and stay unauthenticated
for now; see issue #415.

## Modes

`MAINLOOP_AUTH_MODE`:

- `observe` (chart default): every request is verified and attributed, and nothing is refused.
  What `enforce` would refuse is logged as `auth: would_refuse` with the reason, method, route
  template, principal, remote address and `X-Forwarded-For`. The first request from each principal
  is logged as `auth: principal`. Repeats of either line are suppressed for 10 minutes.
- `enforce`: such requests are refused (`auth: refused` at warn).
- `off`: no verification. Development only (`DEV_MODE` or `IS_TEST_ENV`). An unset mode means
  `off` in development. Anywhere else `serve-api` will not start without a mode, and refuses `off`.
  `enforce` also needs `MAINLOOP_AUTH_OIDC_ISSUER`, so the owner can sign in.

Switching between `observe` and `enforce` is a config change. It is the fast way back from a bad
`enforce`.

## Identities

A request to any operation except `GET /health` must carry one of these. A credential that is
present but invalid is refused; it is never treated as absent.

**The owner (Pocket ID).** A token signed by `MAINLOOP_AUTH_OIDC_ISSUER`, sent as
`Authorization: Bearer` or in the gateway's login cookie (`MAINLOOP_AUTH_OIDC_COOKIE`, default
`mainloop-id`). The bearer wins when both are present.

- Keys come from the issuer's discovery document. They are cached for an hour and fetched again for
  an unknown key id, no more than once every 30 seconds. If the issuer can't be reached, the keys
  already held keep verifying.
- The signature must be RSA, which is what Pocket ID signs with (`none`, HMAC and ECDSA are refused). `iss` must match exactly, `aud`
  must contain `MAINLOOP_AUTH_OIDC_AUDIENCE`, `exp` is required, and `exp`/`nbf` get a minute of leeway.
- `sub` must equal `MAINLOOP_AUTH_OWNER_SUBJECT`. Any other user of the issuer is refused with `403`.
- With `MAINLOOP_AUTH_OWNER_GROUP` set, the token's `groups` claim must also contain that group.
- Headers the gateway derives from claims are never read.

**A ServiceAccount.** A projected token issued for `MAINLOOP_AUTH_SA_AUDIENCE` (default
`mainloop-api`). The backend checks it with a TokenReview, which also refuses a token whose pod is
gone. This needs `create` on `tokenreviews` (`system:auth-delegator`), and works only in the cluster.

- Results are cached by token hash: an accepted token for up to a minute (never past its expiry), a
  refused one for 10 seconds.
- At most 8 reviews run at once.
- A token that doesn't name the audience, or has expired, is refused without a review.
- `MAINLOOP_AUTH_SERVICE_ACCOUNTS` lists the allowed accounts as `namespace/name=scope`, comma-separated.
  An account not on the list is refused with `403`.

## Scopes

| Scope    | May call                                                                                                                                        |
| -------- | ----------------------------------------------------------------------------------------------------------------------------------------------- |
| `owner`  | everything (Pocket ID owner; the break-glass operator ServiceAccount)                                                                           |
| `read`   | `GET`/`HEAD` operations except `GET /sessions/{id}/live` (in-pod `smoke --read-only`)                                                           |
| `canary` | list and create sessions, send a message, read the conversation and native strip, stop the turn, restart, cancel, archive ([canary](canary.md)) |

## Owner writes

An owner request with an unsafe method (anything but `GET`, `HEAD` or `OPTIONS`) must carry
`X-Mainloop-Client` (any value). If it carries an `Origin`, that must be the frontend origin, or
loopback in development. This applies to bearer requests too, because the gateway can turn the login
cookie into a bearer. A page on another origin can't add the header without a CORS preflight, and
only the frontend origin passes the preflight. That covers workspace previews, which are same-site
with the app. A native client sends the header and no `Origin`.

## The web app

The app does no login of its own: the gateway signs the owner in and sets the login cookie, and
the backend verifies it. The app's part:

- Every write carries `X-Mainloop-Client: web`. Reads, the event stream and the `/health` probe
  carry it too when the API is on the app's own origin (a relative API URL, such as `/api`). Then
  the gateway answers a lapsed sign-in with `401` instead of a login redirect that `fetch` cannot
  follow. Cross-origin, reads leave it off, because the header would cost a CORS preflight on every read.
- A `401` from the API, the event stream, or the gateway on `/health` means the sign-in lapsed.
  The app reloads the page once, a top-level navigation the gateway sends through the Pocket ID
  login and back. If another `401` comes within a minute of that reload (the gateway accepts the
  sign-in but the backend refuses it), the app does not reload again. Instead it shows **Signed
  out · Sign in again**: the phone's banner at the top of the screen, or the status bar on
  desktop. A `403` is not a lapsed sign-in and changes nothing. A `401` on `/health` does not
  count as offline.
- Settings has an **Account** section with **Sign out** (the gateway's `/oauth2/logout`), shown
  only when the API is on the app's own origin.
- The live terminal's WebSocket can't carry headers, so it relies on the cookie alone, which a
  same-origin API sends.

## Responses

- `401 {"detail":"Authentication required"}` with `WWW-Authenticate: Bearer realm="mainloop"`.
- `403 {"detail":"Not permitted"}`.

Neither says why; the reason is only logged. Both carry the CORS headers, so the web app can read
them. Authentication runs before the body cap and the request validator, so an anonymous caller
gets `401` rather than a `422` that describes the contract. A path no operation matches is still
`404`/`405`.

## Out of scope

- The MCP (`:8002`) and Git (`:8003`/`:8004`) listeners keep their own per-binding credentials
  ([credentials](credentials.md), [git transport](git-transport.md)).
- kagent never calls the owner API.
- There are no GitHub webhooks.
