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
| Canary, in-pod smoke and operator ServiceAccount tokens (chart); `--token-file`                | implemented                      |
| Web app sends `X-Mainloop-Client`, re-login on `401`, sign-out in Settings                     | implemented                      |
| Production web build served same-origin at `/api` (needs the gateway's `/api` route first)     | implemented (image build)        |
| Pocket ID login at the admin gateway (Envoy SecurityPolicy) for dev, then prod                 | proposed (deployment repository) |
| Device-signed requests from the owner's phones ([devices](devices.md))                         | implemented                      |
| `enforce` on dev, then prod; `mainloop-api.<domain>` route removed                             | proposed                         |

Workspace previews (`*.preview.<domain>`) are routed ahead of this layer. They have their own
sign-in, a per-host preview cookie granted by `POST /workspaces/{id}/previews/{port}/grant` to the
verified owner in every mode but `off` (see [workspaces](workspaces.md), "Preview sign-in").

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

**The app's own sign-in.** With `MAINLOOP_AUTH_DEVICE_CLIENT_ID` set (the iPhone app's public
OIDC client; it must differ from the web audience), a token whose audience is that client also
passes the checks above, but it may only call `POST /devices`. Only such a token registers a
device: a bearer (not the cookie) whose `auth_time` is within the last 10 minutes and whose `nonce`
is the new key's thumbprint. See [devices](devices.md).

**A device.** A JWT signed by a registered phone's Secure Enclave key (`iss` `mainloop-device`),
bound to one request's method, path and body, at most 60 seconds long and good once. It is checked
against the registered, unrevoked devices on every request. Device tokens are refused when
`serve-api` has no database. See [devices](devices.md).

**A ServiceAccount.** A projected token issued for `MAINLOOP_AUTH_SA_AUDIENCE` (default
`mainloop-api`). The backend checks it with a TokenReview, which also refuses a token whose pod is
gone. This needs `create` on `tokenreviews` (`system:auth-delegator`), and works only in the cluster.

- Results are cached by token hash: an accepted token for up to a minute (never past its expiry), a
  refused one for 10 seconds.
- At most 8 reviews run at once.
- A token that doesn't name the audience, or has expired, is refused without a review.
- `MAINLOOP_AUTH_SERVICE_ACCOUNTS` lists the allowed accounts as `namespace/name=scope`, comma-separated.
  An account not on the list is refused with `403`.

## Machine callers

With `auth.serviceAccounts.enabled` (the default; off in the Kind install), the chart renders the
ServiceAccounts below and lists them in `MAINLOOP_AUTH_SERVICE_ACCOUNTS`. It also renders a
ClusterRole `mainloop-tokenreview-<namespace>` that grants `create` on `tokenreviews` and nothing
else, bound to the release's `mainloop-backend`. Extra entries (another instance's canary) go in
`auth.serviceAccounts.extra`. Setting `MAINLOOP_AUTH_SERVICE_ACCOUNTS` in `config` replaces the list.

| ServiceAccount      | Scope    | Token                                                                                                                                    |
| ------------------- | -------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| `mainloop-canary`   | `canary` | projected into the canary CronJob, 10 minutes                                                                                            |
| `mainloop-backend`  | `read`   | projected into each backend container, one hour, for `mainloop smoke --read-only` run in the pod                                         |
| `mainloop-operator` | `owner`  | none mounted; a namespace admin mints one: `kubectl -n <namespace> create token mainloop-operator --audience mainloop-api --duration 1h` |

`mainloop canary` and `mainloop smoke` send the bearer from `--token-file` (default
`$MAINLOOP_API_TOKEN_FILE`, which the chart sets to the mounted token). They re-read it on every
request, so a token the kubelet rotates is picked up. They send it only to the owner API, never to
the MCP or Git listeners, and they send `X-Mainloop-Client`.

**Break-glass**, when Pocket ID is down or the gateway login is broken: port-forward the backend and
call it with an operator token.

```sh
kubectl --context <context> -n mainloop port-forward svc/mainloop-backend 18000:8000 &
TOKEN=$(kubectl --context <context> -n mainloop create token mainloop-operator --audience mainloop-api --duration 1h)
curl -H "Authorization: Bearer $TOKEN" -H 'Host: <API host>' http://127.0.0.1:18000/attention
```

Anyone who can mint that token is already a namespace admin who could read the backend's Secrets,
so this path gives nobody new access. The token expires on its own.

## Scopes

| Scope    | May call                                                                                                                                                 |
| -------- | -------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `owner`  | everything (Pocket ID owner; a registered device; the break-glass operator ServiceAccount), except that only a fresh Pocket ID bearer registers a device |
| `read`   | `GET`/`HEAD` operations except `GET /sessions/{id}/live` (`mainloop-backend`: in-pod `smoke --read-only`)                                                |
| `canary` | list and create sessions, send a message, read the conversation and native strip, stop the turn, restart, cancel, archive ([canary](canary.md))          |

## Owner writes

An owner or device request with an unsafe method (anything but `GET`, `HEAD` or `OPTIONS`) must carry
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
