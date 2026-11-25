# iOS app

An iPhone app for the owner: the web app in a native shell that holds the device's keys. The source is in `ios/`; see [ios/README.md](../../ios/README.md)
to build it.

## Implemented

Built and tested as far as CI reaches (see [Not proven](#not-proven)); nothing here has run on a
phone yet.

The app is a shell around the web app: Mainloop's own web UI in a web view, with native only for
what must stay on the phone: sign-in and pairing, the device keys, notifications, and the Touch ID
approval sheet. The native screens use the web app's default theme (`eighties` in
`web/design/tokens.json`; a test checks every colour against it), square corners and the system
monospaced font, which is the font the web's stack (`ui-monospace`) resolves to. The app icon is
the web's mark, `>_` (`web/public/favicon.svg`), drawn by `ios/Design/make-app-icon.py`.

### First run

1. **A build that names no server** (an empty, invalid or placeholder `MAINLOOP_ORIGIN`:
   `*.example.test`, `*.example`) shows "No server in this build" and which settings to set. It
   never shows a raw error.
2. **Welcome.** **Sign in with Pocket ID** pairs this iPhone (below). A build without a Pocket ID
   issuer and client says sign-in isn't set up and which settings it needs. A failed sign-in says
   what to do in a sentence (Mainloop refused this account or client; Pocket ID or the server
   can't be reached, so connect to the home network or its VPN); a cancelled one says nothing.
   Debug builds also offer **Open without pairing**, which opens the web app as a browser would,
   for a server whose authentication only observes; nothing can be approved there, and This
   iPhone offers **Pair this iPhone**.
3. **Paired:** where the request key and the approval key live, and the server.
4. **Notifications:** **Turn on notifications** asks iOS's permission; **Not now** skips it.
5. **Quickstart:** three cards (the inbox, Approve, This iPhone), then **Open Mainloop**.

Steps 3 to 5 show once per pairing; after that the app opens straight into the web app. If the
server stops knowing the device (revoked elsewhere, or its keys reset), the app deletes its keys
and the web view's data and returns to Welcome, saying why.

### Pairing

**Sign in with Pocket ID** makes a new request key in the Secure Enclave, opens Pocket ID in a
private browser session (no shared cookies; it always asks) for that key, and registers the phone
([devices](devices.md)). From then on every request the app makes itself is signed with that key.
The simulator, which has no Secure Enclave, uses a software key; a phone never does.

**The approval key.** Pairing also makes a second Secure Enclave key, sent as `approval_key`. It
needs a passcode set, stays on this phone, and asks for the currently enrolled Touch ID fingers (or
Face ID) at every signature, so enrolling a new finger makes it unusable. Signing out deletes both
keys. A phone without a passcode or enrolled biometry registers without one. The simulator uses a
software approval key, which asks for nothing.

### The web app in the app

- **The session.** A paired phone starts a device web session with a signed
  `POST /devices/current/web-session` ([devices](devices.md#the-apps-web-view)) and sets its token
  as the cookie `__Host-mainloop-device` (host-only, `Secure`, `HttpOnly`, `SameSite=Strict`, no
  expiry) before the page loads. The session lasts an hour; the app starts a new one ten minutes
  before it ends while open, on returning to the foreground with ten minutes or less left, and when
  the page reports a `401` (at most once every 30 seconds), then reloads. Each new session ends the device's earlier ones.
- **The website data store** is the default, persistent one, so the page's own settings (its
  theme, for one) survive a relaunch. The session cookie has no expiry, so it doesn't outlive the
  app, and is set again at each launch.
- **Navigation.** The main frame stays on Mainloop's origin (same scheme, host and port). A link to
  another web page opens in a Safari sheet with Safari's own cookies. Any other scheme, including
  `javascript:` and `data:`, is refused. Frames inside a page (a workspace preview) may load web
  pages, but the message handler never answers them.
- **The message handler,** `mainloop`, answers only the main frame, and only while that frame's
  security origin and URL are Mainloop's. The page never holds a key, the session's token or a
  signature:
  - **`approve`** (`item_id`, `action_id`; any `digest` the page sends is ignored) opens the
    approval sheet below and answers `approved`, `cancelled` or `failed` with a message. One
    approval at a time.
  - **`sessionExpired`** starts a new session and reloads the page.
  - **`openDeviceSettings`** opens **This iPhone**.
- **Unreachable.** If the session can't start or the first page doesn't load, the app shows the
  mark, a sentence (connect to the home network or its VPN; or the server is too old for device
  web sessions) and **Try again**.
- **This iPhone** (from the web app's Settings): the device, where its keys live, notifications
  (with **Turn on notifications** or **Open iOS Settings**, and a token Apple stopped accepting),
  and **Sign out**, after a confirmation. Sign out ends the device's web sessions
  (`DELETE /devices/current/web-session`), revokes the device, deletes both keys, clears the web
  view's data, unregisters from APNs and clears the badge and delivered notifications. Offline,
  the keys and data are still deleted.

### Approving a privileged action

Approve on a privileged-action card in the web app asks the app; the page sends no Approve itself.
The phone never signs the page's word for what it approves:

1. It fetches the action's `canonical_record` (`GET /privileged-actions/{id}`), computes its SHA-256
   itself and checks that the record names the requested action id. Otherwise it refuses and signs
   nothing. The server binds the signed approval to the card's own action id and digest, so a page
   that pairs a card with another action is refused there too.
2. It opens the **approval sheet**, built only from the record it parsed: the type, the action id,
   the digest's first 16 characters, every argument, every fact preflight froze (a diff, the
   current state) and who asked.
   - A list of words, such as an `argv`, is quoted as one would type it, by the web card's rule: a
     word of letters, digits and `-_./:=,@` as is, any other in single quotes.
   - Every string of several lines nested in a fact, such as each change's `diff`, gets a block of
     its own after the fact, and the fact's JSON says `⟨below: changes[0].diff⟩` where it was. A
     diff is shown line by line, added lines green and removed lines red.
   - Control and format characters (bidi overrides, zero-width characters, line separators) and
     `⟨` itself are shown as ⟨U+XXXX⟩, never applied, so the record can't imitate the sheet's own
     markers.
3. The sheet's **Approve** asks for Touch ID ("Approve _type_ (_action id_)"), signs an approval
   token with the approval key for the record's action id and the digest the phone computed, and
   sends `Approve` to `POST /queue/{id}/respond` with `metadata.reviewed_context` set to that
   digest and `metadata.approval` set to the token, signed with the request key. **Cancel**, or
   swiping the sheet away, sends nothing.

The executor runs an action only when the canonical record it computes from the stored row has the
signed digest, so what runs is what the sheet showed.

- **The approval token** is a compact JWS: header `{"alg":"ES256","typ":"mainloop-approval+jwt","kid":<device id>}`,
  claims `iss` `mainloop-device`, `sub` the device id, `aud` the instance's origin (as for request
  tokens), `iat`, `exp` (`iat` + 120 seconds), a random `jti`, `act` the action id and `dig` the
  digest; the signature is the raw r‖s, unpadded base64url.
- A cancelled Touch ID prompt sends nothing, shows nothing and keeps the sheet open. A finger not recognised, Touch ID
  unavailable, or an approval key the Secure Enclave no longer accepts (as after the enrolled
  fingers change) sends nothing and says so. A `403` shows the server's reason.
- On a phone registered without an approval key, or not paired, the page shows "Pair this iPhone
  to approve privileged actions. Nothing was approved." A card with malformed facts can't be
  approved (the web card offers no Approve).
- The page never sends an Approve on a privileged card in the app; the server refuses an unsigned
  Approve from a device in any case (see [privileged actions](privileged-actions.md#signed-approvals)).
- **Deny** and a note are the web card's own, sent through the device's web session.

### Notifications

Through APNs (the server side is in [push notifications](push-notifications.md)):

- **When it asks.** At the first-run step, or from This iPhone. Never while signed out.
- **The token.** While paired with notifications allowed, the app sends this phone's APNs token to
  `PUT /devices/current/push`, signed with the request key (the web session can't call it), with
  the APNs environment the build's entitlement names: on every launch, on each return to the
  foreground, and whenever iOS hands it a new token.
- **A tap** opens its page in the web app: an attention item `queue:<id>` opens `/inbox?item=<id>`,
  a task its page, anything else `/inbox`. Before the web app is ready, the page opens once it is.
- **In the foreground** a notification shows as a banner and the badge refreshes.
- **The lock screen.** iOS's own **Show Previews** setting decides whether the title and text show
  there. With previews hidden it shows "Mainloop needs you" or "A task changed".
- **No actions** on a notification: approving asks for Touch ID, which needs the app in the
  foreground.
- **The badge** is the inbox's count (`GET /attention`), refreshed on returning to the foreground
  and when a notification arrives in the foreground.

## Not built yet

These are planned (see the iOS plan in the pull request that added `ios/`). None is live:

- through the admin gateway once its login lands (see [devices](devices.md), "Through the gateway");
- Approve and Deny as notification actions.

## Not proven

The app's tests run on the simulator in CI, and its core with `swift test`, with fictional data:
the session's cookie and its renewal, which messages the handler accepts, every navigation
decision, the first-run phases, a notification's page, and the palette against the web's tokens.
The web view itself, the handler's frame and origin check, and the native screens are compiled by
CI but not driven by a test. Request signing is tested with software keys, and a token the app's
code signed is checked by the server's verifier in Go (`server/internal/auth/testdata`). Nothing
has run on a phone, against Pocket ID or against a Mainloop server, and no notification has been
delivered through APNs.

Approvals are tested with software keys: the sheet opens on the record the phone hashed, the
respond body carries that digest whatever the page sent, a record for another action is refused
before anything is signed, and a cancelled prompt keeps the sheet. An approval token this code
signed is checked by the server's verifier in Go (`server/internal/approval/testdata`). The
record's digest is checked against a vector the Go server wrote
(`server/internal/privileged/testdata/canonical-record.json`): its facts keep number text, HTML
characters, U+2028, a character outside the BMP and a bidi override. The Touch ID prompt, the Secure
Enclave key's access control (`.biometryCurrentSet`) and what happens after re-enrolling are
untested, as CI's simulator uses a software key.
