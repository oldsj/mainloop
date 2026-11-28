# Devices

The owner's phones call the owner API with a key that never leaves the phone. Each registered
device holds a P-256 **request key** in its Secure Enclave and signs every request with it. The
server stores only the public key, so nothing in the database or its backups lets anyone sign as a
device. Implemented in `server/internal/auth` (the verifier) and `server/internal/devices` (the
routes and store), and in the iPhone app ([iOS app](ios-app.md)).

Status: tested with software keys and fixtures, and a request token and an approval token signed by
the app's code verify in the server's tests. Not yet proven against a real phone or Pocket ID.

## Registering a phone

1. The app creates a new request key in the Secure Enclave (usable after the phone's first unlock,
   on this device only, never synced or backed up).
2. It signs the owner in with Pocket ID itself: `ASWebAuthenticationSession` in a private session
   with `prompt=login` and `max_age=600` (so the ID token carries `auth_time`), the
   authorization-code flow with PKCE, against a **public** OIDC client of
   its own (no client secret). The server names that client in `MAINLOOP_AUTH_DEVICE_CLIENT_ID`.
   The request's `nonce` is the new key's [RFC 7638](https://www.rfc-editor.org/rfc/rfc7638)
   thumbprint, so the ID token Pocket ID returns names the key it may register.
3. It calls `POST /devices` with the Pocket ID ID token as `Authorization: Bearer`, a name, the
   platform, the key's public JWK and, when the phone could make one, its **approval key**'s public
   JWK (below). The response's `id` (`dev_` and 26 characters) names the
   device from then on.
4. The app then forgets the Pocket ID token. It keeps the device id, the origin and the key's
   Secure Enclave handle in one keychain item (this device only, never synced), and signs only for
   that origin. If the keychain refuses the item, the app revokes the new device with the key it
   still holds.

`POST /devices` is refused (`403`) unless the token:

- is the owner's (the same issuer, subject and group checks as every owner token, see
  [authentication](authentication.md));
- came as a bearer, not the login cookie;
- names the app's client in its audience: the web app's tokens can't register a device;
- has an `auth_time` within the last 10 minutes (and at most a minute ahead): when the owner signed
  in, which a token refresh doesn't renew, unlike `iat`;
- has a `nonce` equal to the thumbprint of the key in the request (checked by the server).

A device token, a ServiceAccount (even the owner-scoped operator) and the login cookie can never
register a device, so a stolen phone cannot enroll another one. A token whose audience is only the
app's client is good for `POST /devices` and nothing else. Registration is single-use both ways: a
key can be registered once, and so can a sign-in (its token's `jti` is recorded); either again is
`409`. A phone that signed out makes a new key and signs in again.

Every registration opens a high-priority **Needs you** item, "New device registered: _name_", with
the device id, the start of its key's thumbprint (and of its approval key's) and when it was
registered. The request key outlives
the sign-in that registered it, so a device the owner did not register must be seen. Its answers:

- **Revoke** revokes the device in the same transaction as the answer, and ends its open requests.
- **Keep** closes the item.

Only the owner's browser sign-in answers it. A device's answer is refused (`403`), so a device
registered by someone else can't keep itself.

## Signed requests

Every other request carries a fresh token in `Authorization: Bearer`, a JWT the request key signs:

| Part         | Value                                                                                       |
| ------------ | ------------------------------------------------------------------------------------------- |
| header `alg` | `ES256`                                                                                     |
| header `typ` | `mainloop-device+jwt`                                                                       |
| header `kid` | the device id                                                                               |
| `iss`        | `mainloop-device`                                                                           |
| `sub`        | the device id (must equal `kid`)                                                            |
| `aud`        | the instance's origin, `scheme://host[:port]`, lowercased (the server's frontend origin)    |
| `iat`, `exp` | required; `exp` after `iat` and at most 60 seconds after it                                 |
| `jti`        | 16 to 128 characters, unique per request                                                    |
| `htm`        | the request method                                                                          |
| `htu`        | the path and query the API sees: behind the gateway, without the `/api` prefix              |
| `bh`         | the unpadded base64url SHA-256 of the request body (of the empty string when there is none) |

The server:

- looks the device up on every request, so a revoked or unknown device is refused (`401`) on its
  next request;
