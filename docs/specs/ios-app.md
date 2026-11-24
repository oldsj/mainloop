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
  - The phone never sends an Approve on a privileged card, whether tapped or typed (in any letter
    case): approving needs a Touch ID signature, which is not built yet. The server would record
    an unsigned Approve as a denial.
  - **Deny** and a note (a denial with that text) are offered.
- **Drafts stay with their item.** A note typed on a card, or a message typed to a session, belongs
  to that card or session. Changing the selection in the side-by-side layout shows the other item's
  own draft, never carries text over, and coming back restores it.
- **Sessions** lists sessions that need the owner first, then live ones, then the rest, newest first,
  without archived ones. A session shows its status and summary, takes a message (each message
  carries a `message_id`, kept while the text is unchanged so a retry is one message), and opens its transcript in the web
  app in Safari's in-app browser.
- **Sample data.** A build whose origin is a placeholder (`*.example.test`, `*.example`) or empty
  runs on fictional fixtures. Answers and messages then change only the app's copy.

## Not built yet

These are planned (see the iOS plan in the pull request that added `ios/`). None is live:

- signing in with Pocket ID and registering the phone's Secure Enclave keys, so a real origin
  works (today every call from the app would be refused);
- push notifications through APNs, alongside ntfy;
- approving privileged actions with a Touch ID signature that the executor verifies;
- native task views and chat;
- voice: on-device dictation first, then real-time voice with main and top-level supervisors.

## Not proven

The app's tests run on the simulator in CI, with sample data. Nothing has run on an iPhone Duo or
against a Mainloop server.
