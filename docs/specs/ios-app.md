# iOS app

A native iPhone app for the owner, laid out for iPhone Duo (Apple's foldable: a 5.4-inch outer
screen and a 7.6-inch inner screen). The source is in `ios/`; see [ios/README.md](../../ios/README.md)
to build it.

## Implemented

- **Three tabs:** Needs you (with the count as a badge), Sessions and Settings. On iPhone Duo the
  system puts the tab bar on a side edge, except on the inner screen in portrait.
- **Layouts by size class.** On the outer screen (compact width) a list pushes its detail. On the
  inner screen (regular width and height) the list and the open item sit side by side in a split
  arrangement, which places its panes around the fold. The open item stays open through folding
  and unfolding. Content is padded clear of the camera regions where the safe area doesn't
  already cover them.
- **Needs you** lists decisions first and problems in their own section, with the same rows as
  [the inbox](inbox.md). A card shows its details, options and expiry, and takes an option or a
  free-text reply. Problems can't be answered.
- **Privileged cards** (see [privileged actions](privileged-actions.md)) show the action type, the
  action id and the first 16 characters of the digest.
  - A card counts as privileged when its context has any `privileged_action_*` key. If those facts
    are malformed, the card shows them as missing.
  - **Review and approve** is offered only when this phone has an approval key (below) and the
    card's action id and digest (64 lowercase hex characters) are well formed. The phone never
    signs the card's word for what it approves:
    1. It fetches the action's `canonical_record` (`GET /privileged-actions/{id}`) and computes its
       SHA-256 itself. Unless that is the card's digest, and the record names the card's action id
       (and type, when the card names one), it refuses and signs nothing.
    2. It opens the **approval sheet**, built only from the record it parsed: the type, every
       argument, every fact preflight froze (a diff, the current state), who asked, the expiry,
       the action id and the digest.
       - A list of words, such as an `argv`, is quoted as one would type it, by the web card's
         rule: a word of letters, digits and `-_./:=,@` as is, any other in single quotes.
       - Every string of several lines nested in a fact, such as each change's `diff`, gets a
         block of its own after the fact, and the fact's JSON says `⟨below: changes[0].diff⟩`
         where it was. A diff is shown line by line, with added lines green and removed lines red.
       - Control and format characters (bidi overrides, zero-width characters, line separators)
         and `⟨` itself are shown as ⟨U+XXXX⟩, never applied, so the record can't imitate the
         sheet's own markers.
    3. The sheet's **Approve** asks for Touch ID ("Approve _type_ (_action id_)", from the record),
       signs an approval token with the approval key for the record's action id and the digest the
       phone computed, and sends `Approve` with `metadata.reviewed_context` set to that digest and
       `metadata.approval` set to the token. The request itself is signed with the request key, as
       every request is.

    The executor runs an action only when the canonical record it computes from the stored row
    has the signed digest, so what runs is what the sheet showed.

  - **The approval token** is a compact JWS: header `{"alg":"ES256","typ":"mainloop-approval+jwt","kid":<device id>}`,
    claims `iss` `mainloop-device`, `sub` the device id, `aud` the instance's origin (as for request
    tokens), `iat`, `exp` (`iat` + 120 seconds), a random `jti`, `act` the action id and `dig` the
    digest; the signature is the raw r‖s, unpadded base64url.
  - A cancelled Touch ID prompt sends nothing, shows nothing and keeps the sheet open. A finger not recognised, Touch ID
    unavailable, or an approval key the Secure Enclave no longer accepts (as after the enrolled
    fingers change) sends nothing and says so. A `403` shows the server's reason.
  - On a phone registered without an approval key the card says "Sign out and in again to approve
    privileged actions on this iPhone." instead. A card with malformed facts can't be approved.
  - Any other Approve, tapped or typed (in any letter case), is never sent on a privileged card:
    the server refuses an unsigned Approve from a phone (see [privileged
    actions](privileged-actions.md#signed-approvals)).
  - **Deny** and a note (a denial with that text) are offered as before.

- **Drafts stay with their item.** A note typed on a card, or a message typed to a session, belongs
  to that card or session. Changing the selection in the side-by-side layout shows the other item's
  own draft, never carries text over, and coming back restores it.
- **Sessions** lists sessions that need the owner first, then live ones, then the rest, newest first,
  without archived ones. A session shows its status and summary, takes a message (each message
  carries a `message_id`, kept while the text is unchanged so a retry is one message), and opens its transcript in the web
  app in Safari's in-app browser.
- **Signing in** (Settings, **This iPhone**): **Sign in with Pocket ID** makes a new request key
  in the Secure Enclave, opens Pocket ID in a private browser session (no shared cookies; it always
  asks) for that key, and registers the phone ([devices](devices.md)). Mainloop then shows a
  Needs-you item for the new device, answered from the web app. From then on every request is
  signed with that key. The section shows the device id and **Sign out**, which revokes the device
  on the server and deletes the key; offline, the key is still deleted. It needs
  `MAINLOOP_OIDC_ISSUER` and `MAINLOOP_OIDC_CLIENT_ID` in the build; without them it reads "Not
  configured". The simulator, which has no Secure Enclave, uses a software key; a phone never
  does.
- **The approval key.** Signing in also makes a second Secure Enclave key, sent as `approval_key`
  when the phone registers. It needs a passcode set, stays on this phone, and asks for the
  currently enrolled Touch ID fingers (or Face ID) at every signature, so enrolling a new finger
  makes it unusable. It is kept in the same keychain item as the request key, and signing out
  deletes both. A phone without a passcode or enrolled biometry registers without one. Settings ›
  **This iPhone** shows **Approvals: Touch ID**, or **Not set up** with "Sign out and in again to
  set up approvals" for a phone registered before approval keys. The simulator uses a software
  approval key, which asks for nothing. Built, not yet proven on a phone.
- **Notifications** through APNs (the server side is in [push notifications](push-notifications.md)).
  Built, not yet proven on a phone:
  - **When it asks.** Right after this phone registers, iOS's permission prompt asks for alerts,
    sounds and badges. It never asks while signed out. If the owner hasn't answered it, Settings ›
    **This iPhone** shows **Notifications: Not asked yet** with **Turn on**; if notifications are
    off, **Off** with **Turn on in Settings**, which opens the app's page in the Settings app.
  - **The token.** While signed in with notifications allowed, the app sends this phone's APNs
    token to `PUT /devices/current/push`, signed with the request key, with the APNs environment
    the build's entitlement names: on every launch, on each return to the foreground, and whenever
    iOS hands it a new token. If the server reports that Apple rejected the token, Settings says
    so.
  - **A tap** on a Needs-you notification opens that card (the list on the outer screen pushes
    it; on the inner screen it opens beside the list). If the card is gone, answered elsewhere,
    the list opens. A task notification opens the task's page of the web app in Safari's in-app
    browser, as there is no native task screen yet. Anything the app doesn't recognise opens Needs
    you.
  - **In the foreground** a notification shows as a banner and Needs you reloads.
  - **The lock screen.** iOS's own **Show Previews** setting decides whether the title and text
    show there. With previews hidden it shows "Mainloop needs you" for a Needs-you notification and
    "A task changed" for a task. Needs-you notifications are time sensitive.
  - **No actions** on a notification: approving asks for Touch ID, which needs the app in the
    foreground, so a card is answered in the app.
  - **The badge** on the app icon is the Needs-you count, set whenever the list loads or a card is
    answered. **Sign out** unregisters from APNs and clears the badge and the delivered
    notifications; its revoke clears the device's token on the server.
- **Sample data.** A build whose origin is a placeholder (`*.example.test`, `*.example`) or empty
  runs on fictional fixtures. Answers and messages then change only the app's copy. Approve on a
  sample privileged card is signed with a throwaway software key, without Touch ID.

## Not built yet

These are planned (see the iOS plan in the pull request that added `ios/`). None is live:

- through the admin gateway once its login lands (see [devices](devices.md), "Through the gateway");
- Approve and Deny as notification actions;
- native task views and chat;
- voice: on-device dictation first, then real-time voice with main and top-level supervisors.

## Not proven

The app's tests run on the simulator in CI, with sample data. Request signing is tested with
software keys, and a token the app's code signed is checked by the server's verifier in Go
(`server/internal/auth/testdata`). Nothing has run on an iPhone Duo, against Pocket ID or against a
Mainloop server. Notifications are tested only as far as `swift test` reaches: the token's
encoding, the signed `PUT /devices/current/push` request and how a payload maps to a screen, all
with fictional data. No notification has been delivered to a phone through APNs.

Approvals are tested with software keys: the token's header, claims and signature, the
registration's second key, the respond body, and that Approve is offered only with an approval
key and well-formed facts. An approval token this code signed is checked by the server's verifier
in Go (`server/internal/approval/testdata`). The record's digest is checked against a vector the
Go server wrote (`server/internal/privileged/testdata/canonical-record.json`): its facts keep
number text, HTML characters, U+2028, a character outside the BMP and a bidi override, and the
app's fixture is a copy of it. A record that isn't the card's is refused before anything is signed.
The approval sheet itself is compiled by CI but not tested. Nothing has run on a phone: the Touch ID prompt, the
Secure Enclave key's access control (`.biometryCurrentSet`) and what happens after re-enrolling
are untested, as CI's simulator uses a software key.
