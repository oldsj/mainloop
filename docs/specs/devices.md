# Devices

The owner's phones call the owner API with a key that never leaves the phone. Each registered
device holds a P-256 **request key** in its Secure Enclave and signs every request with it. The
server stores only the public key, so nothing in the database or its backups lets anyone sign as a
device. Implemented in `server/internal/auth` (the verifier) and `server/internal/devices` (the
routes and store), and in the iPhone app ([iOS app](ios-app.md)).

Status: tested with software keys and fixtures; not yet proven against a real phone or Pocket ID.

## Registering a phone

1. The app signs the owner in with Pocket ID itself: `ASWebAuthenticationSession`, the
   authorization-code flow with PKCE, against a **public** OIDC client of its own (no client
   secret). The server names that client in `MAINLOOP_AUTH_DEVICE_CLIENT_ID`.
2. It creates a new request key in the Secure Enclave (usable after the phone's first unlock, on
   this device only, never synced or backed up).
3. It calls `POST /devices` with the Pocket ID ID token as `Authorization: Bearer`, a name, the
   platform and the key's public JWK. The response's `id` (`dev_` and 26 characters) names the
   device from then on.
4. The app then forgets the Pocket ID token. It keeps only the device id and the key.

`POST /devices` is refused (`403`) unless the token:

- is the owner's (the same issuer, subject and group checks as every owner token, see
  [authentication](authentication.md));
- came as a bearer, not the login cookie;
- was issued within the last 10 minutes.

A device token, a ServiceAccount (even the owner-scoped operator) and the login cookie can never
register a device, so a stolen phone cannot enroll another one. A token whose audience is only the
app's client is good for `POST /devices` and nothing else. A key can be registered once: the same
key again is `409`, and a phone that signed out makes a new key.

## Signed requests

Every other request carries a fresh token in `Authorization: Bearer`, a JWT the request key signs:

| Part             | Value                                                                                     |
| ---------------- | ----------------------------------------------------------------------------------------- |
| header `alg`     | `ES256`                                                                                   |
| header `typ`     | `mainloop-device+jwt`                                                                     |
| header `kid`     | the device id                                                                             |
| `iss`            | `mainloop-device`                                                                         |
| `sub`            | the device id (must equal `kid`)                                                          |
| `aud`            | `mainloop-api`                                                                            |
| `iat`, `exp`     | required; `exp` after `iat` and at most 60 seconds after it                               |
| `jti`            | 16 to 128 characters, unique per request                                                  |
| `htm`            | the request method                                                                        |
| `htu`            | the path and query the API sees: behind the gateway, without the `/api` prefix            |
| `bh`             | the unpadded base64url SHA-256 of the request body (of the empty string when there is none) |

The server:

- looks the device up on every request, so a revoked or unknown device is refused (`401`) on its
  next request;
- checks the signature, the claims above, and the clock with 30 seconds of leeway either way;
- reads the body (up to the API's 8 MiB cap) to check `bh`, then hands the same body on;
- refuses a token it has already accepted (`401`). The replay cache is per process: with several
  `serve-api` replicas, a captured token could be replayed once on another replica within its
  minute, for the same method, path and body only;
- records `last_seen_at`, at most once a minute per device.

A verified device acts with the `owner` scope. Writes follow the [owner-write
rule](authentication.md#owner-writes): the app sends `X-Mainloop-Client: ios` and no `Origin`.

## Managing devices

| Operation                    | Who                                                                        |
| ---------------------------- | -------------------------------------------------------------------------- |
| `GET /devices`               | the owner and devices: newest first, revoked ones included; `current` marks the calling device |
| `GET /devices/current`       | a device: itself; `404` for anyone else                                    |
| `DELETE /devices/{device_id}` | the owner, any device; a device, only itself (signing out). Idempotent   |

Revoking stops the device's key at once and keeps the row for the audit. A revoked device can't be
brought back, and a device's key and identity can't change (a database trigger refuses both).

## Through the gateway

The admin gateway's Pocket ID login (proposed, see [authentication](authentication.md)) would send
a request without its cookie through the browser login. When it lands, native requests need its
`passThroughAuthHeader`, which lets a request through only if the gateway's JWT filter accepts the
bearer. That needs either a JWT provider for `mainloop-device` with the device keys as a key set, or
a route for the app that the login does not cover. `serve-api` verifies every device token itself
either way. Until then the app reaches the API directly.

## Not built yet

- The web app's Settings does not list or revoke devices yet; `DELETE /devices/{id}` with the
  owner's token is the way until it does.
- Push tokens and APNs (next slice), and the separate Touch ID approval key for privileged actions.