- checks the signature, the claims above, and the clock with 30 seconds of leeway either way;
- reads the body (up to the API's 8 MiB cap) to check `bh`, then hands the same body on;
- refuses a token it has already accepted (`401`). Every token is remembered in the process for its
  lifetime. A write's token (any method but `GET`, `HEAD` and `OPTIONS`) is also recorded in the
  database (`device_token_uses`, pruned once expired), so it is good once across replicas and
  restarts. A read's token could be replayed once on another replica within its minute, for the same
  path only;
- records `last_seen_at`, at most once a minute per device.

A verified device acts with the `owner` scope. Writes follow the [owner-write
rule](authentication.md#owner-writes): the app sends `X-Mainloop-Client: ios` and no `Origin`.

A device can't answer a registration's Needs-you item (above). It approves a privileged action
only with a signature of its approval key (below); it may answer anything else on that card, such
as Deny, and answer other cards.

## The approval key

A phone also registers a second P-256 key in its Secure Enclave, which signs approvals of
[privileged actions](privileged-actions.md#signed-approvals). Every signature asks for Touch ID
(or Face ID) with the fingers enrolled when the key was made, so enrolling another one makes the
key unusable. It needs a passcode, and a phone without one, or without enrolled biometry, registers
without an approval key.

- It comes with `POST /devices` as `approval_key` or never. It must be a P-256 point other than the
  request key (`422`), it registers once like the request key (`409`), and it can't be added,
  changed or removed afterwards (the same trigger refuses it). A phone registered without one signs
  out and in again to get one.
- The owner's **Keep** on the registration item covers both keys.
- `GET /devices` returns its thumbprint and public JWK as `approval_key`, or `null`. The owner pins
  that JWK in the executor's configuration; see [privileged
  actions](privileged-actions.md#signed-approvals).

Requests stay open no longer than their device: revoking a device ends its open streams (the event
stream and a live session) in the process that revoked it at once, and in other replicas within 15
seconds.

## The app's web view

The iPhone app shows the web app itself in a web view ([iOS app](ios-app.md)). A web view can't
sign each request with the device key, so the app trades one signed request for a web session:

- `POST /devices/current/web-session`, signed by the device key, answers `201` with
  `cookie_name` (`__Host-mainloop-device`), a `token` (32 random bytes, base64url) and
  `expires_at`, an hour on. Only a request the device key signed starts one: not a web session, so
  a page can't extend its own, and not a Pocket ID sign-in (`403`). A device has one session at a
  time: a new one ends its earlier ones. The token is returned once. The server keeps only its
  SHA-256 (`device_web_sessions`), and expired rows are pruned.
- `DELETE /devices/current/web-session`, signed by the device key, ends the device's sessions
  (`204`). Revoking the device, which signing out does, ends them too.
- The app sets the token as an HttpOnly, Secure, host-only cookie on path `/`, so the page's
  scripts can't read it. The `__Host-` prefix makes a browser refuse such a cookie from any other
  host, so a page on a sibling host (a workspace preview) can't plant one. The app starts a new
  session each time it comes to the foreground, before the current one runs out, and when the page
  reports a `401`.
- A request with the cookie acts as the device (via `session`): the owner's scope, the write checks
  of [authentication](authentication.md#owner-writes) (the page sends `X-Mainloop-Client: web` and
  the app's own `Origin`), and every device limit. It can't register a device, answer a device
  registration, revoke another device, start or end a web session, set or clear the device's push
  token, or approve a privileged action without the phone's Touch ID signature (the page asks the
  app for that; see [iOS app](ios-app.md)).
- A stream the session holds open (the event stream, a live session) ends when the session
  expires, and when it is replaced or ended: at once in the process that ended it, and in other
  replicas within 15 seconds. Revoking the device ends it as above.

## Managing devices

| Operation                             | Who                                                                                            |
| ------------------------------------- | ---------------------------------------------------------------------------------------------- |
| `GET /devices`                        | the owner and devices: newest first, revoked ones included; `current` marks the calling device |
| `GET /devices/current`                | a device: itself; `404` for anyone else                                                        |
| `DELETE /devices/{device_id}`         | the owner, any device; a device, only itself (signing out). Idempotent                         |
| `PUT /devices/current/push`           | a signed device request: sets its APNs token ([push](push-notifications.md#apple-push-apns))   |
| `DELETE /devices/current/push`        | a signed device request: clears its APNs token. Idempotent                                     |
| `POST /devices/current/web-session`   | a signed device request: starts its web session ([above](#the-apps-web-view))                  |
| `DELETE /devices/current/web-session` | a signed device request: ends its web sessions. Idempotent                                     |

A device's web session can't call the four `current` writes: they are the app's own business.

Revoking stops the device's key at once and keeps the row for the audit. A revoked device can't be
brought back, and a device's key and identity can't change (a database trigger refuses both).

The web app's Settings has a **Devices** section that lists every device, revoked ones included:
its name, platform, when it was registered and last seen (or never), its id and the start of its
key's thumbprint (and of its approval key's, or "no approval key"), and **Active** or **Revoked**
with the date. An active device with an approval key has **Pin its approval key for the
executor**, which shows the entry to add to the chart's `executor.approvalKeys.pinned` (with `executor.approvals.mode: signed`). An active device has **Revoke**,
which asks for confirmation, then reads the list again. A device the owner did not register can be
seen and stopped there. An active device's facts also say whether it gets notifications: **on**,
**off** (it set no APNs token), or **stopped** when Apple refused its token. The token itself is
never returned.

## Through the gateway

The admin gateway's Pocket ID login (proposed, see [authentication](authentication.md)) would send
a request without its cookie through the browser login. When it lands, native requests need its
`passThroughAuthHeader`, which lets a request through only if the gateway's JWT filter accepts the
bearer. That needs either a JWT provider for `mainloop-device` with the device keys as a key set, or
a route for the app that the login does not cover. `serve-api` verifies every device token itself
either way. Until then the app reaches the API directly.
